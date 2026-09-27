package main

import (
	"context"
	"log"
	"time"

	"github.com/stratux/stratux/epaper"
)

// dashboardPollInterval is how often the status APIs are read. Refreshing
// the *panel* is decided separately (epaper.DecideDashboard) and happens
// only on a material change, a proof-of-life heartbeat, or a full-refresh
// cadence - never per poll.
const dashboardPollInterval = 5 * time.Second

// dashboardSelected reports whether the operating dashboard is what this
// configuration shows: the dashboard page on the 400x300 panel at a
// rotation the landscape layout supports. Anything else (the 3.7in panel,
// 90/270 rotation, another page) uses the legacy text pages, unchanged.
func dashboardSelected(cfg epaper.Config) bool {
	return cfg.Page == epaper.PageDashboard &&
		cfg.Panel == epaper.PanelWaveshare42V2 &&
		dashboardRotationSupported(cfg.Rotation)
}

// dashSnapshot is the read side of the dashboard runner: what
// refreshDashboard needs at a given instant. Tests supply a scripted one.
type dashSnapshot interface {
	Dashboard(now, wall time.Time) epaper.Dashboard
	dataAgeSeconds(now time.Time) float64
}

// refreshDashboard derives the current dashboard, decides whether the panel
// is due a refresh, and if so renders and writes one frame. Like
// refreshOnce, everything is wrapped in a recover() so a bug in the
// renderer can never crash the service mid-refresh.
func refreshDashboard(ctx context.Context, driver PanelDriver, dash dashSnapshot, cfg epaper.Config, policy *epaper.DashPolicyState, prev epaper.Health, now time.Time) (result epaper.Health) {
	result = prev
	defer func() {
		if r := recover(); r != nil {
			log.Printf("epaper: recovered panic in dashboard refresh: %v", r)
			result = errorHealth(prev, epaper.ErrorSPIWrite)
		}
	}()

	if now.Before(policy.NotBefore) {
		// A panel update just failed and the retry is held off: keep
		// reporting that failure rather than a healthy state.
		result.UpdatedAt = now
		return result
	}
	d := dash.Dashboard(now, now)
	severe := d.Overall == epaper.OverallFault || d.Overall == epaper.OverallNoData
	interval := time.Duration(cfg.RefreshIntervalSeconds) * time.Second
	if interval < epaper.DashboardMinInterval {
		interval = epaper.DashboardMinInterval
	}
	kind, next := epaper.DecideDashboard(*policy, d.MaterialKey(), severe, interval, cfg.FullRefreshEvery, now)

	h := epaper.Health{
		UpdatedAt:             now,
		State:                 epaper.StateRunning,
		ConfiguredPanel:       cfg.Panel,
		PanelDetected:         true,
		LastSuccessfulRefresh: prev.LastSuccessfulRefresh,
		BusyTimeoutCount:      prev.BusyTimeoutCount,
		FullRefreshCount:      prev.FullRefreshCount,
		PartialRefreshCount:   prev.PartialRefreshCount,
		DataAgeSeconds:        dash.dataAgeSeconds(now),
	}
	if d.Overall == epaper.OverallNoData {
		h.LastErrorCat = epaper.ErrorStatusSourceUp
	}
	if kind == epaper.RefreshNone {
		return h
	}

	bmp := RenderDashboard(d, cfg.Rotation)
	if err := driver.Update(ctx, bmp, kind == epaper.RefreshFull); err != nil {
		cat, busy := epaper.ErrorSPIWrite, prev.BusyTimeoutCount
		if err == errBusyTimeout {
			cat, busy = epaper.ErrorBusyTimeout, busy+1
		}
		h = errorHealth(prev, cat)
		h.BusyTimeoutCount = busy
		// Do not commit the decision (the frame is still owed), but wait a
		// full interval before trying again rather than hammering a
		// failing panel every poll.
		policy.NotBefore = now.Add(interval)
		return h
	}
	*policy = next // (also clears NotBefore)
	h.LastSuccessfulRefresh = now
	if kind == epaper.RefreshFull {
		h.FullRefreshCount = prev.FullRefreshCount + 1
	} else {
		h.PartialRefreshCount = prev.PartialRefreshCount + 1
	}
	return h
}
