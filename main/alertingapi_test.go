package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stratux/stratux/alerting"
	"github.com/stratux/stratux/readiness"
)

// withTestAlertEvaluator points the package-level alertEvaluator at a
// fresh Evaluator for the duration of one test, and restores the previous
// value (and channel) afterward - mirrors withTestPreflightStore's
// pattern.
func withTestAlertEvaluator(t *testing.T) *alerting.Evaluator {
	t.Helper()
	ensureStratuxClockForTest()
	origEval := alertEvaluator
	origChan := alertTrafficChan
	cfg := alerting.DefaultConfig()
	cfg.MasterEnabled = true
	cfg.TrafficAudioEnabled = true
	cfg.SystemAudioEnabled = true
	alertEvaluator = alerting.NewEvaluator(cfg, func() time.Time { return stratuxClock.Time })
	alertTrafficChan = nil // handler tests never need the background loop
	t.Cleanup(func() {
		alertEvaluator = origEval
		alertTrafficChan = origChan
	})
	return alertEvaluator
}

func TestHandleGetAlertsRequest_NotInitialized(t *testing.T) {
	origEval := alertEvaluator
	alertEvaluator = nil
	defer func() { alertEvaluator = origEval }()

	req := httptest.NewRequest(http.MethodGet, "/getAlerts", nil)
	w := httptest.NewRecorder()
	handleGetAlertsRequest(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when alerting is not initialized", w.Code)
	}
}

func TestHandleGetAlertsRequest_Valid(t *testing.T) {
	withTestAlertEvaluator(t)
	req := httptest.NewRequest(http.MethodGet, "/getAlerts", nil)
	w := httptest.NewRecorder()
	handleGetAlertsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var snap alerting.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("response is not a valid alerting.Snapshot: %v", err)
	}
	if snap.Disclaimer == "" {
		t.Error("expected a non-empty disclaimer")
	}
}

