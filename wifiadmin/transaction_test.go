package wifiadmin

import (
	"errors"
	"sync"
	"testing"
)

// fakeExecutor is an in-memory stand-in for Executor - it never touches
// a real network interface. It can be configured to fail Apply for a
// specific config (by SSID) to exercise failure/rollback paths.
type fakeExecutor struct {
	mu          sync.Mutex
	live        Config
	applyCalls  []Config
	failForSSID string
	failErr     error
}

func newFakeExecutor(initial Config) *fakeExecutor {
	return &fakeExecutor{live: initial}
}

func (f *fakeExecutor) Apply(cfg Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyCalls = append(f.applyCalls, cfg)
	if f.failForSSID != "" && cfg.SSID == f.failForSSID {
		return f.failErr
	}
	f.live = cfg
	return nil
}

func (f *fakeExecutor) HealthCheck(cfg Config) (Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Health{
		InterfacePresent: true,
		InterfaceAddress: f.live.IPAddress,
		AddressMatches:   f.live.IPAddress == cfg.IPAddress,
	}, nil
}

func (f *fakeExecutor) applyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applyCalls)
}

// fakePersistence is an in-memory stand-in for Persistence.
type fakePersistence struct {
	mu              sync.Mutex
	lastKnownGood   *Config
	pending         *PendingTransactionRecord
	failSaveGood    bool
	failSavePending bool
	failClear       bool
}

func (p *fakePersistence) SaveLastKnownGood(cfg Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failSaveGood {
		return errors.New("fake: save last known good failed")
	}
	c := cfg
	p.lastKnownGood = &c
	return nil
}

func (p *fakePersistence) LoadLastKnownGood() (Config, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastKnownGood == nil {
		return Config{}, false, nil
	}
	return *p.lastKnownGood, true, nil
}

func (p *fakePersistence) SavePendingTransaction(rec PendingTransactionRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failSavePending {
		return errors.New("fake: save pending transaction failed")
	}
	r := rec
	p.pending = &r
	return nil
}

func (p *fakePersistence) LoadPendingTransaction() (PendingTransactionRecord, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		return PendingTransactionRecord{}, false, nil
	}
	return *p.pending, true, nil
}

func (p *fakePersistence) ClearPendingTransaction() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failClear {
		return errors.New("fake: clear pending transaction failed")
	}
	p.pending = nil
	return nil
}

// fakeClock is a simple settable monotonic-seconds source.
type fakeClock struct {
	mu  sync.Mutex
	now float64
}

func (c *fakeClock) Now() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(seconds float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += seconds
}

func sequentialTokenGen() func() string {
	n := 0
	return func() string {
		n++
		return "tok-" + itoa(n)
	}
}

func newTestManager(t *testing.T) (*Manager, *fakeExecutor, *fakePersistence, *fakeClock) {
	t.Helper()
	good := validAPConfig()
	good.SSID = "current-ssid"
	exec := newFakeExecutor(good)
	pers := &fakePersistence{}
	if err := pers.SaveLastKnownGood(good); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: 1000}
	m, err := NewManager("boot-1", clock.Now, exec, pers, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.SetReconnectTimeoutSeconds(30)
	return m, exec, pers, clock
}

