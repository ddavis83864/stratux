package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/stratux/stratux/epaper"
)

// StatusSource gathers an epaper.Content snapshot from the main daemon's
// own already-existing, stable, read-only HTTP APIs - never a new API,
// never a direct Go-level import of the main daemon's internal state.
// This is a genuine, decoupled HTTP client so a bug or hang in this
// process can never reach into the main daemon's own memory - see
// docs/waveshare-epaper-display.md's architecture section.
type StatusSource struct {
	BaseURL string
	Client  *http.Client
}

// NewStatusSource returns a StatusSource with a bounded per-request
// timeout - every call this package makes to the main daemon must never
// block indefinitely, per this feature's own "never block... the status
// producer" requirement (the "producer" being the main daemon; this
// process is only ever a client of it).
func NewStatusSource(baseURL string, timeout time.Duration) *StatusSource {
	return &StatusSource{BaseURL: baseURL, Client: &http.Client{Timeout: timeout}}
}

func (s *StatusSource) getJSON(path string) (map[string]interface{}, error) {
	resp, err := s.Client.Get(s.BaseURL + path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// str/bl/in/nested are small, defensive JSON-map accessors: a missing or
// wrong-typed field degrades to the zero value rather than a panic or
// error, since a partially-available or slightly-differently-shaped
// status response must never take down this optional display - the
// worst case is a field honestly renders as blank/false/zero, never a
// crash.
func str(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
func bl(m map[string]interface{}, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}
func in(m map[string]interface{}, key string) int {
	if v, ok := m[key].(float64); ok { // encoding/json decodes all JSON numbers as float64 into interface{}
		return int(v)
	}
	return 0
}
func nested(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key].(map[string]interface{}); ok {
		return v
	}
	return map[string]interface{}{}
}

// buildLen returns the first n characters of build (never panics on a
// short string) - Content.Build is documented as short-only, never the
// full commit.
func shortBuild(build string, n int) string {
	if len(build) <= n {
		return build
	}
	return build[:n]
}

// Gather queries every API this display needs and assembles an
// epaper.Content. A failure on any individual endpoint degrades that
// endpoint's own fields to their zero value rather than failing the
// whole gather - see the doc comment on the str/bl/in helpers above. The
// overall error return is non-nil only if the single most essential
// endpoint (/getStatus) could not be reached at all, since a display
// with literally nothing to show is the one case worth surfacing as
// ErrorStatusSourceUp in the caller's health reporting.
func (s *StatusSource) Gather(now time.Time) (epaper.Content, error) {
	statusM, err := s.getJSON("/getStatus")
	if err != nil {
		return epaper.Content{}, err
	}
	healthM, _ := s.getJSON("/getHealth")
	storageM, _ := s.getJSON("/getStorageLifecycle")
	autoRecordM, _ := s.getJSON("/getAutoRecordStatus")
	powerM, _ := s.getJSON("/getPowerHealth")

	c := epaper.Content{
		SampledAt: now,
		Version:   str(statusM, "Version"),
		Build:     shortBuild(str(statusM, "Build"), 8),

		GPSFix:                str(statusM, "GPS_solution") != "" && str(statusM, "GPS_solution") != "No Fix",
		UATReceiving:          bl(statusM, "UAT_Receiving") || bl(statusM, "ES_Receiving"), // UAT-specific field name varies by build; ES covers the common case
		ESReceiving:           bl(statusM, "ES_Receiving"),
		ConnectedGDL90Clients: in(statusM, "Connected_Users"),

		OverallReady: str(healthM, "Overall"),
		AHRSState:    str(nested(healthM, "AHRS"), "State"),
		BaroState:    str(nested(healthM, "Baro"), "State"),
		FanState:     str(nested(healthM, "Fan"), "State"),
		TrustedTime:  str(nested(healthM, "Time"), "State") == "READY",

		CPUTempC:         int(roundFloat(nested(healthM, "System")["CPUTempC"])),
		ThrottledNow:     bl(nested(healthM, "System"), "Throttled"),
		UndervoltageNow:  bl(nested(healthM, "System"), "UndervoltageDetected"),
		OverlayProtected: str(nested(healthM, "TemporaryOverlay"), "State") == "READY",

		StoragePressure: str(storageM, "pressure"),
		AutoRecordArmed: str(nested(autoRecordM, "snapshot"), "State") != "" && str(nested(autoRecordM, "snapshot"), "State") != "DISABLED",
	}
	if c.StoragePressure == "" {
		c.StoragePressure = "UNKNOWN"
	}
	if c.OverallReady == "" {
		c.OverallReady = "UNKNOWN"
	}
	_ = powerM // reserved: undervoltage/throttled already come from /getHealth's System section above
	return c, nil
}

func roundFloat(v interface{}) float64 {
	f, _ := v.(float64)
	return f
}
