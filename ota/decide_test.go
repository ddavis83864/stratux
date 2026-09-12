package ota

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func baseState(stage Stage) State {
	return State{
		Stage:           stage,
		PackagePath:     "/var/lib/stratux-data/updates/staged/stratux.deb",
		ExpectedSHA256:  "cafef00d",
		ExpectedVersion: "2.0-pre5",
		ExpectedCommit:  "deadbeef",
	}
}

// --- Idle / terminal ---

func TestDecide_Idle(t *testing.T) {
	d := Decide(baseState(StageIdle), RealSignals{})
	if d.Action != ActionNone {
		t.Errorf("idle should decide ActionNone, got %s", d.Action)
	}
}

func TestDecide_TerminalStagesDecideNone(t *testing.T) {
	for _, stage := range []Stage{StageComplete, StageRolledBack, StageRecoveryExhausted} {
		if d := Decide(baseState(stage), RealSignals{}); d.Action != ActionNone {
			t.Errorf("%s should decide ActionNone, got %s", stage, d.Action)
		}
	}
}

// --- Staged: missing package / hash mismatch / happy path ---

func TestDecide_Staged_MissingPackage(t *testing.T) {
	d := Decide(baseState(StageStaged), RealSignals{PackageFileExists: false})
	if d.Action != ActionFail {
		t.Errorf("missing staged package should decide ActionFail, got %s", d.Action)
	}
}

func TestDecide_Staged_HashMismatch(t *testing.T) {
	d := Decide(baseState(StageStaged), RealSignals{PackageFileExists: true, ComputedSHA256: "wrong"})
	if d.Action != ActionFail {
		t.Errorf("hash mismatch should decide ActionFail, got %s", d.Action)
	}
}

func TestDecide_Staged_HappyPath(t *testing.T) {
	d := Decide(baseState(StageStaged), RealSignals{PackageFileExists: true, ComputedSHA256: "cafef00d"})
	if d.Action != ActionRequestDisable {
		t.Errorf("verified staged package should decide ActionRequestDisable, got %s", d.Action)
	}
}

func TestDecide_Staged_HashComparisonIsCaseInsensitive(t *testing.T) {
	d := Decide(baseState(StageStaged), RealSignals{PackageFileExists: true, ComputedSHA256: "CAFEF00D"})
	if d.Action != ActionRequestDisable {
		t.Errorf("uppercase-hex hash should still match, got %s: %s", d.Action, d.Reason)
	}
}

// --- DisableRequested: stale state recovery, missing package, hash mismatch, happy path ---

func TestDecide_DisableRequested_StillUnderOverlay_IsStaleNotFailure(t *testing.T) {
	d := Decide(baseState(StageDisableRequested), RealSignals{RootFSType: "overlay"})
	if d.Action != ActionAwaitReboot {
		t.Errorf("still-overlay after disable request should decide ActionAwaitReboot (not a failure), got %s", d.Action)
	}
}

func TestDecide_DisableRequested_BareRoot_MissingPackage(t *testing.T) {
	d := Decide(baseState(StageDisableRequested), RealSignals{RootFSType: "ext4", PackageFileExists: false})
	if d.Action != ActionFail {
		t.Errorf("missing package after reboot to bare root should decide ActionFail, got %s", d.Action)
	}
}

func TestDecide_DisableRequested_BareRoot_HashMismatch(t *testing.T) {
	d := Decide(baseState(StageDisableRequested), RealSignals{RootFSType: "ext4", PackageFileExists: true, ComputedSHA256: "tampered"})
	if d.Action != ActionFail {
		t.Errorf("hash mismatch after reboot to bare root should decide ActionFail, got %s", d.Action)
	}
}

func TestDecide_DisableRequested_BareRoot_ReadyToInstall(t *testing.T) {
	d := Decide(baseState(StageDisableRequested), RealSignals{RootFSType: "ext4", PackageFileExists: true, ComputedSHA256: "cafef00d"})
	if d.Action != ActionInstall {
		t.Errorf("confirmed bare root with a verified package should decide ActionInstall, got %s", d.Action)
	}
}

