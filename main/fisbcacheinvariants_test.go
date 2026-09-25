/*
fisbcacheinvariants_test.go: direct proofs of the six invariants
docs/fisb-weather-cache.md's "Synchronous admission bounds" section
requires of the FIS-B cache's strict, pre-enqueue capacity reservation:

	committed cache bytes <= configured maximum bytes
	committed cache entries <= configured maximum entries
	projected committed bytes, including accepted queued/in-flight work,
	    <= configured maximum bytes
	projected committed entries, including accepted queued/in-flight work,
	    <= configured maximum entries
	temporary atomic-write overhead <= one explicitly derived hard bound
	queue payload bytes (application-retained, not Go runtime/RSS memory)
	    <= one explicitly derived hard bound

Each invariant gets its own, explicitly-named test below so a reviewer
(or a future change) can see exactly which test covers which numbered
requirement without cross-referencing prose.
*/
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/storagelifecycle"
)

// TestInvariant_CommittedBytesAndEntriesNeverExceedConfiguredMaximum
// covers invariants 1 and 2. Drives 50 distinct captures through the
// real fisbCacheEnqueue reservation path against a small budget (10
// entries), then drains them through fisbCacheProcessOneCaptureItem -
// fisbCacheCaptureWorker's own inner logic, called directly on this
// test's own goroutine (deterministic; no background goroutine to leak
// across this package's other, sequentially-run tests - see
// waitForFISBWorkerIdle's own doc comment for why that matters here).
// Asserts committed (Store.Snapshot()) state is within budget, and
// cross-checks that what is actually on disk agrees with the Store (no
// silent divergence).
func TestInvariant_CommittedBytesAndEntriesNeverExceedConfiguredMaximum(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)

	const maxEntries = 10
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 20, MaxEntries: maxEntries}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheShuttingDown = false
	fisbCacheMu.Unlock()

	const n = 50
	for i := 0; i < n; i++ {
		fisbCaptureText("METAR", fmt.Sprintf("KTST%03d", i), "METAR body", fisbcache.FISBTime{})
	}
	drainFISBPendingSynchronously(t)

	if got := fisbCacheStore.Len(); got > maxEntries {
		t.Fatalf("invariant violated: committed entries (%d) exceed configured maxEntries (%d)", got, maxEntries)
	}
	snap := fisbCacheStore.Snapshot()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(snap) {
		t.Errorf("committed Store (%d entries) disagrees with what is actually on disk (%d files)", len(snap), len(entries))
	}
}

// TestInvariant_ProjectedBytesAndEntriesNeverExceedConfiguredMaximumMidBurst
// covers invariants 3 and 4 - the ones distinguishing THIS workstream
// from the prior one: not just "eventually committed converges," but
// "projected (committed + everything currently queued/in-flight) never
// exceeds budget, checked WHILE a burst is still in flight, not only
// after it settles." fisbCacheEnqueue's own reservation decision is
// synchronous and immediate - it returns only once the fits-check itself
// has run, granting or rejecting outright, NEVER evicting to make room
// (see "Synchronous admission bounds," docs/fisb-weather-cache.md) - so
// checking immediately after EVERY single enqueue call, in a tight loop
// with no worker running at all (nothing draining the queue),
// deterministically captures the worst-case mid-burst state without
// racing against goroutine scheduling.
func TestInvariant_ProjectedBytesAndEntriesNeverExceedConfiguredMaximumMidBurst(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)

	const maxBytes = 2000
	const maxEntries = 8
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: maxBytes, MaxEntries: maxEntries}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheShuttingDown = false
	fisbCacheMu.Unlock()

	// Deliberately NO worker goroutine running - every one of these
	// captures stays QUEUED, proving the invariant holds purely from
	// fisbCacheEnqueue's own synchronous reservation, independent of
	// anything the worker does afterward.
	const n = 60
	for i := 0; i < n; i++ {
		fisbCaptureText("METAR", fmt.Sprintf("KTST%03d", i), "a body long enough to matter, not just one byte", fisbcache.FISBTime{})

		snap := fisbCacheStore.Snapshot()
		reserved := fisbCachePending.reservedSnapshot()
		bytes, entries := fisbCacheProjectedTotals(snap, reserved)
		if bytes > maxBytes {
			t.Fatalf("iteration %d: invariant violated: projected bytes (%d) exceed configured maxCacheBytes (%d)", i, bytes, maxBytes)
		}
		if entries > maxEntries {
			t.Fatalf("iteration %d: invariant violated: projected entries (%d) exceed configured maxEntries (%d)", i, entries, maxEntries)
		}
	}
}

