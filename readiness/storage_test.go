package readiness

import (
	"errors"
	"testing"
	"time"
)

func thresholds() StorageThresholds {
	return DefaultPersistentStorageThresholds()
}

func statOfPercent(usedPercent float64) StatfsResult {
	const total = uint64(1_000_000_000) // 1 GB, round numbers for readable test math
	used := uint64(float64(total) * usedPercent / 100)
	return StatfsResult{
		TotalBytes:     total,
		AvailableBytes: total - used,
		TotalInodes:    1000,
		FreeInodes:     900,
	}
}

func TestEvaluateStorage_Absent(t *testing.T) {
	h := EvaluateStorage(false, false, false, "", "", "", StatfsResult{}, time.Time{}, false, nil, thresholds())
	if h.State != StateNotReady {
		t.Errorf("absent storage State = %q, want NOT_READY", h.State)
	}
	if h.RecordingAllowed {
		t.Error("absent storage must not allow recording")
	}
}

func TestEvaluateStorage_PresentNotMounted(t *testing.T) {
	h := EvaluateStorage(true, false, false, "", "", "", StatfsResult{}, time.Time{}, false, nil, thresholds())
	if h.State != StateNotReady {
		t.Errorf("unmounted storage State = %q, want NOT_READY", h.State)
	}
	if h.RecordingAllowed {
		t.Error("unmounted storage must not allow recording")
	}
}

func TestEvaluateStorage_WrongUUID(t *testing.T) {
	h := EvaluateStorage(true, true, false, "/dev/mmcblk0p3", "wrong-uuid", "fa3cfa53-8933-4263-a19b-25227dbf13e6",
		statOfPercent(10), time.Now(), true, nil, thresholds())
	if h.State != StateNotReady {
		t.Errorf("wrong-UUID storage State = %q, want NOT_READY", h.State)
	}
	if h.UUIDMatches {
		t.Error("UUIDMatches should be false")
	}
	if h.RecordingAllowed {
		t.Error("wrong-UUID storage must not allow recording, even though it is mounted and writable")
	}
}

func TestEvaluateStorage_CorrectUUID(t *testing.T) {
	uuid := "fa3cfa53-8933-4263-a19b-25227dbf13e6"
	h := EvaluateStorage(true, true, false, "/dev/mmcblk0p3", uuid, uuid,
		statOfPercent(10), time.Now(), true, nil, thresholds())
	if !h.UUIDMatches {
		t.Error("UUIDMatches should be true")
	}
	if h.State != StateReady {
		t.Errorf("healthy storage State = %q, want READY", h.State)
	}
	if !h.RecordingAllowed {
		t.Error("healthy, correctly-identified, low-utilization storage should allow recording")
	}
}

func TestEvaluateStorage_ReadOnly(t *testing.T) {
	h := EvaluateStorage(true, true, true, "/dev/mmcblk0p3", "", "", statOfPercent(5), time.Now(), false, nil, thresholds())
	if h.State != StateNotReady {
		t.Errorf("read-only storage State = %q, want NOT_READY", h.State)
	}
	if h.RecordingAllowed {
		t.Error("read-only storage must not allow recording")
	}
}

func TestEvaluateStorage_WriteTestFailed(t *testing.T) {
	h := EvaluateStorage(true, true, false, "/dev/mmcblk0p3", "", "", statOfPercent(5),
		time.Now(), false, errors.New("input/output error"), thresholds())
	if h.State != StateNotReady {
		t.Errorf("failed write-test State = %q, want NOT_READY", h.State)
	}
	if h.LastWriteTestError == "" {
		t.Error("LastWriteTestError should be populated")
	}
	if h.RecordingAllowed {
		t.Error("a filesystem that just failed its write test must not allow recording, regardless of mount options")
	}
}

// Threshold boundaries: warn at 80, critical at 90, recording-prohibited at 95.
func TestEvaluateStorage_ThresholdBoundaries(t *testing.T) {
	cases := []struct {
		name            string
		percent         float64
		wantState       ComponentState
		wantRecordingOK bool
	}{
		{"just under warn", 79.9, StateReady, true},
		{"exactly at warn", 80, StateDegraded, true},
		{"between warn and critical", 85, StateDegraded, true},
		{"exactly at critical", 90, StateDegraded, true},
		{"between critical and prohibited", 92, StateDegraded, true},
		{"exactly at prohibited", 95, StateNotReady, false},
		{"above prohibited", 99, StateNotReady, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := EvaluateStorage(true, true, false, "/dev/mmcblk0p3", "u", "u", statOfPercent(c.percent),
				time.Now(), true, nil, thresholds())
			if h.State != c.wantState {
				t.Errorf("%.1f%% used: State = %q, want %q", c.percent, h.State, c.wantState)
			}
			if h.RecordingAllowed != c.wantRecordingOK {
				t.Errorf("%.1f%% used: RecordingAllowed = %v, want %v", c.percent, h.RecordingAllowed, c.wantRecordingOK)
			}
		})
	}
}

func TestEvaluateStorage_ConfigurableThresholds(t *testing.T) {
	custom := StorageThresholds{WarnPercent: 50, CriticalPercent: 60, RecordingProhibitedPercent: 70}
	h := EvaluateStorage(true, true, false, "d", "u", "u", statOfPercent(55), time.Now(), true, nil, custom)
	if h.State != StateDegraded {
		t.Errorf("with custom thresholds, 55%% used State = %q, want DEGRADED (default thresholds would say READY)", h.State)
	}
}

