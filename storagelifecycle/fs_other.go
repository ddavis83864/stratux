//go:build !linux

package storagelifecycle

// syscallNoFollow has no non-Linux value: Stratux only runs on Linux, and
// O_NOFOLLOW's numeric value is platform-specific. 0 (no additional flag)
// keeps this package buildable on a non-Linux dev machine; O_CREATE|O_EXCL
// alone (see CreateExclusive) already refuses to open anything that
// exists at that path, symlink or not, so this only weakens the
// belt-and-suspenders half of the defense on a platform Stratux does not
// ship on.
const syscallNoFollow = 0
