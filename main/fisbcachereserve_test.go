/*
fisbcachereserve_test.go: tests for the pre-enqueue capacity-reservation
machinery (fisbcachereserve.go) - the projection math in isolation,
fisbPendingQueue's own mechanics (coalescing, structural capacity,
in-flight lifecycle, purge-clear), and the six invariants
docs/fisb-weather-cache.md's "Synchronous admission bounds" section
requires:

	committed cache bytes <= configured maximum bytes
	committed cache entries <= configured maximum entries
	projected committed bytes, including accepted queued/in-flight work,
	    <= configured maximum bytes
	projected committed entries, including accepted queued/in-flight work,
	    <= configured maximum entries
	temporary atomic-write overhead <= one explicitly derived hard bound
	queue payload bytes (application-retained, not Go runtime/RSS memory)
	    <= one explicitly derived hard bound
*/
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/storagelifecycle"
)

// --- fisbCacheReservationFits: projection math in isolation - a PURE,
// in-memory check, never eviction planning or execution (see
// fisbcachereserve.go's own doc comment for why eviction was removed
// from this whole call path entirely) ------------------------------------

func TestFisbCacheReservationFits_FitsWithinBudget(t *testing.T) {
	committed := map[fisbcache.Key]fisbcache.Entry{
		makeFISBTestKey("KSEA"): {Key: makeFISBTestKey("KSEA"), SizeBytes: 100, ReceivedAtMonotonic: 10},
	}
	if !fisbCacheReservationFits(committed, nil, makeFISBTestKey("KPDX"), 50, 1000, 10) {
		t.Fatal("expected it to fit")
	}
}

func TestFisbCacheReservationFits_RejectsWhenOverByteBudget(t *testing.T) {
	old := fisbcache.Entry{Key: makeFISBTestKey("KOLD"), SizeBytes: 100, ReceivedAtMonotonic: 1}
	committed := map[fisbcache.Key]fisbcache.Entry{old.Key: old}
	// committed already uses 100 of a 150-byte budget; a new 100-byte
	// candidate would push the total to 200 > 150 - must be rejected
	// outright, never evict `old` to make room (this file never evicts).
	if fisbCacheReservationFits(committed, nil, makeFISBTestKey("KNEW"), 100, 150, 10) {
		t.Fatal("expected this to be rejected - over the byte budget, and this function must never evict to make room")
	}
	// `old` must be completely untouched - still exactly as it was.
	if got, ok := committed[old.Key]; !ok || got != old {
		t.Errorf("expected committed state left entirely untouched, got %+v (ok=%v)", got, ok)
	}
}

func TestFisbCacheReservationFits_RejectsWhenOverEntryBudget(t *testing.T) {
	committed := map[fisbcache.Key]fisbcache.Entry{
		makeFISBTestKey("KA"): {Key: makeFISBTestKey("KA"), SizeBytes: 1, ReceivedAtMonotonic: 1},
	}
	if fisbCacheReservationFits(committed, nil, makeFISBTestKey("KNEW"), 1, 1<<20, 1) {
		t.Fatal("expected this to be rejected - maxEntries=1 already satisfied by the one committed entry")
	}
}

func TestFisbCacheReservationFits_ReservedValueCountsTowardTheBudget(t *testing.T) {
	// A key already reserved (queued/in-flight) contributes to the
	// projected total exactly like a committed one - the fits check must
	// account for it even though it is not yet in Store.
	reserved := map[fisbcache.Key]int64{makeFISBTestKey("KOTHER"): 90}
	if fisbCacheReservationFits(nil, reserved, makeFISBTestKey("KNEW"), 20, 100, 10) {
		t.Fatal("expected this to be rejected - reserved (90) + candidate (20) = 110 > 100")
	}
	if !fisbCacheReservationFits(nil, reserved, makeFISBTestKey("KNEW"), 10, 100, 10) {
		t.Fatal("expected this to fit - reserved (90) + candidate (10) = 100 <= 100")
	}
}

