package power

import (
	"errors"
	"fmt"
	"sync"
)

// Stage is one state in the controlled-shutdown state machine. Every
// transition is driven synchronously by a call into Manager from main/'s
// HTTP handlers - there is no background timer advancing this on its
// own, and the only way to reach COMMAND_ISSUED is two separate,
// explicit, human-initiated requests (RequestConfirmation, then Confirm
// with the token RequestConfirmation returned).
type Stage string

const (
	StageIdle                 Stage = "idle"
	StageConfirmationRequired Stage = "confirmation_required"
	StageShutdownRequested    Stage = "shutdown_requested"
	StageFlushing             Stage = "flushing"
	StageReadyToPowerOff      Stage = "ready_to_power_off"
	StageCommandIssued        Stage = "command_issued"
	StageFailed               Stage = "failed"
)

var (
	ErrTokenNotFound           = errors.New("power: shutdown confirmation token not found or does not match")
	ErrTokenUsed               = errors.New("power: shutdown confirmation token already used")
	ErrTokenExpired            = errors.New("power: shutdown confirmation token expired")
	ErrTokenBootSessionChanged = errors.New("power: daemon restarted since the shutdown confirmation was issued")
	ErrPreconditionFailed      = errors.New("power: a precondition blocked the shutdown request")
	ErrAlreadyInProgress       = errors.New("power: a shutdown is already past the confirmation step")
)

// ConfirmationToken is the server-held record behind one outstanding
// shutdown confirmation - the same boot-session-bound, monotonic-expiry,
// single-use-flag shape as configbackup.ConfirmationToken, applied here to
// a much simpler binding (no uploaded-document checksum, since a shutdown
// request carries no content of its own).
type ConfirmationToken struct {
	Token              string
	BootSessionID      string
	IssuedAtMonotonic  float64
	ExpiresAtMonotonic float64
	Used               bool
}

// VerifyToken checks presented against everything a confirm request must
// still match. It mutates nothing - marking a token Used is the caller's
// responsibility, done under Manager's own lock in Confirm.
func VerifyToken(t ConfirmationToken, bootSessionID string, nowMonotonic float64) error {
	if t.Used {
		return ErrTokenUsed
	}
	if nowMonotonic > t.ExpiresAtMonotonic {
		return ErrTokenExpired
	}
	if t.BootSessionID != bootSessionID {
		return ErrTokenBootSessionChanged
	}
	return nil
}

// Executor performs the actual, irreversible steps of a controlled
// shutdown/reboot. The real implementation (main/) calls syscall.Sync()
// and exec.Command("systemctl", "poweroff"/"reboot"); every test in this
// package and main/powerapi_test.go injects a fake that only records that
// it was called, so no automated test run ever powers off or reboots the
// machine it runs on.
type Executor interface {
	Sync() error
	PowerOff() error
}

// Flusher lets the caller hook in subsystem-specific flush-before-
// shutdown behavior (e.g. stopping an active recording) without this
// package importing anything stateful. A nil Flusher is treated as
// "nothing to flush."
type Flusher func() error

// Precondition is a caller-supplied check that must pass (return nil)
// before a shutdown may be requested or confirmed. main/ wires in the
// OTA-busy and configuration-restore-busy checks here, so this package
// never needs to import ota or the configbackup glue.
type Precondition func() error

// defaultTokenTTLSeconds bounds how long an issued-but-unconfirmed
// shutdown confirmation stays valid - long enough for a human to read a
// confirmation dialog and click through it, short enough that a token
// left over from an accidental click cannot be used much later.
const defaultTokenTTLSeconds = 120

// Manager runs the controlled-shutdown state machine. It holds no timers
// or goroutines of its own; every transition happens synchronously inside
// a call from main/'s HTTP handlers, driven by an injected Executor,
// Flusher, Preconditions, and a monotonic-seconds function - so the full
// lifecycle can be exercised deterministically in a unit test with fakes
// and a fixed clock.
type Manager struct {
	mu              sync.Mutex
	stage           Stage
	pendingToken    *ConfirmationToken
	lastError       string
	nowMonotonic    func() float64
	bootSessionID   string
	preconditions   []Precondition
	flush           Flusher
	executor        Executor
	tokenTTLSeconds float64
}

// NewManager constructs an idle Manager. bootSessionID binds every issued
// token to this process's lifetime (a restart invalidates every
// outstanding token, even an unexpired one) - the same binding
// configbackup's tokens already use, reusing main/'s existing
// preflightSessionID is the expected wiring (see docs/power-shutdown-
// resilience.md).
func NewManager(bootSessionID string, nowMonotonic func() float64, preconditions []Precondition, flush Flusher, executor Executor) *Manager {
	return &Manager{
		stage:           StageIdle,
		nowMonotonic:    nowMonotonic,
		bootSessionID:   bootSessionID,
		preconditions:   preconditions,
		flush:           flush,
		executor:        executor,
		tokenTTLSeconds: defaultTokenTTLSeconds,
	}
}

// Status returns the current stage and, if the last attempt failed, why.
func (m *Manager) Status() (Stage, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stage, m.lastError
}

