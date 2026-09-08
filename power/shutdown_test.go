package power

import (
	"errors"
	"sync"
	"testing"
)

// fakeExecutor never touches real hardware - it only records what was
// called, so these tests can run the full state machine, including a
// successful outcome, in a normal `go test` process.
type fakeExecutor struct {
	mu         sync.Mutex
	syncCalls  int
	powerCalls int
	syncErr    error
	powerErr   error
}

func (f *fakeExecutor) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncCalls++
	return f.syncErr
}
func (f *fakeExecutor) PowerOff() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powerCalls++
	return f.powerErr
}
func (f *fakeExecutor) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncCalls, f.powerCalls
}

func fixedClock(seconds float64) func() float64 {
	return func() float64 { return seconds }
}

func sequentialToken() func() string {
	n := 0
	return func() string {
		n++
		return "test-token-" + string(rune('a'+n-1))
	}
}

func TestManager_FullSuccessfulLifecycle(t *testing.T) {
	exec := &fakeExecutor{}
	flushed := false
	m := NewManager("boot-1", fixedClock(1000), nil, func() error { flushed = true; return nil }, exec)

	tok, err := m.RequestConfirmation(sequentialToken())
	if err != nil {
		t.Fatalf("RequestConfirmation: %v", err)
	}
	if stage, _ := m.Status(); stage != StageConfirmationRequired {
		t.Fatalf("expected CONFIRMATION_REQUIRED, got %s", stage)
	}

	stage, err := m.Confirm(tok.Token)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if stage != StageCommandIssued {
		t.Fatalf("expected COMMAND_ISSUED, got %s", stage)
	}
	if !flushed {
		t.Error("expected the flush hook to have run")
	}
	syncCalls, powerCalls := exec.counts()
	if syncCalls != 1 {
		t.Errorf("expected exactly 1 Sync call, got %d", syncCalls)
	}
	if powerCalls != 0 {
		t.Errorf("PowerOff must not be called by Confirm - expected 0, got %d", powerCalls)
	}

	if err := m.IssuePowerOff(); err != nil {
		t.Fatalf("IssuePowerOff: %v", err)
	}
	if _, powerCalls := exec.counts(); powerCalls != 1 {
		t.Errorf("expected exactly 1 PowerOff call after IssuePowerOff, got %d", powerCalls)
	}
}

func TestManager_ConfirmWithoutRequestIsRejected(t *testing.T) {
	m := NewManager("boot-1", fixedClock(0), nil, nil, &fakeExecutor{})
	if _, err := m.Confirm("anything"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("expected ErrTokenNotFound, got %v", err)
	}
}

func TestManager_ConfirmWithWrongTokenIsRejected(t *testing.T) {
	m := NewManager("boot-1", fixedClock(0), nil, nil, &fakeExecutor{})
	if _, err := m.RequestConfirmation(sequentialToken()); err != nil {
		t.Fatalf("RequestConfirmation: %v", err)
	}
	if _, err := m.Confirm("not-the-real-token"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("expected ErrTokenNotFound for a mismatched token, got %v", err)
	}
}

func TestManager_TokenSingleUse(t *testing.T) {
	exec := &fakeExecutor{}
	m := NewManager("boot-1", fixedClock(0), nil, nil, exec)
	tok, _ := m.RequestConfirmation(sequentialToken())
	if _, err := m.Confirm(tok.Token); err != nil {
		t.Fatalf("first Confirm: %v", err)
	}
	if _, err := m.Confirm(tok.Token); !errors.Is(err, ErrTokenUsed) {
		t.Errorf("expected ErrTokenUsed on reuse, got %v", err)
	}
	if syncCalls, _ := exec.counts(); syncCalls != 1 {
		t.Errorf("Sync must not run a second time on a reused token, got %d calls", syncCalls)
	}
}

func TestManager_TokenExpires(t *testing.T) {
	now := 1000.0
	m := NewManager("boot-1", func() float64 { return now }, nil, nil, &fakeExecutor{})
	tok, _ := m.RequestConfirmation(sequentialToken())
	now += defaultTokenTTLSeconds + 1
	if _, err := m.Confirm(tok.Token); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expected ErrTokenExpired, got %v", err)
	}
}

func TestManager_TokenInvalidAfterBootSessionChange(t *testing.T) {
	// Simulates a restart between preview and confirm: a Manager
	// constructed with a new bootSessionID never accepts a token minted
	// by the previous one, even one manually replayed with the same
	// string and an unexpired timestamp.
	m1 := NewManager("boot-1", fixedClock(0), nil, nil, &fakeExecutor{})
	tok, _ := m1.RequestConfirmation(sequentialToken())

	m2 := NewManager("boot-2", fixedClock(0), nil, nil, &fakeExecutor{})
	// m2 has no pending token of its own at all, so this also exercises
	// ErrTokenNotFound - the meaningful assertion is that boot-2 never
	// accepts it.
	if _, err := m2.Confirm(tok.Token); err == nil {
		t.Error("a token from a different boot session must never be accepted")
	}
}

func TestManager_PreconditionBlocksRequestConfirmation(t *testing.T) {
	blocked := errors.New("OTA update in progress")
	m := NewManager("boot-1", fixedClock(0), []Precondition{func() error { return blocked }}, nil, &fakeExecutor{})
	_, err := m.RequestConfirmation(sequentialToken())
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("expected ErrPreconditionFailed, got %v", err)
	}
	if stage, _ := m.Status(); stage != StageIdle {
		t.Errorf("a blocked request must leave the manager IDLE, got %s", stage)
	}
}

