package ota

import "fmt"

// MaxInstallAttempts bounds how many times StageInstalling will retry
// before Decide gives up and calls for a rollback - protecting against an
// infinite retry loop on a permanently-failing install (e.g. persistent
// ENOSPC on the real partition, not just the overlay-mistake case this
// mechanism otherwise prevents).
const MaxInstallAttempts = 3

// Action is what the caller should actually do next, as decided by
// Decide. Both the Go daemon and debian/stratux-pre-start.sh drive their
// behavior from this single source of truth - the shell side re-derives
// the same decision from the same State and real signals it can observe,
// rather than maintaining separate logic.
type Action string

const (
	// ActionNone: nothing to do. Either idle (no update in progress) or a
	// terminal stage (complete/rolled back).
	ActionNone Action = "none"

	// ActionRequestDisable: write the persistent overlay-disable marker
	// and reboot.
	ActionRequestDisable Action = "request_disable"

	// ActionAwaitReboot: a disable was requested but the live root is
	// still the overlay - the reboot has not happened (or not taken
	// effect) yet. Not an error; the caller should simply wait/retry
	// later, not treat this as a failure.
	ActionAwaitReboot Action = "await_reboot"

	// ActionInstall: run the install (dpkg -i) against the confirmed bare
	// ext4 root.
	ActionInstall Action = "install"

	// ActionRequestEnable: dpkg install confirmed healthy and matching
	// the expected commit; remove the disable marker and reboot back to
	// the overlay.
	ActionRequestEnable Action = "request_enable"

	// ActionAwaitRebootToOverlay: enable was requested but the live root
	// is still bare ext4 - the reboot back has not happened yet.
	ActionAwaitRebootToOverlay Action = "await_reboot_to_overlay"

	// ActionVerify: back under the overlay; confirm the running daemon
	// reports the expected commit before declaring success.
	ActionVerify Action = "verify"

	// ActionComplete: verification succeeded; clean up staged files.
	ActionComplete Action = "complete"

	// ActionRollback: something failed; restore the pre-install backup
	// and ensure the overlay is re-enabled.
	ActionRollback Action = "rollback"

	// ActionFail: an unrecoverable problem was found before any
	// destructive step was taken (e.g. staged package missing or hash
	// mismatch while still under the overlay, before ever disabling it) -
	// distinct from ActionRollback, which implies a backup exists to
	// restore because installation was actually attempted.
	ActionFail Action = "fail"
)

// RealSignals is everything Decide needs to observe about the actual,
// live system to make its decision - deliberately a plain data struct so
// tests can supply any combination without touching real hardware.
type RealSignals struct {
	// RootFSType is the current live root filesystem type, e.g.
	// "overlay" or "ext4".
	RootFSType string

	PackageFileExists bool
	ComputedSHA256    string // sha256 of the file at State.PackagePath, if it exists

	Dpkg DpkgStatus

	// InstalledCommit is the commit embedded in the currently installed
	// binary (e.g. parsed from `strings /opt/stratux/bin/stratuxrun`),
	// independent of dpkg's own package-version field, since this
	// project's package version string does not change per commit.
	InstalledCommit string

	// RunningCommit is the commit the currently *running* daemon reports
	// (e.g. via /getStatus's Build field). Empty if not queryable (for
	// example, immediately after a reboot before the service is up).
	RunningCommit string
}

// Decision is Decide's result.
type Decision struct {
	Action Action
	Reason string
}

func decision(a Action, format string, args ...interface{}) Decision {
	return Decision{Action: a, Reason: fmt.Sprintf(format, args...)}
}

// Decide computes the next action for an OTA update given its persisted
// State and the real signals observed about the current system. It
// performs no I/O itself and has no side effects - every stage transition
// this package makes is driven by calling Decide and then acting on its
// result, so the whole sequence is exercised by tests without needing
// real hardware, a real reboot, or a real dpkg install.
func Decide(s State, r RealSignals) Decision {
	switch s.Stage {
	case StageIdle, StageComplete, StageRolledBack:
		return decision(ActionNone, "no update in progress")

	case StageStaged:
		if !r.PackageFileExists {
			return decision(ActionFail, "staged package %s is missing before any install was attempted", s.PackagePath)
		}
		if !equalFoldHex(r.ComputedSHA256, s.ExpectedSHA256) {
			return decision(ActionFail, "staged package hash mismatch (got %s, expected %s) before any install was attempted", r.ComputedSHA256, s.ExpectedSHA256)
		}
		return decision(ActionRequestDisable, "package staged and verified; requesting overlay-disabled boot")

	case StageDisableRequested:
		if r.RootFSType == "overlay" {
			return decision(ActionAwaitReboot, "disable requested but root is still overlay-mounted; reboot has not taken effect yet")
		}
		if !r.PackageFileExists {
			return decision(ActionFail, "staged package %s is missing after rebooting to bare root", s.PackagePath)
		}
		if !equalFoldHex(r.ComputedSHA256, s.ExpectedSHA256) {
			return decision(ActionFail, "staged package hash mismatch after rebooting to bare root (got %s, expected %s)", r.ComputedSHA256, s.ExpectedSHA256)
		}
		return decision(ActionInstall, "confirmed bare ext4 root; ready to install")

	case StageInstalling:
		if r.RootFSType == "overlay" {
			// Installing must only ever happen on bare ext4; finding this
			// stage under the overlay means the state and reality have
			// diverged (e.g. a manual reboot, or a bug elsewhere) - the
			// safe response is to roll back rather than attempt dpkg -i
			// against the overlay, which is the exact mistake this whole
			// mechanism exists to prevent.
			return decision(ActionRollback, "installing stage found while root is overlay-mounted; refusing to risk installing against the overlay")
		}
		if r.InstalledCommit == s.ExpectedCommit && r.Dpkg.Healthy() {
			return decision(ActionRequestEnable, "install confirmed (dpkg healthy, installed commit matches expected); requesting return to overlay")
		}
		if r.Dpkg.Broken() {
			return decision(ActionRollback, "dpkg left the package in a broken state (%q)", r.Dpkg.Status)
		}
		if s.Attempts >= MaxInstallAttempts {
			return decision(ActionRollback, "install did not succeed after %d attempts", s.Attempts)
		}
		return decision(ActionInstall, "install not yet confirmed complete; retrying (attempt %d of %d)", s.Attempts+1, MaxInstallAttempts)

	case StageInstalled:
		if r.RootFSType != "overlay" {
			return decision(ActionAwaitRebootToOverlay, "enable requested but root is still bare ext4; reboot back has not taken effect yet")
		}
		return decision(ActionVerify, "back under the overlay; verifying the new version is actually running")

	case StageVerifying:
		if r.RunningCommit == s.ExpectedCommit {
			return decision(ActionComplete, "running daemon confirms expected commit %s", s.ExpectedCommit)
		}
		return decision(ActionRollback, "post-reboot verification failed: running commit %q does not match expected %q", r.RunningCommit, s.ExpectedCommit)

	case StageFailed:
		return decision(ActionRollback, "update previously marked failed: %s", s.LastError)

	default:
		return decision(ActionFail, "unrecognized OTA stage %q", s.Stage)
	}
}
