package preflight

import (
	"strconv"
)

// itoaLocal and formatFloat are tiny, dependency-free formatting helpers
// used only when building a CheckResult.Reason string - kept separate
// from strconv.Itoa/FormatFloat call sites only so every Reason string in
// checks.go reads the same way (one decimal place for a float, plain
// decimal for an int) without repeating the format spec at every call
// site.
func itoaLocal(n int) string {
	return strconv.Itoa(n)
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', 1, 64)
}