// --- Installing: never against overlay, interrupted install, failed dpkg state, ENOSPC/retry bound, success ---

func TestDecide_Installing_NeverAgainstOverlay(t *testing.T) {
	// Never run dpkg against the 250 MiB overlay - if the installing
	// stage is somehow reached while root is overlay-mounted, roll back
	// rather than risk it.
	d := Decide(baseState(StageInstalling), RealSignals{RootFSType: "overlay"})
	if d.Action != ActionRollback {
		t.Errorf("installing stage under overlay must decide ActionRollback, got %s", d.Action)
	}
}

func TestDecide_Installing_InterruptedInstall_AlreadySucceeded(t *testing.T) {
	// Simulates a power loss right after dpkg completed but before the
	// state file (or the caller) recorded success - resuming must detect
	// success from real signals, not blindly retry the install.
	d := Decide(baseState(StageInstalling), RealSignals{
		RootFSType:      "ext4",
		InstalledCommit: "deadbeef",
		Dpkg:            DpkgStatus{Status: "install ok installed"},
	})
	if d.Action != ActionRequestEnable {
		t.Errorf("a resumed install that actually succeeded should decide ActionRequestEnable, got %s: %s", d.Action, d.Reason)
	}
}

func TestDecide_Installing_InterruptedInstall_NotYetDone(t *testing.T) {
	// Power loss happened before dpkg ran at all (or before it finished);
	// resuming with no dpkg record yet and attempts under the bound
	// should retry, not give up immediately.
	d := Decide(baseState(StageInstalling), RealSignals{RootFSType: "ext4", Dpkg: DpkgStatus{}})
	if d.Action != ActionInstall {
		t.Errorf("an interrupted, not-yet-complete install should decide ActionInstall (retry), got %s", d.Action)
	}
}

func TestDecide_Installing_FailedDpkgState(t *testing.T) {
	d := Decide(baseState(StageInstalling), RealSignals{
		RootFSType: "ext4",
		Dpkg:       DpkgStatus{Status: "half-installed"},
	})
	if d.Action != ActionRollback {
		t.Errorf("a broken dpkg status should decide ActionRollback, got %s", d.Action)
	}
}

func TestDecide_Installing_ENOSPC_ExhaustsRetriesIntoRollback(t *testing.T) {
	s := baseState(StageInstalling)
	s.Attempts = MaxInstallAttempts
	d := Decide(s, RealSignals{RootFSType: "ext4", Dpkg: DpkgStatus{}})
	if d.Action != ActionRollback {
		t.Errorf("exhausting install attempts (e.g. persistent ENOSPC) should decide ActionRollback, got %s", d.Action)
	}
}

func TestDecide_Installing_RetriesBeforeExhaustion(t *testing.T) {
	s := baseState(StageInstalling)
	s.Attempts = MaxInstallAttempts - 1
	d := Decide(s, RealSignals{RootFSType: "ext4", Dpkg: DpkgStatus{}})
	if d.Action != ActionInstall {
		t.Errorf("attempts below the bound should still retry, got %s", d.Action)
	}
}

// --- Installed: await reboot to overlay vs. verify ---

func TestDecide_Installed_StillBareRoot(t *testing.T) {
	d := Decide(baseState(StageInstalled), RealSignals{RootFSType: "ext4"})
	if d.Action != ActionAwaitRebootToOverlay {
		t.Errorf("installed-but-still-bare-root should decide ActionAwaitRebootToOverlay, got %s", d.Action)
	}
}

func TestDecide_Installed_BackUnderOverlay(t *testing.T) {
	d := Decide(baseState(StageInstalled), RealSignals{RootFSType: "overlay"})
	if d.Action != ActionVerify {
		t.Errorf("installed and back under overlay should decide ActionVerify, got %s", d.Action)
	}
}

// --- Verifying: success and failure ---

func TestDecide_Verifying_Success(t *testing.T) {
	d := Decide(baseState(StageVerifying), RealSignals{RunningCommit: "deadbeef"})
	if d.Action != ActionComplete {
		t.Errorf("matching running commit should decide ActionComplete, got %s", d.Action)
	}
}

