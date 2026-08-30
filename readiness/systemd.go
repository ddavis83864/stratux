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
