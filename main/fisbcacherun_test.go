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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	fisbCachePending = newFISBPendingQueue(2) // deliberately tiny structural capacity
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
		// Five DISTINCT products against a structural capacity of 2 keys
		// with no worker draining it - this must return promptly (never
		// block) regardless of how many are offered. Distinct keys
		// matter here: this is testing the structural
		// distinct-key-slot cap, not the byte/entry budget (which easily
		// accommodates all 5 of these small payloads on its own).
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

	if depth, _, _, _ := fisbCachePending.stats(); depth != 2 {
		t.Errorf("expected the queue to be at its structural capacity (2), got %d", depth)
	}
	fisbCacheMu.Lock()
	dropped := fisbCacheDroppedWrites
	fisbCacheMu.Unlock()
	if dropped != 3 {
		t.Errorf("expected 3 of 5 offers dropped-and-counted (2 fit, 3 don't), got %d", dropped)
	}
}

// --- synchronous admission bounds ---------------------------------------
//
// These tests cover the strict, admission-time capacity model described
// in docs/fisb-weather-cache.md's "Synchronous admission bounds" section:
// a single entry that can never fit the configured byte budget is
// rejected before it ever occupies a queue slot or a Store entry; a
// retention/eviction decision planned against a stale Snapshot can never
// discard data a concurrent admission has since made current; and the
// configured budget is enforced synchronously - after every single
// admission, at the end of startup recovery, and immediately on a
// settings change - not merely eventually, on the next periodic tick.

func TestFISBCacheEnqueue_OversizedPayloadRejectedNeverQueuedOrAdmitted(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)

	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, MaxCacheBytes: 100, MaxEntries: 10}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheShuttingDown = false
	origRejected := fisbCacheOversizedRejected
	fisbCacheOversizedRejected = 0
	fisbCacheMu.Unlock()
	t.Cleanup(func() {
		fisbCacheMu.Lock()
		fisbCacheOversizedRejected = origRejected
		fisbCacheMu.Unlock()
	})

	oversized := make([]byte, 101) // one byte over the 100-byte budget
	for i := range oversized {
		oversized[i] = 'A'
	}
	fisbCaptureText("METAR", "KSEA", string(oversized), fisbcache.FISBTime{})

	if depth, _, _, _ := fisbCachePending.stats(); depth != 0 {
		t.Errorf("expected the oversized entry never queued, got queue depth %d", depth)
	}
	if fisbCacheStore.Len() != 0 {
		t.Error("expected the oversized entry never admitted into the Store")
	}
	fisbCacheMu.Lock()
	rejected := fisbCacheOversizedRejected
	fisbCacheMu.Unlock()
	if rejected != 1 {
		t.Errorf("expected fisbCacheOversizedRejected=1, got %d", rejected)
	}
}

func TestFISBCacheEnqueue_PayloadExactlyAtBudgetIsAccepted(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)

	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, MaxCacheBytes: 100, MaxEntries: 10}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheShuttingDown = false
	fisbCacheMu.Unlock()

	exact := make([]byte, 100) // exactly at the budget, not over it
	for i := range exact {
		exact[i] = 'A'
	}
	fisbCaptureText("METAR", "KSEA", string(exact), fisbcache.FISBTime{})

	if depth, _, _, _ := fisbCachePending.stats(); depth != 1 {
		t.Errorf("expected an exactly-at-budget entry to be queued normally, got queue depth %d", depth)
	}
}

func TestFISBCacheEvictKeyIfUnchanged_RefusesStalePlanEvenAfterConcurrentReplacementIsPersisted(t *testing.T) {
	withTestFISBCacheStorage(t)
	dir := fisbCacheDir
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	key := makeFISBTestKey("KSEA")
	stale := fisbcache.Entry{Key: key, ReceivedAtMonotonic: 100}
	if err := fisbCachePersist(stale, "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
		t.Fatal(err)
	}
	fisbCacheStore.Admit(stale)

	// Simulate exactly what a concurrent capture-worker iteration does
	// between a retention pass's Snapshot and its eventual eviction
	// execution: admit AND persist a genuinely fresher copy of the same
	// key.
	fresh := fisbcache.Entry{Key: key, ReceivedAtMonotonic: 200}
	if got := fisbCacheStore.Admit(fresh); got != fisbcache.AdmitSuperseded {
		t.Fatalf("test precondition failed: got %q, want superseded", got)
	}
	if err := fisbCachePersist(fresh, "METAR KSEA 091953Z AUTO 00000KT 10SM CLR 16/10 A3001"); err != nil {
		t.Fatal(err)
	}

	// A retention pass that snapshotted BEFORE the above (still holding
	// `stale` as its planned-for-eviction expectation) now tries to
	// execute that stale plan.
	deleted, err := fisbCacheEvictKeyIfUnchanged(key, stale)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted {
		t.Fatal("expected the stale plan to be refused - the key has since been superseded")
	}

	// Both the Store and the file on disk must still reflect the FRESH
	// entry - a stale plan must never discard live data.
	got, ok := fisbCacheStore.Get(key)
	if !ok || got != fresh {
		t.Fatalf("expected the fresh entry to remain in the Store untouched, got %+v (ok=%v)", got, ok)
	}
	raw, err := fisbReadFileBounded(dir + "/" + fisbCacheEntryFileName(key))
	if err != nil {
		t.Fatalf("expected the fresh entry's file to still exist, got %v", err)
	}
	_, payload, err := fisbcache.DecodePersistedEntry(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("expected the surviving file to still decode cleanly: %v", err)
	}
	const freshPayload = "METAR KSEA 091953Z AUTO 00000KT 10SM CLR 16/10 A3001"
	if payload != freshPayload {
		t.Errorf("expected the surviving file's payload to be the FRESH write %q, got %q (a stale plan wrongly won)", freshPayload, payload)
	}
}

