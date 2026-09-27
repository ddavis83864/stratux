package epaper

import "time"

// Dashboard refresh policy. A pure decision function (no I/O, explicit
// time), like Decide for the legacy text pages: given the dashboard's
// material state and the clock it says whether the panel needs a partial
// refresh, a full refresh, or nothing.
//
// Rules, in order:
//  1. The first frame after (re)initialisation is a full refresh.
//  2. A material change (Dashboard.MaterialKey differs) refreshes once at
//     least refreshInterval has passed since the last refresh. Changes that
//     arrive faster coalesce: the next allowed refresh shows the latest
//     state, never each intermediate one.
//  3. With no change at all, a proof-of-life partial refresh happens every
//     DashboardHeartbeat, so the footer time keeps proving the display
//     service is alive; without it a wedged service would leave a
//     plausible-looking, silently frozen screen.
//  4. Vendor guidance (Waveshare's 4.2in module manual): refresh no more
//     often than every 180 s in continuous use, and fully refresh after
//     several partial refreshes (its FAQ says after 5). The dashboard
//     therefore (a) never refreshes faster than DashboardMinInterval even
//     if the configured interval is shorter, (b) falls back to
//     DashboardStormSpacing between refreshes once DashboardStormCount
//     refreshes have happened inside DashboardStormWindow (a flapping
//     input must not wear the panel), and (c) caps partial refreshes at
//     DashboardMaxPartials between full ones.
//  5. A refresh is FULL (flashing, ghost-clearing) instead of partial if
//     fullRefreshEvery partial refreshes have accumulated, if the last full
//     refresh is older than DashboardFullMaxAge, or if the banner's
//     black/white inversion changed (a large-area polarity change ghosts
//     badly on a partial waveform).
const (
	// DashboardHeartbeat: the longest a healthy, unchanging dashboard goes
	// without a refresh (6 per hour, negligible wear).
	DashboardHeartbeat = 10 * time.Minute
	// DashboardFullMaxAge: upper bound on the time between full refreshes,
	// so a mostly-static screen still gets its ghost-clearing pass.
	DashboardFullMaxAge = 4 * time.Hour
	// DashboardMinInterval floors the configured refresh interval.
	DashboardMinInterval = 30 * time.Second
	// The storm guard: DashboardStormCount refreshes within
	// DashboardStormWindow -> spacing of DashboardStormSpacing (the
	// vendor's recommended 180 s) until the window drains.
	DashboardStormCount   = 6
	DashboardStormWindow  = 10 * time.Minute
	DashboardStormSpacing = 180 * time.Second
	// DashboardMaxPartials caps partial refreshes between full ones,
	// whatever EpaperFullRefreshEvery says (vendor FAQ: 5).
	DashboardMaxPartials = 5
)

// DashPolicyState is what DecideDashboard carries between calls (memory
// only; nothing is written to disk).
type DashPolicyState struct {
	LastKey       string
	LastInverted  bool
	LastRefreshAt time.Time
	LastFullAt    time.Time
	Partials      int // since the last full refresh
	Has           bool
	// NotBefore holds every decision (even the first frame) until this
	// instant; set after a failed panel update so a failing panel is
	// retried once per interval, not once per poll.
	NotBefore time.Time
	// Recent holds the times of the refreshes inside the storm window.
	Recent []time.Time
	// Storm says the storm guard is engaged.
	Storm bool
}

// DecideDashboard implements the rules above. inverted says whether the
// current frame has an inverted (white-on-black) banner.
func DecideDashboard(st DashPolicyState, key string, inverted bool, refreshInterval time.Duration, fullEvery int, now time.Time) (RefreshKind, DashPolicyState) {
	if now.Before(st.NotBefore) {
		return RefreshNone, st
	}
	if !st.Has {
		return RefreshFull, DashPolicyState{LastKey: key, LastInverted: inverted, LastRefreshAt: now, LastFullAt: now, Has: true, Recent: []time.Time{now}}
	}
	if refreshInterval < DashboardMinInterval {
		refreshInterval = DashboardMinInterval
	}
	if fullEvery > DashboardMaxPartials || fullEvery <= 0 {
		fullEvery = DashboardMaxPartials
	}
	// Drop refreshes that have left the storm window, then apply the guard.
	recent := st.Recent[:0:0]
	for _, at := range st.Recent {
		if now.Sub(at) < DashboardStormWindow {
			recent = append(recent, at)
		}
	}
	st.Recent = recent
	// The guard engages at DashboardStormCount refreshes in the window and
	// stays engaged (hysteresis) until the window has drained to two or
	// fewer, so sustained flapping settles at one refresh per 180 s instead
	// of bursting back to the short interval every time the window empties.
	if !st.Storm && len(recent) >= DashboardStormCount {
		st.Storm = true
	} else if st.Storm && len(recent) <= 2 {
		st.Storm = false
	}
	if st.Storm && refreshInterval < DashboardStormSpacing {
		refreshInterval = DashboardStormSpacing
	}
	changed := key != st.LastKey
	since := now.Sub(st.LastRefreshAt)
	switch {
	case changed && since >= refreshInterval:
	case !changed && since >= DashboardHeartbeat:
	default:
		return RefreshNone, st
	}

	full := st.Partials >= fullEvery || now.Sub(st.LastFullAt) >= DashboardFullMaxAge || inverted != st.LastInverted
	st.LastKey, st.LastInverted, st.LastRefreshAt, st.NotBefore = key, inverted, now, time.Time{}
	st.Recent = append(st.Recent, now)
	if full {
		st.Partials, st.LastFullAt = 0, now
		return RefreshFull, st
	}
	st.Partials++
	return RefreshPartial, st
}
