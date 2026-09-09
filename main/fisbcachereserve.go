/*
fisbcachereserve.go: strict, synchronous, PRE-ENQUEUE capacity reservation
for the Rolling FIS-B Weather Cache - decided ENTIRELY in memory, with
ZERO filesystem I/O, so it can safely run synchronously on the live
UAT/978 decode goroutine.

This corrects the prior design's remaining gap: fisbCacheRunRetention
(fisbcacherun.go) already brought committed (persisted + in-memory Store)
state back under budget synchronously, immediately after each admission -
but "after admission" is still after the item was already queued and
already occupying a Store slot. Nothing accounted for what was sitting in
the queue, or being actively written by the worker, when deciding whether
a NEW capture could be admitted at all.

This file makes capacity a RESERVATION, decided synchronously at
fisbCacheEnqueue time, before an item ever occupies a queue slot:

  - fisbPendingQueue is a bounded, per-key-coalescing pending-admission
    structure (replaces the prior plain channel) - at most one pending
    item per product Key, so a second capture for a still-queued key
    always SUPERSEDES its reservation rather than adding a second,
    separate one.
  - fisbCacheReservationFits computes whether admitting one more
    candidate fits within the configured budget once every currently
    committed (persisted/Store) entry AND every currently reserved
    (queued or in-flight, not yet Admit-decided) entry is accounted for.
    This is a PURE, in-memory computation over two already-in-memory
    snapshots (fisbCacheStore.Snapshot(), an in-memory map copy under
    Store's own mutex - no disk access whatsoever) - never a disk
    operation, and never followed by one here.

Critically, THIS FILE NEVER EVICTS. An earlier design had
reserveAndEnqueue itself evict committed entries (real file deletions,
via fisbCacheEvictKeyIfUnchanged) synchronously, right here, to make an
offered capture fit - but reserveAndEnqueue runs on the live UAT/978
decode goroutine (fisbCacheEnqueue is called directly from
main/gen_gdl90.go's parseInput, the same call chain that updates live
stats and forwards to GDL90/the /weatherraw websocket), and file
deletion is genuine filesystem I/O this project's own established
"never block the live decoder" contract for this feature explicitly
forbids on that path. When a candidate does not fit, this file's own
reserveAndEnqueue now does exactly two things: reject the offer
(fisbReserveReasonCapacity) and signal the asynchronous cleanup worker
(fisbCacheRequestCleanup, main/fisbcacherun.go) to make room - a
non-blocking, coalescing send, never a wait. A later retransmission of
the same product is admitted normally once cleanup has actually freed
the room. See docs/fisb-weather-cache.md's "Synchronous admission
bounds" section for the full model, the exact invariants this
establishes and proves, and TestFISBCacheEnqueue_NeverPerformsFilesystemIO
(main/fisbcachecapturepath_test.go) for the regression proof.
*/
package main

import (
	"sync"

	"github.com/stratux/stratux/fisbcache"
)

// fisbCachePendingCapacity bounds fisbPendingQueue's own STRUCTURAL size
// - the maximum number of DISTINCT product keys that may have an
// outstanding reservation (queued or in-flight) at once, independent of
// the configured byte/entry budget. This is the same value
// fisbCaptureQueueDepth used before this file existed (a plain channel's
// own capacity) - kept identical for continuity: generous for this
// project's own observed 978 uplink rate (which realistically touches
// far fewer than this many DISTINCT products within one worker-drain
// cycle), small enough that even a completely stalled worker can never
// let this feature's own application-retained queue payload bytes grow
// past a small, fixed bound (see "Queue payload bytes,"
// docs/fisb-weather-cache.md).
const fisbCachePendingCapacity = 256

// fisbReserveReason names why reserveAndEnqueue refused a reservation -
// distinct counters exist for each so an operator can tell "the queue's
// own distinct-key slot count is exhausted" (fisbCacheDroppedWrites,
// structurally rare - it requires this many DISTINCT unresolved products
// at once) apart from "the configured byte/entry budget itself has no
// room left, even after evicting everything evictable"
// (fisbCacheCapacityRejected, the ordinary, expected way a small budget
// pushes back under sustained load).
type fisbReserveReason int

const (
	fisbReserveReasonNone fisbReserveReason = iota
	fisbReserveReasonStructuralFull
	fisbReserveReasonCapacity
)

