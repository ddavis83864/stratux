package readiness

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// StorageThresholds are the utilization points at which persistent storage
// health degrades. Percentages are of used space (0-100).
//
// The defaults match the mission requirement: warn at 80%, critical at
// 90%, and prohibit new recording at 95% or when the filesystem is
// read-only/unavailable. They are fields, not constants, precisely so a
// deployment can retune them without a code change.
type StorageThresholds struct {
	WarnPercent                float64
	CriticalPercent            float64
	RecordingProhibitedPercent float64
}

// DefaultPersistentStorageThresholds returns the mission-required initial
// thresholds for the persistent data partition.
func DefaultPersistentStorageThresholds() StorageThresholds {
	return StorageThresholds{
		WarnPercent:                80,
		CriticalPercent:            90,
		RecordingProhibitedPercent: 95,
	}
}

// StatfsResult is the subset of a filesystem's statfs(2) result that
// storage health needs. It exists so EvaluateStorage can be exercised with
// synthetic values in tests without touching a real filesystem, while
// StatPath (below) is the thin wrapper that fills one in for real.
//
// ext4 (the persistent data partition's filesystem) reserves a percentage
// of blocks that only a privileged (root) process may use - `tune2fs -l`
// on the live device confirmed this. That split shows up here as two
// distinct free-space quantities:
//
//   - FreeBytes is statfs's Bfree: every free block, including the
//     root-reserved ones. This is what `df` subtracts from Total to get
//     its "Used" column - confirmed against the live device with exact
//     byte-level arithmetic (`df -B1`, `stat -f`, `tune2fs -l`) - and it is
//     also what stratuxrun, which always runs as root, can actually still
//     write: root is not restricted by the reserved-blocks percentage.
//   - AvailableBytes is statfs's Bavail: free blocks minus the
//     root-reserved percentage, i.e. what an unprivileged process could
//     still write. Kept as a raw, informational field (mission
//     requirement to expose the raw numbers) so an operator can see the
//     reserved-blocks gap directly, but it is not what this package bases
//     "Used"/"available for recording" on, because the recording process
//     is not unprivileged.
//
// The one model this package uses everywhere - UsedBytes,
// UtilizationPercent, and recording admission - is the FreeBytes-based
// one, precisely so `/getHealth` agrees with `df` instead of appearing to
// disagree by the size of the reserved-blocks percentage (the discrepancy
// this field split exists to resolve; see
// docs/readiness-and-time-trust.md for the measured evidence).
//
// On tmpfs (the temporary overlay), there is no root-reserved-blocks
// concept at all, so the kernel reports FreeBytes == AvailableBytes there
// - the two models coincide automatically for that mount, without any
// special-casing in this package.
type StatfsResult struct {
	TotalBytes     uint64
	FreeBytes      uint64 // statfs Bfree: all free blocks, including root-reserved ones - what df's Used and this package's admission model use
	AvailableBytes uint64 // statfs Bavail: free blocks available to an unprivileged process - raw/informational only, see type doc
	TotalInodes    uint64
	FreeInodes     uint64
}

// UsedBytes returns TotalBytes-FreeBytes, matching what `df` reports as
// Used (see the type doc for why FreeBytes, not AvailableBytes, is the
// basis for this).
func (r StatfsResult) UsedBytes() uint64 {
	if r.FreeBytes >= r.TotalBytes {
		return 0
	}
	return r.TotalBytes - r.FreeBytes
}

// UtilizationPercent returns used/total as a 0-100 percentage, on the same
// FreeBytes-based model as UsedBytes. A zero-total filesystem (never
// expected on a real mount, but possible from a zero-value StatfsResult)
// reports 0, not NaN or a divide-by-zero panic.
func (r StatfsResult) UtilizationPercent() float64 {
	if r.TotalBytes == 0 {
		return 0
	}
	return float64(r.UsedBytes()) / float64(r.TotalBytes) * 100
}

// AvailableForRecording returns the bytes actually available to the
// recording process, which runs as root and can therefore use the
// ext4 reserved-blocks percentage the same as any other root-owned write
// - so this is FreeBytes, not AvailableBytes. A deployment that ever runs
// the recording process as a non-root user would need AvailableBytes
// instead; stratuxrun does not.
func (r StatfsResult) AvailableForRecording() uint64 {
	return r.FreeBytes
}

// InodeUtilizationPercent returns used-inodes/total-inodes as a 0-100
// percentage, or 0 if the filesystem does not report a fixed inode count
// (TotalInodes == 0, as tmpfs and some network filesystems do).
func (r StatfsResult) InodeUtilizationPercent() float64 {
	if r.TotalInodes == 0 {
		return 0
	}
	used := r.TotalInodes - r.FreeInodes
	return float64(used) / float64(r.TotalInodes) * 100
}

