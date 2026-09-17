package epaper

import "time"

// Content is everything the display can show, gathered from the main
// daemon's existing stable, read-only JSON APIs (/getStatus, /getHealth,
// /getSettings, /getStorageLifecycle, /getAutoRecordStatus) - never a new
// API, never coordinates, never a credential, never a client identifier.
// epaper_main is the only code that populates this from HTTP; everything
// in this package treats it as an opaque, already-gathered snapshot.
type Content struct {
	// SampledAt is when this snapshot was gathered (from epaper_main's
	// own clock) - the basis for Health.DataAgeSeconds and for
	// ShouldShowStale.
	SampledAt time.Time `json:"sampledAt"`

	Version string `json:"version"`
	Build   string `json:"build"` // short (first 8 chars) - never the full commit, to keep the panel legible

	// OverallReady mirrors readiness.Report.Overall verbatim
	// (READY/CAUTION/NOT_READY/UNKNOWN) - never re-interpreted or
	// softened here.
	OverallReady string `json:"overallReady"`

	GPSFix                bool `json:"gpsFix"`
	TrustedTime           bool `json:"trustedTime"`
	UATReceiving          bool `json:"uatReceiving"`
	ESReceiving           bool `json:"esReceiving"`
	ConnectedGDL90Clients int  `json:"connectedGdl90Clients"`
	TrafficTargets        int  `json:"trafficTargets"`

	AHRSState string `json:"ahrsState"` // READY/DEGRADED/NOT_READY/NOT_INSTALLED/UNKNOWN
	BaroState string `json:"baroState"`
	FanState  string `json:"fanState"`

	// CPUTempC is intentionally rounded by the caller (epaper_main) to
	// the nearest degree before being placed here - see the package doc
	// comment on avoiding rapidly-changing tactical-looking values.
	CPUTempC        int  `json:"cpuTempC"`
	UndervoltageNow bool `json:"undervoltageNow"`
	ThrottledNow    bool `json:"throttledNow"`

	OverlayProtected bool   `json:"overlayProtected"`
	StoragePressure  string `json:"storagePressure"` // NORMAL/WARNING/CRITICAL/UNKNOWN

	AutoRecordArmed bool `json:"autoRecordArmed"`
	AlertsEnabled   bool `json:"alertsEnabled"`
	AlertsMuted     bool `json:"alertsMuted"`
}

// MaterialChange reports whether next differs from prev in a way that
// warrants a refresh, per the change-driven policy in
// docs/waveshare-epaper-display.md: SampledAt itself is deliberately
// excluded (time always "changes" and must never alone trigger a
// refresh - only rendered/derived values matter), and count fields
// that increment continuously (ConnectedGDL90Clients, TrafficTargets)
// still compare by exact value since a *change* in a low-single-digit
// count is exactly the kind of low-frequency, meaningful event this
// display exists to show.
func MaterialChange(prev, next Content) bool {
	prev.SampledAt = time.Time{}
	next.SampledAt = time.Time{}
	return prev != next
}

// ShouldShowStale reports whether age exceeds the stale-data threshold -
// past this point the panel must show an explicit "stale/offline"
// indication rather than continuing to display an unchanged (and by now
// possibly misleading) last-known value. See
// docs/waveshare-epaper-display.md.
func ShouldShowStale(age time.Duration) bool {
	return age > StaleDataThresholdSeconds*time.Second
}