// fisbPendingQueue is this feature's own bounded, key-coalescing
// pending-admission structure. Safe for concurrent use.
//
// Lifecycle of one key's reservation: reserveAndEnqueue adds/updates
// items[key] (QUEUED) -> pop moves it from items to inFlight (IN-FLIGHT,
// still reserved, now owned by the one capture-worker goroutine
// processing it) -> releaseInFlight removes it entirely (the key's
// reservation is over - either it is now part of committed Store state,
// via Store.Admit, or it was rejected by Admit and contributes nothing).
// A key is in AT MOST ONE of items/inFlight at any instant, never both -
// reservedBytesExcluding relies on this.
type fisbPendingQueue struct {
	mu       sync.Mutex
	items    map[fisbcache.Key]fisbCaptureItem // queued, not yet dequeued
	order    []fisbcache.Key                   // FIFO arrival order of `items` keys only - a supersede does not move a key to the back, so a burst of updates for one hot key can never starve everything behind it
	inFlight map[fisbcache.Key]int64           // key -> reserved bytes, dequeued but not yet Admit-decided
	wake     chan struct{}                     // buffered(1) level-triggered "there is pending work" signal
	maxKeys  int
}

func newFISBPendingQueue(maxKeys int) *fisbPendingQueue {
	return &fisbPendingQueue{
		items:    make(map[fisbcache.Key]fisbCaptureItem),
		inFlight: make(map[fisbcache.Key]int64),
		wake:     make(chan struct{}, 1),
		maxKeys:  maxKeys,
	}
}

// reservedBytesExcluding returns every OTHER currently reserved (queued
// or in-flight) key's own reserved byte size - never including `key`
// itself, since a caller reserving/updating `key` is about to supply its
// own, possibly different, candidate size separately. Caller must
// already hold q.mu.
func (q *fisbPendingQueue) reservedBytesExcluding(key fisbcache.Key) map[fisbcache.Key]int64 {
	out := make(map[fisbcache.Key]int64, len(q.items)+len(q.inFlight))
	for k, it := range q.items {
		if k == key {
			continue
		}
		out[k] = int64(len(it.payload))
	}
	for k, sz := range q.inFlight {
		if k == key {
			continue
		}
		out[k] = sz
	}
	return out
}

// reserveAndEnqueue attempts to reserve capacity for one candidate
// capture and, if successful, adds/updates its queued reservation - all
// as one atomic, purely in-memory operation relative to every other
// reserveAndEnqueue/pop/releaseInFlight/clear call. It NEVER performs any
// filesystem operation - see this file's own doc comment for exactly
// why. If the projected total (committed + every other reservation +
// this one) does not already fit, the offer is rejected outright
// (fisbReserveReasonCapacity) and the asynchronous cleanup worker is
// signaled (fisbCacheRequestCleanup) to make room for a FUTURE offer -
// this one is not retried, delayed, or queued to wait for room; a
// retransmitted copy of the same product is what actually gets admitted
// once cleanup has freed the room, exactly like this project's other
// drop-and-count-rather-than-block admission gates
// (fisbCacheDroppedWrites, fisbCachePressureRejected).
func (q *fisbPendingQueue) reserveAndEnqueue(key fisbcache.Key, item fisbCaptureItem, settings FISBCacheSettings) (accepted bool, reason fisbReserveReason) {
	q.mu.Lock()
	defer q.mu.Unlock()

	_, alreadyPending := q.items[key]
	_, alreadyInFlight := q.inFlight[key]
	if !alreadyPending && !alreadyInFlight && len(q.items)+len(q.inFlight) >= q.maxKeys {
		return false, fisbReserveReasonStructuralFull
	}

	// Both calls below are pure in-memory reads - fisbCacheStore.Snapshot()
	// copies Store's own already-in-memory map under its own mutex (no
	// disk access at all, ever - Store holds only metadata, never
	// payload content or file handles); reservedBytesExcluding reads
	// this type's own already-in-memory maps, which q.mu (held for this
	// entire function) already makes a consistent, race-free view.
	committed := fisbCacheStore.Snapshot()
	reserved := q.reservedBytesExcluding(key)
	candidateSize := int64(len(item.payload))

	if !fisbCacheReservationFits(committed, reserved, key, candidateSize, settings.MaxCacheBytes, settings.MaxEntries) {
		fisbCacheRequestCleanup()
		return false, fisbReserveReasonCapacity
	}

	if !alreadyPending && !alreadyInFlight {
		q.order = append(q.order, key)
	}
	q.items[key] = item
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true, fisbReserveReasonNone
}