// StorageHealth is the full health record for one storage area (the
// persistent data partition, or separately the temporary overlay). It is
// the JSON shape exposed by the health API's Storage/TemporaryOverlay
// fields.
type StorageHealth struct {
	State  ComponentState
	Reason string

	Present  bool // the mount point exists / was configured
	Mounted  bool // something is actually mounted there right now
	ReadOnly bool

	Source         string // device/mount source reported by the OS, e.g. "/dev/mmcblk0p3"
	FilesystemUUID string
	ExpectedUUID   string // empty means "not checked"
	UUIDMatches    bool

	TotalBytes uint64
	UsedBytes  uint64 // TotalBytes-FreeBytes; matches `df`'s Used column - see StatfsResult's doc comment
	// FreeBytes and AvailableBytes are both raw statfs numbers, exposed
	// alongside the derived UsedBytes/UtilizationPercent/RecordingAllowed
	// so an operator (or a future consumer with different privilege
	// assumptions) can see the reserved-blocks split for themselves rather
	// than trusting only this package's chosen model.
	FreeBytes               uint64 // statfs Bfree: all free blocks, including root-reserved ones
	AvailableBytes          uint64 // statfs Bavail: free blocks available to an unprivileged process
	UtilizationPercent      float64
	InodeUtilizationPercent float64

	LastWriteTestTime  time.Time
	LastWriteTestOK    bool
	LastWriteTestError string

	Thresholds       StorageThresholds
	RecordingAllowed bool
}

// EvaluateStorage derives a StorageHealth from already-gathered signals. It
// performs no I/O itself, so it is exercised directly by tests with
// synthetic StatfsResult values covering every threshold boundary.
//
// expectedUUID may be empty, meaning "the caller does not want UUID
// verification" (used for the temporary overlay, which has no fixed
// identity to check).
func EvaluateStorage(
	present, mounted, readOnly bool,
	source, actualUUID, expectedUUID string,
	stat StatfsResult,
	writeTestTime time.Time, writeTestOK bool, writeTestErr error,
	thresholds StorageThresholds,
) StorageHealth {
	h := StorageHealth{
		Present:                 present,
		Mounted:                 mounted,
		ReadOnly:                readOnly,
		Source:                  source,
		FilesystemUUID:          actualUUID,
		ExpectedUUID:            expectedUUID,
		TotalBytes:              stat.TotalBytes,
		UsedBytes:               stat.UsedBytes(),
		FreeBytes:               stat.FreeBytes,
		AvailableBytes:          stat.AvailableBytes,
		UtilizationPercent:      stat.UtilizationPercent(),
		InodeUtilizationPercent: stat.InodeUtilizationPercent(),
		LastWriteTestTime:       writeTestTime,
		LastWriteTestOK:         writeTestOK,
		Thresholds:              thresholds,
	}
	if expectedUUID != "" {
		h.UUIDMatches = actualUUID == expectedUUID
	}
	if writeTestErr != nil {
		h.LastWriteTestError = writeTestErr.Error()
	}

	switch {
	case !present:
		h.State = StateNotReady
		h.Reason = "persistent storage is not configured on this system"
	case !mounted:
		h.State = StateNotReady
		h.Reason = "persistent storage partition is configured but not mounted"
	case expectedUUID != "" && !h.UUIDMatches:
		h.State = StateNotReady
		h.Reason = fmt.Sprintf("mounted filesystem UUID %q does not match the expected %q - wrong device is mounted at this path", actualUUID, expectedUUID)
	case readOnly:
		h.State = StateNotReady
		h.Reason = "persistent storage is mounted read-only"
	case !writeTestOK:
		h.State = StateNotReady
		h.Reason = "persistent storage failed its write test: " + h.LastWriteTestError
	case stat.UtilizationPercent() >= thresholds.RecordingProhibitedPercent:
		h.State = StateNotReady
		h.Reason = fmt.Sprintf("persistent storage is %.1f%% full, at or above the %.0f%% recording-prohibited threshold", stat.UtilizationPercent(), thresholds.RecordingProhibitedPercent)
	case stat.UtilizationPercent() >= thresholds.CriticalPercent:
		h.State = StateDegraded
		h.Reason = fmt.Sprintf("persistent storage is %.1f%% full, at or above the %.0f%% critical threshold", stat.UtilizationPercent(), thresholds.CriticalPercent)
	case stat.UtilizationPercent() >= thresholds.WarnPercent:
		h.State = StateDegraded
		h.Reason = fmt.Sprintf("persistent storage is %.1f%% full, at or above the %.0f%% warning threshold", stat.UtilizationPercent(), thresholds.WarnPercent)
	default:
		h.State = StateReady
		h.Reason = fmt.Sprintf("persistent storage healthy, %.1f%% used", stat.UtilizationPercent())
	}

	h.RecordingAllowed = present && mounted && !readOnly && writeTestOK &&
		(expectedUUID == "" || h.UUIDMatches) &&
		stat.UtilizationPercent() < thresholds.RecordingProhibitedPercent

	return h
}

// StatPath runs statfs(2) on path and returns the result. It is the one
// real-filesystem-touching half of storage certification; EvaluateStorage
// above does the actual judgment and is what tests exercise.
func StatPath(path string) (StatfsResult, error) {
	return statPath(path)
}

// MountInfo describes what /proc/mounts (via findmnt) reports for a path.
type MountInfo struct {
	Mounted  bool
	Source   string
	FSType   string
	UUID     string
	ReadOnly bool
}