func TestFisbCacheReservationFits_ZeroOrNegativeBudgetsMeanNoLimit(t *testing.T) {
	committed := map[fisbcache.Key]fisbcache.Entry{
		makeFISBTestKey("KA"): {Key: makeFISBTestKey("KA"), SizeBytes: 1 << 30, ReceivedAtMonotonic: 1},
	}
	if !fisbCacheReservationFits(committed, nil, makeFISBTestKey("KNEW"), 1<<30, 0, 0) {
		t.Fatal("expected maxBytes<=0 and maxEntries<=0 to both mean 'no limit', matching fisbcache.PlanEviction's own convention")
	}
}

// --- fisbCacheProjectedTotals ---------------------------------------------

func TestFisbCacheProjectedTotals_ReservedValueWinsOverStaleCommitted(t *testing.T) {
	k := makeFISBTestKey("KSEA")
	committed := map[fisbcache.Key]fisbcache.Entry{k: {Key: k, SizeBytes: 100}}
	reserved := map[fisbcache.Key]int64{k: 40} // a smaller, fresher, not-yet-committed replacement
	bytes, entries := fisbCacheProjectedTotals(committed, reserved)
	if entries != 1 {
		t.Errorf("expected exactly 1 entry (reserved supersedes committed for the same key), got %d", entries)
	}
	if bytes != 40 {
		t.Errorf("expected the RESERVED value (40) to win over the stale committed value (100), got %d", bytes)
	}
}

// --- fisbPendingQueue: coalescing, structural capacity, lifecycle --------

func newTestPendingItem(payload string) fisbCaptureItem {
	return fisbCaptureItem{entry: fisbcache.Entry{SizeBytes: int64(len(payload))}, payload: payload}
}

func TestFisbPendingQueue_SecondReservationForSameKeySupersedesNotAdds(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	q := newFISBPendingQueue(10)
	k := makeFISBTestKey("KSEA")
	settings := FISBCacheSettings{MaxCacheBytes: 1 << 20, MaxEntries: 100}

	ok1, _ := q.reserveAndEnqueue(k, newTestPendingItem("first"), settings)
	ok2, _ := q.reserveAndEnqueue(k, newTestPendingItem("second-and-longer"), settings)
	if !ok1 || !ok2 {
		t.Fatalf("expected both reservations to succeed, got ok1=%v ok2=%v", ok1, ok2)
	}

	depth, _, bytes, _ := q.stats()
	if depth != 1 {
		t.Errorf("expected exactly 1 queued key (the second supersedes the first, not adds), got depth=%d", depth)
	}
	if bytes != int64(len("second-and-longer")) {
		t.Errorf("expected the reserved size to reflect the LATEST offer, got %d", bytes)
	}
}

func TestFisbPendingQueue_StructuralCapacityRejectsAFullDistinctKeySet(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	q := newFISBPendingQueue(2)
	settings := FISBCacheSettings{MaxCacheBytes: 1 << 20, MaxEntries: 100}
	ok1, _ := q.reserveAndEnqueue(makeFISBTestKey("K1"), newTestPendingItem("a"), settings)
	ok2, _ := q.reserveAndEnqueue(makeFISBTestKey("K2"), newTestPendingItem("b"), settings)
	ok3, reason3 := q.reserveAndEnqueue(makeFISBTestKey("K3"), newTestPendingItem("c"), settings)
	if !ok1 || !ok2 {
		t.Fatalf("expected the first two distinct keys to be accepted, got ok1=%v ok2=%v", ok1, ok2)
	}
	if ok3 {
		t.Fatal("expected the third DISTINCT key to be rejected - structural capacity is 2")
	}
	if reason3 != fisbReserveReasonStructuralFull {
		t.Errorf("expected fisbReserveReasonStructuralFull, got %v", reason3)
	}

	// A second offer for an ALREADY-reserved key must still succeed even
	// while structurally full - it is a supersede, not a new slot.
	ok1b, _ := q.reserveAndEnqueue(makeFISBTestKey("K1"), newTestPendingItem("a-updated"), settings)
	if !ok1b {
		t.Error("expected a supersede of an already-reserved key to succeed even at structural capacity")
	}
}