// RequestConfirmation is step one of the two-step flow: it runs every
// precondition and, if all pass, issues a fresh single-use token and
// moves to CONFIRMATION_REQUIRED. It never touches system state - only
// read-only preconditions run here (by contract; this package cannot
// enforce that on the caller's own closures) - so it is always safe to
// call, including to refresh an expired or about-to-expire token: calling
// it again while already in CONFIRMATION_REQUIRED or after a FAILED
// attempt is allowed and simply issues a new token, silently invalidating
// any prior unconfirmed one.
func (m *Manager) RequestConfirmation(newToken func() string) (ConfirmationToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stage != StageIdle && m.stage != StageConfirmationRequired && m.stage != StageFailed {
		return ConfirmationToken{}, ErrAlreadyInProgress
	}
	for _, p := range m.preconditions {
		if p == nil {
			continue
		}
		if err := p(); err != nil {
			return ConfirmationToken{}, fmt.Errorf("%w: %s", ErrPreconditionFailed, err.Error())
		}
	}
	now := m.nowMonotonic()
	tok := ConfirmationToken{
		Token:              newToken(),
		BootSessionID:      m.bootSessionID,
		IssuedAtMonotonic:  now,
		ExpiresAtMonotonic: now + m.tokenTTLSeconds,
	}
	m.pendingToken = &tok
	m.stage = StageConfirmationRequired
	m.lastError = ""
	return tok, nil
}

// Confirm is step two: given the exact token string RequestConfirmation
// returned, it re-checks preconditions (state may have changed since
// preview - e.g. an OTA could have started), then synchronously runs
// flush -> sync, ending at COMMAND_ISSUED on success or FAILED on error.
//
// Confirm deliberately stops at COMMAND_ISSUED and does not itself call
// PowerOff: the mission requirement is that the HTTP response describing
// success is sent to the client before the device actually powers off,
// and the only way this package can guarantee that ordering is to make
// "issue the actual power-off command" a separate, later call
// (IssuePowerOff) that the HTTP handler makes only after writing its
// response. See main/powerapi.go's handleConfirmShutdownRequest for the
// exact sequencing this enables.
func (m *Manager) Confirm(presentedToken string) (Stage, error) {
	m.mu.Lock()
	// The token match/validity check runs before the stage check and
	// looks at m.pendingToken regardless of the current stage (it is
	// never cleared except by Reset or a fresh RequestConfirmation) so a
	// token presented a second time - whether racing concurrently against
	// its own first use, or replayed later after the flow has already
	// moved on to FLUSHING/COMMAND_ISSUED/FAILED - is reported as the
	// specific, meaningful ErrTokenUsed rather than a generic "not
	// found," which would be true but less useful to a client trying to
	// understand what happened to its own request.
	if m.pendingToken == nil || m.pendingToken.Token != presentedToken {
		stage := m.stage
		m.mu.Unlock()
		return stage, ErrTokenNotFound
	}
	tok := *m.pendingToken
	if err := VerifyToken(tok, m.bootSessionID, m.nowMonotonic()); err != nil {
		stage := m.stage
		m.mu.Unlock()
		return stage, err
	}
	if m.stage != StageConfirmationRequired {
		stage := m.stage
		m.mu.Unlock()
		return stage, ErrAlreadyInProgress
	}
	for _, p := range m.preconditions {
		if p == nil {
			continue
		}
		if err := p(); err != nil {
			m.mu.Unlock()
			return StageConfirmationRequired, fmt.Errorf("%w: %s", ErrPreconditionFailed, err.Error())
		}
	}
	// Mark the token used and advance past CONFIRMATION_REQUIRED before
	// releasing the lock, so a second concurrent Confirm call (racing on
	// a double-submitted request) always sees Used=true and gets
	// ErrTokenUsed instead of also running the flush/sync sequence.
	m.pendingToken.Used = true
	m.stage = StageShutdownRequested
	m.mu.Unlock()

	m.setStage(StageFlushing)
	if m.flush != nil {
		if err := m.flush(); err != nil {
			m.setFailed("flush failed: " + err.Error())
			return StageFailed, fmt.Errorf("flush failed: %w", err)
		}
	}

	m.setStage(StageReadyToPowerOff)
	if err := m.executor.Sync(); err != nil {
		m.setFailed("sync failed: " + err.Error())
		return StageFailed, fmt.Errorf("sync failed: %w", err)
	}

	m.setStage(StageCommandIssued)
	return StageCommandIssued, nil
}

// IssuePowerOff performs the actual, final, irreversible power-off. Call
// it only after Confirm has returned StageCommandIssued and the caller's
// HTTP response has already been written - see Confirm's doc comment.
func (m *Manager) IssuePowerOff() error {
	return m.executor.PowerOff()
}

// Reset returns the Manager to IDLE, clearing any pending token and last
// error - used after a FAILED outcome so a fresh attempt can be made.
// Never used to interrupt an in-progress flush/sync/power-off: those run
// to completion (success or failure) once Confirm has been called, since
// by then the flow has already committed to shutting the device down.
func (m *Manager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stage = StageIdle
	m.pendingToken = nil
	m.lastError = ""
}

func (m *Manager) setStage(s Stage) {
	m.mu.Lock()
	m.stage = s
	m.mu.Unlock()
}

func (m *Manager) setFailed(reason string) {
	m.mu.Lock()
	m.stage = StageFailed
	m.lastError = reason
	m.mu.Unlock()
}
