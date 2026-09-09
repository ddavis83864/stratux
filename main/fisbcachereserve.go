/*
fisbcachereserve.go: strict, synchronous, PRE-ENQUEUE capacity reservation
for the Rolling FIS-B Weather Cache.

This corrects the prior design's remaining gap: fisbCacheRunRetention
(fisbcacherun.go) already brought committed (persisted + in-memory Store)
state back under budget synchronously, immediately after each admission -
but "after admission" is still after the item was already queued and
already occupying a Store slot. Nothing accounted for what was sitting in
the queue, or being actively written by the worker, when deciding whether
a NEW capture could be admitted at all. A burst of many distinct products
could still queue up well past what the configured budget could ever
actually hold, all before a single one of them was ever evaluated against
that budget.

This file makes capacity a RESERVATION, decided synchronously at
fisbCacheEnqueue time, before an item ever occupies a queue slot:

  - fisbPendingQueue is a bounded, per-key-coalescing pending-admission
    structure (replaces the prior plain channel) - at most one pending
    item per product Key, so a second capture for a still-queued key
    always SUPERSEDES its reservation rather than adding a second,
    separate one.
  - fisbCachePlanReservation computes whether admitting one more
    candidate fits within the configured budget once every currently
    committed (persisted/Store) entry AND every currently reserved
    (queued or in-flight, not yet Admit-decided) entry is accounted for
  - and, if it does not fit as offered, which committed entries would
    need to be evicted, synchronously, right now, to make room for it.

See docs/fisb-weather-cache.md's "Synchronous admission bounds" section
for the full model and the exact invariants this establishes and proves.
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
// let this feature's own in-process memory grow past a small, fixed
// bound (see "queue memory," docs/fisb-weather-cache.md).
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
// as one atomic operation relative to every other reserveAndEnqueue/pop/
// releaseInFlight/clear call. If the projected total (committed + every
// other reservation + this one) does not already fit, it evicts
// COMMITTED entries (oldest-received/expired-first, via the same safe,
// concurrency-correct fisbCacheEvictKeyIfUnchanged primitive retention
// uses - see fisbcacherun.go) to make room, synchronously, before ever
// admitting the reservation - never after.
func (q *fisbPendingQueue) reserveAndEnqueue(key fisbcache.Key, item fisbCaptureItem, settings FISBCacheSettings) (accepted bool, reason fisbReserveReason) {
	q.mu.Lock()
	defer q.mu.Unlock()

	_, alreadyPending := q.items[key]
	_, alreadyInFlight := q.inFlight[key]
	if !alreadyPending && !alreadyInFlight && len(q.items)+len(q.inFlight) >= q.maxKeys {
		return false, fisbReserveReasonStructuralFull
	}

	committed := fisbCacheStore.Snapshot()
	reserved := q.reservedBytesExcluding(key)
	candidateSize := int64(len(item.payload))
	now := monotonicSeconds()

	fits, evictKeys := fisbCachePlanReservation(committed, reserved, key, candidateSize, settings.MaxCacheBytes, settings.MaxEntries, now)
	if !fits && len(evictKeys) == 0 {
		return false, fisbReserveReasonCapacity
	}
	if len(evictKeys) > 0 {
		for _, k := range evictKeys {
			// A safe no-op (not an error) if a concurrent change already
			// invalidated this exact plan for k - see
			// fisbCacheEvictKeyIfUnchanged's own doc comment.
			fisbCacheEvictKeyIfUnchanged(k, committed[k])
		}
		// Re-verify against the REAL resulting state - deliberately
		// fisbCacheReservationFits (a plain as-is check), never another
		// call to fisbCachePlanReservation here: a second planning call
		// would derive a FRESH plan from whatever is still in the
		// refreshed snapshot and report fits=true by simulating ITS
		// removal too, even if the eviction just attempted above
		// silently failed to actually remove anything (a real,
		// concurrency-driven case - see fisbCacheReservationFits' own
		// doc comment for the exact scenario this was found and fixed
		// against). Only a real, verified, already-reflected-in-Store
		// change is ever trusted to have freed room.
		committed = fisbCacheStore.Snapshot()
		if !fisbCacheReservationFits(committed, reserved, key, candidateSize, settings.MaxCacheBytes, settings.MaxEntries) {
			return false, fisbReserveReasonCapacity
		}
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
// fisbCachePlanReservation performs internally for one specific
// candidate reservation, exposed here on its own (no candidate) for the
// status API to report directly as projectedBytes/projectedEntries.
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
// candidateSize) already fits within maxBytes/maxEntries given the
// CURRENT, as-is committed/reserved state - no further eviction planning
// or simulation, unlike fisbCachePlanReservation. This is deliberately
// the ONLY function reserveAndEnqueue trusts to verify a reservation
// after it has actually, really executed an eviction: calling
// fisbCachePlanReservation again there would re-derive a fresh eviction
// plan from whatever committed entries are STILL present and report
// fits=true by simulating THEIR removal too - even if the eviction this
// function's caller just attempted silently failed (see
// fisbCacheEvictKeyIfUnchanged's own doc comment: a stale or otherwise-
// unactionable plan is a safe no-op, not an error, so nothing forces the
// named key to actually be gone). Trusting a fresh plan's own optimistic
// simulation there - instead of just checking what is REALLY in
// `committed` right now - would silently let a reservation through
// without ever actually having freed the room it needed. See
// docs/fisb-weather-cache.md's "Synchronous admission bounds" section
// for the concurrent scenario this was found and fixed against.
func fisbCacheReservationFits(committed map[fisbcache.Key]fisbcache.Entry, reserved map[fisbcache.Key]int64, candidateKey fisbcache.Key, candidateSize int64, maxBytes int64, maxEntries int) bool {
	bytes, entries := fisbCacheProjectedTotalsWithCandidate(committed, reserved, candidateKey, candidateSize)
	return (maxBytes <= 0 || bytes <= maxBytes) && (maxEntries <= 0 || entries <= maxEntries)
}

// fisbCacheProjectedTotalsWithCandidate is fisbCacheProjectedTotals plus
// one additional candidate key/size, forced to win over both a stale
// committed value and a stale reserved value for the same key (mirrors
// fisbCachePlanReservation's own internal `projected` closure - factored
// out so fisbCacheReservationFits can share the exact same merge logic
// without duplicating it).
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

// fisbCachePlanReservation computes whether admitting candidateKey (size
// candidateSize) fits within maxBytes/maxEntries once `committed`
// (fisbCacheStore.Snapshot()) and `reserved` (every OTHER currently
// queued/in-flight key's own reserved size - never candidateKey itself)
// are combined into one PROJECTED final state: for each distinct key
// across committed ∪ reserved ∪ {candidateKey}, its contributing size is
// `reserved`'s value if reserved (a reservation's own value always wins
// over a stale committed one for the same key, since committing it will
// supersede whatever is currently persisted), else candidateSize if it
// IS candidateKey, else its own committed size.
//
// If that already fits, returns (true, nil). If not, it evicts from a
// pool of committed entries that are neither candidateKey nor already
// separately reserved (evicting a reserved key's current, about-to-be-
// superseded committed file would be pointless and wasteful - `reserved`
// already accounts for its eventual value), oldest-received/expired
// first via fisbcache.PlanEviction, adjusting that function's own
// maxBytes/maxEntries parameters downward by whatever reserved+candidate
// have already claimed. Returns (true, evictKeys) if evicting exactly
// evictKeys would make it fit; (false, evictKeys) if evicting every
// eligible committed entry (already the full contents of evictKeys) still
// would not be enough.
func fisbCachePlanReservation(committed map[fisbcache.Key]fisbcache.Entry, reserved map[fisbcache.Key]int64, candidateKey fisbcache.Key, candidateSize int64, maxBytes int64, maxEntries int, now float64) (fits bool, evictKeys []fisbcache.Key) {
	projected := func(evicted map[fisbcache.Key]bool) (bytes int64, entries int) {
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
			if evicted[k] {
				continue
			}
			add(k, e.SizeBytes)
		}
		return
	}

	withinBudget := func(bytes int64, entries int) bool {
		return (maxBytes <= 0 || bytes <= maxBytes) && (maxEntries <= 0 || entries <= maxEntries)
	}

	if b, n := projected(nil); withinBudget(b, n) {
		return true, nil
	}

	evictable := make(map[fisbcache.Key]fisbcache.Entry, len(committed))
	for k, e := range committed {
		if k == candidateKey {
			continue
		}
		if _, isReserved := reserved[k]; isReserved {
			continue
		}
		evictable[k] = e
	}

	var reservedBytesSum int64
	for _, sz := range reserved {
		reservedBytesSum += sz
	}
	adjustedMaxBytes, evictAllBytes := fisbReservationAdjustedByteBudget(maxBytes, reservedBytesSum+candidateSize)
	adjustedMaxEntries, evictAllEntries := fisbReservationAdjustedEntryBudget(maxEntries, len(reserved)+1)

	var plan []fisbcache.Key
	if evictAllBytes || evictAllEntries {
		for k := range evictable {
			plan = append(plan, k)
		}
	} else {
		plan = fisbcache.PlanEviction(evictable, adjustedMaxBytes, adjustedMaxEntries, now)
	}

	evicted := make(map[fisbcache.Key]bool, len(plan))
	for _, k := range plan {
		evicted[k] = true
	}
	b, n := projected(evicted)
	return withinBudget(b, n), plan
}

// fisbReservationAdjustedByteBudget shrinks maxBytes by `claimed`
// (everything already reserved, plus the new candidate) to get the
// budget remaining for eligible committed entries. Mirrors
// fisbcache.PlanEviction's own "<=0 means no limit" convention for
// maxBytes<=0 (budget disabled outright - nothing to evict for the byte
// reason). Critically, does NOT reuse that same convention for an
// adjusted value that goes non-positive because claimed>=maxBytes: that
// means the byte budget is already fully consumed by reserved+candidate
// alone, i.e. EVERY eligible committed entry must be evicted - the
// opposite of "no limit" - so this reports evictAll=true instead of
// silently handing PlanEviction a non-positive value it would otherwise
// misinterpret as unlimited.
func fisbReservationAdjustedByteBudget(maxBytes, claimed int64) (adjusted int64, evictAll bool) {
	if maxBytes <= 0 {
		return 0, false
	}
	adjusted = maxBytes - claimed
	if adjusted <= 0 {
		return 0, true
	}
	return adjusted, false
}

// fisbReservationAdjustedEntryBudget is fisbReservationAdjustedByteBudget's
// exact entry-count counterpart.
func fisbReservationAdjustedEntryBudget(maxEntries, claimed int) (adjusted int, evictAll bool) {
	if maxEntries <= 0 {
		return 0, false
	}
	adjusted = maxEntries - claimed
	if adjusted <= 0 {
		return 0, true
	}
	return adjusted, false
}