func TestDecide_Verifying_Failure(t *testing.T) {
	d := Decide(baseState(StageVerifying), RealSignals{RunningCommit: "wrongcommit"})
	if d.Action != ActionRollback {
		t.Errorf("mismatched running commit should decide ActionRollback, got %s", d.Action)
	}
}

// --- Failed stage: bounded recovery retry (regression coverage for the
// exact "overlayctl lock: exit status 32: mount point is busy" incident,
// where StageFailed retried every five seconds forever - see
// MaxRecoveryAttempts, RecoveryBackoff, and StageRecoveryExhausted) ---

func TestDecide_Failed_FirstAttemptRollsBackImmediately(t *testing.T) {
	// A fresh failure (zero-value Recovery) must retry right away, not
	// wait out a backoff that was never set.
	d := Decide(baseState(StageFailed), RealSignals{})
	if d.Action != ActionRollback {
		t.Errorf("a fresh failure should decide ActionRollback immediately, got %s: %s", d.Action, d.Reason)
	}
}

func TestDecide_Failed_BackoffBlocksRetryBeforeItElapses(t *testing.T) {
	now := time.Date(2026, 9, 11, 21, 0, 0, 0, time.UTC)
	s := baseState(StageFailed)
	s.Recovery.Attempts = 1
	s.Recovery.NextAttemptAt = now.Add(10 * time.Second)

	d := Decide(s, RealSignals{Now: now})
	if d.Action != ActionNone {
		t.Errorf("a retry attempted before its backoff elapses should decide ActionNone, got %s: %s", d.Action, d.Reason)
	}
}

func TestDecide_Failed_RetriesOnceBackoffElapses(t *testing.T) {
	now := time.Date(2026, 9, 11, 21, 0, 0, 0, time.UTC)
	s := baseState(StageFailed)
	s.Recovery.Attempts = 1
	s.Recovery.NextAttemptAt = now.Add(-1 * time.Second) // already in the past

	d := Decide(s, RealSignals{Now: now})
	if d.Action != ActionRollback {
		t.Errorf("a retry attempted after its backoff elapses should decide ActionRollback, got %s: %s", d.Action, d.Reason)
	}
}

func TestDecide_Failed_ExhaustsAfterMaxRecoveryAttempts(t *testing.T) {
	now := time.Date(2026, 9, 11, 21, 0, 0, 0, time.UTC)
	s := baseState(StageFailed)
	s.Recovery.Attempts = MaxRecoveryAttempts
	s.LastError = "overlayctl lock: exit status 32: mount point is busy"
	s.Recovery.LastError = "overlayctl lock: exit status 32: mount point is busy"

	d := Decide(s, RealSignals{Now: now})
	if d.Action != ActionRecoveryExhausted {
		t.Errorf("reaching MaxRecoveryAttempts should decide ActionRecoveryExhausted, got %s: %s", d.Action, d.Reason)
	}
	if !containsAll(d.Reason, "5 attempts", "mount point is busy") {
		t.Errorf("exhaustion reason should name the attempt count and the original failure, got: %s", d.Reason)
	}
}

func TestDecide_Failed_ExhaustionTakesPriorityOverPendingBackoff(t *testing.T) {
	// If Attempts already reached the cap, it must not matter whether a
	// backoff timer also happens to still be pending - exhaustion wins,
	// so the caller definitely stops retrying rather than waiting once
	// more only to exhaust on the following tick.
	now := time.Date(2026, 9, 11, 21, 0, 0, 0, time.UTC)
	s := baseState(StageFailed)
	s.Recovery.Attempts = MaxRecoveryAttempts
	s.Recovery.NextAttemptAt = now.Add(time.Minute)

	d := Decide(s, RealSignals{Now: now})
	if d.Action != ActionRecoveryExhausted {
		t.Errorf("exhaustion should be decided even with a backoff timer still pending, got %s", d.Action)
	}
}