func TestManager_PreviewApplyConfirm_HappyPath(t *testing.T) {
	m, exec, pers, clock := newTestManager(t)
	newGen := sequentialTokenGen()

	proposed := validAPConfig()
	proposed.SSID = "new-ssid"

	preview, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !preview.HasChanges || !preview.WillDisconnect {
		t.Errorf("expected changes and disconnection: %+v", preview)
	}
	if m.Status().Stage != StagePreviewed {
		t.Errorf("stage = %v, want previewed", m.Status().Stage)
	}

	stage, err := m.Apply(tok.Token, newGen)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stage != StageAwaitingReconnection {
		t.Errorf("stage = %v, want awaiting_reconnection", stage)
	}
	if exec.applyCount() != 1 {
		t.Errorf("expected exactly 1 executor.Apply call, got %d", exec.applyCount())
	}

	reconnectTok, ok := m.ReconnectToken()
	if !ok || reconnectTok == "" {
		t.Fatal("expected a reconnect token to be available")
	}

	stage, err = m.ConfirmReconnection(reconnectTok)
	if err != nil {
		t.Fatalf("ConfirmReconnection: %v", err)
	}
	if stage != StageIdle {
		t.Errorf("stage = %v, want idle", stage)
	}
	status := m.Status()
	if status.LastResult != ResultCommitted {
		t.Errorf("lastResult = %v, want committed", status.LastResult)
	}
	if status.LastKnownGood.SSID != "new-ssid" {
		t.Errorf("last known good SSID = %q, want new-ssid", status.LastKnownGood.SSID)
	}
	good, ok, _ := pers.LoadLastKnownGood()
	if !ok || good.SSID != "new-ssid" {
		t.Error("expected persisted last known good to be updated to the new config")
	}
	if _, ok, _ := pers.LoadPendingTransaction(); ok {
		t.Error("expected the pending transaction record to be cleared after commit")
	}
	_ = clock
}

func TestManager_TokenExpiry(t *testing.T) {
	m, _, _, clock := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(defaultTokenTTLSeconds + 1)
	if _, err := m.Apply(tok.Token, newGen); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("Apply after expiry: err=%v, want ErrTokenExpired", err)
	}
}

func TestManager_TokenMismatch(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	if _, _, err := m.Preview(proposed, newGen); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply("wrong-token", newGen); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("Apply with wrong token: err=%v, want ErrTokenNotFound", err)
	}
}

func TestManager_TokenReuseRejected(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(tok.Token, newGen); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, err := m.Apply(tok.Token, newGen); !errors.Is(err, ErrTokenAlreadyUsed) {
		t.Errorf("second Apply with same token: err=%v, want ErrTokenAlreadyUsed", err)
	}
}

func TestManager_TokenInvalidatedByInterveningStateChange(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	// Cancel and start an entirely different preview - simulates "state
	// changed since preview" (a fresh preview replaces the baseline
	// fingerprint the old token was bound to).
	if err := m.Cancel(); err != nil {
		t.Fatal(err)
	}
	other := validAPConfig()
	other.SSID = "different-ssid"
	if _, _, err := m.Preview(other, newGen); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(tok.Token, newGen); err == nil {
		t.Error("expected the old token to be rejected as not-found after a new preview replaced it")
	}
}

func TestManager_ApplyFailure_AutomaticRollback(t *testing.T) {
	m, exec, pers, _ := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "bad-ssid"
	exec.failForSSID = "bad-ssid"
	exec.failErr = errors.New("simulated: wpa_supplicant failed to start")

	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := m.Apply(tok.Token, newGen)
	if err == nil {
		t.Fatal("expected Apply to report the underlying failure")
	}
	if stage != StageIdle {
		t.Errorf("stage after auto-rollback = %v, want idle", stage)
	}
	status := m.Status()
	if status.LastResult != ResultFailed {
		t.Errorf("lastResult = %v, want failed", status.LastResult)
	}
	if status.LastKnownGood.SSID != "current-ssid" {
		t.Errorf("last known good should remain unchanged: got %q", status.LastKnownGood.SSID)
	}
	if _, ok, _ := pers.LoadPendingTransaction(); ok {
		t.Error("expected pending transaction cleared after successful auto-rollback")
	}
	// Two Apply calls: one for the failed proposed config, one for the
	// rollback to the previous config.
	if exec.applyCount() != 2 {
		t.Errorf("expected 2 executor.Apply calls (attempt + rollback), got %d", exec.applyCount())
	}
}

