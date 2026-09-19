package ota

import (
	"os"
	"path/filepath"
	"testing"
)

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
	// / is always its own findmnt target, on any system, in any mode
	// (overlay or bare) - this is the one universally-true dedicated-mount
	// fact every test environment shares, and IsDedicatedPersistentMount's
	// own tests below build directly on it.
	if id.Target != "/" {
		t.Errorf("Target = %q, want \"/\" - / must always be its own mount target", id.Target)
	}
}

func TestStatMount_NonexistentPathErrors(t *testing.T) {
	if _, err := StatMount("/nonexistent/ota/mount/test/path"); err == nil {
		t.Error("expected an error statting a nonexistent path")
	}
}

// TestStatMount_SymlinkResolvesToRealTarget proves a symlink cannot be used
// to substitute a volatile location for a genuine one: stat(2) (via
// syscall.Stat, which follows symlinks) and findmnt --target (which also
// resolves the real path) must agree on the same, real destination - not
// whatever the symlink's own containing directory happens to be.
func TestStatMount_SymlinkResolvesToRealTarget(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link-to-root")
	if err := os.Symlink("/", link); err != nil {
		t.Fatalf("could not create symlink: %v", err)
	}
	id, err := StatMount(link)
	if err != nil {
		t.Fatalf("StatMount(symlink to /) error: %v", err)
	}
	if id.Target != "/" {
		t.Errorf("Target = %q, want \"/\" - a symlink to / must resolve to /'s own real mount, not the symlink's containing directory", id.Target)
	}
}

// --- IsDedicatedPersistentMount ---

func TestIsDedicatedPersistentMount_AcceptsGenuineDedicatedMount(t *testing.T) {
	// Target == the requested path itself is exactly what findmnt reports
	// for a real, separately-mounted filesystem - a dedicated partition
	// (a different device than root entirely, the one currently-known-
	// correct hardware layout) and a bind-mounted subtree of the real
	// lower root (a possible future provisioning design, sharing root's
	// own device) both satisfy this identically, as they must.
	candidate := MountIdentity{Path: "/var/lib/stratux-data", Device: 999, FSType: "ext4", Target: "/var/lib/stratux-data"}
	ok, reason := IsDedicatedPersistentMount(candidate, "/var/lib/stratux-data")
	if !ok {
		t.Fatalf("a genuine dedicated mount must be accepted, got rejection: %s", reason)
	}
}

func TestIsDedicatedPersistentMount_RejectsPathCoveredByAncestorMount(t *testing.T) {
	// This is the exact incident this function exists to prevent: the
	// requested path is merely an ordinary directory reached through the
	// root overlay (or any other covering ancestor mount) - findmnt's own
	// Target then names that ancestor ("/"), not the requested path.
	candidate := MountIdentity{Path: "/var/lib/stratux-data", Device: 45826, FSType: "overlay", Target: "/"}
	ok, reason := IsDedicatedPersistentMount(candidate, "/var/lib/stratux-data")
	if ok {
		t.Fatal("a path only covered by an ancestor mount must be rejected, not accepted as dedicated")
	}
	if reason == "" {
		t.Error("rejection must include a reason")
	}
}

func TestIsDedicatedPersistentMount_RejectsVolatileFSTypeEvenAtOwnTarget(t *testing.T) {
	// Even if a future misconfiguration mounted tmpfs directly and exactly
	// at the requested path (Target does equal the path), it must still
	// be rejected - being a dedicated mount is necessary but not
	// sufficient; the filesystem type itself must not be volatile.
	for _, fstype := range []string{"tmpfs", "overlay", "ramfs", "devtmpfs", "aufs", "unionfs", "overlayfs"} {
		candidate := MountIdentity{Path: "/var/lib/stratux-data", Device: 1, FSType: fstype, Target: "/var/lib/stratux-data"}
		if ok, _ := IsDedicatedPersistentMount(candidate, "/var/lib/stratux-data"); ok {
			t.Errorf("fstype %q must be rejected as volatile even when it is its own dedicated mount target", fstype)
		}
	}
}

func TestIsDedicatedPersistentMount_DeviceNumberIsIrrelevant(t *testing.T) {
	// Deliberately does NOT compare device numbers against root or
	// anything else - a dedicated partition (a different device than
	// root) and a same-device bind-mounted subtree of the real lower root
	// must both be accepted equally. This is the exact bug this function
	// replaces: an earlier version of this guard compared device numbers
	// against /overlay/robase and would have wrongly rejected a real,
	// correctly-configured dedicated partition for having a different
	// device number than root.
	sameDeviceAsRoot := MountIdentity{Path: "/var/lib/stratux-data", Device: 45826, FSType: "ext4", Target: "/var/lib/stratux-data"}
	differentDeviceThanRoot := MountIdentity{Path: "/var/lib/stratux-data", Device: 987654, FSType: "ext4", Target: "/var/lib/stratux-data"}
	for _, c := range []MountIdentity{sameDeviceAsRoot, differentDeviceThanRoot} {
		if ok, reason := IsDedicatedPersistentMount(c, "/var/lib/stratux-data"); !ok {
			t.Errorf("device number %d must not affect the result, got rejection: %s", c.Device, reason)
		}
	}
}
