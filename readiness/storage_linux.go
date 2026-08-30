//go:build linux

package readiness

import "syscall"

// statPath is the Linux implementation of StatPath, using the statfs(2)
// syscall directly (not the du package main/ uses elsewhere) so inode
// counts are available alongside byte counts.
//
// Bavail (blocks available to an unprivileged user), not Bfree (blocks
// free including those reserved for root), is used for AvailableBytes:
// stratuxrun runs as root today (see debian/stratux.service), but
// Bavail is what `df` reports and what an operator comparing the
// dashboard against `df -h` on the device expects to see.
func statPath(path string) (StatfsResult, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return StatfsResult{}, err
	}
	bsize := uint64(st.Bsize)
	return StatfsResult{
		TotalBytes:     st.Blocks * bsize,
		AvailableBytes: uint64(st.Bavail) * bsize,
		TotalInodes:    st.Files,
		FreeInodes:     st.Ffree,
	}, nil
}
