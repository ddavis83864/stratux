/*
fisbcachestorage_test.go: production-level integration tests for this
feature's storagelifecycle.CacheLifecycle implementation
(fisbcachestorage.go) - proving the properties the design doc claims but
that no test previously exercised at this level: namespace confinement
(a cleanup can never escape the cache's own directory), that the
namespace is registered exactly once, that global storage pressure
genuinely inhibits new admission (not merely changes a reported label),
and that eviction touches only the files it was explicitly asked to
remove.
*/
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/storagelifecycle"
)

// withTestFISBCacheStorage wires fisbCacheFS/fisbCacheAtomicWriter/
// fisbCacheNamespace/fisbCacheDir at a temp directory for the duration
// of one test, mirroring withFISBCacheTestEnv (fisbcacheapi_test.go) -
// duplicated here (rather than reused) because this file also needs to
// control storageManager directly, which that helper deliberately does
// not touch.
func withTestFISBCacheStorage(t *testing.T) string {
	t.Helper()
	ensureStratuxClockForTest()
	origDir := fisbCacheDir
	origNamespace := fisbCacheNamespace
	origFS := fisbCacheFS
	origWriter := fisbCacheAtomicWriter

	dir := filepath.Join(t.TempDir(), "fisb-weather-cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("could not create temp cache directory: %v", err)
	}
	fisbCacheDir = dir
	fisbCacheNamespace.Root = dir
	fisbCacheFS = storagelifecycle.NewOSFS()
	fisbCacheAtomicWriter = storagelifecycle.NewAtomicWriter(fisbCacheFS)

	t.Cleanup(func() {
		fisbCacheDir = origDir
		fisbCacheNamespace = origNamespace
		fisbCacheFS = origFS
		fisbCacheAtomicWriter = origWriter
	})
	return dir
}

// withTestStorageManagerReportingPressure installs a storageManager that
// always reports exactly `p` (bypassing an actual filesystem scan's own
// thresholds) for the duration of one test - the same fake-callback
// pattern withTestStorageManagerAtPressure (autorecordstorage_test.go)
// already establishes for autorecord, adapted here to force a specific
// PressureState directly rather than deriving one from a utilization
// percentage (fisbCacheStoragePressureProhibited cares only about the
// final classification, so this is the more direct, less brittle way to
// drive every branch: NORMAL, ELEVATED, HIGH, CRITICAL, UNKNOWN).
func withTestStorageManagerReportingPressure(t *testing.T, p storagelifecycle.PressureState) {
	t.Helper()
	dir := t.TempDir()
	registry := storagelifecycle.NewRegistry()
	must(t, registry.Register(storagelifecycle.Namespace{
		ID: "probe", Root: dir,
		Criticality: storagelifecycle.CriticalityBounded, ItemKind: storagelifecycle.ItemKindFile,
	}))
	ensureStratuxClockForTest()

	// A single required-consecutive-sample threshold plus a filesystem-
	// pressure callback whose percentage is chosen to land in exactly
	// the requested band, per readiness.DefaultPersistentStorageThresholds
	// (80/90/95) - UNKNOWN is driven by returning ok=false instead.
	var pct float64
	ok := true
	switch p {
	case storagelifecycle.PressureNormal:
		pct = 10
	case storagelifecycle.PressureElevated:
		pct = 85
	case storagelifecycle.PressureHigh:
		pct = 92
	case storagelifecycle.PressureCritical:
		pct = 100
	case storagelifecycle.PressureUnknown:
		ok = false
	}

	orig := storageManager
	storageManager = storagelifecycle.NewManager(storagelifecycle.ManagerConfig{
		Registry:             registry,
		Policy:               storagelifecycle.Policy{RequiredConsecutivePressureSamples: 1},
		FS:                   storagelifecycle.NewOSFS(),
		Clock:                monotonicSeconds,
		FilesystemPressure:   func() (float64, bool) { return pct, ok },
		FilesystemThresholds: readiness.DefaultPersistentStorageThresholds(),
	})
	t.Cleanup(func() { storageManager = orig })
	if _, performed := storageManager.Scan(); !performed {
		t.Fatal("expected Scan to actually run")
	}
	if got, _ := fisbCacheStoragePressureProhibited(); storagelifecycle.PressureState(got) != p {
		t.Fatalf("test setup failed: wanted pressure %s, storageManager reports %s", p, got)
	}
}

// --- fisbCacheStoragePressureProhibited ---------------------------------