func TestFisbPendingQueue_PopFollowsFIFOArrivalOrderAndMovesToInFlight(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	q := newFISBPendingQueue(10)
	settings := FISBCacheSettings{MaxCacheBytes: 1 << 20, MaxEntries: 100}
	keys := []fisbcache.Key{makeFISBTestKey("K1"), makeFISBTestKey("K2"), makeFISBTestKey("K3")}
	for _, k := range keys {
		if ok, _ := q.reserveAndEnqueue(k, newTestPendingItem("x"), settings); !ok {
			t.Fatalf("test setup failed reserving %s", k.Identity)
		}
	}

	for i, want := range keys {
		got, _, ok := q.pop()
		if !ok {
			t.Fatalf("pop %d: expected an item, got none", i)
		}
		if got != want {
			t.Errorf("pop %d: expected FIFO order %s, got %s", i, want.Identity, got.Identity)
		}
	}
	if _, _, ok := q.pop(); ok {
		t.Error("expected pop to report empty once everything has been dequeued")
	}

	_, inFlight, _, inFlightBytes := q.stats()
	if inFlight != 3 {
		t.Errorf("expected all 3 popped items to now be in-flight, got %d", inFlight)
	}
	if inFlightBytes != 3 {
		t.Errorf("expected 3 in-flight bytes (1 each), got %d", inFlightBytes)
	}
}

func TestFisbPendingQueue_ReleaseInFlightEndsTheReservation(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	q := newFISBPendingQueue(10)
	k := makeFISBTestKey("KSEA")
	settings := FISBCacheSettings{MaxCacheBytes: 1 << 20, MaxEntries: 100}
	q.reserveAndEnqueue(k, newTestPendingItem("x"), settings)
	q.pop()
	q.releaseInFlight(k)

	depth, inFlight, _, _ := q.stats()
	if depth != 0 || inFlight != 0 {
		t.Errorf("expected the reservation fully released, got depth=%d inFlight=%d", depth, inFlight)
	}
}

func TestFisbPendingQueue_ClearDiscardsQueuedButNotInFlight(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	q := newFISBPendingQueue(10)
	settings := FISBCacheSettings{MaxCacheBytes: 1 << 20, MaxEntries: 100}
	inFlightKey, queuedKey := makeFISBTestKey("KINFLIGHT"), makeFISBTestKey("KQUEUED")
	q.reserveAndEnqueue(inFlightKey, newTestPendingItem("a"), settings)
	q.pop() // moves inFlightKey to in-flight
	q.reserveAndEnqueue(queuedKey, newTestPendingItem("b"), settings)

	q.clear()

	depth, inFlight, _, _ := q.stats()
	if depth != 0 {
		t.Errorf("expected the still-queued reservation discarded by clear, got depth=%d", depth)
	}
	if inFlight != 1 {
		t.Errorf("expected the in-flight reservation left untouched by clear, got inFlight=%d", inFlight)
	}
}

// --- end-to-end: the real worker, through the real reservation path -----

// waitForFISBWorkerIdle waits until the real fisbCacheCaptureWorker
// goroutine has genuinely gone idle (blocked back on
// fisbCachePending.wake), not merely until fisbCachePending.stats()
// first reports empty - releaseInFlight (which is what makes stats()
// report empty) runs BEFORE that same iteration's own
// fisbCacheRunRetention/persist calls, so a bare stats()-emptiness check
// alone can still race with the worker's own tail-end work for the very
// last item. Every test in this package that starts a real
// fisbCacheCaptureWorker goroutine MUST call this before returning: this
// package's tests run sequentially, sharing package-level state
// (fisbCacheStore, fisbCachePending, ...) with no per-test isolation of
// their own, so a worker goroutine a test failed to let fully settle
// keeps right on reading/mutating whatever the NEXT test's setup just
// reassigned those globals to - a genuine, `go test -race`-visible
// cross-test race, not a production concern (production has exactly one
// fisbCacheCaptureWorker for the life of the process, never torn down
// and rebuilt the way a test's own setup/cleanup does).
//
// Declares idle only once fisbCachePending reports empty AND
// fisbCacheStore's own entry count has stopped changing across
// `stableRounds` consecutive polls - a fixed-point check, more robust
// than a single snapshot or a blind fixed sleep against exactly this
// kind of tail-end race.
func waitForFISBWorkerIdle(t *testing.T, timeout time.Duration) {
	t.Helper()
	const stableRounds = 5
	const pollInterval = 2 * time.Millisecond
	deadline := time.Now().Add(timeout)
	stable := 0
	lastLen := -1
	for {
		queued, inFlight, _, _ := fisbCachePending.stats()
		curLen := fisbCacheStore.Len()
		if queued == 0 && inFlight == 0 && curLen == lastLen {
			stable++
			if stable >= stableRounds {
				// fisbCachePending reporting empty only proves
				// releaseInFlight has run for the last item - that same
				// worker iteration's OWN fisbCacheRunRetention/persist
				// calls (which still touch fisbCacheStore) run
				// immediately afterward, on the same goroutine, and are
				// not otherwise observable from here. A short fixed
				// grace period, on top of the stable-empty check above,
				// is a pragmatic (not mathematically airtight) backstop
				// for exactly that tail window - retention+persist
				// against a local temp directory in these tests
				// completes in well under this margin in practice.
				time.Sleep(20 * time.Millisecond)
				return
			}
		} else {
			stable = 0
		}
		lastLen = curLen
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the capture worker to go idle (queued=%d inFlight=%d storeLen=%d)", queued, inFlight, curLen)
		}
		time.Sleep(pollInterval)
	}
}

