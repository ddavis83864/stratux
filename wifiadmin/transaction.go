package wifiadmin

import (
	"errors"
	"fmt"
	"sync"
)

// Stage is one state in the Wi-Fi apply/rollback state machine. Every
// transition is driven synchronously by a call into Manager from main/'s
// HTTP handlers, or by CheckDeadline (called periodically by main/'s own
// ticker, passing in its own monotonic clock reading) - Manager holds no
// timer or goroutine of its own, matching power.Manager's own documented
// approach, so the full lifecycle is exercisable deterministically with
// a fake clock in tests.
type Stage string

const (
	StageIdle                 Stage = "idle"
	StagePreviewed            Stage = "previewed"
	StageApplying             Stage = "applying"
	StageAwaitingReconnection Stage = "awaiting_reconnection"
	StageRollingBack          Stage = "rolling_back"
	StageFailed               Stage = "failed"
	StageRecoveryRequired     Stage = "recovery_required"
)

// LastResult records the outcome of the most recently COMPLETED
// transaction - persists across a return to StageIdle so a status poll
// immediately after a commit/rollback can still show what happened,
// matching power.Manager's own lastError-survives-past-FAILED behavior.
type LastResult string

const (
	ResultNone       LastResult = ""
	ResultCommitted  LastResult = "committed"
	ResultRolledBack LastResult = "rolled_back"
	ResultFailed     LastResult = "failed"
)

var (
	ErrAlreadyInProgress  = errors.New("wifiadmin: a Wi-Fi transaction is already in progress")
	ErrNotAwaitingConfirm = errors.New("wifiadmin: no Wi-Fi change is awaiting reconnection confirmation")
	ErrReconnectTokenBad  = errors.New("wifiadmin: reconnection confirmation token not found, expired, or already used")
	ErrPreconditionFailed = errors.New("wifiadmin: a precondition blocked the Wi-Fi transaction")
	ErrNoPendingPreview   = errors.New("wifiadmin: no preview is pending to cancel")
	ErrCannotCancelNow    = errors.New("wifiadmin: the pending change has already been applied and cannot be cancelled - use rollback instead")
)

// defaultReconnectTimeoutSeconds bounds how long an applied,
// connectivity-affecting change may go unconfirmed before this package
// automatically rolls back to the last known good configuration. This
// value is deliberately conservative and derived, not guessed: it must
// exceed the existing ifdown/ifup wlan0 cycle's own observed latency
// (main/networksettings.go sleeps 1s before ifdown, then runs ifdown and
// ifup each as a blocking exec.Command.Wait() - unbounded in the
// existing code, but a healthy wpa_supplicant/dnsmasq restart on this
// project's target hardware is a low-single-digit-second operation) PLUS
// realistic client-side Wi-Fi reassociation/DHCP-lease time on a
// phone/tablet (commonly 5-20s including the OS's own network-change
// settling delay) PLUS margin for a human to notice the SSID changed and
// switch to it manually. 90 seconds is chosen to comfortably cover a
// human reconnecting to a renamed/re-secured AP by hand, without leaving
// a badly misconfigured AP unreachably live for so long that a second,
// unrelated attempt to fix it (e.g. physical console access) becomes the
// only realistic recourse. It is a package-level var, not a const, and
// is also directly settable per-Manager via SetReconnectTimeoutSeconds,
// specifically so tests can use a tiny deadline instead of waiting on a
// real clock.
var defaultReconnectTimeoutSeconds float64 = 90

// Precondition is a caller-supplied check that must pass (return nil)
// before a Wi-Fi transaction may be previewed or applied - main/ wires
// in the OTA-busy, Configuration-Restore-busy, and shutdown-pending
// checks here, so this package never needs to import those subsystems.
// Mirrors power.Precondition exactly.
type Precondition func() error

