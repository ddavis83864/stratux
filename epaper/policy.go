package epaper

import "time"

// RefreshKind is what kind of physical refresh (if any) a Decide call
// determined is due.
type RefreshKind string

const (
	RefreshNone    RefreshKind = "NONE"
	RefreshPartial RefreshKind = "PARTIAL"
	RefreshFull    RefreshKind = "FULL"
)

// PolicyState is the minimal state Decide needs across calls - owned and
// persisted only in memory by epaper_main's own main loop (never written
// to disk, matching the "avoid writes to persistent storage during
// normal refreshes" requirement).
type PolicyState struct {
	LastContent           Content
	LastRefreshAt         time.Time
	PartialRefreshesSince int // since the last full refresh
	HasRefreshedOnce      bool
}

// Decide is the single, pure change-driven refresh decision function.
// Given the current policy state, the newly-sampled content, the
// configured intervals, and now, it returns what kind of refresh (if
// any) is due and the PolicyState to carry forward - it performs no I/O
// and touches no hardware, so every combination of timing/content is
// directly exercised by go test.
//
// Rules (see docs/waveshare-epaper-display.md's update-frequency and
// full-refresh-cadence policy for the rationale):
//  1. The very first call always does a full refresh (there is nothing
//     on the panel yet to partially update from).
//  2. Otherwise, refresh only if content changed AND at least
//     refreshInterval has elapsed since the last refresh - a change-
//     driven display should not refresh on every single tick, but it
//     also must not refresh mid-interval just because the clock ticked
//     with no actual change.
//  3. A refresh that is due is a FULL refresh if fullRefreshEvery
//     partial refreshes have already occurred since the last full one
//     (ghosting mitigation), otherwise PARTIAL.
func Decide(state PolicyState, next Content, refreshInterval time.Duration, fullRefreshEvery int, now time.Time) (RefreshKind, PolicyState) {
	if !state.HasRefreshedOnce {
		state.LastContent = next
		state.LastRefreshAt = now
		state.PartialRefreshesSince = 0
		state.HasRefreshedOnce = true
		return RefreshFull, state
	}

	changed := MaterialChange(state.LastContent, next)
	dueByInterval := now.Sub(state.LastRefreshAt) >= refreshInterval
	if !changed || !dueByInterval {
		return RefreshNone, state
	}

	state.LastContent = next
	state.LastRefreshAt = now

	if state.PartialRefreshesSince >= fullRefreshEvery {
		state.PartialRefreshesSince = 0
		return RefreshFull, state
	}
	state.PartialRefreshesSince++
	return RefreshPartial, state
}