// drainFISBPendingSynchronously drives fisbCachePending's own pop loop
// to completion on the CALLING (test) goroutine, via
// fisbCacheProcessOneCaptureItem - fisbCacheCaptureWorker's exact inner
// logic, without ever starting a real background worker goroutine.
// Deterministic and leak-free: prefer this over `go fisbCacheCaptureWorker()`
// in any test that does not specifically need to prove the wake/pop
// goroutine plumbing itself (see waitForFISBWorkerIdle's own doc comment
// for why a leaked worker goroutine is a real, `go test -race`-visible
// hazard across this package's sequentially-run tests).
func drainFISBPendingSynchronously(t *testing.T) {
	t.Helper()
	for {
		key, item, ok := fisbCachePending.pop()
		if !ok {
			return
		}
		fisbCacheProcessOneCaptureItem(key, item)
	}
}

// TestFISBCacheEnqueue_EndToEndThroughRealWorkerAdmitsAndPersists starts
// the actual fisbCacheCaptureWorker goroutine and drives captures through
// the real fisbCacheEnqueue -> reserveAndEnqueue -> pop -> Admit ->
// releaseInFlight -> persist pipeline, proving the pieces genuinely work
// together, not just in isolation.
func TestFISBCacheEnqueue_EndToEndThroughRealWorkerAdmitsAndPersists(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)

	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 20, MaxEntries: 100}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheShuttingDown = false
	fisbCacheMu.Unlock()

	go fisbCacheCaptureWorker()

	const n = 20
	for i := 0; i < n; i++ {
		fisbCaptureText("METAR", fmt.Sprintf("KTST%02d", i), "METAR body", fisbcache.FISBTime{})
	}

	waitForFISBWorkerIdle(t, 5*time.Second)

	if got := fisbCacheStore.Len(); got != n {
		t.Fatalf("expected the real worker to have admitted all %d captures, got Store.Len()=%d", n, got)
	}
	snap := fisbCacheStore.Snapshot()
	for k := range snap {
		path := dir + "/" + fisbCacheEntryFileName(k)
		waitForFile(t, path, 2*time.Second)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := fisbReadFileBounded(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to be persisted", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- failed/rejected work releases its reservation ------------------

// TestFISBCacheProcessOneCaptureItem_RejectedAdmitReleasesReservationAndPersistsNothing
// covers "failed, rejected, canceled, or interrupted work": a capture
// that reserved capacity but is then REJECTED by Store.Admit (an older
// copy of an already-cached product) must have its reservation released
// exactly like an accepted one, and must never be persisted.
func TestFISBCacheProcessOneCaptureItem_RejectedAdmitReleasesReservationAndPersistsNothing(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 20, MaxEntries: 100}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheMu.Unlock()

	key := makeFISBTestKey("KSEA")
	// A newer copy is already committed directly (bypassing reservation,
	// as recovery would) - Admit will reject anything older for this key.
	newer := fisbcache.Entry{Key: key, ReceivedAtMonotonic: 1000}
	if got := fisbCacheStore.Admit(newer); got != fisbcache.AdmitAccepted {
		t.Fatalf("test setup failed: got %q", got)
	}

	older := fisbcache.Entry{Key: key, ReceivedAtMonotonic: 1}
	item := fisbCaptureItem{entry: older, payload: "stale METAR"}
	accepted, _ := fisbCachePending.reserveAndEnqueue(key, item, fisbCacheSettingsCache)
	if !accepted {
		t.Fatal("expected the reservation itself to succeed - capacity, not freshness, is reserveAndEnqueue's own concern")
	}
	if depth, inFlight, _, _ := fisbCachePending.stats(); depth != 1 || inFlight != 0 {
		t.Fatalf("expected exactly one queued reservation before processing, got depth=%d inFlight=%d", depth, inFlight)
	}

	drainFISBPendingSynchronously(t)

	if depth, inFlight, _, _ := fisbCachePending.stats(); depth != 0 || inFlight != 0 {
		t.Errorf("expected the rejected item's reservation fully released, got depth=%d inFlight=%d", depth, inFlight)
	}
	got, _ := fisbCacheStore.Get(key)
	if got != newer {
		t.Errorf("expected the newer entry to remain untouched by the rejected older one, got %+v", got)
	}
}

// --- purge and retention concurrency ---------------------------------

// TestFISBCachePurgeUsingSnapshot_NeverDestroysAConcurrentReplacement
// mirrors TestFISBCacheEvictKeyIfUnchanged_RefusesStalePlanEvenAfterConcurrentReplacementIsPersisted
// at the purge-handler's own plan-then-execute granularity
// (fisbCachePurgeUsingSnapshot, the confirmed-purge handler's inner
// step): a purge executed against a Snapshot taken BEFORE a concurrent
// replacement must never destroy the fresher content that replaced it
// in the meantime - the stale snapshot is supplied directly here
// (mirroring how a real race would leave confirm's own already-taken
// Snapshot stale by the time its deletes run) rather than relying on
// goroutine timing to reproduce the race indirectly.
func TestFISBCachePurgeUsingSnapshot_NeverDestroysAConcurrentReplacement(t *testing.T) {
	withTestFISBCacheStorage(t)
	dir := fisbCacheDir
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	key := makeFISBTestKey("KSEA")
	stale := fisbcache.Entry{Key: key, ReceivedAtMonotonic: 100}
	fisbCacheStore.Admit(stale)
	if err := fisbCachePersist(stale, "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
		t.Fatal(err)
	}
	// The plan a real confirm request would have captured before the
	// race window.
	stalePlan := fisbCacheStore.Snapshot()

	// A concurrent live capture supersedes the SAME key with fresher
	// content AFTER that plan was captured, but before it executes.
	fresh := fisbcache.Entry{Key: key, ReceivedAtMonotonic: 200}
	if got := fisbCacheStore.Admit(fresh); got != fisbcache.AdmitSuperseded {
		t.Fatalf("test precondition failed: got %q", got)
	}
	if err := fisbCachePersist(fresh, "METAR KSEA 091953Z AUTO 00000KT 10SM CLR 16/10 A3001"); err != nil {
		t.Fatal(err)
	}

	deleted, errCount := fisbCachePurgeUsingSnapshot(stalePlan)
	if errCount != 0 {
		t.Errorf("expected no errors, got %d", errCount)
	}
	if deleted != 0 {
		t.Errorf("expected the stale plan's only key to be refused (already superseded), got deleted=%d", deleted)
	}

	// The fresh content must survive - both in the Store and on disk.
	got, ok := fisbCacheStore.Get(key)
	if !ok || got != fresh {
		t.Errorf("expected the concurrently-superseded fresh entry to survive the purge, got %+v (ok=%v)", got, ok)
	}
	raw, err := fisbReadFileBounded(dir + "/" + fisbCacheEntryFileName(key))
	if err != nil {
		t.Fatalf("expected the fresh entry's file to still exist, got %v", err)
	}
	_, payload, err := fisbcache.DecodePersistedEntry(raw, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	const freshPayload = "METAR KSEA 091953Z AUTO 00000KT 10SM CLR 16/10 A3001"
	if payload != freshPayload {
		t.Errorf("expected the surviving file's payload to be the FRESH write, got %q", payload)
	}
}

// TestHandleConfirmFISBCachePurge_ClearsQueuedReservations proves the
// confirmed-purge handler discards anything still queued (not yet
// dequeued), so a purge is never immediately, silently undone by
// whatever was waiting behind it.
func TestHandleConfirmFISBCachePurge_ClearsQueuedReservations(t *testing.T) {
	withFISBCacheTestEnv(t)
	withFISBCachePurgeStateReset(t)

	key := makeFISBTestKey("KQUEUED")
	fisbCachePending.reserveAndEnqueue(key, newTestPendingItem("still queued"), fisbCacheSettingsCache)
	if depth, _, _, _ := fisbCachePending.stats(); depth != 1 {
		t.Fatalf("test precondition failed: expected 1 queued reservation, got %d", depth)
	}

	prepReq := httptest.NewRequest(http.MethodPost, "/prepareFISBCachePurge", nil)
	prepRec := httptest.NewRecorder()
	handlePrepareFISBCachePurgeRequest(prepRec, prepReq)
	token, _ := decodeFISBJSONBody(t, prepRec)["token"].(string)

	confirmBody, _ := json.Marshal(map[string]string{"token": token})
	confirmReq := httptest.NewRequest(http.MethodPost, "/confirmFISBCachePurge", bytes.NewReader(confirmBody))
	confirmRec := httptest.NewRecorder()
	handleConfirmFISBCachePurgeRequest(confirmRec, confirmReq)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", confirmRec.Code, confirmRec.Body.String())
	}

	if depth, _, _, _ := fisbCachePending.stats(); depth != 0 {
		t.Errorf("expected the confirmed purge to clear the still-queued reservation, got depth=%d", depth)
	}
}

// --- settings-limit reductions ----------------------------------------

// TestFISBCacheProcessOneCaptureItem_SettingsTighteningDuringOutstandingReservationSelfCorrects
// proves the "grandfathered reservation" claim in
// docs/fisb-weather-cache.md's "Synchronous admission bounds" section:
// a reservation accepted under a looser budget, followed by a settings
// tightening BEFORE that reservation is processed, still results in a
// committed state that respects the NEW, tighter budget once cleanup
// actually runs - fisbCacheRunRetention re-validates against whatever
// settings are current when IT runs, not whatever was current when the
// reservation was made. Commit itself (fisbCacheProcessOneCaptureItem)
// no longer enforces this inline - it only REQUESTS a cleanup pass
// (fisbCacheRequestCleanup, asynchronous - see that function's own doc
// comment) - so this test drains the reservation to commit, then invokes
// fisbCacheRunRetention directly (standing in for the real
// fisbCacheCleanupWorker goroutine, which this test never starts - see
// drainFISBPendingSynchronously's own doc comment for why tests prefer
// the synchronous, worker-free path) to prove that WHEN cleanup runs, it
// correctly self-corrects.
func TestFISBCacheProcessOneCaptureItem_SettingsTighteningDuringOutstandingReservationSelfCorrects(t *testing.T) {
	withTestFISBCacheStorage(t)
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 20, MaxEntries: 100}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheMu.Unlock()

	// Reserved under the loose (maxEntries=100) budget above.
	key := makeFISBTestKey("KGRANDFATHERED")
	entry := fisbcache.Entry{Key: key, ReceivedAtMonotonic: monotonicSeconds()}
	item := fisbCaptureItem{entry: entry, payload: "METAR body"}
	if accepted, _ := fisbCachePending.reserveAndEnqueue(key, item, fisbCacheSettingsCache); !accepted {
		t.Fatal("expected the reservation to succeed under the loose starting budget")
	}

	// The budget tightens to 0 additional room (maxEntries=1, and a
	// DIFFERENT entry is already committed) before this reservation is
	// ever processed.
	other := fisbcache.Entry{Key: makeFISBTestKey("KOTHER"), ReceivedAtMonotonic: monotonicSeconds()}
	fisbCacheStore.Admit(other)
	fisbCacheMu.Lock()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 20, MaxEntries: 1}
	fisbCacheMu.Unlock()

	drainFISBPendingSynchronously(t)
	// Commit only REQUESTED cleanup above; run the pass directly here,
	// standing in for the real asynchronous worker.
	fisbCacheRunRetention()

	if got := fisbCacheStore.Len(); got > 1 {
		t.Errorf("expected the now-tighter maxEntries=1 budget enforced once cleanup runs, got Store.Len()=%d", got)
	}
}