// Executor performs the actual, disruptive steps of applying a Wi-Fi
// configuration: writing the derived wpa_supplicant/wpa_supplicant_ap/
// dnsmasq/interfaces files and restarting the affected network
// interface. The real implementation (main/) wraps
// main/networksettings.go's existing writeTemplate/ifdown/ifup
// machinery; every test in this package and main/wifiadminapi_test.go
// injects a fake that only records what it was asked to do, so no
// automated test run ever touches a real network interface.
type Executor interface {
	// Apply makes cfg the live configuration. An error here means the
	// attempt is considered to have failed outright (e.g. a config file
	// could not be written, or a required service failed to start) -
	// Manager treats this as needing an immediate rollback attempt,
	// never a "wait and see."
	Apply(cfg Config) error
	// HealthCheck reports a best-effort, single point-in-time
	// observation of whether cfg actually appears to be the live
	// configuration (e.g. the AP interface exists and carries cfg's own
	// address) - it never blocks waiting for a client to reconnect and
	// is not itself proof of reconnection (see ConfirmReconnection's own
	// doc comment for why only an actual confirm request over the new
	// network can prove that).
	HealthCheck(cfg Config) (Health, error)
}

// Health is Executor.HealthCheck's result.
type Health struct {
	InterfacePresent bool   `json:"interfacePresent"`
	InterfaceAddress string `json:"interfaceAddress"`
	AddressMatches   bool   `json:"addressMatches"`
	Detail           string `json:"detail,omitempty"`
}

// PendingTransactionRecord is the durable record Persistence must be
// able to save/load/clear so a crash or reboot mid-transaction can be
// recovered safely at the next startup - see Manager's own doc comment
// on NewManager for exactly how it is used.
type PendingTransactionRecord struct {
	BootSessionID  string `json:"bootSessionId"`
	ProposedConfig Config `json:"proposedConfig"`
	PreviousConfig Config `json:"previousConfig"`
	Stage          Stage  `json:"stage"`
}

// Persistence is this package's storage boundary - the real
// implementation (main/) follows this project's own established
// temp-file+fsync+atomic-rename pattern (see main/alertsettings.go's
// saveAlertSettings, quoted in docs/wifi-administration-hardening.md).
// Every method must be safe to call from tests with a fake backed by an
// in-memory map or a temp directory.
type Persistence interface {
	SaveLastKnownGood(cfg Config) error
	// LoadLastKnownGood returns ok=false (never an error) if nothing has
	// ever been persisted - the caller falls back to DefaultConfig(),
	// which is this project's own existing, already-deployed default,
	// so a fresh install or one that has never used this feature is
	// unaffected.
	LoadLastKnownGood() (cfg Config, ok bool, err error)
	SavePendingTransaction(rec PendingTransactionRecord) error
	LoadPendingTransaction() (rec PendingTransactionRecord, ok bool, err error)
	ClearPendingTransaction() error
}

// Manager runs the Wi-Fi preview/apply/confirm/rollback state machine.
// It holds no timers or goroutines of its own; every transition happens
// synchronously inside a call from main/'s HTTP handlers or ticker, so
// the full lifecycle is exercisable deterministically in a unit test
// with fakes and a fixed clock - the same design as power.Manager.
type Manager struct {
	mu    sync.Mutex
	stage Stage

	lastKnownGood Config

	pendingToken    *ConfirmationToken
	pendingProposed Config
	previousConfig  Config

	reconnectToken             string
	reconnectTokenUsed         bool
	reconnectDeadlineMonotonic float64

	lastError  string
	lastResult LastResult

	nowMonotonic            func() float64
	bootSessionID           string
	executor                Executor
	persistence             Persistence
	preconditions           []Precondition
	tokenTTLSeconds         float64
	reconnectTimeoutSeconds float64
}

const defaultTokenTTLSeconds = 300 // matches configbackup's own 300s apply-preview TTL