func TestDecide_Failed_ThousandsOfTicksAfterExhaustionPerformNoFurtherWork(t *testing.T) {
	// Regression for the exact defect: once exhausted, arbitrarily many
	// subsequent health ticks (any Stage/Now combination) must keep
	// deciding ActionNone from StageRecoveryExhausted - never another
	// ActionRollback, and the reported reason must stay informative
	// rather than reverting to a generic "no update in progress".
	now := time.Date(2026, 9, 11, 21, 0, 0, 0, time.UTC)
	s := baseState(StageFailed)
	s.Recovery.Attempts = MaxRecoveryAttempts
	s.LastError = "overlayctl lock: exit status 32: mount point is busy"
	d := Decide(s, RealSignals{Now: now})
	if d.Action != ActionRecoveryExhausted {
		t.Fatalf("setup: expected exhaustion, got %s", d.Action)
	}

	exhausted := baseState(StageRecoveryExhausted)
	exhausted.LastError = s.LastError
	exhausted.Recovery = s.Recovery
	for i := 0; i < 5000; i++ {
		tick := now.Add(time.Duration(i) * 5 * time.Second)
		d := Decide(exhausted, RealSignals{Now: tick})
		if d.Action != ActionNone {
			t.Fatalf("tick %d: exhausted state performed further work: %s (%s)", i, d.Action, d.Reason)
		}
		if !containsAll(d.Reason, "operator", "resetOTA") {
			t.Fatalf("tick %d: exhausted reason lost operator-action guidance: %s", i, d.Reason)
		}
	}
}

func TestDecide_Failed_OriginalErrorPreservedAcrossRetries(t *testing.T) {
	// The historical defect: each retry's Decide reason was written back
	// into State.LastError, nesting "update previously marked failed: "
	// once per five-second tick forever. LastError must now stay fixed
	// at the original cause for the whole episode - only Recovery
	// changes between attempts.
	s := baseState(StageFailed)
	s.LastError = "overlayctl lock: exit status 32: mount point is busy"
	original := s.LastError
	now := time.Date(2026, 9, 11, 21, 0, 0, 0, time.UTC)

	for attempt := 0; attempt < MaxRecoveryAttempts; attempt++ {
		d := Decide(s, RealSignals{Now: now})
		if d.Action != ActionRollback {
			t.Fatalf("attempt %d: expected ActionRollback, got %s (%s)", attempt, d.Action, d.Reason)
		}
		// Simulate the daemon recording that this attempt's own
		// requestOverlayDisable call failed again with the same busy
		// error - this must never touch s.LastError.
		s = s.RecordRecoveryFailure(errors.New("overlayctl lock: exit status 32: mount point is busy"), now)
		if s.LastError != original {
			t.Fatalf("attempt %d: original LastError was overwritten: got %q, want %q", attempt, s.LastError, original)
		}
		now = s.Recovery.NextAttemptAt // advance past this attempt's backoff before the next one
	}
	if len(s.LastError) > 200 {
		t.Fatalf("LastError grew unbounded (%d bytes) - the nesting defect has recurred", len(s.LastError))
	}
}

// --- RecoveryBackoff: doubling schedule and cap ---

func TestRecoveryBackoff_DoublesThenCaps(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 80 * time.Second},
		{5, 2 * time.Minute}, // capped
		{100, 2 * time.Minute},
	}
	for _, c := range cases {
		if got := RecoveryBackoff(c.attempts); got != c.want {
			t.Errorf("RecoveryBackoff(%d) = %s, want %s", c.attempts, got, c.want)
		}
	}
}

// --- EnterFailed / RecordRecoveryFailure: pure bookkeeping helpers ---

func TestEnterFailed_StartsFreshRecoveryBudget(t *testing.T) {
	s := baseState(StageInstalling)
	s.Recovery = Recovery{Attempts: 3, LastError: "stale"} // pretend a prior episode left this dirty

	s = s.EnterFailed("dpkg left the package broken")
	if s.Stage != StageFailed {
		t.Errorf("EnterFailed must set Stage to StageFailed, got %s", s.Stage)
	}
	if s.LastError != "dpkg left the package broken" {
		t.Errorf("EnterFailed must record the reason as LastError, got %q", s.LastError)
	}
	if s.Recovery != (Recovery{}) {
		t.Errorf("EnterFailed must reset Recovery to zero value, got %+v", s.Recovery)
	}
}

