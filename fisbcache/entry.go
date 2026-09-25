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

// AgeBasis says which clock an entry's effective age was measured from.
type AgeBasis string

const (
	// AgeBasisSource: the age is the product's own age - reception age plus
	// how long before reception the product's trusted source time lay.
	AgeBasisSource AgeBasis = "source"
	// AgeBasisReception: no trustworthy source time is available (or the
	// policy does not use one), so the age is the time since the last
	// reception. It says nothing about how old the weather itself is.
	AgeBasisReception AgeBasis = "reception"
)

// maxSourceLag defensively bounds how long before reception a source time may
// lie when it comes from a persisted file (the reconstruction itself already
// bounds it at maxPastSkew) - an age must never be unbounded.
const maxSourceLag = 7 * 24 * time.Hour

// ReceptionAge is how long ago (monotonic) this entry was last received.
func (e Entry) ReceptionAge(nowMonotonic float64) time.Duration { return e.Age(nowMonotonic) }

// SourceLag is how long BEFORE its last reception the product's own (trusted,
// reconstructed) source time lay: ReceivedAtUTC - Source.UTC. Both are wall
// times captured at the moment of reception while the wall clock was trusted,
// so the difference is immune to any later clock correction; the entry's age
// then advances on the monotonic clock only. ok is false when there is no
// trustworthy source time or no trusted reception wall time. A source time a
// little AFTER reception (clock-domain skew, at most maxFutureSkew) yields a
// zero lag - never a negative one that would make the product look younger.
func (e Entry) SourceLag() (lag time.Duration, ok bool) {
	if !e.Source.Trusted || e.Source.UTC.IsZero() || e.ReceivedAtUTC.IsZero() {
		return 0, false
	}
	lag = e.ReceivedAtUTC.Sub(e.Source.UTC)
	if lag < 0 {
		lag = 0
	}
	if lag > maxSourceLag {
		lag = maxSourceLag
	}
	return lag, true
}

// SourceAge is the product's own age now: reception age plus SourceLag. It
// grows with the monotonic clock and is NOT reset by a retransmission of the
// same product (the retransmission carries the same source time, so its lag
// grows by exactly the time since the earlier reception).
func (e Entry) SourceAge(nowMonotonic float64) (time.Duration, bool) {
	lag, ok := e.SourceLag()
	if !ok {
		return 0, false
	}
	return e.ReceptionAge(nowMonotonic) + lag, true
}

// EffectiveAge is the age used for every user-facing freshness decision:
// max(reception age, source age) when a trustworthy source time exists and the
// policy uses it - which, since source age = reception age + a non-negative
// lag, is the source age - and the reception age otherwise. A cached product
// therefore never looks fresher than its reception OR its own age permits.
func (e Entry) EffectiveAge(policy ProductPolicy, nowMonotonic float64) (time.Duration, AgeBasis) {
	reception := e.ReceptionAge(nowMonotonic)
	if policy.UsesSourceAge() {
		if src, ok := e.SourceAge(nowMonotonic); ok && src >= reception {
			return src, AgeBasisSource
		}
	}
	return reception, AgeBasisReception
}

// Freshness classifies e given policy and the current monotonic time - pure,
// deterministic, no I/O - from its EFFECTIVE age (see EffectiveAge), so a
// product whose own time is old is never labelled fresh merely because it
// was received a moment ago. A caller with no policy for e.Key.Class (should
// never happen for an entry this package itself admitted, since Store.Admit
// already refuses anything ClassifyProductID doesn't recognize) gets
// FreshnessUnsupported rather than a panic.
func Freshness(e Entry, policy ProductPolicy, nowMonotonic float64) FreshnessState {
	if !policy.known {
		return FreshnessUnsupported
	}
	age, _ := e.EffectiveAge(policy, nowMonotonic)
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