// NewManager constructs a Manager, loading the last known good
// configuration from persistence (or this project's own existing
// default if none has ever been saved) and performing startup recovery:
// if persistence holds a PendingTransactionRecord, this process crashed
// or was restarted mid-transaction. Since every outstanding
// confirmation token is boot-session-bound (see VerifyToken), no token
// issued before this restart can ever be presented successfully again -
// there is no way to obtain a legitimate ConfirmReconnection for that
// abandoned transaction, so the only safe action is to immediately roll
// back to the transaction's own recorded PreviousConfig. If that
// rollback itself fails, Manager starts in StageRecoveryRequired rather
// than silently accepting the half-applied (or unknown) state as
// "last known good" - never, per this package's own mandate, a silent
// upgrade of an unconfirmed configuration into a trusted one.
func NewManager(bootSessionID string, nowMonotonic func() float64, executor Executor, persistence Persistence, preconditions []Precondition) (*Manager, error) {
	m := &Manager{
		stage:                   StageIdle,
		nowMonotonic:            nowMonotonic,
		bootSessionID:           bootSessionID,
		executor:                executor,
		persistence:             persistence,
		preconditions:           preconditions,
		tokenTTLSeconds:         defaultTokenTTLSeconds,
		reconnectTimeoutSeconds: defaultReconnectTimeoutSeconds,
	}

	good, ok, err := persistence.LoadLastKnownGood()
	if err != nil {
		return nil, fmt.Errorf("wifiadmin: could not load last known good configuration: %w", err)
	}
	if !ok {
		good = DefaultConfig()
	}
	m.lastKnownGood = good

	rec, ok, err := persistence.LoadPendingTransaction()
	if err != nil {
		return nil, fmt.Errorf("wifiadmin: could not load pending transaction: %w", err)
	}
	if !ok {
		return m, nil
	}

	// A pending transaction survived a restart - recover by rolling back
	// to its own recorded PreviousConfig, never by trusting
	// ProposedConfig or the just-loaded lastKnownGood blindly.
	if err := executor.Apply(rec.PreviousConfig); err != nil {
		m.stage = StageRecoveryRequired
		m.lastError = "startup recovery: rollback to previous configuration failed: " + err.Error()
		// Deliberately keep the pending record - it is the only record
		// of what was attempted, needed for manual diagnosis.
		return m, nil
	}
	m.lastKnownGood = rec.PreviousConfig
	if err := persistence.SaveLastKnownGood(rec.PreviousConfig); err != nil {
		m.stage = StageRecoveryRequired
		m.lastError = "startup recovery: rolled back live configuration but could not persist it as last known good: " + err.Error()
		return m, nil
	}
	if err := persistence.ClearPendingTransaction(); err != nil {
		m.stage = StageRecoveryRequired
		m.lastError = "startup recovery: rolled back but could not clear the pending-transaction record: " + err.Error()
		return m, nil
	}
	m.stage = StageIdle
	m.lastResult = ResultRolledBack
	m.lastError = "startup recovery: an unconfirmed Wi-Fi change from before the last restart was automatically rolled back"
	return m, nil
}

// SetReconnectTimeoutSeconds overrides the default deadline - tests use
// a small value; main/ leaves the conservative default in place unless
// an owner-configurable override is added later (not part of this
// mission's scope).
func (m *Manager) SetReconnectTimeoutSeconds(seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconnectTimeoutSeconds = seconds
}

// ManagerStatus is Status's read-only snapshot.
type ManagerStatus struct {
	Stage                      Stage      `json:"stage"`
	LastResult                 LastResult `json:"lastResult"`
	LastError                  string     `json:"lastError,omitempty"`
	LastKnownGood              Redacted   `json:"lastKnownGood"`
	PendingProposed            *Redacted  `json:"pendingProposed,omitempty"`
	ReconnectDeadlineMonotonic *float64   `json:"reconnectDeadlineMonotonic,omitempty"`
	ReconnectTokenAvailable    bool       `json:"reconnectTokenAvailable"`
}

// Status returns a full, redacted snapshot - safe to serialize directly
// into an HTTP response; never contains a real passphrase.
func (m *Manager) Status() ManagerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := ManagerStatus{
		Stage:         m.stage,
		LastResult:    m.lastResult,
		LastError:     m.lastError,
		LastKnownGood: m.lastKnownGood.Redact(),
	}
	if m.stage == StagePreviewed || m.stage == StageApplying || m.stage == StageAwaitingReconnection {
		r := m.pendingProposed.Redact()
		s.PendingProposed = &r
	}
	if m.stage == StageAwaitingReconnection {
		d := m.reconnectDeadlineMonotonic
		s.ReconnectDeadlineMonotonic = &d
		s.ReconnectTokenAvailable = m.reconnectToken != "" && !m.reconnectTokenUsed
	}
	return s
}

// ReconnectToken returns the current reconnection-confirmation token,
// only while one is outstanding (StageAwaitingReconnection) - separate
// from ManagerStatus so a caller can deliberately choose whether to
// expose the token value itself (e.g. only over the newly-applied
// network, never in a general status broadcast) versus just the boolean
// ReconnectTokenAvailable.
func (m *Manager) ReconnectToken() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stage != StageAwaitingReconnection || m.reconnectTokenUsed {
		return "", false
	}
	return m.reconnectToken, true
}