func TestRecordRecoveryFailure_IncrementsAttemptsAndSetsBackoffWithoutTouchingLastError(t *testing.T) {
	now := time.Date(2026, 9, 11, 21, 0, 0, 0, time.UTC)
	s := baseState(StageFailed)
	s.LastError = "original cause"

	s = s.RecordRecoveryFailure(errors.New("overlayctl lock: busy"), now)
	if s.Recovery.Attempts != 1 {
		t.Errorf("expected Recovery.Attempts == 1, got %d", s.Recovery.Attempts)
	}
	if s.Recovery.LastError != "overlayctl lock: busy" {
		t.Errorf("expected Recovery.LastError to record the new error, got %q", s.Recovery.LastError)
	}
	if s.LastError != "original cause" {
		t.Errorf("RecordRecoveryFailure must never modify the original LastError, got %q", s.LastError)
	}
	wantNext := now.Add(RecoveryBackoff(0))
	if !s.Recovery.NextAttemptAt.Equal(wantNext) {
		t.Errorf("expected NextAttemptAt = %s, got %s", wantNext, s.Recovery.NextAttemptAt)
	}
}

// containsAll reports whether s contains every one of substrs, for
// loosely asserting on human-readable Reason strings without pinning
// their exact wording.
func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// --- Unrecognized stage ---

func TestDecide_UnrecognizedStage(t *testing.T) {
	s := baseState(Stage("something_else"))
	d := Decide(s, RealSignals{})
	if d.Action != ActionFail {
		t.Errorf("an unrecognized stage should decide ActionFail, got %s", d.Action)
	}
}

// --- Full successful sequence, end to end through Decide alone ---

func TestDecide_FullSuccessfulSequence(t *testing.T) {
	pkgOK := RealSignals{PackageFileExists: true, ComputedSHA256: "cafef00d"}

	// staged -> request_disable
	d := Decide(baseState(StageStaged), pkgOK)
	if d.Action != ActionRequestDisable {
		t.Fatalf("step 1: got %s", d.Action)
	}

	// disable_requested, still overlay -> await
	d = Decide(baseState(StageDisableRequested), RealSignals{RootFSType: "overlay"})
	if d.Action != ActionAwaitReboot {
		t.Fatalf("step 2: got %s", d.Action)
	}

	// disable_requested, now bare ext4 -> install
	sig := pkgOK
	sig.RootFSType = "ext4"
	d = Decide(baseState(StageDisableRequested), sig)
	if d.Action != ActionInstall {
		t.Fatalf("step 3: got %s", d.Action)
	}

	// installing, succeeded -> request_enable
	sig2 := RealSignals{RootFSType: "ext4", InstalledCommit: "deadbeef", Dpkg: DpkgStatus{Status: "install ok installed"}}
	d = Decide(baseState(StageInstalling), sig2)
	if d.Action != ActionRequestEnable {
		t.Fatalf("step 4: got %s", d.Action)
	}

	// installed, still bare root -> await reboot to overlay
	d = Decide(baseState(StageInstalled), RealSignals{RootFSType: "ext4"})
	if d.Action != ActionAwaitRebootToOverlay {
		t.Fatalf("step 5: got %s", d.Action)
	}

	// installed, back under overlay -> verify
	d = Decide(baseState(StageInstalled), RealSignals{RootFSType: "overlay"})
	if d.Action != ActionVerify {
		t.Fatalf("step 6: got %s", d.Action)
	}

	// verifying, matches -> complete
	d = Decide(baseState(StageVerifying), RealSignals{RunningCommit: "deadbeef"})
	if d.Action != ActionComplete {
		t.Fatalf("step 7: got %s", d.Action)
	}
}
