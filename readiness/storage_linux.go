//go:build linux

package readiness

import "syscall"

// statPath is the Linux implementation of StatPath, using the statfs(2)
// syscall directly (not the du package main/ uses elsewhere) so inode
// counts are available alongside byte counts.
//
// Both Bfree (all free blocks, including those reserved for root) and
// Bavail (blocks free to an unprivileged process) are captured - see
// StatfsResult's doc comment for which one each derived quantity uses and
// why. A prior version of this comment claimed Bavail was "what df
// reports" for its Used column; that was checked against a live device
// and found wrong (df's Used matches Total-Bfree, not Total-Bavail) -
// see docs/readiness-and-time-trust.md's storage-accounting section for
// the measured evidence.
func statPath(path string) (StatfsResult, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return StatfsResult{}, err
	}
	bsize := uint64(st.Bsize)
	return StatfsResult{
		TotalBytes:     st.Blocks * bsize,
		FreeBytes:      uint64(st.Bfree) * bsize,
		AvailableBytes: uint64(st.Bavail) * bsize,
		TotalInodes:    st.Files,
		FreeInodes:     st.Ffree,
	}, nil
}
