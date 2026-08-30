package ota

import "testing"

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
	for _, stage := range []Stage{StageComplete, StageRolledBack} {
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

// --- Failed stage always leads to rollback ---

func TestDecide_Failed_AlwaysRollsBack(t *testing.T) {
	d := Decide(baseState(StageFailed), RealSignals{})
	if d.Action != ActionRollback {
		t.Errorf("a previously-failed update should decide ActionRollback, got %s", d.Action)
	}
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
