package fisbcache

import "time"

// FreshnessState is one cache entry's current age classification -
// distinct from CacheState (the cache subsystem's own overall
// operational state). Naming follows this project's established
// UPPER_SNAKE string-state convention (see readiness.ComponentState).
type FreshnessState string

const (
	// FreshnessLive is never set by this package on a cache-read entry -
	// it exists only so a caller (the dashboard, the API) has one shared
	// vocabulary for "this is the live value just received, not a cache
	// hit at all" versus every other state below, which always describes
	// something served FROM the cache. See docs/fisb-weather-cache.md's
	// explicit "live versus cached" labeling requirement.
	FreshnessLive FreshnessState = "LIVE"
	// FreshnessCachedFresh: within the product's own FreshLimit.
	FreshnessCachedFresh FreshnessState = "CACHED_FRESH"
	// FreshnessCachedAging: past FreshLimit but within StaleLimit.
	FreshnessCachedAging FreshnessState = "CACHED_AGING"
	// FreshnessStale: past StaleLimit but not yet expired - still
	// retained (visible, never presented as current) but a strong
	// candidate for eviction under pressure.
	FreshnessStale FreshnessState = "STALE"
	// FreshnessExpired: past the product's expiration rule - eligible
	// for removal from the cache-owned namespace only.
	FreshnessExpired FreshnessState = "EXPIRED"
	// FreshnessInvalid: the entry's own metadata failed validation
	// (corrupt checksum, future-dated source time, schema mismatch) -
	// never served, always a removal candidate.
	FreshnessInvalid FreshnessState = "INVALID"
	// FreshnessUnsupported: a product class this package does not
	// persist at all (see ClassifyProductID) - never actually stored,
	// this value exists only for API/dashboard symmetry when reporting
	// "why wasn't this admitted."
	FreshnessUnsupported FreshnessState = "UNSUPPORTED"
)

// Entry is one cached product's full runtime record - metadata only;
// the payload itself is handled separately (see main/fisbcachestorage.go
// and storagelifecycle.CacheLifecycle.ReplaceProduct's WriteFunc), so an
// Entry is always cheap to hold in memory even with many cached
// products.
type Entry struct {
	Key Key

	// ReceivedAtMonotonic is when THIS process most recently received
	// (or re-received, superseding a prior copy) this product -
	// monotonic-only, per storagelifecycle.CacheEntryMetadata's own
	// established convention, so a GNSS/NTP wall-clock correction can
	// never make an unexpired entry look expired or vice versa.
	ReceivedAtMonotonic float64
	// Source is this product's best-effort reconstructed issue/broadcast
	// time - see ReconstructSourceTime. Never rewritten to "now" on a
	// mere re-receipt of an identical or refreshed product unless the
	// newly received copy's own reconstructed source time is itself
	// later (see Store.Admit) - this package never fabricates recency.
	Source SourceTime
	// ReceivedAtUTC is this entry's receive time in the wall-clock
	// domain, recorded ONLY when the receive-time wall clock was itself
	// trusted at the moment of receipt - display/persistence use only,
	// exactly like storagelifecycle.CacheEntryMetadata.GeneratedAtWallClock.
	// The zero value means "not recorded" (clock was not trusted then).
	ReceivedAtUTC time.Time

	SizeBytes int64
}

// Age reports how long ago (monotonic) this entry was last received,
// given the current monotonic time.
func (e Entry) Age(nowMonotonic float64) time.Duration {
	if nowMonotonic < e.ReceivedAtMonotonic {
		return 0
	}
	return time.Duration((nowMonotonic - e.ReceivedAtMonotonic) * float64(time.Second))
}

// Freshness classifies e given policy and the current monotonic time -
// pure, deterministic, no I/O. A caller with no policy for e.Key.Class
// (should never happen for an entry this package itself admitted, since
// Store.Admit already refuses anything ClassifyProductID doesn't
// recognize) gets FreshnessUnsupported rather than a panic.
func Freshness(e Entry, policy ProductPolicy, nowMonotonic float64) FreshnessState {
	if !policy.known {
		return FreshnessUnsupported
	}
	age := e.Age(nowMonotonic)
	switch {
	case age <= policy.FreshLimit:
		return FreshnessCachedFresh
	case age <= policy.StaleLimit:
		return FreshnessCachedAging
	case age <= policy.ExpireLimit:
		return FreshnessStale
	default:
		return FreshnessExpired
	}
}