func TestHandleGetAlertsRequest_WrongMethod(t *testing.T) {
	withTestAlertEvaluator(t)
	req := httptest.NewRequest(http.MethodPost, "/getAlerts", nil)
	w := httptest.NewRecorder()
	handleGetAlertsRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestHandleGetAlertSettingsRequest_ReturnsDefaults(t *testing.T) {
	withTestAlertSettingsPath(t)
	req := httptest.NewRequest(http.MethodGet, "/getAlertSettings", nil)
	w := httptest.NewRecorder()
	handleGetAlertSettingsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var s AlertSettings
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if s != DefaultAlertSettings() {
		t.Errorf("expected defaults, got %+v", s)
	}
}

func TestHandleSetAlertSettingsRequest_ValidUpdateRoundTrips(t *testing.T) {
	withTestAlertSettingsPath(t)
	withTestAlertEvaluator(t)

	s := DefaultAlertSettings()
	s.AudioVolume = 0.9
	body, _ := json.Marshal(s)
	req := httptest.NewRequest(http.MethodPost, "/setAlertSettings", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleSetAlertSettingsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	loaded := loadAlertSettings()
	if loaded.AudioVolume != 0.9 {
		t.Errorf("AudioVolume = %v, want 0.9", loaded.AudioVolume)
	}
	if alertEvaluator.Config().NoticeHorizontalMeters != loaded.NoticeHorizontalNM*1852.0 {
		t.Error("expected the live evaluator config to be updated in place")
	}
}

func TestHandleSetAlertSettingsRequest_InvalidRejected(t *testing.T) {
	withTestAlertSettingsPath(t)
	withTestAlertEvaluator(t)

	req := httptest.NewRequest(http.MethodPost, "/setAlertSettings", strings.NewReader(`{"audioVolume": 99}`))
	w := httptest.NewRecorder()
	handleSetAlertSettingsRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleSetAlertSettingsRequest_OversizedPayload(t *testing.T) {
	withTestAlertSettingsPath(t)
	withTestAlertEvaluator(t)

	huge := `{"comment":"` + strings.Repeat("a", maxAlertRequestBytes+1000) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/setAlertSettings", strings.NewReader(huge))
	w := httptest.NewRecorder()
	handleSetAlertSettingsRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized body", w.Code)
	}
}

func TestHandleSetAlertSettingsRequest_WrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/setAlertSettings", nil)
	w := httptest.NewRecorder()
	handleSetAlertSettingsRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestHandleAcknowledgeAlertRequest_UnknownIDIsIdempotentNotError(t *testing.T) {
	withTestAlertEvaluator(t)
	req := httptest.NewRequest(http.MethodPost, "/acknowledgeAlert?id=NOTREAL", nil)
	w := httptest.NewRecorder()
	handleAcknowledgeAlertRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even for an unknown id", w.Code)
	}
	var body map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["found"] != false {
		t.Errorf("found = %v, want false", body["found"])
	}
}

func TestHandleAcknowledgeAlertRequest_KnownTarget(t *testing.T) {
	e := withTestAlertEvaluator(t)
	e.EvaluateTraffic([]alerting.TrafficObservation{{
		TargetID: "ABC123", PositionValid: true, DistanceValid: true,
		DistanceMeters: 4000, RelativeAltitudeValid: true, RelativeAltitudeFeet: 500, AgeSeconds: 1,
	}}, true)

	req := httptest.NewRequest(http.MethodPost, "/acknowledgeAlert?id=ABC123", nil)
	w := httptest.NewRecorder()
	handleAcknowledgeAlertRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["found"] != true {
		t.Errorf("found = %v, want true for a known target", body["found"])
	}
}

func TestHandleAcknowledgeAlertRequest_MissingID(t *testing.T) {
	withTestAlertEvaluator(t)
	req := httptest.NewRequest(http.MethodPost, "/acknowledgeAlert", nil)
	w := httptest.NewRecorder()
	handleAcknowledgeAlertRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleMuteAlertsRequest_IndefiniteAndUnmute(t *testing.T) {
	withTestAlertSettingsPath(t)
	e := withTestAlertEvaluator(t)

	req := httptest.NewRequest(http.MethodPost, "/muteAlerts", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handleMuteAlertsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !e.IsMuted() {
		t.Error("expected evaluator to be muted immediately")
	}
	if !loadAlertSettings().MutedIndefinitely {
		t.Error("expected mute preference to persist as indefinite")
	}

	req2 := httptest.NewRequest(http.MethodPost, "/unmuteAlerts", nil)
	w2 := httptest.NewRecorder()
	handleUnmuteAlertsRequest(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w2.Code)
	}
	if e.IsMuted() {
		t.Error("expected evaluator to be unmuted")
	}
	if loadAlertSettings().Muted {
		t.Error("expected mute preference to be cleared in persisted settings")
	}
}

func TestHandleMuteAlertsRequest_TimedMute(t *testing.T) {
	withTestAlertSettingsPath(t)
	e := withTestAlertEvaluator(t)

	req := httptest.NewRequest(http.MethodPost, "/muteAlerts", strings.NewReader(`{"durationSeconds": 60}`))
	w := httptest.NewRecorder()
	handleMuteAlertsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !e.IsMuted() {
		t.Error("expected muted")
	}
	s := loadAlertSettings()
	if s.MutedIndefinitely {
		t.Error("a timed mute must not be recorded as indefinite")
	}
	if s.MuteUntilUnixSeconds <= time.Now().Unix() {
		t.Error("expected MuteUntilUnixSeconds in the future")
	}
}

// TestHandleMuteAlertsRequest_OverflowDurationRejected reproduces a real
// bug found during development: an astronomically large durationSeconds
// value, once converted to a nanosecond-based time.Duration, silently
// overflowed int64 and wrapped around to a value that incorrectly passed
// the maximum-duration check. The fix compares in float64 seconds before
// ever constructing a time.Duration - see main/alertingapi.go and
// main/alertsettings.go's identical fix.
func TestHandleMuteAlertsRequest_OverflowDurationRejected(t *testing.T) {
	withTestAlertSettingsPath(t)
	withTestAlertEvaluator(t)
	for _, seconds := range []string{"1e30", "9223372036854775807"} {
		req := httptest.NewRequest(http.MethodPost, "/muteAlerts", strings.NewReader(`{"durationSeconds": `+seconds+`}`))
		w := httptest.NewRecorder()
		handleMuteAlertsRequest(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("durationSeconds=%s: status = %d, want 400 (overflow must not silently pass)", seconds, w.Code)
		}
	}
}

func TestHandleMuteAlertsRequest_NaNAndInfRejected(t *testing.T) {
	withTestAlertSettingsPath(t)
	withTestAlertEvaluator(t)
	for _, body := range []string{`{"durationSeconds": 1e999}`, `{"durationSeconds": -1e999}`} {
		req := httptest.NewRequest(http.MethodPost, "/muteAlerts", strings.NewReader(body))
		w := httptest.NewRecorder()
		handleMuteAlertsRequest(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want 400", body, w.Code)
		}
	}
}

func TestHandleMuteAlertsRequest_ExcessiveDurationRejected(t *testing.T) {
	withTestAlertSettingsPath(t)
	withTestAlertEvaluator(t)
	req := httptest.NewRequest(http.MethodPost, "/muteAlerts", strings.NewReader(`{"durationSeconds": 999999999}`))
	w := httptest.NewRecorder()
	handleMuteAlertsRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an excessive mute duration", w.Code)
	}
}

func TestHandleMuteAlertsRequest_NegativeDurationRejected(t *testing.T) {
	withTestAlertSettingsPath(t)
	withTestAlertEvaluator(t)
	req := httptest.NewRequest(http.MethodPost, "/muteAlerts", strings.NewReader(`{"durationSeconds": -5}`))
	w := httptest.NewRecorder()
	handleMuteAlertsRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a negative mute duration", w.Code)
	}
}

