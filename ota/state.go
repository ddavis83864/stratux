package ota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Stage is one step of the deterministic OTA update sequence. Every
// stage transition is driven by an explicit, persisted State (see below),
// so the sequence can resume correctly after a power loss at any point -
// including mid-reboot, which for this mechanism is not a hypothetical:
// the sequence deliberately reboots twice.
type Stage string

const (
	// StageIdle: no update in progress. The zero value's meaning.
	StageIdle Stage = "idle"

	// StageStaged: a package has been uploaded, verified as a well-formed
	// .deb, hashed, and its expected SHA-256/version recorded. Not yet
	// installed; the overlay has not been touched.
	StageStaged Stage = "staged"

	// StageDisableRequested: the persistent overlay-disable marker has
	// been written (via the narrow remount-rw/write/sync/relock-ro
	// sequence, at the proven-persistent path), and a reboot has been
	// requested. The next boot is expected to be bare ext4.
	StageDisableRequested Stage = "disable_requested"

	// StageInstalling: the bare-ext4 boot has begun installing - a backup
	// of the pre-install state has been taken and dpkg -i is in progress
	// or was in progress when this was last written. A resume finding
	// this stage must re-verify dpkg's own state rather than assume
	// install completed or failed.
	StageInstalling Stage = "installing"

	// StageInstalled: dpkg reports the package installed and the expected
	// version. The overlay-disable marker has been removed and a reboot
	// back to the protected overlay has been requested.
	StageInstalled Stage = "installed"

	// StageVerifying: back under the overlay; the newly-installed version
	// is being confirmed live (e.g. via the running daemon's own
	// reported build) before declaring success.
	StageVerifying Stage = "verifying"

	// StageComplete: verified successful. Staged files may now be cleaned
	// up.
	StageComplete Stage = "complete"

	// StageFailed: a step failed. RollbackNeeded (see State) indicates
	// whether a backup exists to restore. Not terminal by itself: Decide
	// retries the overlay-disable/rollback handoff from here, bounded by
	// MaxRecoveryAttempts and backed off per RecoveryBackoff, until either
	// it succeeds (a reboot follows, and the bare-ext4 side moves the
	// stage on) or recovery is exhausted into StageRecoveryExhausted.
	StageFailed Stage = "failed"

	// StageRolledBack: a failure was detected and the pre-install backup
	// was restored; the overlay has been re-enabled.
	StageRolledBack Stage = "rolled_back"

	// StageRecoveryExhausted: StageFailed's automatic recovery (the
	// overlay-disable/rollback handoff) did not succeed within
	// MaxRecoveryAttempts. Terminal: Decide takes no further automatic
	// action from here, so a persistently busy overlay (or any other
	// reason requestOverlayDisable keeps failing) produces one bounded
	// burst of retries and then a quiet, explicit stop - not an
	// indefinite five-second retry loop. State.LastError (the original
	// failure) and State.Recovery.LastError (the most recent secondary
	// error from a retry) record why, for an operator to read via
	// GET /getOTAStatus. Leaving this stage requires an explicit
	// POST /resetOTA (see main/ota.go) - there is no automatic path out.
	StageRecoveryExhausted Stage = "recovery_exhausted"
)

// Valid reports whether s is one of the defined stages.
func (s Stage) Valid() bool {
	switch s {
	case StageIdle, StageStaged, StageDisableRequested, StageInstalling,
		StageInstalled, StageVerifying, StageComplete, StageFailed, StageRolledBack,
		StageRecoveryExhausted:
		return true
	}
	return false
}

// Terminal reports whether s is an end state - no further automatic
// transition should occur without a new update being staged (StageComplete,
// StageRolledBack) or without an explicit operator reset
// (StageRecoveryExhausted).
func (s Stage) Terminal() bool {
	return s == StageComplete || s == StageRolledBack || s == StageRecoveryExhausted
}