func TestFISBCacheRunRetention_SynchronousEnforcementNeverExceedsEntryBudget(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 30, MaxEntries: 3}
	fisbCacheMu.Unlock()

	// Mirrors fisbCacheCaptureWorker's own exact sequence
	// (Admit -> fisbCacheRunRetention -> persist) for 10 distinct
	// products against a budget of 3 - proving the Store never exceeds
	// its configured entry budget at any point along the way, not just
	// "eventually" after all 10 have been offered.
	for i := 0; i < 10; i++ {
		e := fisbcache.Entry{
			Key:                 makeFISBTestKey("KTEST" + string(rune('A'+i))),
			ReceivedAtMonotonic: monotonicSeconds() + float64(i), // strictly increasing "now"
		}
		if got := fisbCacheStore.Admit(e); got != fisbcache.AdmitAccepted {
			t.Fatalf("iteration %d: expected AdmitAccepted, got %q", i, got)
		}
		fisbCacheRunRetention()
		if fisbCacheStore.Len() > 3 {
			t.Fatalf("iteration %d: Store exceeded its configured MaxEntries=3 budget (Len=%d) - synchronous enforcement must never let this happen even momentarily between admissions", i, fisbCacheStore.Len())
		}
		if err := fisbCachePersist(e, "METAR "+e.Key.Identity+" 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
			t.Fatal(err)
		}
	}

	if fisbCacheStore.Len() != 3 {
		t.Errorf("expected exactly 3 entries to survive (the 3 most recently admitted), got %d", fisbCacheStore.Len())
	}
	// The 3 most recent keys (KTESTG, KTESTH, KTESTI... - i=7,8,9) must be
	// the ones that survived; oldest-first eviction must have kept them.
	for _, i := range []int{7, 8, 9} {
		k := makeFISBTestKey("KTEST" + string(rune('A'+i)))
		if _, ok := fisbCacheStore.Get(k); !ok {
			t.Errorf("expected the most recently admitted key %s to have survived", k.Identity)
		}
	}
}