// TestManager_RollbackAlsoFails_RecoveryRequired exercises the worst
// case: the applied configuration fails AND the automatic rollback
// attempt to restore the previous one also fails - e.g. a genuinely
// wedged network stack. This must surface as StageRecoveryRequired, an
// honest "needs manual intervention" state, never a silent return to
// StageIdle that would misrepresent an unknown live configuration as
// trustworthy.
func TestManager_RollbackAlsoFails_RecoveryRequired(t *testing.T) {
	good := validAPConfig()
	good.SSID = "current-ssid"
	pers := &fakePersistence{}
	if err := pers.SaveLastKnownGood(good); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: 1000}
	always := &alwaysFailExecutor{err: errors.New("simulated: total network stack failure")}
	m, err := NewManager("boot-1", clock.Now, always, pers, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.SetReconnectTimeoutSeconds(30)

	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "bad-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := m.Apply(tok.Token, newGen)
	if err == nil {
		t.Fatal("expected an error when both apply and rollback fail")
	}
	if stage != StageRecoveryRequired {
		t.Errorf("stage = %v, want recovery_required", stage)
	}
	if m.Status().Stage != StageRecoveryRequired {
		t.Errorf("Status().Stage = %v, want recovery_required", m.Status().Stage)
	}
	if _, ok, _ := pers.LoadPendingTransaction(); !ok {
		t.Error("expected the pending transaction record to be preserved when recovery fails, for manual diagnosis")
	}
}

// alwaysFailExecutor fails every Apply call - used only to exercise the
// "rollback itself fails" path.
type alwaysFailExecutor struct {
	err error
}

func (e *alwaysFailExecutor) Apply(cfg Config) error { return e.err }
func (e *alwaysFailExecutor) HealthCheck(cfg Config) (Health, error) {
	return Health{}, nil
}

func TestManager_CancelBeforeApply(t *testing.T) {
	m, exec, _, _ := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	if _, _, err := m.Preview(proposed, newGen); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if m.Status().Stage != StageIdle {
		t.Errorf("stage after cancel = %v, want idle", m.Status().Stage)
	}
	if exec.applyCount() != 0 {
		t.Error("Cancel before Apply must never touch the executor")
	}
}

func TestManager_CancelDuringProhibitedPhaseRejected(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(tok.Token, newGen); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(); !errors.Is(err, ErrCannotCancelNow) {
		t.Errorf("Cancel while awaiting reconnection: err=%v, want ErrCannotCancelNow", err)
	}
}

func TestManager_ConcurrentApplyAttempts(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = m.Apply(tok.Token, newGen)
		}(i)
	}
	wg.Wait()

	successCount := 0
	for _, err := range results {
		if err == nil {
			successCount++
		}
	}
	if successCount != 1 {
		t.Errorf("expected exactly 1 successful Apply among 10 concurrent attempts, got %d", successCount)
	}
}

func TestManager_MissingConfirmationTriggersAutomaticRollback(t *testing.T) {
	m, exec, pers, clock := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(tok.Token, newGen); err != nil {
		t.Fatal(err)
	}
	if m.Status().Stage != StageAwaitingReconnection {
		t.Fatal("expected awaiting_reconnection")
	}

	// Deadline is 30s out; advance past it without ever confirming.
	clock.Advance(31)
	m.CheckDeadline(clock.Now())

	status := m.Status()
	if status.Stage != StageIdle {
		t.Errorf("stage after deadline miss = %v, want idle (auto-rolled-back)", status.Stage)
	}
	if status.LastResult != ResultRolledBack {
		t.Errorf("lastResult = %v, want rolled_back", status.LastResult)
	}
	if status.LastKnownGood.SSID != "current-ssid" {
		t.Errorf("expected rollback to the original SSID, got %q", status.LastKnownGood.SSID)
	}
	if exec.applyCount() != 2 {
		t.Errorf("expected 2 executor.Apply calls (apply + rollback), got %d", exec.applyCount())
	}
	if _, ok, _ := pers.LoadPendingTransaction(); ok {
		t.Error("expected pending transaction cleared after automatic rollback")
	}
}

func TestManager_CheckDeadlineBeforeDeadlineIsNoOp(t *testing.T) {
	m, exec, _, clock := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(tok.Token, newGen); err != nil {
		t.Fatal(err)
	}
	clock.Advance(5) // well before the 30s deadline
	m.CheckDeadline(clock.Now())
	if m.Status().Stage != StageAwaitingReconnection {
		t.Error("CheckDeadline before the deadline must not roll back")
	}
	if exec.applyCount() != 1 {
		t.Error("CheckDeadline before the deadline must not call the executor again")
	}
}

