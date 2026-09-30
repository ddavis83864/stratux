package fisbrecorder

import "github.com/ricochet2200/go-disk-usage/du"

// realDiskFreeBytes reports free space on the filesystem containing path,
// scoped to that path specifically - not "/" - which is exactly the
// mistake the design doc documents main/trace.go's own TraceLog making
// (checking "/" - the overlay - while writing to /var/log, a different
// filesystem). Reuses this project's own already-declared dependency
// (github.com/ricochet2200/go-disk-usage/du, already imported by
// main/trace.go) rather than adding a new one.
func realDiskFreeBytes(path string) (uint64, error) {
	return du.NewDiskUsage(path).Available(), nil
}
