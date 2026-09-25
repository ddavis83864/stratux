package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stratux/stratux/fisbcache"
)

// withUnmountedPersistentData makes the production persistent-mount guard
// (see ensurePersistentDataMounted) report that the data partition is NOT
// genuinely mounted, for one test.
func withUnmountedPersistentData(t *testing.T) {
	t.Helper()
	orig := ensurePersistentDataMounted
	ensurePersistentDataMounted = func() error { return errors.New("persistent data partition is not mounted") }
	t.Cleanup(func() { ensurePersistentDataMounted = orig })
}

// The FIS-B cache is one more persistence namespace under
// PersistentDataPath. Like the others (docs/persistent-data-partition.md),
// it must never write into the RAM-backed overlay directory that exists at
// that path when the real partition failed to mount.

func TestFISBCacheSettings_SaveRefusedWhenDataPartitionNotMounted(t *testing.T) {
	withTestFISBCacheSettingsPath(t)
	withUnmountedPersistentData(t)
	s := DefaultFISBCacheSettings()
	s.Enabled = true
	if err := saveFISBCacheSettings(s); err == nil {
		t.Fatal("saveFISBCacheSettings must fail when the data partition is not mounted")
	}
	if _, err := os.Stat(fisbCacheSettingsPath); !os.IsNotExist(err) {
		t.Fatalf("no settings file may be written, stat err = %v", err)
	}
	if _, err := os.Stat(fisbCacheSettingsPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("no temp settings file may be written, stat err = %v", err)
	}
}

func TestFISBCacheSettings_SaveStillWorksWhenMounted(t *testing.T) {
	withTestFISBCacheSettingsPath(t)
	s := DefaultFISBCacheSettings()
	s.Enabled = true
	if err := saveFISBCacheSettings(s); err != nil {
		t.Fatalf("save with a mounted partition: %v", err)
	}
	if got := loadFISBCacheSettings(); !got.Enabled {
		t.Fatalf("round trip lost Enabled: %+v", got)
	}
}

func TestFISBCachePersist_RefusedWhenDataPartitionNotMounted(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	withUnmountedPersistentData(t)
	entry := fisbcache.Entry{Key: makeFISBTestKey("KSEA"), ReceivedAtMonotonic: 1}
	if err := fisbCachePersist(entry, "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err == nil {
		t.Fatal("fisbCachePersist must fail when the data partition is not mounted")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("nothing may be written into the cache directory, found %v", files)
	}
}

func TestFISBCachePersist_ResumesOnceDataPartitionIsMounted(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	orig := ensurePersistentDataMounted
	mounted := false
	ensurePersistentDataMounted = func() error {
		if !mounted {
			return errors.New("not mounted yet")
		}
		return nil
	}
	t.Cleanup(func() { ensurePersistentDataMounted = orig })
	entry := fisbcache.Entry{Key: makeFISBTestKey("KSEA"), ReceivedAtMonotonic: 1}
	payload := "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"
	if err := fisbCachePersist(entry, payload); err == nil {
		t.Fatal("expected refusal before the mount is ready")
	}
	mounted = true
	if err := fisbCachePersist(entry, payload); err != nil {
		t.Fatalf("persist after the mount became ready: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 1 {
		t.Fatalf("expected exactly one cache file after the mount is ready, found %v", files)
	}
}

func TestInitFISBCache_DoesNotCreateCacheDirWhenDataPartitionNotMounted(t *testing.T) {
	withTestFISBCacheSettingsPath(t)
	origDir := fisbCacheDir
	fisbCacheDir = filepath.Join(t.TempDir(), "fisb-weather-cache")
	t.Cleanup(func() { fisbCacheDir = origDir })
	withUnmountedPersistentData(t)
	ensureStratuxClockForTest()
	initFISBCache()
	if _, err := os.Stat(fisbCacheDir); !os.IsNotExist(err) {
		t.Fatalf("the cache directory must not be created without a mounted data partition, stat err = %v", err)
	}
	fisbCacheMu.Lock()
	failed := fisbCacheRecoveryError
	fisbCacheMu.Unlock()
	if !failed {
		t.Fatal("the unmounted partition must be reported (recovery error flag), not silently ignored")
	}
}