func TestFISBCacheStoragePressureProhibited_NilManagerIsProhibited(t *testing.T) {
	orig := storageManager
	storageManager = nil
	t.Cleanup(func() { storageManager = orig })
	pressure, prohibited := fisbCacheStoragePressureProhibited()
	if !prohibited || pressure != "UNKNOWN" {
		t.Fatalf("got pressure=%q prohibited=%v, want UNKNOWN/true when storageManager is nil", pressure, prohibited)
	}
}

func TestFISBCacheStoragePressureProhibited_AllBands(t *testing.T) {
	cases := []struct {
		p          storagelifecycle.PressureState
		prohibited bool
	}{
		{storagelifecycle.PressureNormal, false},
		{storagelifecycle.PressureElevated, false},
		{storagelifecycle.PressureHigh, true},
		{storagelifecycle.PressureCritical, true},
		{storagelifecycle.PressureUnknown, true},
	}
	for _, c := range cases {
		t.Run(string(c.p), func(t *testing.T) {
			withTestStorageManagerReportingPressure(t, c.p)
			_, prohibited := fisbCacheStoragePressureProhibited()
			if prohibited != c.prohibited {
				t.Errorf("pressure %s: got prohibited=%v, want %v", c.p, prohibited, c.prohibited)
			}
		})
	}
}

// --- admission actually gated by pressure (not just the reported label) -

func TestFISBCacheEnqueue_HighPressureRejectsAdmissionNotJustLabel(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureHigh)

	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, MaxCacheBytes: 1024, MaxEntries: 10}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheShuttingDown = false
	origRejected := fisbCachePressureRejected
	fisbCachePressureRejected = 0
	fisbCacheMu.Unlock()
	t.Cleanup(func() {
		fisbCacheMu.Lock()
		fisbCachePressureRejected = origRejected
		fisbCacheMu.Unlock()
	})

	fisbCaptureText("METAR", "KSEA", "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000", fisbcache.FISBTime{})

	if depth, _, _, _ := fisbCachePending.stats(); depth != 0 {
		t.Fatalf("expected nothing enqueued while storage pressure is HIGH, queue depth = %d", depth)
	}
	fisbCacheMu.Lock()
	rejected := fisbCachePressureRejected
	fisbCacheMu.Unlock()
	if rejected != 1 {
		t.Errorf("expected fisbCachePressureRejected to increment exactly once, got %d", rejected)
	}
}

func TestFISBCacheEnqueue_NormalPressureAdmitsToQueue(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)

	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, MaxCacheBytes: 1024, MaxEntries: 10}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheShuttingDown = false
	fisbCacheMu.Unlock()

	fisbCaptureText("METAR", "KSEA", "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000", fisbcache.FISBTime{})

	if depth, _, _, _ := fisbCachePending.stats(); depth != 1 {
		t.Fatalf("expected exactly one item enqueued at NORMAL pressure, got queue depth %d", depth)
	}
}

// --- namespace registered exactly once ----------------------------------

func TestFISBCacheNamespace_RegisteredExactlyOnce(t *testing.T) {
	registry := storagelifecycle.NewRegistry()
	if err := registry.Register(fisbCacheNamespace); err != nil {
		t.Fatalf("first registration must succeed: %v", err)
	}
	if err := registry.Register(fisbCacheNamespace); err == nil {
		t.Fatal("a second registration of the same namespace ID must be rejected, not silently accepted")
	}
}

// --- eviction: namespace confinement, deletes only requested keys ------

func makeFISBTestKey(id string) fisbcache.Key {
	return fisbcache.TextKey(fisbcache.TextProductMETAR, id)
}

