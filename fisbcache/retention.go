package fisbcache

import "sort"

// PlanEviction is this cache's own deterministic retention decision -
// see docs/fisb-weather-cache.md's "Storage Lifecycle integration"
// section for why this lives here rather than being re-derived from
// storagelifecycle.Plan: this package's own in-memory Store snapshot IS
// the authoritative index of what is persisted (one file per Entry),
// so retention needs no separate filesystem scan - it only needs the
// same "oldest/most-expired first, deterministic tie-breaking" spirit
// storagelifecycle.Plan already establishes elsewhere in this project.
//
// Ordering, deterministic given identical inputs:
//  1. Every entry already FreshnessExpired is always included, regardless
//     of maxBytes/maxEntries (an expired entry is never worth keeping).
//  2. If, after removing every expired entry, the remaining set still
//     exceeds maxBytes or maxEntries, additional non-expired entries are
//     selected oldest-received-first (ReceivedAtMonotonic ascending, tie-
//     broken by Key.Identity ascending for full determinism) until both
//     budgets are met.
//
// maxBytes<=0 or maxEntries<=0 disables that specific budget (treated as
// "no limit") - PlanEviction never invents a limit the caller did not
// configure.
func PlanEviction(snap map[Key]Entry, maxBytes int64, maxEntries int, nowMonotonic float64) []Key {
	type scored struct {
		key   Key
		entry Entry
	}
	all := make([]scored, 0, len(snap))
	for k, e := range snap {
		all = append(all, scored{key: k, entry: e})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].entry.ReceivedAtMonotonic != all[j].entry.ReceivedAtMonotonic {
			return all[i].entry.ReceivedAtMonotonic < all[j].entry.ReceivedAtMonotonic
		}
		return all[i].key.Identity < all[j].key.Identity
	})

	var evict []Key
	evicted := make(map[Key]bool)
	var remainingBytes int64
	remainingCount := 0
	for _, s := range all {
		if Freshness(s.entry, PolicyFor(s.key), nowMonotonic) == FreshnessExpired {
			evict = append(evict, s.key)
			evicted[s.key] = true
			continue
		}
		remainingBytes += s.entry.SizeBytes
		remainingCount++
	}

	overBudget := func() bool {
		if maxBytes > 0 && remainingBytes > maxBytes {
			return true
		}
		if maxEntries > 0 && remainingCount > maxEntries {
			return true
		}
		return false
	}
	if !overBudget() {
		return evict
	}
	for _, s := range all {
		if evicted[s.key] {
			continue
		}
		if !overBudget() {
			break
		}
		evict = append(evict, s.key)
		remainingBytes -= s.entry.SizeBytes
		remainingCount--
	}
	return evict
}