func TestEvaluateStorage_TemporaryOverlayHasNoUUIDCheck(t *testing.T) {
	// The temporary overlay has no fixed identity to verify - passing an
	// empty expectedUUID must skip the check entirely, not fail it.
	h := EvaluateStorage(true, true, false, "tmpfs", "", "", statOfPercent(10), time.Now(), true, nil, thresholds())
	if h.UUIDMatches {
		t.Error("UUIDMatches should be false (unset), not true, when no expectedUUID was given")
	}
	if h.State != StateReady {
		t.Errorf("overlay with no UUID check should still read READY when healthy, got %q: %s", h.State, h.Reason)
	}
}

func TestStatfsResult_ZeroTotalDoesNotPanic(t *testing.T) {
	var r StatfsResult
	if got := r.UtilizationPercent(); got != 0 {
		t.Errorf("zero-value UtilizationPercent = %v, want 0", got)
	}
	if got := r.InodeUtilizationPercent(); got != 0 {
		t.Errorf("zero-value InodeUtilizationPercent = %v, want 0", got)
	}
}

func TestWriteTest_RealTempDir(t *testing.T) {
	dir := t.TempDir()
	when, ok, err := WriteTest(dir)
	if !ok || err != nil {
		t.Fatalf("WriteTest(%q) = ok=%v err=%v, want ok=true err=nil", dir, ok, err)
	}
	if time.Since(when) > time.Minute {
		t.Errorf("WriteTest timestamp %v looks stale", when)
	}
}

func TestWriteTest_NonexistentDirFails(t *testing.T) {
	_, ok, err := WriteTest("/nonexistent/path/for/readiness/test")
	if ok || err == nil {
		t.Error("WriteTest against a nonexistent directory should fail")
	}
}

func TestStatPath_RealTempDir(t *testing.T) {
	dir := t.TempDir()
	stat, err := StatPath(dir)
	if err != nil {
		t.Fatalf("StatPath(%q) error: %v", dir, err)
	}
	if stat.TotalBytes == 0 {
		t.Error("a real filesystem should report nonzero TotalBytes")
	}
}

func TestFindMount_RealTempDir(t *testing.T) {
	dir := t.TempDir()
	info, err := FindMount(dir)
	if err != nil {
		t.Fatalf("FindMount(%q) error: %v", dir, err)
	}
	// A tmp dir is not its own mount point, so findmnt should resolve to
	// whatever filesystem contains it (still "Mounted", just not at this
	// exact path) - the important thing is that it does not error.
	_ = info
}

func TestCertifyPersistentStorage_RealTempDirIsWritableAndReady(t *testing.T) {
	dir := t.TempDir()
	h := CertifyPersistentStorage(dir, "", DefaultPersistentStorageThresholds())
	if !h.Present {
		t.Error("an existing temp directory should report Present=true")
	}
	if !h.LastWriteTestOK {
		t.Errorf("a writable temp directory should pass its write test: %s", h.LastWriteTestError)
	}
}

func TestDiscoverableMount_GoodExt4Mount(t *testing.T) {
	info := MountInfo{Mounted: true, FSType: "ext4", UUID: "fa3cfa53-8933-4263-a19b-25227dbf13e6", ReadOnly: false}
	if !DiscoverableMount(info, true, "ext4") {
		t.Error("a present, mounted, writable ext4 filesystem with a UUID should be discoverable")
	}
}

func TestDiscoverableMount_RejectsWrongFilesystemType(t *testing.T) {
	// An overlay or tmpfs mount at the same path must never be silently
	// pinned as the persistent-data partition.
	info := MountInfo{Mounted: true, FSType: "overlay", UUID: "", ReadOnly: false}
	if DiscoverableMount(info, true, "ext4") {
		t.Error("an overlay filesystem must not be discoverable as the ext4 persistent-data partition")
	}
	tmpfs := MountInfo{Mounted: true, FSType: "tmpfs", UUID: "", ReadOnly: false}
	if DiscoverableMount(tmpfs, true, "ext4") {
		t.Error("a tmpfs filesystem must not be discoverable as the ext4 persistent-data partition")
	}
}

func TestDiscoverableMount_RejectsReadOnly(t *testing.T) {
	info := MountInfo{Mounted: true, FSType: "ext4", UUID: "u", ReadOnly: true}
	if DiscoverableMount(info, true, "ext4") {
		t.Error("a read-only mount must not be discovered/pinned")
	}
}

func TestDiscoverableMount_RejectsNotMountedOrAbsent(t *testing.T) {
	if DiscoverableMount(MountInfo{Mounted: false}, true, "ext4") {
		t.Error("an unmounted path must not be discoverable")
	}
	if DiscoverableMount(MountInfo{Mounted: true, FSType: "ext4", UUID: "u"}, false, "ext4") {
		t.Error("an absent path must not be discoverable even if MountInfo looks fine")
	}
}

func TestDiscoverableMount_RejectsEmptyUUID(t *testing.T) {
	info := MountInfo{Mounted: true, FSType: "ext4", UUID: "", ReadOnly: false}
	if DiscoverableMount(info, true, "ext4") {
		t.Error("a mount with no UUID at all must not be discoverable - there would be nothing to pin")
	}
}

func TestCertifyPersistentStorage_MissingPathIsNotReady(t *testing.T) {
	h := CertifyPersistentStorage("/nonexistent/readiness/mission/path", "", DefaultPersistentStorageThresholds())
	if h.Present {
		t.Error("a nonexistent path should report Present=false")
	}
	if h.State != StateNotReady {
		t.Errorf("missing storage State = %q, want NOT_READY", h.State)
	}
	if h.RecordingAllowed {
		t.Error("missing storage must not allow recording")
	}
}
