package ota

import (
	"fmt"
	"time"
)

// MaxInstallAttempts bounds how many times StageInstalling will retry
// before Decide gives up and calls for a rollback - protecting against an
// infinite retry loop on a permanently-failing install (e.g. persistent
// ENOSPC on the real partition, not just the overlay-mistake case this
// mechanism otherwise prevents).
const MaxInstallAttempts = 3

// MaxRecoveryAttempts bounds how many times StageFailed will retry the
// overlay-disable/rollback handoff (requestOverlayDisable) before Decide
// gives up into StageRecoveryExhausted. Deliberately a separate constant
// from MaxInstallAttempts: this bounds a different operation (relocking
// the overlay, not running dpkg) with a different observed failure mode
// (a persistently busy mount, not disk space) - see the exit-status-32
// "mount point is busy" incident this constant, RecoveryBackoff, and
// StageRecoveryExhausted exist to bound. Five attempts, spaced by
// RecoveryBackoff, gives a transient busy condition a real chance to clear
// (about four and a half minutes total, see RecoveryBackoff) while still
// guaranteeing the retry loop ends on its own within a human-scale window
// even if the condition is permanent for the rest of the boot.
const MaxRecoveryAttempts = 5

// recoveryBackoffBase and recoveryBackoffMax bound RecoveryBackoff's
// doubling schedule: 5s, 10s, 20s, 40s, 80s (capped at recoveryBackoffMax)
// - roughly 4m35s of total elapsed time across MaxRecoveryAttempts
// attempts before StageRecoveryExhausted, versus the unbounded
// every-five-seconds-forever behavior this replaces.
const (
	recoveryBackoffBase = 5 * time.Second
	recoveryBackoffMax  = 2 * time.Minute
)

// RecoveryBackoff returns how long to wait before the next StageFailed
// recovery retry, given attemptsSoFar prior attempts have already been
// made in the current failure episode (0 before the first retry). It
// doubles from recoveryBackoffBase each attempt, capped at
// recoveryBackoffMax, so a persistently busy overlay is retried with
// increasing patience rather than on every five-second health tick, while
// MaxRecoveryAttempts still guarantees a bounded total wait before giving
// up.
func RecoveryBackoff(attemptsSoFar int) time.Duration {
	d := recoveryBackoffBase
	for i := 0; i < attemptsSoFar; i++ {
		d *= 2
		if d >= recoveryBackoffMax {
			return recoveryBackoffMax
		}
	}
	return d
}

// EnterFailed returns a copy of s transitioned into StageFailed for
// reason, starting a fresh Recovery budget. Call this exactly once per
// failure episode - at the moment Stage first becomes StageFailed - not
// on each subsequent retry of the same failure (see RecordRecoveryFailure
// for retries, and Decide's StageFailed case for how the budget is spent).
func (s State) EnterFailed(reason string) State {
	s.Stage = StageFailed
	s.LastError = reason
	s.Recovery = Recovery{}
	return s
}

// RecordRecoveryFailure returns a copy of s with one more StageFailed
// recovery attempt recorded: Recovery.Attempts incremented,
// Recovery.LastError set to err (never touching the original
// State.LastError), and Recovery.NextAttemptAt pushed out by
// RecoveryBackoff. Call this when a recovery attempt
// (requestOverlayDisable) itself fails while already in StageFailed.
func (s State) RecordRecoveryFailure(err error, now time.Time) State {
	s.Recovery.Attempts++
	s.Recovery.LastError = err.Error()
	s.Recovery.NextAttemptAt = now.Add(RecoveryBackoff(s.Recovery.Attempts - 1))
	return s
}

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

	// ActionRecoveryExhausted: StageFailed's automatic recovery
	// (requestOverlayDisable retries) did not succeed within
	// MaxRecoveryAttempts. The caller should transition to
	// StageRecoveryExhausted and take no further automatic action -
	// leaving this state requires an explicit operator POST /resetOTA.
	ActionRecoveryExhausted Action = "recovery_exhausted"
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

	// Now is the current time, used only by StageFailed's backoff check
	// (State.Recovery.NextAttemptAt). A field here, not a hidden
	// time.Now() call inside Decide, for the same reason every other
	// signal is passed in explicitly: tests can supply any value without
	// depending on wall-clock timing.
	Now time.Time
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

	case StageRecoveryExhausted:
		return decision(ActionNone, "automatic recovery exhausted after %d attempts; original failure: %s; last recovery error: %s; operator reset required (POST /resetOTA)",
			s.Recovery.Attempts, s.LastError, s.Recovery.LastError)

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
		// StageFailed's own bounded retry of the overlay-disable/rollback
		// handoff - see MaxRecoveryAttempts, RecoveryBackoff, and
		// StageRecoveryExhausted's doc comment for why this exists.
		// Unlike the pre-fix behavior, this deliberately does NOT format
		// its reason from s.LastError the way earlier stages' Reason
		// strings feed into the next stage's LastError elsewhere in this
		// file - doing that here is exactly what produced the historical
		// unbounded "update previously marked failed: update previously
		// marked failed: ..." nesting, since main/ota.go's caller used to
		// write decision.Reason straight back into State.LastError on
		// every retry. State.LastError now stays fixed at the original
		// failure for the whole episode; only State.Recovery tracks retry
		// progress.
		if s.Recovery.Attempts >= MaxRecoveryAttempts {
			return decision(ActionRecoveryExhausted, "automatic recovery exhausted after %d attempts; original failure: %s; last recovery error: %s",
				s.Recovery.Attempts, s.LastError, s.Recovery.LastError)
		}
		if !s.Recovery.NextAttemptAt.IsZero() && r.Now.Before(s.Recovery.NextAttemptAt) {
			return decision(ActionNone, "recovery backoff in effect until %s (attempt %d of %d pending; original failure: %s)",
				s.Recovery.NextAttemptAt.Format(time.RFC3339), s.Recovery.Attempts+1, MaxRecoveryAttempts, s.LastError)
		}
		return decision(ActionRollback, "retrying overlay-disable recovery (attempt %d of %d); original failure: %s",
			s.Recovery.Attempts+1, MaxRecoveryAttempts, s.LastError)

	default:
		return decision(ActionFail, "unrecognized OTA stage %q", s.Stage)
	}
}