func (m *Manager) runPreconditionsLocked() error {
	for _, p := range m.preconditions {
		if p == nil {
			continue
		}
		if err := p(); err != nil {
			return fmt.Errorf("%w: %s", ErrPreconditionFailed, err.Error())
		}
	}
	return nil
}

// Preview validates proposed, diffs it against the last known good
// configuration, and - if there is anything to change - issues a fresh,
// single-use confirmation token bound to both the exact proposed
// configuration and the exact current baseline (see VerifyToken). It
// never touches live configuration. Calling Preview again while already
// in StagePreviewed is allowed and simply replaces any prior unconfirmed
// token, exactly matching power.Manager.RequestConfirmation's own
// documented re-callable behavior.
func (m *Manager) Preview(proposed Config, newToken func() string) (Preview, ConfirmationToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stage != StageIdle && m.stage != StagePreviewed && m.stage != StageFailed {
		return Preview{}, ConfirmationToken{}, ErrAlreadyInProgress
	}
	if err := m.runPreconditionsLocked(); err != nil {
		return Preview{}, ConfirmationToken{}, err
	}
	if err := proposed.Validate(); err != nil {
		return Preview{}, ConfirmationToken{}, err
	}

	preview := ComputePreview(m.lastKnownGood, proposed)

	proposedChecksum, err := ChecksumConfig(proposed)
	if err != nil {
		return Preview{}, ConfirmationToken{}, fmt.Errorf("wifiadmin: could not checksum proposed configuration: %w", err)
	}
	currentFingerprint, err := ChecksumConfig(m.lastKnownGood)
	if err != nil {
		return Preview{}, ConfirmationToken{}, fmt.Errorf("wifiadmin: could not fingerprint current configuration: %w", err)
	}

	now := m.nowMonotonic()
	tok := ConfirmationToken{
		Token:                    newToken(),
		ProposedConfigChecksum:   proposedChecksum,
		CurrentConfigFingerprint: currentFingerprint,
		BootSessionID:            m.bootSessionID,
		IssuedAtMonotonic:        now,
		ExpiresAtMonotonic:       now + m.tokenTTLSeconds,
	}
	m.pendingToken = &tok
	m.pendingProposed = proposed
	m.previousConfig = m.lastKnownGood
	m.stage = StagePreviewed
	m.lastError = ""
	return preview, tok, nil
}

// Cancel discards a pending, not-yet-applied preview. It is a no-op
// error (ErrCannotCancelNow) once Apply has already started - by then
// the network is already disrupted and only ConfirmReconnection or
// RequestRollback can move the transaction forward.
func (m *Manager) Cancel() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.stage {
	case StageIdle, StageFailed:
		return ErrNoPendingPreview
	case StagePreviewed:
		m.pendingToken = nil
		m.pendingProposed = Config{}
		m.previousConfig = Config{}
		m.stage = StageIdle
		return nil
	default:
		return ErrCannotCancelNow
	}
}