func TestFISBCacheExecuteEviction_DeletesOnlyRequestedKeysWithinNamespace(t *testing.T) {
	dir := withTestFISBCacheStorage(t)

	keyA := makeFISBTestKey("KSEA")
	keyB := makeFISBTestKey("KPDX")
	keyC := makeFISBTestKey("KBOI")
	for _, k := range []fisbcache.Key{keyA, keyB, keyC} {
		entry := fisbcache.Entry{Key: k, ReceivedAtMonotonic: 1}
		if err := fisbCachePersist(entry, "METAR "+k.Identity+" 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
			t.Fatalf("could not persist entry for %s: %v", k.Identity, err)
		}
	}

	// An unrelated, unmanaged file in the same directory must never be
	// touched by eviction - it was never named in the plan.
	sentinel := filepath.Join(dir, "not-a-cache-entry.txt")
	if err := os.WriteFile(sentinel, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}

	deleted, errs := fisbCacheExecuteEviction([]fisbcache.Key{keyA, keyB})
	if len(errs) != 0 {
		t.Fatalf("expected no errors evicting existing entries, got %v", errs)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 files deleted, got %d", deleted)
	}

	if _, err := os.Stat(filepath.Join(dir, fisbCacheEntryFileName(keyA))); !os.IsNotExist(err) {
		t.Errorf("expected keyA's file removed, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fisbCacheEntryFileName(keyB))); !os.IsNotExist(err) {
		t.Errorf("expected keyB's file removed, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fisbCacheEntryFileName(keyC))); err != nil {
		t.Errorf("expected keyC's file (never named in the plan) to remain untouched, got %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("expected the unrelated sentinel file to remain untouched, got %v", err)
	}
}

func TestFISBCacheExecuteEviction_MissingFSReturnsErrorNeverPanics(t *testing.T) {
	origFS := fisbCacheFS
	fisbCacheFS = nil
	t.Cleanup(func() { fisbCacheFS = origFS })

	deleted, errs := fisbCacheExecuteEviction([]fisbcache.Key{makeFISBTestKey("KSEA")})
	if deleted != 0 {
		t.Errorf("expected 0 deletions with no filesystem initialized, got %d", deleted)
	}
	if len(errs) == 0 {
		t.Error("expected an error reported, not silent success")
	}
}

func TestFISBCacheExecuteEviction_MissingFileReportsErrorContinuesOthers(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	keyA := makeFISBTestKey("KSEA")
	keyMissing := makeFISBTestKey("DOES-NOT-EXIST")

	if err := fisbCachePersist(fisbcache.Entry{Key: keyA, ReceivedAtMonotonic: 1}, "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
		t.Fatal(err)
	}

	deleted, errs := fisbCacheExecuteEviction([]fisbcache.Key{keyMissing, keyA})
	if deleted != 1 {
		t.Errorf("expected exactly 1 real deletion (the existing entry), got %d", deleted)
	}
	if len(errs) != 1 {
		t.Errorf("expected exactly 1 error (the missing file), got %d: %v", len(errs), errs)
	}
	if _, err := os.Stat(filepath.Join(dir, fisbCacheEntryFileName(keyA))); !os.IsNotExist(err) {
		t.Errorf("expected the real entry still removed despite the other key's error, stat error = %v", err)
	}
}

// --- fisbCacheLifecycle: ReplaceProduct / IsExpired / ReclaimPriority --

func TestFISBCacheLifecycle_ReclaimPriorityIsCache(t *testing.T) {
	if (fisbCacheLifecycle{}).ReclaimPriority() != storagelifecycle.CriticalityCache {
		t.Fatal("fisbCacheLifecycle must always report CriticalityCache - it is the only evictable criticality this feature may claim")
	}
}

func TestFISBCacheLifecycle_ReplaceProduct_NilWriterReturnsError(t *testing.T) {
	withTestFISBCacheStorage(t)
	origWriter := fisbCacheAtomicWriter
	fisbCacheAtomicWriter = nil
	t.Cleanup(func() { fisbCacheAtomicWriter = origWriter })

	err := fisbCachePersist(fisbcache.Entry{Key: makeFISBTestKey("KSEA"), ReceivedAtMonotonic: 1}, "irrelevant")
	if err == nil {
		t.Fatal("expected an error persisting with no atomic writer initialized, got nil")
	}
}

// TestFISBCacheLifecycle_ReplaceProduct_WritesAndRoundTrips proves
// ReplaceProduct (via fisbCachePersist, its one production caller)
// actually writes a file this package's own DecodePersistedEntry can
// read back byte-for-byte, exercising the real EncodePersistedEntry ->
// AtomicWriter.Write -> fisbCacheValidateEntryFile -> rename path end to
// end, not just the pure fisbcache package's own encode/decode tests in
// isolation.
func TestFISBCacheLifecycle_ReplaceProduct_WritesAndRoundTrips(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	key := makeFISBTestKey("KSEA")
	payload := "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"
	entry := fisbcache.Entry{Key: key, ReceivedAtMonotonic: 42, SizeBytes: int64(len(payload))}

	if err := fisbCachePersist(entry, payload); err != nil {
		t.Fatalf("fisbCachePersist: %v", err)
	}

	path := filepath.Join(dir, fisbCacheEntryFileName(key))
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("expected a file at %s, got %v", path, statErr)
	}
	raw, readErr := fisbReadFileBounded(path)
	if readErr != nil {
		t.Fatalf("could not read back the persisted entry: %v", readErr)
	}
	decoded, decPayload, decErr := fisbcache.DecodePersistedEntry(raw, time.Time{})
	if decErr != nil {
		t.Fatalf("persisted entry failed to decode: %v", decErr)
	}
	if decPayload != payload {
		t.Errorf("payload mismatch: got %q, want %q", decPayload, payload)
	}
	if decoded.Key != key {
		t.Errorf("key mismatch: got %+v, want %+v", decoded.Key, key)
	}
}