func TestManager_ExplicitRequestRollback(t *testing.T) {
	m, exec, _, _ := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(tok.Token, newGen); err != nil {
		t.Fatal(err)
	}
	stage, err := m.RequestRollback()
	if err != nil {
		t.Fatalf("RequestRollback: %v", err)
	}
	if stage != StageIdle {
		t.Errorf("stage = %v, want idle", stage)
	}
	if m.Status().LastKnownGood.SSID != "current-ssid" {
		t.Error("expected manual rollback to restore the original SSID")
	}
	if exec.applyCount() != 2 {
		t.Errorf("expected 2 executor.Apply calls, got %d", exec.applyCount())
	}
}

func TestManager_ReconnectTokenMismatchRejected(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(tok.Token, newGen); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConfirmReconnection("wrong-reconnect-token"); !errors.Is(err, ErrReconnectTokenBad) {
		t.Errorf("ConfirmReconnection with wrong token: err=%v, want ErrReconnectTokenBad", err)
	}
	if m.Status().Stage != StageAwaitingReconnection {
		t.Error("a bad reconnect token must not disturb the pending transaction")
	}
}

func TestManager_ReconnectTokenReuseRejected(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(tok.Token, newGen); err != nil {
		t.Fatal(err)
	}
	reconnectTok, _ := m.ReconnectToken()
	if _, err := m.ConfirmReconnection(reconnectTok); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	// Start a whole new transaction so we're not just testing
	// "wrong stage" - reuse of the OLD token string must fail even if
	// presented while idle.
	if _, err := m.ConfirmReconnection(reconnectTok); err == nil {
		t.Error("expected reconnect-token reuse to be rejected")
	}
}

func TestManager_ConfirmBeforeApplyRejected(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	if _, err := m.ConfirmReconnection("anything"); !errors.Is(err, ErrNotAwaitingConfirm) {
		t.Errorf("ConfirmReconnection with nothing pending: err=%v, want ErrNotAwaitingConfirm", err)
	}
}

