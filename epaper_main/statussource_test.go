package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fixtureServer returns an httptest.Server that serves canned JSON bodies
// for exactly the paths this package's Gather queries, and fails the test
// if any other path is requested - so a future added/renamed endpoint
// call is caught here rather than silently returning zero values.
func fixtureServer(t *testing.T, byPath map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := byPath[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(body)
	}))
}

// TestGather_PopulatesEveryContentField is a regression test for a defect
// found during documentation review: Gather originally never called
// /getAlertSettings or /getAlerts, and never summed the UAT/ES traffic-
// target counters, so AlertsEnabled/AlertsMuted/TrafficTargets always
// silently read as their zero values (false/false/0) regardless of the
// main daemon's real state - exactly the kind of quiet-but-wrong display
// content this feature's own design explicitly warns against.
func TestGather_PopulatesEveryContentField(t *testing.T) {
	srv := fixtureServer(t, map[string]interface{}{
		"/getStatus": map[string]interface{}{
			"Version":                      "v2.0.0",
			"Build":                        "0123456789abcdef",
			"GPS_solution":                 "3D GPS",
			"UAT_Receiving":                true,
			"ES_Receiving":                 true,
			"Connected_Users":              2,
			"UAT_traffic_targets_tracking": 3,
			"ES_traffic_targets_tracking":  4,
		},
		"/getHealth": map[string]interface{}{
			"Overall":          "READY",
			"AHRS":             map[string]interface{}{"State": "READY"},
			"Baro":             map[string]interface{}{"State": "READY"},
			"Fan":              map[string]interface{}{"State": "READY"},
			"Time":             map[string]interface{}{"State": "READY"},
			"System":           map[string]interface{}{"CPUTempC": 42.0, "Throttled": false, "UndervoltageDetected": false},
			"TemporaryOverlay": map[string]interface{}{"State": "READY"},
		},
		"/getStorageLifecycle": map[string]interface{}{"pressure": "NORMAL"},
		"/getAutoRecordStatus": map[string]interface{}{"snapshot": map[string]interface{}{"State": "ARMED_WAITING"}},
		"/getPowerHealth":      map[string]interface{}{},
		"/getAlertSettings":    map[string]interface{}{"masterEnabled": true},
		"/getAlerts":           map[string]interface{}{"muted": true},
	})
	defer srv.Close()

	src := NewStatusSource(srv.URL, 3*time.Second)
	c, err := src.Gather(time.Now())
	if err != nil {
		t.Fatalf("Gather returned an error: %v", err)
	}

	if c.TrafficTargets != 7 {
		t.Errorf("TrafficTargets = %d, want 7 (3 UAT + 4 ES)", c.TrafficTargets)
	}
	if !c.AlertsEnabled {
		t.Error("AlertsEnabled = false, want true (from /getAlertSettings masterEnabled)")
	}
	if !c.AlertsMuted {
		t.Error("AlertsMuted = false, want true (from /getAlerts muted)")
	}
	if c.Version != "v2.0.0" || c.OverallReady != "READY" || c.ConnectedGDL90Clients != 2 {
		t.Errorf("unexpected core fields: %+v", c)
	}
}

// TestGather_MissingOptionalEndpointsDegradeToZeroValue confirms a
// non-essential endpoint being unreachable (e.g. an older main daemon
// build, or a transient hiccup) degrades only that endpoint's own fields
// to their zero value rather than failing the whole Gather - /getStatus
// is the only endpoint whose absence is itself an error.
func TestGather_MissingOptionalEndpointsDegradeToZeroValue(t *testing.T) {
	srv := fixtureServer(t, map[string]interface{}{
		"/getStatus": map[string]interface{}{"Version": "v2.0.0"},
	})
	defer srv.Close()

	src := NewStatusSource(srv.URL, 3*time.Second)
	c, err := src.Gather(time.Now())
	if err != nil {
		t.Fatalf("Gather returned an error: %v", err)
	}
	if c.AlertsEnabled || c.AlertsMuted || c.TrafficTargets != 0 {
		t.Errorf("expected zero-valued alert/traffic fields when those endpoints are unreachable, got %+v", c)
	}
	if c.StoragePressure != "UNKNOWN" || c.OverallReady != "UNKNOWN" {
		t.Errorf("expected UNKNOWN fallback for missing storage/health data, got StoragePressure=%q OverallReady=%q", c.StoragePressure, c.OverallReady)
	}
}

func TestGather_EssentialEndpointDownIsAnError(t *testing.T) {
	srv := fixtureServer(t, map[string]interface{}{})
	defer srv.Close()

	src := NewStatusSource(srv.URL, 3*time.Second)
	if _, err := src.Gather(time.Now()); err == nil {
		t.Error("expected an error when /getStatus itself is unreachable")
	}
}
