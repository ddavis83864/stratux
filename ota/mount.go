// Package ota implements a deterministic, resumable OTA update state
// machine for the .deb-based package update mechanism, built on the
// project's protected read-only-root overlay architecture
// (image_build/stage2/10-stratux/files/{overlayctl,init-overlay},
// debian/stratux-pre-start.sh).
//
// The central fact this package encodes and defends against: while the
// system is running under the overlay, the path "/overlay/disable" (which
// /sbin/init-overlay checks at the very start of the next boot, before any
// overlay/tmpfs setup happens) is NOT itself a stable location - a marker
// written there lands on whatever is currently mounted at that exact
// path, which during a live overlay session can be a volatile shadow
// mount rather than genuinely persistent storage. This was proven
// directly on hardware (not assumed): with the overlay active,
// "/overlay/pivot/overlay" - one of two paths that both plausibly look
// like "the real underlying directory" - is device-number-verified to be
// a tmpfs left mounted there by init-overlay's own mount choreography (it
// never explicitly relocates the original top-level "/overlay" tmpfs
// mount when moving its named children into the pivoted root), while
// "/overlay/robase/overlay" shares its device number with the real
// mounted ext4 partition. A marker written to the tmpfs path vanished on
// reboot; a marker written to the ext4-backed path survived byte-for-byte
// on the same device and inode, and was independently proven to actually
// cause the next boot to mount bare ext4 (not the overlay) via its root
// mount source, and marker removal was proven to restore the overlay on
// the following boot. See docs/ota.md for the full evidence.
package ota

import (
	"fmt"
	"syscall"

	"github.com/stratux/stratux/readiness"
)

// MountIdentity captures a path's device number and filesystem type, used
// to prove a marker write actually lands on genuinely persistent storage
// rather than a volatile shadow mounted over the intended directory.
type MountIdentity struct {
	Path   string
	Device uint64
	FSType string
}

// volatileFSTypes are filesystem types that never persist across a
// reboot. A candidate marker location reporting one of these must always
// be rejected, regardless of what device number it happens to report.
var volatileFSTypes = map[string]bool{
	"tmpfs":     true,
	"overlay":   true,
	"overlayfs": true,
	"ramfs":     true,
	"devtmpfs":  true,
	"aufs":      true,
	"unionfs":   true,
}

// IsPersistent reports whether candidate is safe to use as a persistent
// marker location, given reference - a MountIdentity for a path already
// known to be on genuinely persistent storage (e.g. the real ext4 lower
// root itself). candidate must not be a volatile filesystem type, and
// must share reference's device number: a bind mount, or a direct mount
// of the same block device, both report the same device number as their
// source, while any filesystem stacked on top afterward (a tmpfs
// deliberately or accidentally mounted at the same path) does not.
//
// This is the automated form of the exact check that proved the marker
// hypothesis on real hardware: device-number equality, not path spelling,
// is what makes a location trustworthy.
func IsPersistent(candidate, reference MountIdentity) (bool, string) {
	if volatileFSTypes[candidate.FSType] {
		return false, fmt.Sprintf("%s is a volatile filesystem (%s), not persistent storage", candidate.Path, candidate.FSType)
	}
	if candidate.Device != reference.Device {
		return false, fmt.Sprintf("%s (device %d) does not match the reference persistent device %s (device %d) - likely a shadow mount", candidate.Path, candidate.Device, reference.Path, reference.Device)
	}
	return true, ""
}

// StatMount gathers the MountIdentity for a real path via stat(2) for the
// device number and findmnt (via readiness.FindMount) for the filesystem
// type. This is the one real-filesystem-touching half; IsPersistent above
// is what tests exercise directly with synthetic values.
func StatMount(path string) (MountIdentity, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return MountIdentity{}, fmt.Errorf("stat %s: %w", path, err)
	}
	mnt, err := readiness.FindMount(path)
	if err != nil {
		return MountIdentity{}, fmt.Errorf("findmnt %s: %w", path, err)
	}
	return MountIdentity{Path: path, Device: uint64(st.Dev), FSType: mnt.FSType}, nil
}
