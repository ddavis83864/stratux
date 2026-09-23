package readiness

import "time"

// EpaperHealth is the health record for the optional Waveshare e-paper
// display service - a separate process (epaper_main/), never linked into
// this daemon - see docs/waveshare-epaper-display.md. This display is
// explicitly out of scope for anything safety-related: it is a
// supplemental status surface only, never a moving map, artificial
// horizon, or safety-critical annunciator. Its own absence or failure
// must never itself read as, or be confused with, degraded ADS-B/GDL90/
// AHRS/GPS health - only this dedicated tile ever reports it, and (per
// BuildEpaperHealth's policy below) it is excluded from dragging down
// Overall whenever it is merely disabled or not installed, the same
// Rollup exclusion AutoRecordHealth relies on.
//
// main/'s glue translates epaper_main's own self-reported epaper.Health
// (read from common.EpaperStatusPath) plus the stratux_epaper systemd
// unit's load/active state into this shape - this package stays a leaf
// dependency and does not import epaper, the same avoidance pattern
// StorageLifecycleHealth and AutoRecordHealth already use.
type EpaperHealth struct {
	State  ComponentState
	Reason string

	Enabled bool

	// ServiceInstalled/ServiceActive/StatusAvailable/Malformed mirror
	// FanHealth's own fields exactly - see that type's doc comments.
	ServiceInstalled bool
	ServiceActive    bool
	StatusAvailable  bool
	Malformed        bool

	// ServiceState/PanelDetected/LastErrorCategory are epaper_main's own
	// self-reported ServiceState/PanelDetected/ErrorCategory (epaper/
	// status.go), carried here as plain strings/bool so this package need
	// not import epaper. Empty ServiceState means no status has been
	// observed yet.
	ServiceState      string
	PanelDetected     bool
	LastErrorCategory string

	ConfiguredPanel     string
	FullRefreshCount    uint64
	PartialRefreshCount uint64
	BusyTimeoutCount    uint64
	ConsecutiveFailures uint64

	LastUpdateTime       OptionalTime
	LastUpdateAgeSeconds *float64
	Stale                bool
}

// BuildEpaperHealth derives an EpaperHealth from the stratux_epaper
// systemd unit's load/active state and epaper_main's own self-reported
// status file, both already gathered by the caller (main/epaperstatus.go).
// It performs no I/O.
//
// Policy, evaluated in order:
//   - NOT_INSTALLED: the feature is disabled in settings (a deliberate,
//     opt-in choice, never a failure - per Rollup's own documented
//     exclusion of NOT_INSTALLED/UNKNOWN, this never degrades overall
//     system readiness merely by being off), OR the systemd unit is not
//     installed on this system at all (a platform/build fact, matching
//     FanHealth's identical treatment of ServiceInstalled).
//   - NOT_READY: enabled and the unit is installed but not active - the
//     operator asked for this and it is not running.
//   - DEGRADED: active but no runtime status has been observed yet, or
//     the status file is malformed, or the panel was not detected (most
//     commonly: no display is physically wired up yet - never itself a
//     failure of any other subsystem), or epaper_main reported an internal
//     error, or the status is stale. Never NOT_READY for any of these:
//     this hardware is purely cosmetic/supplemental, so even a confirmed
//     driver fault is reported as an attention-worthy amber tile, never a
//     red one that could be mistaken for a core radio/AHRS/GPS failure.
//   - READY: enabled, installed, active, and the panel is detected with
//     no reported error and fresh status.
func BuildEpaperHealth(enabled, serviceInstalled, serviceActive, statusAvailable, malformed bool, serviceState string, panelDetected bool, lastErrorCategory, configuredPanel string, fullRefreshCount, partialRefreshCount, busyTimeoutCount, consecutiveFailures uint64, lastUpdateTime, now time.Time, staleAfter time.Duration) EpaperHealth {
	h := EpaperHealth{
		Enabled:             enabled,
		ServiceInstalled:    serviceInstalled,
		ServiceActive:       serviceActive,
		StatusAvailable:     statusAvailable,
		Malformed:           malformed,
		ServiceState:        serviceState,
		PanelDetected:       panelDetected,
		LastErrorCategory:   lastErrorCategory,
		ConfiguredPanel:     configuredPanel,
		FullRefreshCount:    fullRefreshCount,
		PartialRefreshCount: partialRefreshCount,
		BusyTimeoutCount:    busyTimeoutCount,
		ConsecutiveFailures: consecutiveFailures,
		LastUpdateTime:      SomeTime(lastUpdateTime),
	}
	if !lastUpdateTime.IsZero() {
		age := now.Sub(lastUpdateTime).Seconds()
		h.LastUpdateAgeSeconds = &age
		h.Stale = age > staleAfter.Seconds()
	}

	switch {
	case !enabled:
		h.State = StateNotInstalled
		h.Reason = "e-paper display is disabled"
	case !serviceInstalled:
		h.State = StateNotInstalled
		h.Reason = "e-paper display service is not installed on this system"
	case !serviceActive:
		h.State = StateNotReady
		h.Reason = "e-paper display service is installed but not active"
	case !statusAvailable:
		h.State = StateDegraded
		h.Reason = "e-paper display service active but no runtime status has been observed yet"
	case malformed:
		h.State = StateDegraded
		h.Reason = "e-paper display status file present but malformed"
	case !panelDetected:
		h.State = StateDegraded
		h.Reason = "e-paper display enabled but no panel detected - check wiring, or this is expected if none is connected yet"
	case lastErrorCategory != "":
		h.State = StateDegraded
		h.Reason = "e-paper display reported an error: " + lastErrorCategory
	case h.Stale:
		h.State = StateDegraded
		h.Reason = "e-paper display status is stale"
	default:
		h.State = StateReady
		h.Reason = "e-paper display active (" + serviceState + ")"
	}
	return h
}