// TestFISBCache_ConcurrentAdmitPersistAndRetentionNeverDivergesStoreFromDisk
// mirrors production's actual concurrency shape, not an artificially
// harder one: admission itself is always single-threaded in production
// (one capture-worker goroutine, processing one item at a time - see
// fisbCacheCaptureWorker's own doc comment), but fisbCacheRunRetention
// can legitimately be invoked concurrently from other goroutines too
// (the periodic retention ticker, a settings change). This test
// reproduces exactly that: one goroutine plays the capture worker
// (Admit -> fisbCacheRunRetention -> persist, one item at a time, for
// many distinct keys against a tight budget) while a second goroutine
// concurrently hammers fisbCacheRunRetention on its own, for the whole
// duration - proving under `go test -race` both that fisbCacheDiskMu
// makes this genuinely race-free and that the final state is correct:
// once the single-threaded admission side has finished (with its own
// last synchronous enforcement pass already applied), the Store never
// exceeds its configured budget, and every entry it reports has a
// correspondingly correct file on disk.
func TestFISBCache_ConcurrentAdmitPersistAndRetentionNeverDivergesStoreFromDisk(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 30, MaxEntries: 4}
	fisbCacheMu.Unlock()

	const total = 40

	extraRetentionDone := make(chan struct{})
	var extraRetentionWG sync.WaitGroup
	extraRetentionWG.Add(1)
	go func() {
		defer extraRetentionWG.Done()
		for {
			select {
			case <-extraRetentionDone:
				return
			default:
				fisbCacheRunRetention()
			}
		}
	}()

	// The single-threaded "capture worker" side - exactly mirroring
	// fisbCacheCaptureWorker's own sequence, one item at a time.
	for i := 0; i < total; i++ {
		e := fisbcache.Entry{
			Key:                 makeFISBTestKey(fmt.Sprintf("KTEST%03d", i)),
			ReceivedAtMonotonic: monotonicSeconds() + float64(i),
		}
		result := fisbCacheStore.Admit(e)
		if result != fisbcache.AdmitAccepted && result != fisbcache.AdmitSuperseded {
			continue
		}
		fisbCacheRunRetention()
		if _, stillPresent := fisbCacheStore.Get(e.Key); !stillPresent {
			continue
		}
		if err := fisbCachePersist(e, "METAR "+e.Key.Identity+" 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
			t.Errorf("persist for %s: %v", e.Key.Identity, err)
		}
	}
	close(extraRetentionDone)
	extraRetentionWG.Wait()

	if fisbCacheStore.Len() > 4 {
		t.Errorf("expected the Store to never exceed MaxEntries=4 once the single-threaded admission side finished (its own last enforcement pass already applied), got %d", fisbCacheStore.Len())
	}
	snap := fisbCacheStore.Snapshot()
	for k := range snap {
		path := dir + "/" + fisbCacheEntryFileName(k)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("Store reports %s as cached but its file is missing: %v", k.Identity, err)
		}
	}
}

func TestFISBCacheStartupRecovery_EnforcesBudgetBeforeReportingComplete(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	withTrustedTimeForTest(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	// A tighter budget than what was persisted below - simulating an
	// owner who lowered MaxEntries since these files were written on an
	// earlier, more permissive boot.
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 30, MaxEntries: 2}
	fisbCacheMu.Unlock()

	// Persist 5 distinct, currently-fresh (unexpired) entries directly to
	// disk - more than the now-configured MaxEntries=2 - without going
	// through Store.Admit at all, exactly matching what recovery itself
	// will find via a directory scan.
	now := monotonicSeconds()
	for i := 0; i < 5; i++ {
		e := fisbcache.Entry{
			Key:                 makeFISBTestKey("KOLD" + string(rune('A'+i))),
			ReceivedAtMonotonic: now,
			ReceivedAtUTC:       time.Now().UTC(),
		}
		if err := fisbCachePersist(e, "METAR "+e.Key.Identity+" 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
			t.Fatal(err)
		}
	}

	fisbCacheStartupRecovery()

	if fisbCacheStore.Len() > 2 {
		t.Errorf("expected recovery to enforce the now-tighter MaxEntries=2 budget before reporting complete, got Store.Len()=%d", fisbCacheStore.Len())
	}
	// The Store and disk must agree - recovery's own enforcement pass
	// must have deleted the files for whatever it evicted, not merely
	// dropped them from the in-memory index.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 2 {
		t.Errorf("expected at most 2 files remaining on disk after recovery's own budget enforcement, got %d", len(entries))
	}
}

func TestHandleSetFISBCacheSettings_TighteningBudgetEvictsImmediatelyNotAfterATick(t *testing.T) {
	withFISBCacheTestEnv(t)

	// Admit and persist 5 entries under a generous starting budget.
	fisbCacheMu.Lock()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 30, MaxEntries: 100}
	fisbCacheMu.Unlock()
	for i := 0; i < 5; i++ {
		e := fisbcache.Entry{
			Key:                 makeFISBTestKey("KTIGHT" + string(rune('A'+i))),
			ReceivedAtMonotonic: monotonicSeconds() + float64(i),
		}
		fisbCacheStore.Admit(e)
		if err := fisbCachePersist(e, "METAR "+e.Key.Identity+" 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
			t.Fatal(err)
		}
	}
	if fisbCacheStore.Len() != 5 {
		t.Fatalf("test precondition failed: expected 5 entries admitted, got %d", fisbCacheStore.Len())
	}

	body := `{"schemaVersion":1,"enabled":true,"persistenceEnabled":true,"replayEnabled":false,"maxCacheBytes":268435456,"maxEntries":2}`
	req := httptest.NewRequest(http.MethodPost, "/setFISBCacheSettings", strings.NewReader(body))
	rr := httptest.NewRecorder()
	handleSetFISBCacheSettingsRequest(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if fisbCacheStore.Len() > 2 {
		t.Errorf("expected the tightened maxEntries=2 budget enforced synchronously by the settings handler itself, got Store.Len()=%d immediately after the request returned", fisbCacheStore.Len())
	}
}