// Apply consumes token (issued by Preview) and, if it still matches
// exactly the proposed configuration and current baseline it was issued
// for, performs the actual disruptive configuration write and service
// restart via Executor.Apply. On success, it issues a NEW, separate
// reconnection-confirmation token (never the same spent token - see the
// package doc comment on why apply and reconnect-confirm are
// deliberately two distinct tokens) and moves to
// StageAwaitingReconnection with a deadline
// reconnectTimeoutSeconds from now. On failure, it immediately attempts
// an automatic rollback to the previous configuration - see
// rollbackLocked.
func (m *Manager) Apply(presentedToken string, newReconnectToken func() string) (Stage, error) {
	m.mu.Lock()
	if m.pendingToken == nil || m.pendingToken.Token != presentedToken {
		stage := m.stage
		m.mu.Unlock()
		return stage, ErrTokenNotFound
	}
	tok := *m.pendingToken
	proposedChecksum, err := ChecksumConfig(m.pendingProposed)
	if err != nil {
		m.mu.Unlock()
		return m.stage, fmt.Errorf("wifiadmin: could not checksum pending configuration: %w", err)
	}
	currentFingerprint, err := ChecksumConfig(m.previousConfig)
	if err != nil {
		m.mu.Unlock()
		return m.stage, fmt.Errorf("wifiadmin: could not fingerprint baseline configuration: %w", err)
	}
	if err := VerifyToken(tok, proposedChecksum, currentFingerprint, m.bootSessionID, m.nowMonotonic()); err != nil {
		stage := m.stage
		m.mu.Unlock()
		return stage, err
	}
	if m.stage != StagePreviewed {
		stage := m.stage
		m.mu.Unlock()
		return stage, ErrAlreadyInProgress
	}
	if err := m.runPreconditionsLocked(); err != nil {
		m.mu.Unlock()
		return StagePreviewed, err
	}

	// Mark used and advance stage before releasing the lock, so a
	// concurrent second Apply call racing on a double-submitted request
	// always observes Used=true and gets ErrTokenAlreadyUsed instead of
	// also running the executor.
	m.pendingToken.Used = true
	m.stage = StageApplying
	proposed := m.pendingProposed
	previous := m.previousConfig
	m.mu.Unlock()

	// Persist the pending transaction BEFORE the actually-disruptive
	// executor call, so a crash mid-apply is recoverable at next startup
	// (see NewManager's own doc comment) - the record always names
	// `previous`, never `proposed`, as the safe rollback target.
	if err := m.persistence.SavePendingTransaction(PendingTransactionRecord{
		BootSessionID:  m.bootSessionID,
		ProposedConfig: proposed,
		PreviousConfig: previous,
		Stage:          StageApplying,
	}); err != nil {
		m.setFailedLocked("could not persist pending transaction before applying: " + err.Error())
		return StageFailed, err
	}

	if err := m.executor.Apply(proposed); err != nil {
		return m.rollbackAfterFailedApply("apply failed: " + err.Error())
	}

	reconnectTok := newReconnectToken()
	m.mu.Lock()
	m.reconnectToken = reconnectTok
	m.reconnectTokenUsed = false
	m.reconnectDeadlineMonotonic = m.nowMonotonic() + m.reconnectTimeoutSeconds
	m.stage = StageAwaitingReconnection
	m.lastError = ""
	m.mu.Unlock()

	// Update the persisted record to reflect the new stage, so startup
	// recovery after a crash HERE (already applied, awaiting
	// reconnection) still knows to roll back to `previous`, not to
	// half-trust `proposed`.
	_ = m.persistence.SavePendingTransaction(PendingTransactionRecord{
		BootSessionID:  m.bootSessionID,
		ProposedConfig: proposed,
		PreviousConfig: previous,
		Stage:          StageAwaitingReconnection,
	})

	return StageAwaitingReconnection, nil
}

// rollbackAfterFailedApply is Apply's own failure path: an immediate,
// synchronous rollback attempt (no point waiting for a deadline - the
// apply itself already failed) - shared logic with CheckDeadline/
// RequestRollback's own rollback attempt via rollbackLocked's sibling
// below, kept separate here only because this path's caller (Apply) has
// already released the lock and captured `previous` locally.
func (m *Manager) rollbackAfterFailedApply(reason string) (Stage, error) {
	m.mu.Lock()
	previous := m.previousConfig
	m.stage = StageRollingBack
	m.mu.Unlock()

	if err := m.executor.Apply(previous); err != nil {
		m.mu.Lock()
		m.stage = StageRecoveryRequired
		m.lastError = reason + "; automatic rollback ALSO failed: " + err.Error()
		m.mu.Unlock()
		return StageRecoveryRequired, errors.New(reason + "; automatic rollback also failed: " + err.Error())
	}
	if err := m.persistence.ClearPendingTransaction(); err != nil {
		m.mu.Lock()
		m.stage = StageRecoveryRequired
		m.lastError = reason + "; rolled back but could not clear the pending-transaction record: " + err.Error()
		m.mu.Unlock()
		return StageRecoveryRequired, err
	}
	m.mu.Lock()
	m.stage = StageIdle
	m.lastResult = ResultFailed
	m.lastError = reason + "; automatically rolled back to the previous configuration"
	m.pendingToken = nil
	m.mu.Unlock()
	return StageIdle, errors.New(reason)
}

func (m *Manager) setFailedLocked(reason string) {
	m.mu.Lock()
	m.stage = StageFailed
	m.lastError = reason
	m.mu.Unlock()
}

