package ota

import "testing"

func ext4Reference() MountIdentity {
	return MountIdentity{Path: "/overlay/robase", Device: 45826, FSType: "ext4"}
}

// --- Volatile marker rejection ---

func TestIsPersistent_RejectsTmpfs(t *testing.T) {
	// This is exactly the /overlay/pivot/overlay shadow case proven on
	// hardware: same-looking path, different (tmpfs) device.
	candidate := MountIdentity{Path: "/overlay/pivot/overlay", Device: 20, FSType: "tmpfs"}
	ok, reason := IsPersistent(candidate, ext4Reference())
	if ok {
		t.Fatal("a tmpfs-backed candidate must be rejected as non-persistent")
	}
	if reason == "" {
		t.Error("rejection must include a reason")
	}
}

func TestIsPersistent_RejectsOverlay(t *testing.T) {
	candidate := MountIdentity{Path: "/", Device: 22, FSType: "overlay"}
	ok, _ := IsPersistent(candidate, ext4Reference())
	if ok {
		t.Fatal("an overlay-backed candidate must be rejected as non-persistent")
	}
}

func TestIsPersistent_RejectsRamfsAndDevtmpfs(t *testing.T) {
	for _, fstype := range []string{"ramfs", "devtmpfs", "aufs", "unionfs", "overlayfs"} {
		candidate := MountIdentity{Path: "/x", Device: 45826, FSType: fstype}
		if ok, _ := IsPersistent(candidate, ext4Reference()); ok {
			t.Errorf("fstype %q must be rejected as volatile even with a matching device number", fstype)
		}
	}
}

func TestIsPersistent_RejectsDeviceMismatchEvenIfNotVolatileType(t *testing.T) {
	// A non-volatile fstype string on the wrong device (e.g. a second,
	// unrelated ext4 filesystem) must still be rejected - device identity
	// is the load-bearing check, not the fstype name alone.
	candidate := MountIdentity{Path: "/mnt/other-ext4", Device: 999, FSType: "ext4"}
	ok, reason := IsPersistent(candidate, ext4Reference())
	if ok {
		t.Fatal("a device mismatch must be rejected even when fstype matches")
	}
	if reason == "" {
		t.Error("rejection must include a reason")
	}
}

// --- Persistent marker creation ---

func TestIsPersistent_AcceptsMatchingExt4Device(t *testing.T) {
	// This is exactly the /overlay/robase/overlay case proven on
	// hardware: same device number as the real mounted ext4 root.
	candidate := MountIdentity{Path: "/overlay/robase/overlay", Device: 45826, FSType: "ext4"}
	ok, reason := IsPersistent(candidate, ext4Reference())
	if !ok {
		t.Fatalf("a same-device ext4 candidate must be accepted, got rejection: %s", reason)
	}
}

func TestStatMount_RealPath(t *testing.T) {
	id, err := StatMount("/")
	if err != nil {
		t.Fatalf("StatMount(/) error: %v", err)
	}
	if id.FSType == "" {
		t.Error("expected a non-empty filesystem type for /")
	}
}

func TestStatMount_NonexistentPathErrors(t *testing.T) {
	if _, err := StatMount("/nonexistent/ota/mount/test/path"); err == nil {
		t.Error("expected an error statting a nonexistent path")
	}
}
