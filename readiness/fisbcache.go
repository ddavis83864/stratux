package readiness

// FISBCacheHealth reports the Rolling FIS-B Weather Cache's own state,
// WITHOUT importing the fisbcache package (readiness stays a leaf
// dependency - see AutoRecordHealth/StorageLifecycleHealth's identical
// avoidance pattern). main/'s glue translates a fisbcache.State into
// this shape.
type FISBCacheHealth struct {
	State  ComponentState
	Reason string

	Enabled bool
	// CacheState is one of fisbcache.State's own string values
	// ("DISABLED"/"STARTUP_GRACE"/"WAITING_FOR_TRUSTED_TIME"/"LIVE"/
	// "DEGRADED"/"READ_ONLY"/"PRESSURE_INHIBITED"/"ERROR"), kept as a
	// plain string for the same import-direction reason as the rest of
	// this type.
	CacheState   string
	TotalEntries int
}

// BuildFISBCacheHealth derives a FISBCacheHealth from already-gathered
// signals - performs no I/O, so it is exercised directly by tests with
// synthetic values.
//
// Policy - deliberately never allows an optional, disabled cache to
// degrade overall readiness (see docs/fisb-weather-cache.md's readiness
// section):
//   - NOT_INSTALLED: the feature is disabled - never a failure, and (per
//     Rollup's own documented exclusion of NOT_INSTALLED/UNKNOWN) never
//     degrades overall system readiness merely by being off.
//   - UNKNOWN: enabled but still in STARTUP_GRACE or
//     WAITING_FOR_TRUSTED_TIME - a normal, expected transient state, not
//     a fault.
//   - NOT_READY: the cache is in ERROR (its own startup recovery hit a
//     genuine, non-entry-level failure) - this never marks the 978
//     receiver itself failed, and never masks a real receiver/storage
//     failure, both of which are reported by entirely separate
//     ComponentStates this function has no influence over.
//   - DEGRADED: LIVE reception is absent but a usable fresh cache exists,
//     the cache is READ_ONLY, PRESSURE_INHIBITED, or reports its own
//     DEGRADED state (non-fatal recovery errors were found and
//     quarantined) - worth the operator's attention, never blocking.
//   - READY: LIVE with no other concern.
func BuildFISBCacheHealth(enabled bool, cacheState string, totalEntries int) FISBCacheHealth {
	h := FISBCacheHealth{Enabled: enabled, CacheState: cacheState, TotalEntries: totalEntries}
	switch {
	case !enabled:
		h.State = StateNotInstalled
		h.Reason = "FIS-B weather cache is disabled"
	case cacheState == "" || cacheState == "STARTUP_GRACE" || cacheState == "WAITING_FOR_TRUSTED_TIME":
		h.State = StateUnknown
		h.Reason = "FIS-B weather cache has not yet initialized"
	case cacheState == "ERROR":
		h.State = StateNotReady
		h.Reason = "FIS-B weather cache reported an internal error"
	case cacheState == "DEGRADED" || cacheState == "READ_ONLY" || cacheState == "PRESSURE_INHIBITED":
		h.State = StateDegraded
		h.Reason = "FIS-B weather cache is " + cacheState
	default:
		h.State = StateReady
		h.Reason = "FIS-B weather cache nominal"
	}
	return h
}