func TestHandleMuteAlertsRequest_RepeatedIsIdempotent(t *testing.T) {
	withTestAlertSettingsPath(t)
	e := withTestAlertEvaluator(t)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/muteAlerts", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		handleMuteAlertsRequest(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i, w.Code)
		}
	}
	if !e.IsMuted() {
		t.Error("expected muted after repeated mute calls")
	}
}

func TestHandleTestAlertSoundRequest_DoesNotCreateRealEvent(t *testing.T) {
	e := withTestAlertEvaluator(t)
	before := len(e.Snapshot().RecentHistory)

	req := httptest.NewRequest(http.MethodPost, "/testAlertSound", nil)
	w := httptest.NewRecorder()
	handleTestAlertSoundRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	after := len(e.Snapshot().RecentHistory)
	if after != before {
		t.Errorf("expected /testAlertSound to create no real event history entry: before=%d after=%d", before, after)
	}
}

func TestSubmitTrafficObservationsForAlerting_OverflowDropsNotBlocks(t *testing.T) {
	withTestAlertEvaluator(t)
	origChan := alertTrafficChan
	alertTrafficChan = make(chan []alerting.TrafficObservation, 1)
	defer func() { alertTrafficChan = origChan }()

	// Fill the channel, then submit again - must return immediately
	// (nonblocking) rather than stall traffic ingestion.
	alertTrafficChan <- []alerting.TrafficObservation{}
	done := make(chan struct{})
	go func() {
		submitTrafficObservationsForAlerting([]alerting.TrafficObservation{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("submitTrafficObservationsForAlerting blocked on a full channel")
	}
	if alertEvaluator.Snapshot().Counters.DroppedEvaluations != 1 {
		t.Errorf("DroppedEvaluations = %d, want 1", alertEvaluator.Snapshot().Counters.DroppedEvaluations)
	}
}

func TestAlertingHealthMapping_ComponentStateTranslation(t *testing.T) {
	if mapTimeState(readiness.TimeGNSSSynced) != alerting.ComponentReady {
		t.Error("TimeGNSSSynced should map to Ready")
	}
	if mapTimeState(readiness.TimeUnsynchronized) != alerting.ComponentCaution {
		t.Error("TimeUnsynchronized should map to Caution (not Unknown) so a later loss-of-sync can alert")
	}
	if mapTimeState(readiness.TimeInvalid) != alerting.ComponentNotReady {
		t.Error("TimeInvalid should map to NotReady")
	}
	if mapComponentState(readiness.StateNotInstalled) != alerting.ComponentUnknown {
		t.Error("StateNotInstalled (optional disabled component) should map to Unknown, never a degradation")
	}
	if mapComponentState(readiness.StateReady) != alerting.ComponentReady {
		t.Error("StateReady should map to Ready")
	}
}
