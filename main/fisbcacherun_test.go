/*
fisbcacherun_test.go: production-level integration tests for this
feature's runtime glue (fisbcacherun.go) - startup recovery's symlink/
non-regular-file skip, corrupt-entry quarantine, cross-reboot age
bridging, and its bounded wait for trusted time; the retention loop's
deterministic execution against real persisted files; and the capture
queue's bounded, drop-and-count overflow behavior. The pure fisbcache
package's own tests already cover PlanEviction/ReconstructSourceTime/
DecodePersistedEntry in isolation; this file proves the main/ glue
actually wires them together safely against a real (temp) filesystem.
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

// withTrustedTimeForTest drives the package-level timeTrust into
// TimeGNSSSynced with a fresh TimeTrust and three self-consistent GNSS
// samples (readiness.DefaultTimeTrustConfig's own RequiredConsecutive),
// restoring the original afterward - so fisbCacheStartupRecovery's own
// "wait for trusted time" loop exits on its very first check instead of
// actually blocking.
func withTrustedTimeForTest(t *testing.T) {
	t.Helper()
	orig := timeTrust
	timeTrust = readiness.NewTimeTrust(readiness.DefaultTimeTrustConfig())
	t.Cleanup(func() { timeTrust = orig })

	base := time.Now().UTC()
	for i := 0; i < 3; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		sample := readiness.GNSSTimeSample{
			HardwarePresent: true, ChecksumValid: true, StatusValid: true,
			Parseable: true, AcceptableFix: true,
			UTC: now, ReceivedAt: now,
		}
		timeTrust.ObserveGNSS(sample, now, now, false)
	}
	if timeTrust.State() != readiness.TimeGNSSSynced {
		t.Fatalf("test setup failed: expected TimeGNSSSynced after 3 consistent samples, got %s", timeTrust.State())
	}
}

// --- startup recovery: symlink / non-regular-file skip ------------------

func TestFISBCacheStartupRecovery_SkipsSymlinksAndNonRegularFiles(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	withTrustedTimeForTest(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	realKey := makeFISBTestKey("KSEA")
	receivedUTC := time.Now().UTC().Add(-time.Minute)
	realEntry := fisbcache.Entry{Key: realKey, ReceivedAtMonotonic: monotonicSeconds(), ReceivedAtUTC: receivedUTC}
	if err := fisbCachePersist(realEntry, "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
		t.Fatalf("could not persist the real entry: %v", err)
	}

	// A symlink sitting in the cache-owned directory - recovery must
	// never open or remove it.
	symlinkTarget := filepath.Join(t.TempDir(), "outside-the-cache.txt")
	if err := os.WriteFile(symlinkTarget, []byte("must never be touched"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(dir, "a-symlink.json")
	if err := os.Symlink(symlinkTarget, symlinkPath); err != nil {
		t.Skipf("symlinks not supported in this test environment: %v", err)
	}

	// A subdirectory (non-regular) sitting in the same directory.
	subdir := filepath.Join(dir, "a-subdirectory")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	fisbCacheStartupRecovery()

	if _, err := os.Lstat(symlinkPath); err != nil {
		t.Errorf("expected the symlink itself to remain untouched, got %v", err)
	}
	if _, err := os.Lstat(symlinkTarget); err != nil {
		t.Errorf("expected the symlink's target to remain completely untouched, got %v", err)
	}
	if _, err := os.Stat(subdir); err != nil {
		t.Errorf("expected the subdirectory to remain untouched, got %v", err)
	}
	if _, ok := fisbCacheStore.Get(realKey); !ok {
		t.Error("expected the one real entry to have been recovered despite the symlink/subdirectory sitting alongside it")
	}

	fisbCacheMu.Lock()
	recovered, recoveryErr := fisbCacheStartupRecovered, fisbCacheRecoveryError
	fisbCacheMu.Unlock()
	if !recovered {
		t.Error("expected fisbCacheStartupRecovered to be set true")
	}
	if recoveryErr {
		t.Error("a symlink/subdirectory sitting alongside real entries is not a fatal recovery error")
	}
}

// --- startup recovery: corrupt-entry quarantine -------------------------

func TestFISBCacheStartupRecovery_QuarantinesCorruptEntryWithoutCrashing(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	withTrustedTimeForTest(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	corruptPath := filepath.Join(dir, fisbCacheEntryFileName(makeFISBTestKey("CORRUPT")))
	if err := os.WriteFile(corruptPath, []byte("{not valid json at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	fisbCacheStartupRecovery() // must not panic

	if _, err := os.Stat(corruptPath); !os.IsNotExist(err) {
		t.Errorf("expected the corrupt entry to be quarantined (removed), stat error = %v", err)
	}
	fisbCacheMu.Lock()
	nonFatal := fisbCacheNonFatalErrors
	recoveryErr := fisbCacheRecoveryError
	fisbCacheMu.Unlock()
	if !nonFatal {
		t.Error("expected HasNonFatalErrors/fisbCacheNonFatalErrors to be set true")
	}
	if recoveryErr {
		t.Error("one corrupt entry is not a fatal recovery error - the directory itself was still readable")
	}
	if fisbCacheStore.Len() != 0 {
		t.Errorf("expected nothing admitted from a corrupt-only directory, got %d entries", fisbCacheStore.Len())
	}
}

// --- startup recovery: cross-reboot age bridging ------------------------

func TestFISBCacheStartupRecovery_BridgesAgeAcrossReboot(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTrustedTimeForTest(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	const simulatedAge = 5 * time.Minute
	key := makeFISBTestKey("KSEA") // METAR: FreshLimit 15m, well within ExpireLimit 3h
	receivedUTC := time.Now().UTC().Add(-simulatedAge)
	entry := fisbcache.Entry{Key: key, ReceivedAtMonotonic: 0, ReceivedAtUTC: receivedUTC}
	if err := fisbCachePersist(entry, "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
		t.Fatal(err)
	}

	beforeMono := monotonicSeconds()
	fisbCacheStartupRecovery()

	recovered, ok := fisbCacheStore.Get(key)
	if !ok {
		t.Fatal("expected the entry to be re-admitted after recovery")
	}
	age := recovered.Age(monotonicSeconds())
	// The entry's age must reflect the real elapsed wall-clock time
	// (~5 minutes), not reset to ~0 just because this process restarted -
	// allow a few seconds of test-execution slack either side.
	if age < simulatedAge-10*time.Second || age > simulatedAge+10*time.Second {
		t.Errorf("expected bridged age near %s, got %s", simulatedAge, age)
	}
	// And it must never simply be left at whatever was persisted - a
	// monotonic value is never itself persisted at all (DecodePersistedEntry
	// always returns 0 for it, deliberately - see its own doc comment), so
	// this proves real bridging math ran: ReceivedAtMonotonic must equal
	// (this process's own monotonic reading at recovery time) minus the
	// bridged age, which for a genuinely-in-the-past receive time is a
	// value well below the process's own current monotonic reading - here,
	// since this test's clock was only just created, that is legitimately
	// negative (an arbitrary monotonic epoch has no "must be positive"
	// rule; only "must be internally consistent" does).
	wantApprox := beforeMono - simulatedAge.Seconds()
	if recovered.ReceivedAtMonotonic < wantApprox-10 || recovered.ReceivedAtMonotonic > wantApprox+10 {
		t.Errorf("expected ReceivedAtMonotonic near %v (this process's own clock minus the bridged age), got %v", wantApprox, recovered.ReceivedAtMonotonic)
	}
}

func TestFISBCacheStartupRecovery_AlreadyExpiredEntryRemovedNotReadmitted(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	withTrustedTimeForTest(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	// METAR's ExpireLimit is 3h (fisbcache/policy.go) - 6h old is well
	// past it by the time this bridges.
	key := makeFISBTestKey("KOLD")
	receivedUTC := time.Now().UTC().Add(-6 * time.Hour)
	entry := fisbcache.Entry{Key: key, ReceivedAtMonotonic: 0, ReceivedAtUTC: receivedUTC}
	path := filepath.Join(dir, fisbCacheEntryFileName(key))
	if err := fisbCachePersist(entry, "METAR KOLD 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
		t.Fatal(err)
	}

	fisbCacheStartupRecovery()

	if _, ok := fisbCacheStore.Get(key); ok {
		t.Error("an already-expired-by-bridged-age entry must never be re-admitted to the Store")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected the already-expired entry's file removed during recovery, stat error = %v", err)
	}
}

// --- startup recovery: bounded wait for trusted time ---------------------

func TestFISBCacheStartupRecovery_BoundedWaitForUntrustedTime(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	// A fresh, never-synced TimeTrust - fisbCacheTrustedTimeState() will
	// report false for the whole test.
	origTrust := timeTrust
	timeTrust = readiness.NewTimeTrust(readiness.DefaultTimeTrustConfig())
	t.Cleanup(func() { timeTrust = origTrust })

	origTimeout := fisbCacheStartupRecoveryTimeout
	fisbCacheStartupRecoveryTimeout = 200 * time.Millisecond
	t.Cleanup(func() { fisbCacheStartupRecoveryTimeout = origTimeout })

	done := make(chan struct{})
	start := time.Now()
	go func() {
		fisbCacheStartupRecovery()
		close(done)
	}()
	select {
	case <-done:
		elapsed := time.Since(start)
		if elapsed > 2*time.Second {
			t.Errorf("expected recovery to give up near the shortened %s bound, took %s", fisbCacheStartupRecoveryTimeout, elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fisbCacheStartupRecovery did not return within 5s of an untrusted clock - the wait is not actually bounded")
	}

	fisbCacheMu.Lock()
	recovered, recoveryErr := fisbCacheStartupRecovered, fisbCacheRecoveryError
	fisbCacheMu.Unlock()
	if !recovered {
		t.Error("expected fisbCacheStartupRecovered=true once the bound is hit (never true indefinitely blocked)")
	}
	if recoveryErr {
		t.Error("trusted time never arriving is documented as non-fatal (RecoveryError must stay false) - only cross-reboot recovery is skipped this boot")
	}
	if fisbCacheStore.Len() != 0 {
		t.Error("nothing should have been re-admitted when trusted time never arrived")
	}
}

// --- retention: deterministic execution against real files -------------

func TestFISBCacheRunRetention_DeletesExpiredFromDiskAndStore(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, MaxCacheBytes: 1 << 30, MaxEntries: 1000}
	fisbCacheMu.Unlock()

	now := monotonicSeconds()
	fresh := fisbcache.Entry{Key: makeFISBTestKey("KFRESH"), ReceivedAtMonotonic: now}
	// METAR ExpireLimit is 3h - 4h ago is expired.
	expired := fisbcache.Entry{Key: makeFISBTestKey("KOLD"), ReceivedAtMonotonic: now - 4*3600}
	for _, e := range []fisbcache.Entry{fresh, expired} {
		if err := fisbCachePersist(e, "METAR "+e.Key.Identity+" 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
			t.Fatal(err)
		}
		fisbCacheStore.Admit(e)
	}

	fisbCacheRunRetention()

	if _, ok := fisbCacheStore.Get(fresh.Key); !ok {
		t.Error("expected the fresh entry to remain in the Store")
	}
	if _, ok := fisbCacheStore.Get(expired.Key); ok {
		t.Error("expected the expired entry to be removed from the Store")
	}
	if _, err := os.Stat(filepath.Join(dir, fisbCacheEntryFileName(fresh.Key))); err != nil {
		t.Errorf("expected the fresh entry's file to remain, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fisbCacheEntryFileName(expired.Key))); !os.IsNotExist(err) {
		t.Errorf("expected the expired entry's file removed, stat error = %v", err)
	}

	fisbCacheMu.Lock()
	lastCount := fisbCacheLastCleanupCount
	fisbCacheMu.Unlock()
	if lastCount != 1 {
		t.Errorf("expected fisbCacheLastCleanupCount=1, got %d", lastCount)
	}
}

func TestFISBCacheRunRetention_NoOpWhenNothingToEvict(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, MaxCacheBytes: 1 << 30, MaxEntries: 1000}
	origCount := fisbCacheLastCleanupCount
	fisbCacheLastCleanupCount = -1 // sentinel: must remain unchanged if nothing is evicted
	fisbCacheMu.Unlock()
	t.Cleanup(func() {
		fisbCacheMu.Lock()
		fisbCacheLastCleanupCount = origCount
		fisbCacheMu.Unlock()
	})

	fresh := fisbcache.Entry{Key: makeFISBTestKey("KFRESH"), ReceivedAtMonotonic: monotonicSeconds()}
	fisbCacheStore.Admit(fresh)

	fisbCacheRunRetention()

	fisbCacheMu.Lock()
	lastCount := fisbCacheLastCleanupCount
	fisbCacheMu.Unlock()
	if lastCount != -1 {
		t.Errorf("expected no cleanup bookkeeping update when nothing was evicted, got lastCleanupCount=%d", lastCount)
	}
}

// --- capture queue: bounded, drop-and-count overflow ---------------------

func TestFISBCacheEnqueue_QueueOverflowDropsAndCountsNeverBlocks(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)

	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, MaxCacheBytes: 1024, MaxEntries: 10}
	fisbCacheQueue = make(chan fisbCaptureItem, 2) // deliberately tiny
	fisbCacheShuttingDown = false
	origDropped := fisbCacheDroppedWrites
	fisbCacheDroppedWrites = 0
	fisbCacheMu.Unlock()
	t.Cleanup(func() {
		fisbCacheMu.Lock()
		fisbCacheDroppedWrites = origDropped
		fisbCacheMu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		// Five distinct products against a 2-capacity queue with no
		// worker draining it - this must return promptly (never block
		// on a full channel) regardless of how many are offered.
		for i := 0; i < 5; i++ {
			fisbCaptureText("METAR", "KTEST"+string(rune('A'+i)), "irrelevant", fisbcache.FISBTime{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fisbCaptureText blocked on a full queue instead of dropping and counting")
	}

	if got := len(fisbCacheQueue); got != 2 {
		t.Errorf("expected the queue to be at its capacity (2), got %d", got)
	}
	fisbCacheMu.Lock()
	dropped := fisbCacheDroppedWrites
	fisbCacheMu.Unlock()
	if dropped != 3 {
		t.Errorf("expected 3 of 5 offers dropped-and-counted (2 fit, 3 don't), got %d", dropped)
	}
}