// ConfirmReconnection is presented by a client that has reconnected to
// the just-applied network and reached this same running daemon over
// it - the strongest confirmation this architecture can offer that the
// new configuration is actually reachable (see the package doc comment
// on why GDL90/ForeFlight-application-level identity is never claimed
// here: only "an HTTP client reached the management API" is proven).
// token must be the SEPARATE reconnect token Apply issued - never the
// original preview/apply token, which is already spent by then. On
// success, proposed becomes the new last known good, persisted, and the
// pending transaction record is cleared.
func (m *Manager) ConfirmReconnection(token string) (Stage, error) {
	m.mu.Lock()
	if m.stage != StageAwaitingReconnection {
		stage := m.stage
		m.mu.Unlock()
		return stage, ErrNotAwaitingConfirm
	}
	if m.reconnectTokenUsed || token == "" || token != m.reconnectToken {
		m.mu.Unlock()
		return StageAwaitingReconnection, ErrReconnectTokenBad
	}
	if m.nowMonotonic() > m.reconnectDeadlineMonotonic {
		m.mu.Unlock()
		return StageAwaitingReconnection, ErrReconnectTokenBad
	}
	m.reconnectTokenUsed = true
	proposed := m.pendingProposed
	m.mu.Unlock()

	if err := m.persistence.SaveLastKnownGood(proposed); err != nil {
		m.setFailedAwaitingLocked("confirmed but could not persist as last known good: " + err.Error())
		return StageFailed, err
	}
	if err := m.persistence.ClearPendingTransaction(); err != nil {
		m.setFailedAwaitingLocked("confirmed and persisted, but could not clear the pending-transaction record: " + err.Error())
		return StageFailed, err
	}

	m.mu.Lock()
	m.lastKnownGood = proposed
	m.stage = StageIdle
	m.lastResult = ResultCommitted
	m.lastError = ""
	m.pendingToken = nil
	m.pendingProposed = Config{}
	m.previousConfig = Config{}
	m.mu.Unlock()
	return StageIdle, nil
}

func (m *Manager) setFailedAwaitingLocked(reason string) {
	m.mu.Lock()
	m.stage = StageFailed
	m.lastError = reason
	m.mu.Unlock()
}

// CheckDeadline is called periodically by main/'s own ticker (passing
// its own current monotonic reading) - if a transaction has been
// awaiting reconnection past its deadline, it is automatically rolled
// back. Safe to call at any stage or arbitrarily often; it is a no-op
// unless StageAwaitingReconnection's own deadline has actually passed.
func (m *Manager) CheckDeadline(now float64) {
	m.mu.Lock()
	if m.stage != StageAwaitingReconnection || now <= m.reconnectDeadlineMonotonic {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.RequestRollback()
}

// RequestRollback performs an immediate rollback to the last known good
// (previous) configuration - callable manually while
// StageAwaitingReconnection (an owner who realizes the new network is
// unreachable, rather than waiting for the deadline) or
// StageRecoveryRequired (a retry after fixing whatever made the first
// rollback attempt fail). It is a no-op error otherwise.
func (m *Manager) RequestRollback() (Stage, error) {
	m.mu.Lock()
	if m.stage != StageAwaitingReconnection && m.stage != StageRecoveryRequired {
		stage := m.stage
		m.mu.Unlock()
		return stage, errors.New("wifiadmin: no rollback is applicable in the current stage")
	}
	previous := m.previousConfig
	m.stage = StageRollingBack
	m.mu.Unlock()

	if err := m.executor.Apply(previous); err != nil {
		m.mu.Lock()
		m.stage = StageRecoveryRequired
		m.lastError = "rollback failed: " + err.Error()
		m.mu.Unlock()
		return StageRecoveryRequired, err
	}
	if err := m.persistence.ClearPendingTransaction(); err != nil {
		m.mu.Lock()
		m.stage = StageRecoveryRequired
		m.lastError = "rolled back but could not clear the pending-transaction record: " + err.Error()
		m.mu.Unlock()
		return StageRecoveryRequired, err
	}

	m.mu.Lock()
	m.stage = StageIdle
	m.lastResult = ResultRolledBack
	m.lastError = ""
	m.pendingToken = nil
	m.pendingProposed = Config{}
	m.previousConfig = Config{}
	m.reconnectToken = ""
	m.mu.Unlock()
	return StageIdle, nil
}
