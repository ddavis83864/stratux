package power

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SessionMarker is the durable record power.go's caller (main/) writes
// once at process startup and again the moment a controlled shutdown or
// reboot is actually issued. Its presence and ClosedCleanly value are all
// EvaluatePreviousSession has to work with - see that function's doc
// comment for why its conclusions are deliberately hedged.
type SessionMarker struct {
	// SessionID identifies the boot or daemon session this marker was
	// written for - the caller decides what that means (main/ uses
	// Linux's /proc/sys/kernel/random/boot_id when it can read it, falling
	// back to the daemon's own random per-process session id otherwise;
	// see main/powerapi.go's currentBootOrSessionID). Never a wall-clock
	// timestamp, so a clock that is wrong or not yet trusted cannot
	// corrupt this record.
	SessionID string `json:"sessionId"`
	// ClosedCleanly is false the entire time a session is running, and is
	// set true only immediately before main/ actually issues a reboot or
	// poweroff command - see docs/power-shutdown-resilience.md.
	ClosedCleanly bool `json:"closedCleanly"`
	// ClosedReason is a short machine-readable tag ("controlled-shutdown",
	// "reboot") set alongside ClosedCleanly - informational only.
	ClosedReason string `json:"closedReason,omitempty"`
	// UpdatedAtMonoSeconds is monotonic seconds (main/'s stratuxClock),
	// never wall-clock - see docs/readiness-and-time-trust.md for why
	// this project avoids trusting an unsynchronized wall clock.
	UpdatedAtMonoSeconds float64 `json:"updatedAtMonoSeconds"`
}

// ReadSessionMarker reads and parses the marker file at path. A missing
// file is not an error: it returns ok=false, meaning "no previous-session
// record exists" (the normal, unremarkable state on first boot after
// upgrading to a version that has this feature at all). A file that
// exists but fails to parse is treated the same conservative way, via a
// non-nil error - the caller (EvaluatePreviousSession by way of main/)
// must never turn "the record itself is unreadable" into a claim about
// what happened last session.
func ReadSessionMarker(path string) (marker SessionMarker, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SessionMarker{}, false, nil
		}
		return SessionMarker{}, false, fmt.Errorf("power: could not read session marker: %w", err)
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return SessionMarker{}, false, fmt.Errorf("power: could not parse session marker: %w", err)
	}
	return marker, true, nil
}

// WriteSessionMarkerAtomic writes marker to path atomically - a temp file
// in the same directory followed by os.Rename - so a concurrent reader
// (or a crash mid-write) never observes a partially-written marker,
// matching this project's established atomic-write pattern (see
// readiness.WriteDiagnosticBundle).
func WriteSessionMarkerAtomic(path string, marker SessionMarker) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("power: could not create session marker directory: %w", err)
	}
	data, err := json.MarshalIndent(&marker, "", "  ")
	if err != nil {
		return fmt.Errorf("power: could not marshal session marker: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("power: could not write session marker: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("power: could not finalize session marker: %w", err)
	}
	return nil
}

// PreviousSessionAssessment is EvaluatePreviousSession's conservative
// conclusion about the marker found (or not found) at startup.
type PreviousSessionAssessment struct {
	// Available is false when no previous marker exists at all - not
	// evidence of anything, since this feature may simply not have run
	// yet on this installation.
	Available bool
	// EndedCleanly is only meaningful when Available is true.
	EndedCleanly bool
	// Note is a short, human-readable, deliberately hedged explanation
	// suitable for direct display - see EvaluatePreviousSession.
	Note string
}

// EvaluatePreviousSession decides, in conservative language, whether the
// marker found at startup (previous, previousExists - the result of
// ReadSessionMarker, read before this boot's own marker is written) shows
// that the last session ended with a recorded clean shutdown or reboot.
//
// This function deliberately never claims "power loss," "crash," or any
// other specific diagnosis. The only fact it can actually establish is
// narrower: whether main/'s own shutdown/reboot code path ran and
// recorded ClosedCleanly before this boot started. A false result can
// also mean a bug, a `kill -9`, a watchdog reset, or simply that this
// feature was added after the previous boot already started - conflating
// any of those with "the battery died" would be a claim this package has
// no way to verify, so it does not make it.
func EvaluatePreviousSession(previous SessionMarker, previousExists bool) PreviousSessionAssessment {
	if !previousExists {
		return PreviousSessionAssessment{
			Available: false,
			Note:      "no previous-session record found (first boot with this feature, or the record was cleared)",
		}
	}
	if previous.ClosedCleanly {
		return PreviousSessionAssessment{
			Available:    true,
			EndedCleanly: true,
			Note:         "the previous session recorded a clean shutdown or reboot",
		}
	}
	return PreviousSessionAssessment{
		Available:    true,
		EndedCleanly: false,
		Note: "the previous session did not record a clean shutdown or reboot before this boot - " +
			"this can happen after a power interruption, a hard reset, a crash, or simply because " +
			"this record only started being kept in a newer version; it is not a confirmed diagnosis of power loss",
	}
}
