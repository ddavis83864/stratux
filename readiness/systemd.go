package readiness

import (
	"os/exec"
	"strings"
)

// ParseFailedUnits parses `systemctl --failed --no-legend --plain` output
// into a list of failed unit names. It performs no I/O and is exercised
// directly by tests.
func ParseFailedUnits(output string) []string {
	var units []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		units = append(units, fields[0])
	}
	return units
}

// ListFailedUnits runs systemctl to list currently-failed units. A nil
// slice and nil error together mean "no failed units" - the normal case,
// not something to be reported as UNKNOWN.
func ListFailedUnits() ([]string, error) {
	out, err := exec.Command("systemctl", "--failed", "--no-legend", "--plain").Output()
	if err != nil {
		return nil, err
	}
	return ParseFailedUnits(string(out)), nil
}

// ParseUnitActiveState trims the raw stdout of `systemctl is-active <unit>`
// into its state string (e.g. "active", "inactive", "failed",
// "activating"). It performs no I/O and is exercised directly by tests.
func ParseUnitActiveState(output string) string {
	return strings.TrimSpace(output)
}

// UnitActiveState runs `systemctl is-active <unit>` and returns its parsed
// state string. systemctl exits non-zero for every state except "active",
// so the error return is deliberately discarded here - only the parsed
// state string is meaningful, including "unknown" or "" for a unit that
// has never been loaded (not installed on this platform/build).
func UnitActiveState(unit string) string {
	out, _ := exec.Command("systemctl", "is-active", unit).Output()
	return ParseUnitActiveState(string(out))
}

// UnitInstalled reports whether state (as returned by UnitActiveState)
// indicates the unit is loaded at all, as opposed to never having existed
// on this system - "inactive"/"failed"/"activating"/"active" all mean the
// unit is loaded (installed), even if not currently running; "unknown" and
// "" mean systemctl has no record of it.
func UnitInstalled(state string) bool {
	switch state {
	case "", "unknown":
		return false
	default:
		return true
	}
}