// DiscoverableMount reports whether info is structurally sound enough to
// safely pin as the expected persistent-data filesystem the first time no
// UUID has been configured yet: actually present, actually mounted,
// read-write, and - critically - the expected filesystem type (ext4 for
// the mission's dedicated data partition).
//
// This is the gate between "configurable" and "discoverable" in the
// installation-safety design: an operator can always set an expected UUID
// explicitly ahead of time, but if none is set, the very first UUID this
// package will ever trust is only pinned from a mount that already looks
// exactly like the intended partition - never from an arbitrary mount
// (e.g. an accidental bind mount, a USB stick, or the tmpfs overlay
// itself, which would never pass fsType=="ext4"). Once pinned, every
// subsequent check is the ordinary strict UUID comparison in
// EvaluateStorage - this function is only consulted for the one-time
// discovery decision, not on every check.
func DiscoverableMount(info MountInfo, present bool, expectedFSType string) bool {
	return present && info.Mounted && !info.ReadOnly && info.FSType == expectedFSType && info.UUID != ""
}

var findmntPairPattern = regexp.MustCompile(`(\w+)="([^"]*)"`)

// FindMount reports the current mount for path using findmnt(8), which is
// already a project convention (see debian/stratux-pre-start.sh's overlay
// detection). Mounted is false, with no error, if nothing is mounted
// exactly at path - that is an expected, health-relevant state
// (StateNotReady via EvaluateStorage), not a Go error.
//
// Output is parsed in findmnt's "pairs" form (-P: KEY="value" tokens) rather
// than its default fixed-column form. The default form collapses empty
// columns when splitting on whitespace, which silently misaligns every
// field after the first empty one - and UUID is routinely empty for
// exactly the mount types this package spends the most time on: overlay
// and tmpfs (the Raspberry Pi's own root and its writable overlay layer).
func FindMount(path string) (MountInfo, error) {
	out, err := exec.Command("findmnt", "-n", "-P", "-o", "SOURCE,FSTYPE,UUID,OPTIONS", "--target", path).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// findmnt exits 1 when nothing matches the target - not an error.
			return MountInfo{Mounted: false}, nil
		}
		return MountInfo{}, err
	}
	fields := map[string]string{}
	for _, m := range findmntPairPattern.FindAllStringSubmatch(string(out), -1) {
		fields[m[1]] = m[2]
	}
	if _, ok := fields["SOURCE"]; !ok {
		return MountInfo{}, fmt.Errorf("unexpected findmnt output: %q", string(out))
	}
	info := MountInfo{
		Mounted: true,
		Source:  fields["SOURCE"],
		FSType:  fields["FSTYPE"],
		UUID:    fields["UUID"],
	}
	for _, opt := range strings.Split(fields["OPTIONS"], ",") {
		if opt == "ro" {
			info.ReadOnly = true
		}
	}
	return info, nil
}

// WriteTest attempts to create, write, and remove a small marker file in
// dir, returning whether the directory is genuinely writable right now. A
// filesystem can report itself as mounted read-write while still being
// unwritable in practice (e.g. out of space, permissions, or a
// wedged/corrupted filesystem returning I/O errors) - this is the check
// that catches that case rather than trusting the mount option alone.
func WriteTest(dir string) (time.Time, bool, error) {
	now := time.Now().UTC()
	marker := fmt.Sprintf("%s/.readiness-write-test-%d", dir, now.UnixNano())
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return now, false, err
	}
	if _, err := f.WriteString("readiness write test\n"); err != nil {
		f.Close()
		os.Remove(marker)
		return now, false, err
	}
	if err := f.Close(); err != nil {
		os.Remove(marker)
		return now, false, err
	}
	if err := os.Remove(marker); err != nil {
		// The write succeeded even though cleanup failed; that is still a
		// successful write test; report the cleanup problem separately
		// rather than failing the whole test on it.
		return now, true, fmt.Errorf("write test succeeded but cleanup failed: %w", err)
	}
	return now, true, nil
}

// CertifyPersistentStorage gathers live signals for path (statfs, mount
// source/UUID/read-only state, a write test) and evaluates them against
// thresholds. expectedUUID may be empty to skip UUID verification.
func CertifyPersistentStorage(path, expectedUUID string, thresholds StorageThresholds) StorageHealth {
	mnt, mntErr := FindMount(path)
	present := true
	if _, statErr := os.Stat(path); statErr != nil {
		present = false
	}

	stat, statfsErr := StatPath(path)
	var writeTime time.Time
	var writeOK bool
	var writeErr error
	if mntErr == nil && mnt.Mounted && !mnt.ReadOnly && present {
		writeTime, writeOK, writeErr = WriteTest(path)
	} else if statfsErr != nil {
		writeErr = statfsErr
	} else if mntErr != nil {
		writeErr = mntErr
	}

	return EvaluateStorage(
		present, mnt.Mounted, mnt.ReadOnly,
		mnt.Source, mnt.UUID, expectedUUID,
		stat,
		writeTime, writeOK, writeErr,
		thresholds,
	)
}