// pop removes and returns the oldest still-queued reservation, moving it
// to in-flight (still reserved - see this type's own doc comment).
// Reports false once nothing remains queued.
func (q *fisbPendingQueue) pop() (fisbcache.Key, fisbCaptureItem, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.order) > 0 {
		k := q.order[0]
		q.order = q.order[1:]
		item, ok := q.items[k]
		if !ok {
			// Cannot happen given this type's own invariants (a key is
			// only ever appended to order when inserted into items, and
			// removed from both together) - defensive, not a real path.
			continue
		}
		delete(q.items, k)
		q.inFlight[k] = int64(len(item.payload))
		return k, item, true
	}
	return fisbcache.Key{}, fisbCaptureItem{}, false
}

// releaseInFlight ends k's reservation - called exactly once per popped
// item, immediately after Store.Admit decides its fate (accepted,
// superseded, or rejected), regardless of outcome. See this type's own
// doc comment for why this specific moment, not dequeue time and not
// after persistence, is the correct end of the "reserved" window.
func (q *fisbPendingQueue) releaseInFlight(k fisbcache.Key) {
	q.mu.Lock()
	delete(q.inFlight, k)
	q.mu.Unlock()
}

// clear discards every currently QUEUED (not yet dequeued) reservation -
// used by the confirmed-purge flow (main/fisbcacheapi.go), so a purge is
// never immediately, silently undone by whatever was still waiting in
// the queue at that moment. Deliberately leaves any currently IN-FLIGHT
// item alone (the one capture-worker goroutine may already be
// Admit-deciding it) - this feature's own established "never abandon
// in-flight work, never delay shutdown/administrative actions
// indefinitely" posture applies here too; see
// docs/fisb-weather-cache.md's "purge and retention concurrency"
// coverage for the narrow, documented, low-probability consequence (at
// most the one item the worker happened to be mid-flight on can reappear
// immediately after a purge).
func (q *fisbPendingQueue) clear() {
	q.mu.Lock()
	q.items = make(map[fisbcache.Key]fisbCaptureItem)
	q.order = nil
	q.mu.Unlock()
}

// stats returns a point-in-time summary for the status API - a copy,
// never a reference to live internal state.
func (q *fisbPendingQueue) stats() (queuedEntries, inFlightEntries int, queuedBytes, inFlightBytes int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, it := range q.items {
		queuedEntries++
		queuedBytes += int64(len(it.payload))
	}
	for _, sz := range q.inFlight {
		inFlightEntries++
		inFlightBytes += sz
	}
	return
}

// reservedSnapshot returns every currently reserved (queued or
// in-flight) key's own reserved byte size - a copy, never a reference to
// live internal state. Unlike reservedBytesExcluding, this never
// excludes anything - it is this type's complete reservation ledger at
// one instant, exactly what the status API's projectedBytes/
// projectedEntries fields are computed from (see
// fisbCacheProjectedTotals).
//
// On its own, calling this and fisbCacheStore.Snapshot() as two separate
// calls is only ever an OBSERVATIONAL best-effort combination, not a
// jointly-atomic one: fisbCacheStore has its own, independent mutex, so
// a capture-worker admission can complete between the two reads,
// momentarily attributing one key's contribution to neither, or - far
// more visibly - a reservation's own eviction can complete between them,
// making the resulting combination describe a mix of two different real
// instants that, individually, were each correct, but together were
// never a single true state. This is why fisbCacheEnqueue's own
// admission decisions never rely on two separate reads like this (see
// reserveAndEnqueue, which holds q.mu across both) - and why any strict
// proof of the projected-bounds invariant needs
// fisbCacheProjectedSnapshotAtomic instead of this method, unless a
// brief, self-correcting inconsistency in a purely observational read
// (e.g. the status API) is acceptable, which it is there - see that
// function's own doc comment.
func (q *fisbPendingQueue) reservedSnapshot() map[fisbcache.Key]int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reservedSnapshotLocked()
}

// reservedSnapshotLocked is reservedSnapshot's body, factored out so
// fisbCacheProjectedSnapshotAtomic can call it while ALREADY holding
// q.mu (see that function's own doc comment for why that matters).
// Caller must already hold q.mu.
func (q *fisbPendingQueue) reservedSnapshotLocked() map[fisbcache.Key]int64 {
	out := make(map[fisbcache.Key]int64, len(q.items)+len(q.inFlight))
	for k, it := range q.items {
		out[k] = int64(len(it.payload))
	}
	for k, sz := range q.inFlight {
		out[k] = sz
	}
	return out
}