func TestManager_PreconditionReCheckedAtConfirmTime(t *testing.T) {
	// A precondition that passes at RequestConfirmation time but fails by
	// the time Confirm is called (e.g. an OTA started in between) must
	// still block the actual flush/sync/power-off sequence.
	blockNow := false
	precondition := func() error {
		if blockNow {
			return errors.New("OTA started after preview")
		}
		return nil
	}
	exec := &fakeExecutor{}
	m := NewManager("boot-1", fixedClock(0), []Precondition{precondition}, nil, exec)
	tok, err := m.RequestConfirmation(sequentialToken())
	if err != nil {
		t.Fatalf("RequestConfirmation: %v", err)
	}
	blockNow = true
	if _, err := m.Confirm(tok.Token); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("expected ErrPreconditionFailed at confirm time, got %v", err)
	}
	if syncCalls, powerCalls := exec.counts(); syncCalls != 0 || powerCalls != 0 {
		t.Errorf("a precondition failure at confirm time must never touch the executor, got sync=%d power=%d", syncCalls, powerCalls)
	}
}

func TestManager_FlushFailureReachesFailedWithoutSync(t *testing.T) {
	exec := &fakeExecutor{}
	m := NewManager("boot-1", fixedClock(0), nil, func() error { return errors.New("flush disk full") }, exec)
	tok, _ := m.RequestConfirmation(sequentialToken())
	stage, err := m.Confirm(tok.Token)
	if err == nil {
		t.Fatal("expected an error when flush fails")
	}
	if stage != StageFailed {
		t.Errorf("expected FAILED after a flush error, got %s", stage)
	}
	if syncCalls, powerCalls := exec.counts(); syncCalls != 0 || powerCalls != 0 {
		t.Errorf("Sync/PowerOff must never run after a flush failure, got sync=%d power=%d", syncCalls, powerCalls)
	}
}

func TestManager_SyncFailureReachesFailedWithoutPowerOff(t *testing.T) {
	exec := &fakeExecutor{syncErr: errors.New("sync: i/o error")}
	m := NewManager("boot-1", fixedClock(0), nil, nil, exec)
	tok, _ := m.RequestConfirmation(sequentialToken())
	stage, err := m.Confirm(tok.Token)
	if err == nil {
		t.Fatal("expected an error when sync fails")
	}
	if stage != StageFailed {
		t.Errorf("expected FAILED after a sync error, got %s", stage)
	}
	if _, powerCalls := exec.counts(); powerCalls != 0 {
		t.Errorf("PowerOff must never run after a sync failure, got %d calls", powerCalls)
	}
}

func TestManager_ResetReturnsToIdleAfterFailure(t *testing.T) {
	exec := &fakeExecutor{syncErr: errors.New("boom")}
	m := NewManager("boot-1", fixedClock(0), nil, nil, exec)
	tok, _ := m.RequestConfirmation(sequentialToken())
	m.Confirm(tok.Token)
	if stage, _ := m.Status(); stage != StageFailed {
		t.Fatalf("setup: expected FAILED, got %s", stage)
	}
	m.Reset()
	if stage, msg := m.Status(); stage != StageIdle || msg != "" {
		t.Errorf("expected IDLE with no error after Reset, got stage=%s msg=%q", stage, msg)
	}
}

func TestManager_ConcurrentConfirmOnlyOneWins(t *testing.T) {
	exec := &fakeExecutor{}
	m := NewManager("boot-1", fixedClock(0), nil, nil, exec)
	tok, _ := m.RequestConfirmation(sequentialToken())

	const n = 20
	var wg sync.WaitGroup
	var successes, failures int32
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Confirm(tok.Token)
			mu.Lock()
			if err == nil {
				successes++
			} else {
				failures++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Errorf("expected exactly 1 successful Confirm out of %d concurrent attempts, got %d", n, successes)
	}
	if failures != n-1 {
		t.Errorf("expected %d rejected attempts, got %d", n-1, failures)
	}
	if syncCalls, powerCalls := exec.counts(); syncCalls != 1 || powerCalls != 0 {
		t.Errorf("expected exactly 1 Sync call total and 0 PowerOff calls, got sync=%d power=%d", syncCalls, powerCalls)
	}
}

func TestManager_RequestConfirmationBlockedWhilePastConfirmationRequired(t *testing.T) {
	exec := &fakeExecutor{}
	m := NewManager("boot-1", fixedClock(0), nil, func() error {
		// Block inside the flush hook long enough to observe the
		// ShutdownRequested/Flushing stage from the outside... instead,
		// simpler: just confirm synchronously and check the stage
		// immediately after, then try RequestConfirmation while stage is
		// COMMAND_ISSUED (a terminal, in-progress-adjacent stage that is
		// not one of the three RequestConfirmation allows).
		return nil
	}, exec)
	tok, _ := m.RequestConfirmation(sequentialToken())
	if _, err := m.Confirm(tok.Token); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if stage, _ := m.Status(); stage != StageCommandIssued {
		t.Fatalf("setup: expected COMMAND_ISSUED, got %s", stage)
	}
	if _, err := m.RequestConfirmation(sequentialToken()); !errors.Is(err, ErrAlreadyInProgress) {
		t.Errorf("expected ErrAlreadyInProgress once COMMAND_ISSUED has been reached, got %v", err)
	}
}