func TestManager_StartupRecovery_PendingTransactionRolledBack(t *testing.T) {
	good := validAPConfig()
	good.SSID = "original-ssid"
	bad := validAPConfig()
	bad.SSID = "abandoned-ssid"

	exec := newFakeExecutor(bad) // simulate: process died with `bad` live
	pers := &fakePersistence{}
	if err := pers.SaveLastKnownGood(good); err != nil {
		t.Fatal(err)
	}
	if err := pers.SavePendingTransaction(PendingTransactionRecord{
		BootSessionID:  "old-boot-session",
		ProposedConfig: bad,
		PreviousConfig: good,
		Stage:          StageAwaitingReconnection,
	}); err != nil {
		t.Fatal(err)
	}

	clock := &fakeClock{now: 1000}
	m, err := NewManager("new-boot-session", clock.Now, exec, pers, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	status := m.Status()
	if status.Stage != StageIdle {
		t.Errorf("stage after startup recovery = %v, want idle", status.Stage)
	}
	if status.LastResult != ResultRolledBack {
		t.Errorf("lastResult = %v, want rolled_back", status.LastResult)
	}
	if status.LastKnownGood.SSID != "original-ssid" {
		t.Errorf("expected recovery to the original SSID, got %q", status.LastKnownGood.SSID)
	}
	if exec.live.SSID != "original-ssid" {
		t.Errorf("expected executor to have re-applied the original config, live SSID = %q", exec.live.SSID)
	}
	if _, ok, _ := pers.LoadPendingTransaction(); ok {
		t.Error("expected pending transaction cleared after startup recovery")
	}
}

func TestManager_StartupRecovery_RollbackItselfFails(t *testing.T) {
	good := validAPConfig()
	good.SSID = "original-ssid"
	bad := validAPConfig()
	bad.SSID = "abandoned-ssid"

	exec := &alwaysFailExecutor{err: errors.New("simulated: cannot restore network")}
	pers := &fakePersistence{}
	if err := pers.SaveLastKnownGood(good); err != nil {
		t.Fatal(err)
	}
	if err := pers.SavePendingTransaction(PendingTransactionRecord{
		BootSessionID:  "old-boot-session",
		ProposedConfig: bad,
		PreviousConfig: good,
		Stage:          StageAwaitingReconnection,
	}); err != nil {
		t.Fatal(err)
	}

	clock := &fakeClock{now: 1000}
	m, err := NewManager("new-boot-session", clock.Now, exec, pers, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	status := m.Status()
	if status.Stage != StageRecoveryRequired {
		t.Errorf("stage = %v, want recovery_required", status.Stage)
	}
	if status.LastError == "" {
		t.Error("expected a non-empty lastError explaining the recovery failure")
	}
	// The pending record must survive so the failure is diagnosable.
	if _, ok, _ := pers.LoadPendingTransaction(); !ok {
		t.Error("expected the pending transaction record to be preserved when recovery fails")
	}
}

func TestManager_NoPendingTransaction_NormalStartup(t *testing.T) {
	good := validAPConfig()
	exec := newFakeExecutor(good)
	pers := &fakePersistence{}
	clock := &fakeClock{now: 1000}
	m, err := NewManager("boot-1", clock.Now, exec, pers, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m.Status().Stage != StageIdle {
		t.Error("a fresh install with no persisted state should start idle")
	}
	if m.Status().LastKnownGood.SSID != DefaultConfig().SSID {
		t.Error("a fresh install with no persisted last-known-good should fall back to DefaultConfig")
	}
	if exec.applyCount() != 0 {
		t.Error("normal startup with nothing pending must never call the executor")
	}
}

func TestManager_PreconditionFailureBlocksPreviewAndApply(t *testing.T) {
	good := validAPConfig()
	exec := newFakeExecutor(good)
	pers := &fakePersistence{}
	if err := pers.SaveLastKnownGood(good); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: 1000}
	blocked := true
	precond := func() error {
		if blocked {
			return errors.New("OTA in progress")
		}
		return nil
	}
	m, err := NewManager("boot-1", clock.Now, exec, pers, []Precondition{precond})
	if err != nil {
		t.Fatal(err)
	}
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	if _, _, err := m.Preview(proposed, newGen); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("Preview while blocked: err=%v, want ErrPreconditionFailed", err)
	}
	blocked = false
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatalf("Preview once unblocked: %v", err)
	}
	blocked = true
	if _, err := m.Apply(tok.Token, newGen); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("Apply while blocked: err=%v, want ErrPreconditionFailed", err)
	}
}

func TestManager_PersistenceFailureDuringApply_SurfacesFailedStage(t *testing.T) {
	good := validAPConfig()
	exec := newFakeExecutor(good)
	pers := &fakePersistence{failSavePending: true}
	if err := pers.SaveLastKnownGood(good); err != nil {
		t.Fatal(err)
	}
	pers.failSavePending = true
	clock := &fakeClock{now: 1000}
	m, err := NewManager("boot-1", clock.Now, exec, pers, nil)
	if err != nil {
		t.Fatal(err)
	}
	newGen := sequentialTokenGen()
	proposed := validAPConfig()
	proposed.SSID = "new-ssid"
	_, tok, err := m.Preview(proposed, newGen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(tok.Token, newGen); err == nil {
		t.Error("expected Apply to fail when the pending-transaction record cannot be persisted")
	}
	if exec.applyCount() != 0 {
		t.Error("the executor must never be called if the pending record could not be persisted first (crash-safety ordering)")
	}
}

func TestManagerStatus_IdempotentReads(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	s1 := m.Status()
	s2 := m.Status()
	if s1.Stage != s2.Stage || s1.LastResult != s2.LastResult {
		t.Error("repeated Status() calls should be idempotent with no state change")
	}
}
