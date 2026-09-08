//go:build linux

package storagelifecycle

import "syscall"

// syscallNoFollow is OR'd into CreateExclusive's open flags so a symlink
// planted at the target path (a TOCTOU attack, or simply a stray file
// left by something else) is never followed - combined with O_EXCL,
// which already refuses to open anything that exists at all, this is
// belt-and-suspenders rather than the primary defense.
const syscallNoFollow = syscall.O_NOFOLLOW
