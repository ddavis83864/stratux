package fisbcache

import "sync"

// AdmitResult is Store.Admit's outcome, so a caller (the runtime capture
// path, a test) can observe exactly what happened without re-deriving it
// from before/after Snapshot calls.
type AdmitResult string

const (
	// AdmitAccepted: a new entry was created (no prior entry for this Key).
	AdmitAccepted AdmitResult = "accepted"
	// AdmitSuperseded: an existing entry for this Key was replaced,
	// because the newly received copy's own reconstructed source time is
	// at least as new as the one it replaces (see Store.Admit) - this is
	// this cache's dedup mechanism: the same product rebroadcast
	// verbatim by a ground station (this project's own existing pipeline
	// has no deduplication at all today - see docs/fisb-weather-cache.md)
	// updates ReceivedAtMonotonic without creating a second entry.
	AdmitSuperseded AdmitResult = "superseded"
	// AdmitRejectedOlder: an existing entry already has a newer (or
	// equally new, but this process already holds it) reconstructed
	// source time than the one just offered - never regresses a cache
	// entry to older content.
	AdmitRejectedOlder AdmitResult = "rejected_older"
	// AdmitRejectedUnsupported: k.Class has no known ProductPolicy (see
	// PolicyFor) - this package never admits a product it cannot assign
	// an explicit, evidence-based freshness policy to.
	AdmitRejectedUnsupported AdmitResult = "rejected_unsupported"
)

// Store is this package's pure, in-memory index of currently-cached
// entries - metadata only, never the payload itself (see Entry's doc
// comment). Safe for concurrent use; the mutex here is Store's own,
// entirely independent of anything main/gen_gdl90.go already holds (see
// docs/fisb-weather-cache.md's lock-ordering section) - a caller must
// never call back into this package's own methods while already holding
// this Store's lock (none of Store's methods are reentrant), and this
// package never calls out to slow I/O while holding it.
type Store struct {
	mu      sync.Mutex
	entries map[Key]Entry
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{entries: make(map[Key]Entry)}
}

// Admit offers one newly received/decoded product to the store. It never
// performs I/O itself (persistence is the caller's separate concern,
// triggered only after AdmitAccepted/AdmitSuperseded - see
// main/fisbcacherun.go) and never blocks.
func (s *Store) Admit(candidate Entry) AdmitResult {
	policy := PolicyFor(candidate.Key)
	if !policy.known {
		return AdmitRejectedUnsupported
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.entries[candidate.Key]
	if !ok {
		s.entries[candidate.Key] = candidate
		return AdmitAccepted
	}

	if newerSource(candidate, existing) {
		s.entries[candidate.Key] = candidate
		return AdmitSuperseded
	}
	return AdmitRejectedOlder
}

// newerSource reports whether candidate should replace existing: a
// trusted reconstructed source time strictly newer than existing's own
// always wins; if neither has a trusted source time (both untrusted -
// e.g. received before this device's own clock was ever trusted), the
// more recently RECEIVED (monotonic) copy wins instead, since that is
// the only ordering information available - this still never lets an
// untrusted-source-time entry win over one with a trusted, newer source
// time, and never lets an older trusted source time win over a newer
// one regardless of receive order (a delayed/rebroadcast older product
// must never appear to supersede a genuinely newer one already cached).
func newerSource(candidate, existing Entry) bool {
	switch {
	case candidate.Source.Trusted && existing.Source.Trusted:
		if candidate.Source.UTC.After(existing.Source.UTC) {
			return true
		}
		if candidate.Source.UTC.Equal(existing.Source.UTC) {
			return candidate.ReceivedAtMonotonic > existing.ReceivedAtMonotonic
		}
		return false
	case candidate.Source.Trusted && !existing.Source.Trusted:
		return true
	case !candidate.Source.Trusted && existing.Source.Trusted:
		return false
	default: // neither trusted
		return candidate.ReceivedAtMonotonic > existing.ReceivedAtMonotonic
	}
}

// Get returns the current entry for k, if any.
func (s *Store) Get(k Key) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[k]
	return e, ok
}

// Delete removes k unconditionally - used only to reconcile the in-memory
// index with an executed eviction/purge (see main/fisbcacherun.go);
// Store itself never decides to evict anything on its own initiative.
func (s *Store) Delete(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, k)
}

// DeleteIfUnchanged removes k only if its current entry is still exactly
// expected (compared by value - Entry is a plain, comparable struct) -
// used by retention/budget-enforcement execution so a decision made
// against an earlier Snapshot can never discard an entry that was
// concurrently admitted/superseded after that snapshot was taken (see
// main/fisbcacherun.go's fisbCacheEvictKeyIfUnchanged for the full
// synchronization this closes). Reports whether it actually deleted
// anything.
func (s *Store) DeleteIfUnchanged(k Key, expected Entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.entries[k]
	if !ok || cur != expected {
		return false
	}
	delete(s.entries, k)
	return true
}

// Snapshot returns every current entry, keyed by Key - a copy, safe to
// range over without holding Store's lock.
func (s *Store) Snapshot() map[Key]Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Key]Entry, len(s.entries))
	for k, e := range s.entries {
		out[k] = e
	}
	return out
}

// Len reports the current entry count.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// ExpiredKeys returns every entry currently classified FreshnessExpired,
// given nowMonotonic - pure, deterministic, and does not itself remove
// anything (see Delete) - matching storagelifecycle's own established
// plan-then-execute separation.
func (s *Store) ExpiredKeys(nowMonotonic float64) []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Key
	for k, e := range s.entries {
		if Freshness(e, PolicyFor(k), nowMonotonic) == FreshnessExpired {
			out = append(out, k)
		}
	}
	return out
}

// Stats is a bounded, deterministic summary of the store's current
// contents - safe to expose via the HTTP API/dashboard/diagnostics
// directly (see docs/fisb-weather-cache.md).
type Stats struct {
	TotalEntries   int
	TotalBytes     int64
	ByFreshness    map[FreshnessState]int
	ByProductClass map[ProductClass]int
}

// ComputeStats summarizes snap - a pure function of a Snapshot, so it is
// exercised identically whether called from production or a test.
func ComputeStats(snap map[Key]Entry, nowMonotonic float64) Stats {
	st := Stats{
		ByFreshness:    make(map[FreshnessState]int),
		ByProductClass: make(map[ProductClass]int),
	}
	for k, e := range snap {
		st.TotalEntries++
		st.TotalBytes += e.SizeBytes
		st.ByFreshness[Freshness(e, PolicyFor(k), nowMonotonic)]++
		st.ByProductClass[k.Class]++
	}
	return st
}
