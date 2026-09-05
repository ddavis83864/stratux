//go:build !linux

package readiness

import "fmt"

// statPath has no non-Linux implementation: Stratux only runs on Linux
// (Raspberry Pi OS / Debian), and the exact Statfs_t layout used by
// storage_linux.go is Linux-specific. This stub exists only so the
// readiness package still compiles (with a clear runtime error, not a
// build failure) if it is ever imported from a non-Linux build.
func statPath(path string) (StatfsResult, error) {
	return StatfsResult{}, fmt.Errorf("readiness.StatPath: unsupported on this OS, Stratux targets Linux only")
}