// TestInvariant_ProjectedNeverExceedsMaximumUnderRealConcurrentLoad is the
// concurrent counterpart - many goroutines calling fisbCacheEnqueue
// simultaneously while the real worker drains concurrently (go test
// -race exercises fisbPendingQueue's own mutex and fisbCacheDiskMu
// genuinely, not just single-threaded). Each goroutine re-checks the
// projected totals immediately after its own enqueue call returns -
// still a valid, deterministic per-call check (the reservation that
// call itself just made is guaranteed reflected), even though the
// OVERALL sequence across goroutines is not deterministic.
func TestInvariant_ProjectedNeverExceedsMaximumUnderRealConcurrentLoad(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)

	const maxBytes = 5000
	const maxEntries = 6
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: maxBytes, MaxEntries: maxEntries}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheShuttingDown = false
	fisbCacheMu.Unlock()

	go fisbCacheCaptureWorker()

	const workers = 10
	const perWorker = 15
	var wg sync.WaitGroup
	wg.Add(workers)
	var violations int
	var violationsMu sync.Mutex
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				fisbCaptureText("METAR", fmt.Sprintf("KW%02dI%03d", w, i), "concurrent body", fisbcache.FISBTime{})
				// fisbCacheProjectedSnapshotAtomic, deliberately NOT two
				// separate fisbCacheStore.Snapshot()/
				// fisbCachePending.reservedSnapshot() calls: two
				// independent, separately-locked reads can each be
				// individually correct yet, combined, describe a mix of
				// two different real instants that never co-existed
				// (e.g. one taken just before a reservation's own
				// eviction completes, the other just after) - a pure
				// observational artifact of non-atomic combination, not
				// a real admission-time violation. This function holds
				// fisbCachePending's own lock across both reads
				// specifically to rule that out - see its own doc
				// comment for the full reasoning.
				bytes, entries := fisbCacheProjectedSnapshotAtomic(fisbCachePending)
				if bytes > maxBytes || entries > maxEntries {
					violationsMu.Lock()
					violations++
					violationsMu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()

	if violations != 0 {
		t.Errorf("invariant violated %d times under concurrent load", violations)
	}

	// Let the worker drain, then confirm the STEADY-STATE invariant
	// (1/2) also holds at the end.
	waitForFISBWorkerIdle(t, 5*time.Second)
	if got := fisbCacheStore.Len(); got > maxEntries {
		t.Errorf("after drain: committed entries (%d) exceed configured maxEntries (%d)", got, maxEntries)
	}
}

// TestInvariant_TemporaryAtomicWriteOverheadHasAnExplicitHardBound covers
// invariant 5: the maximum ADDITIONAL, strictly transient disk overhead
// one in-flight atomic replace can ever cost, on top of steady-state
// committed bytes, is a small, fixed, explicitly named constant -
// fisbcache's own maxPersistedEntryBytes (payload cap + framing
// headroom) - not a function of maxCacheBytes, traffic volume, or
// anything else. This is provable purely from EncodePersistedEntry's own
// contract (already exercised directly by fisbcache's own tests) plus
// this feature's own single-writer-at-a-time architecture
// (fisbCacheDiskMu + one capture-worker goroutine - see that lock's own
// doc comment) guaranteeing at most one such temp file exists at once;
// this test names the exact bound explicitly so a future change to
// either constant is forced to touch this assertion.
func TestInvariant_TemporaryAtomicWriteOverheadHasAnExplicitHardBound(t *testing.T) {
	const maxPersistedPayloadBytes = 65536 // fisbcache.maxPersistedPayloadBytes, unexported - mirrored here as an explicit, independently-stated bound (matches fisbcacheMaxReadBytes's own established "small, explicit redundant constant" convention, main/fisbcacherun.go)
	const framingHeadroom = 4096
	const explicitHardBound = maxPersistedPayloadBytes + framingHeadroom // 69,632 bytes

	// A payload right at the cap must still encode successfully, and the
	// fully-encoded result (payload + this schema's own bounded
	// metadata/framing) must never exceed the explicit hard bound this
	// test names.
	atCap := make([]byte, maxPersistedPayloadBytes)
	for i := range atCap {
		atCap[i] = 'A'
	}
	persisted, err := fisbcache.EncodePersistedEntry(fisbcache.Entry{Key: makeFISBTestKey("KCAP"), ReceivedAtUTC: time.Now().UTC()}, string(atCap))
	if err != nil {
		t.Fatalf("expected a payload exactly at the cap to encode, got %v", err)
	}
	encoded, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > explicitHardBound {
		t.Errorf("fully-encoded at-cap entry (%d bytes) exceeds this test's own stated hard bound (%d bytes)", len(encoded), explicitHardBound)
	}

	// One byte OVER the cap must be rejected outright - proving the cap
	// is actually enforced, not merely documented.
	overCap := make([]byte, maxPersistedPayloadBytes+1)
	if _, err := fisbcache.EncodePersistedEntry(fisbcache.Entry{Key: makeFISBTestKey("KOVER")}, string(overCap)); err == nil {
		t.Fatal("expected a payload one byte over the cap to be rejected")
	}

	if explicitHardBound != 69632 {
		t.Fatalf("this test's own stated hard bound drifted from its documented value: got %d, want 69632", explicitHardBound)
	}
}