// Recovery holds StageFailed's own bounded-retry bookkeeping - see
// StageRecoveryExhausted and Decide's StageFailed case. The zero value
// means "no recovery attempt has been made yet in the current StageFailed
// episode" (Attempts == 0, NextAttemptAt zero meaning "now, no backoff
// pending").
//
// This is kept separate from State.Attempts/State.LastError, which belong
// to StageInstalling and to the original failure cause respectively,
// because retrying the overlay-disable/rollback handoff is a different
// operation, with a different failure mode and policy, than retrying a
// dpkg install - mechanically reusing StageInstalling's fields and
// MaxInstallAttempts here would conflate two unrelated retry budgets and
// two unrelated error histories.
type Recovery struct {
	// Attempts counts how many times requestOverlayDisable has been
	// retried since Stage most recently became StageFailed. Reset to 0
	// whenever Stage transitions into StageFailed from something else -
	// a fresh failure gets a fresh recovery budget; retries of the same
	// failure do not each reset it. See MaxRecoveryAttempts.
	Attempts int

	// LastError is the most recent secondary error produced by a
	// recovery attempt itself (e.g. another "overlayctl lock: busy"),
	// deliberately kept separate from State.LastError (the original
	// failure that caused entry into StageFailed) so repeated retries
	// never overwrite or nest into the original cause. This replaces the
	// historical defect where each retry's Decide reason - itself
	// formatted from the previous LastError - was written back into
	// LastError, producing an unbounded
	// "update previously marked failed: update previously marked
	// failed: ..." string that grew by one wrapper per five-second tick.
	LastError string

	// NextAttemptAt is when the next recovery attempt is allowed to run.
	// Zero means "now" (no backoff pending). Set by RecoveryBackoff after
	// each failed attempt so retries space out with increasing patience
	// rather than firing on every five-second health tick.
	NextAttemptAt time.Time
}

// State is the full persisted OTA state, read and written by both the Go
// daemon (staging, disable-request, post-reboot verification) and
// debian/stratux-pre-start.sh (the bare-ext4 install step, which must run
// before the Go daemon exists on that boot). Both sides read/write the
// same JSON file - see Path.
type State struct {
	Stage Stage

	PackagePath     string // where the staged .deb currently lives
	ExpectedSHA256  string
	ExpectedVersion string
	ExpectedCommit  string

	BackupPath string // pre-install backup archive, for rollback

	StagedAt  time.Time
	UpdatedAt time.Time

	Attempts  int    // install attempts at the current stage (StageInstalling only - see MaxInstallAttempts). Not used by StageFailed; see Recovery.
	LastError string // the error that caused entry into StageFailed - set once per failure episode, never overwritten by a later recovery attempt's own error (see Recovery.LastError)

	Recovery Recovery
}

// NewState returns a freshly-staged State for a package with the given
// path, hash, and expected version/commit.
func NewState(packagePath, sha256Hex, expectedVersion, expectedCommit string, now time.Time) State {
	return State{
		Stage:           StageStaged,
		PackagePath:     packagePath,
		ExpectedSHA256:  sha256Hex,
		ExpectedVersion: expectedVersion,
		ExpectedCommit:  expectedCommit,
		StagedAt:        now,
		UpdatedAt:       now,
	}
}

// StateFileName is the canonical name of the persisted state file within
// the OTA updates directory.
const StateFileName = "state.json"

// StatePath returns the full path to the state file within dir.
func StatePath(dir string) string {
	return filepath.Join(dir, StateFileName)
}

// LoadState reads and parses the state file at StatePath(dir). A missing
// file is reported as StageIdle with no error - "no update in progress"
// is a normal, expected condition, not a failure to read state.
func LoadState(dir string) (State, error) {
	data, err := os.ReadFile(StatePath(dir))
	if os.IsNotExist(err) {
		return State{Stage: StageIdle}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("could not read OTA state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("OTA state file is corrupt: %w", err)
	}
	if !s.Stage.Valid() {
		return State{}, fmt.Errorf("OTA state file has unrecognized stage %q", s.Stage)
	}
	return s, nil
}

// SaveState writes s to StatePath(dir) atomically (write to a temp file in
// the same directory, then rename) so a power loss mid-write cannot leave
// a half-written, corrupt state file - exactly the failure mode this
// package's resumability depends on not happening to its own state.
func SaveState(dir string, s State, now time.Time) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("could not create OTA state directory: %w", err)
	}
	s.UpdatedAt = now
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal OTA state: %w", err)
	}
	tmp := StatePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("could not write OTA state: %w", err)
	}
	if err := os.Rename(tmp, StatePath(dir)); err != nil {
		return fmt.Errorf("could not commit OTA state: %w", err)
	}
	return nil
}

// ClearState removes the state file, returning to StageIdle. Used after a
// successful update's cleanup, or after a confirmed rollback.
func ClearState(dir string) error {
	err := os.Remove(StatePath(dir))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not clear OTA state: %w", err)
	}
	return nil
}