// fisbCacheProjectedTotals merges committed (fisbCacheStore.Snapshot())
// with reserved (queued+in-flight, keyed - a reserved key's own value
// always wins over a stale committed one for the same key, since
// committing it will supersede whatever is currently persisted) and sums
// the result. This is the same "projected final state" computation
// fisbCacheReservationFits performs internally, with one additional
// candidate folded in, for a single reservation decision - exposed here
// on its own (no candidate) both for the status API to report directly
// as projectedBytes/projectedEntries and for fisbCacheReservationFits
// itself to build on (see that function's own doc comment).
func fisbCacheProjectedTotals(committed map[fisbcache.Key]fisbcache.Entry, reserved map[fisbcache.Key]int64) (bytes int64, entries int) {
	seen := make(map[fisbcache.Key]bool, len(committed)+len(reserved))
	for k, sz := range reserved {
		if seen[k] {
			continue
		}
		seen[k] = true
		bytes += sz
		entries++
	}
	for k, e := range committed {
		if seen[k] {
			continue
		}
		seen[k] = true
		bytes += e.SizeBytes
		entries++
	}
	return
}

// fisbCacheProjectedSnapshotAtomic returns committed+reserved combined
// into one JOINTLY consistent read, unlike calling
// fisbCacheStore.Snapshot() and fisbCachePending.reservedSnapshot()
// separately (see reservedSnapshot's own doc comment for exactly why
// those two, taken independently, are only ever a best-effort
// combination). Holding q.mu across the nested fisbCacheStore.Snapshot()
// call excludes every reserveAndEnqueue call - including its own
// eviction execution - for this whole read's duration, which is what
// closes the gap: nothing can move an entry from committed to evicted
// (or reserve a new one) while this read is in progress. A capture-
// worker Admit() can still complete concurrently (it never needs q.mu),
// but that only ever transitions a key from reserved to ALSO-committed
// without yet releasing its reservation (releaseInFlight itself needs
// q.mu, so it blocks until this read finishes) - a case
// fisbCacheProjectedTotals already de-duplicates correctly (reserved's
// value always wins for a key present in both). Use this - not two
// separate calls - anywhere the projected-bounds invariant must be
// PROVEN, not merely reported for a dashboard.
func fisbCacheProjectedSnapshotAtomic(q *fisbPendingQueue) (bytes int64, entries int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	committed := fisbCacheStore.Snapshot()
	reserved := q.reservedSnapshotLocked()
	return fisbCacheProjectedTotals(committed, reserved)
}

// --- projection ----------------------------------------------------------

// fisbCacheReservationFits reports whether candidateKey (size
// candidateSize) fits within maxBytes/maxEntries given the CURRENT,
// as-is committed/reserved state - a pure, in-memory computation, never
// eviction planning or execution. This is the ONLY capacity check
// reserveAndEnqueue ever performs - see this file's own doc comment for
// why eviction itself can never happen here (this runs on the live
// UAT/978 decode goroutine).
func fisbCacheReservationFits(committed map[fisbcache.Key]fisbcache.Entry, reserved map[fisbcache.Key]int64, candidateKey fisbcache.Key, candidateSize int64, maxBytes int64, maxEntries int) bool {
	bytes, entries := fisbCacheProjectedTotalsWithCandidate(committed, reserved, candidateKey, candidateSize)
	return (maxBytes <= 0 || bytes <= maxBytes) && (maxEntries <= 0 || entries <= maxEntries)
}

// fisbCacheProjectedTotalsWithCandidate is fisbCacheProjectedTotals plus
// one additional candidate key/size, forced to win over both a stale
// committed value and a stale reserved value for the same key.
func fisbCacheProjectedTotalsWithCandidate(committed map[fisbcache.Key]fisbcache.Entry, reserved map[fisbcache.Key]int64, candidateKey fisbcache.Key, candidateSize int64) (bytes int64, entries int) {
	seen := make(map[fisbcache.Key]bool, len(committed)+len(reserved)+1)
	add := func(k fisbcache.Key, sz int64) {
		if seen[k] {
			return
		}
		seen[k] = true
		bytes += sz
		entries++
	}
	add(candidateKey, candidateSize)
	for k, sz := range reserved {
		add(k, sz)
	}
	for k, e := range committed {
		add(k, e.SizeBytes)
	}
	return
}