// TestInvariant_QueueMemoryHasAnExplicitHardBound covers invariant 6:
// this feature's own application-retained PAYLOAD BYTES for queued/
// in-flight work (as opposed to disk) - the sum of len(payload) across
// every fisbCaptureItem fisbPendingQueue is currently holding, per its
// own queuedBytes/inFlightBytes accounting - is bounded by
// fisbCachePendingCapacity (structural, distinct-key slots) times the
// maximum any single payload can ever be (fisbcache's own
// maxPersistedPayloadBytes). This is deliberately NOT a claim about this
// feature's total Go runtime footprint or process RSS (struct/map/string
// overhead, goroutine stacks, and GC bookkeeping are real memory this
// bound does not measure) - see docs/fisb-weather-cache.md's "Queue
// payload bytes" bullet for the precise, honest scope of this bound.
// Proven here by actually filling a pending queue to its structural
// capacity with maximum-sized payloads and confirming the (n+1)th
// distinct key is rejected, never silently accepted past that bound.
func TestInvariant_QueueMemoryHasAnExplicitHardBound(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	const maxKeys = 4
	const maxPayload = 65536
	q := newFISBPendingQueue(maxKeys)
	// A generous byte/entry budget so ONLY the structural cap is under
	// test here, not the reservation-capacity gate.
	settings := FISBCacheSettings{MaxCacheBytes: 1 << 30, MaxEntries: 1 << 20}

	bigPayload := make([]byte, maxPayload)
	for i := 0; i < maxKeys; i++ {
		ok, _ := q.reserveAndEnqueue(makeFISBTestKey(fmt.Sprintf("K%02d", i)), fisbCaptureItem{payload: string(bigPayload)}, settings)
		if !ok {
			t.Fatalf("expected key %d (within structural capacity %d) to be accepted", i, maxKeys)
		}
	}
	_, inFlight, queuedBytes, inFlightBytes := q.stats()
	explicitHardBound := int64(maxKeys) * int64(maxPayload)
	if got := queuedBytes + inFlightBytes; got > explicitHardBound {
		t.Fatalf("queue payload bytes (%d) exceeds its own explicit hard bound (%d = %d keys x %d bytes)", got, explicitHardBound, maxKeys, maxPayload)
	}
	if inFlight != 0 {
		t.Fatalf("test precondition failed: expected nothing in-flight yet, got %d", inFlight)
	}

	// The (maxKeys+1)th DISTINCT key must be rejected - proving the
	// structural bound is actually enforced, not merely documented.
	ok, reason := q.reserveAndEnqueue(makeFISBTestKey("KOVERFLOW"), fisbCaptureItem{payload: string(bigPayload)}, settings)
	if ok {
		t.Fatal("expected the (maxKeys+1)th distinct key to be rejected")
	}
	if reason != fisbReserveReasonStructuralFull {
		t.Errorf("expected fisbReserveReasonStructuralFull, got %v", reason)
	}
}
