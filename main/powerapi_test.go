package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stratux/stratux/power"
)

// fakePowerExecutor never touches real hardware - see power/shutdown_test.go's
// identical rationale.
type fakePowerExecutor struct {
	syncCalls   int
	powerCalls  int
	rebootCalls int
	syncErr     error
}

func (f *fakePowerExecutor) Sync() error {
	f.syncCalls++
	return f.syncErr
}
func (f *fakePowerExecutor) PowerOff() error {
	f.powerCalls++
	return nil
}
func (f *fakePowerExecutor) Reboot() error {
	f.rebootCalls++
	return nil
}

// withTestShutdownManager installs a Manager backed by a fake executor
// for the duration of one test, and restores the original afterward -
// mirrors alertingapi_test.go's withTestAlertEvaluator pattern.
func withTestShutdownManager(t *testing.T, preconditions []power.Precondition) *fakePowerExecutor {
	t.Helper()
	orig := shutdownManager
	exec := &fakePowerExecutor{}
	ensureStratuxClockForTest()
	shutdownManager = power.NewManager("test-boot-session", monotonicSeconds, preconditions, nil, exec)
	t.Cleanup(func() { shutdownManager = orig })
	return exec
}

// withTestRebootManager mirrors withTestShutdownManager exactly, for the
// reboot flow's own Manager instance.
func withTestRebootManager(t *testing.T, preconditions []power.Precondition) *fakePowerExecutor {
	t.Helper()
	orig := rebootManager
	exec := &fakePowerExecutor{}
	ensureStratuxClockForTest()
	rebootManager = power.NewManager("test-boot-session", monotonicSeconds, preconditions, nil, exec)
	t.Cleanup(func() { rebootManager = orig })
	return exec
}

func withTestPowerSessionMarkerPath(t *testing.T) {
	t.Helper()
	orig := powerSessionMarkerPath
	powerSessionMarkerPath = filepath.Join(t.TempDir(), "power-session.json")
	t.Cleanup(func() { powerSessionMarkerPath = orig })
}

func requestShutdownToken(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/requestShutdown", nil)
	w := httptest.NewRecorder()
	handleRequestShutdownRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("requestShutdown: status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("requestShutdown: invalid JSON response: %v", err)
	}
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatalf("requestShutdown: no token in response: %s", w.Body.String())
	}
	return tok
}

func requestRebootToken(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/requestReboot", nil)
	w := httptest.NewRecorder()
	handleRequestRebootRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("requestReboot: status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("requestReboot: invalid JSON response: %v", err)
	}
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatalf("requestReboot: no token in response: %s", w.Body.String())
	}
	return tok
}

func TestHandleGetPowerHealthRequest_ReportsCapabilityHonesty(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/getPowerHealth", nil)
	w := httptest.NewRecorder()
	handleGetPowerHealthRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["hasTrustedBatterySignal"] != false {
		t.Errorf("hasTrustedBatterySignal must be false, got %v", body["hasTrustedBatterySignal"])
	}
	if body["hasRuntimeEstimate"] != false {
		t.Errorf("hasRuntimeEstimate must be false, got %v", body["hasRuntimeEstimate"])
	}
	notes, _ := body["notes"].([]interface{})
	if len(notes) == 0 {
		t.Error("expected non-empty notes explaining the capability limits")
	}
}

func TestHandleGetPowerHealthRequest_WrongMethodRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/getPowerHealth", nil)
	w := httptest.NewRecorder()
	handleGetPowerHealthRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestShutdownFlow_FullSuccessThroughHTTP(t *testing.T) {
	exec := withTestShutdownManager(t, nil)
	withTestPowerSessionMarkerPath(t)

	tok := requestShutdownToken(t)

	body, _ := json.Marshal(confirmShutdownRequest{Token: tok})
	req := httptest.NewRequest(http.MethodPost, "/confirmShutdown", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleConfirmShutdownRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("confirmShutdown: status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("expected success:true, got %v", resp)
	}
	if resp["stage"] != string(power.StageCommandIssued) {
		t.Errorf("expected stage=command_issued, got %v", resp["stage"])
	}
	if exec.syncCalls != 1 {
		t.Errorf("expected 1 Sync call, got %d", exec.syncCalls)
	}
	if exec.powerCalls != 1 {
		t.Errorf("expected 1 PowerOff call (issued after the response was written), got %d", exec.powerCalls)
	}

	marker, ok, err := power.ReadSessionMarker(powerSessionMarkerPath)
	if err != nil || !ok {
		t.Fatalf("expected a session marker to have been written, ok=%v err=%v", ok, err)
	}
	if !marker.ClosedCleanly || marker.ClosedReason != "controlled-shutdown" {
		t.Errorf("expected a clean controlled-shutdown marker, got %+v", marker)
	}
}

func TestShutdownFlow_RequestBlockedByPrecondition(t *testing.T) {
	blocked := func() error { return errFakePrecondition }
	withTestShutdownManager(t, []power.Precondition{blocked})

	req := httptest.NewRequest(http.MethodPost, "/requestShutdown", nil)
	w := httptest.NewRecorder()
	handleRequestShutdownRequest(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

var errFakePrecondition = errFake("OTA update in progress")

type errFake string

func (e errFake) Error() string { return string(e) }

func TestShutdownFlow_ConfirmWithoutRequestIsRejected(t *testing.T) {
	withTestShutdownManager(t, nil)
	body, _ := json.Marshal(confirmShutdownRequest{Token: "not-a-real-token"})
	req := httptest.NewRequest(http.MethodPost, "/confirmShutdown", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleConfirmShutdownRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestShutdownFlow_ConfirmMissingTokenRejected(t *testing.T) {
	withTestShutdownManager(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/confirmShutdown", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	handleConfirmShutdownRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a missing token, got %d: %s", w.Code, w.Body.String())
	}
}

func TestShutdownFlow_ConfirmMalformedBodyRejected(t *testing.T) {
	withTestShutdownManager(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/confirmShutdown", bytes.NewReader([]byte(`{not json`)))
	w := httptest.NewRecorder()
	handleConfirmShutdownRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a malformed body, got %d: %s", w.Code, w.Body.String())
	}
}

func TestShutdownFlow_WrongMethodsRejected(t *testing.T) {
	withTestShutdownManager(t, nil)
	for _, tc := range []struct {
		method  string
		handler http.HandlerFunc
		path    string
	}{
		{http.MethodGet, handleRequestShutdownRequest, "/requestShutdown"},
		{http.MethodGet, handleConfirmShutdownRequest, "/confirmShutdown"},
		{http.MethodPost, handleGetShutdownStatusRequest, "/getShutdownStatus"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		tc.handler(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: expected 405, got %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestHandleGetShutdownStatusRequest_ReflectsManagerState(t *testing.T) {
	withTestShutdownManager(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/getShutdownStatus", nil)
	w := httptest.NewRecorder()
	handleGetShutdownStatusRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["stage"] != string(power.StageIdle) {
		t.Errorf("expected idle, got %v", body["stage"])
	}
}

// --- Reboot flow - mirrors the shutdown flow's own coverage exactly,
// against the separate rebootManager instance and IssueReboot instead of
// IssuePowerOff. See docs/power-shutdown-resilience.md.

func TestRebootFlow_FullSuccessThroughHTTP(t *testing.T) {
	exec := withTestRebootManager(t, nil)
	withTestPowerSessionMarkerPath(t)

	tok := requestRebootToken(t)

	body, _ := json.Marshal(confirmRebootRequest{Token: tok})
	req := httptest.NewRequest(http.MethodPost, "/confirmReboot", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleConfirmRebootRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("confirmReboot: status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("expected success:true, got %v", resp)
	}
	if resp["stage"] != string(power.StageCommandIssued) {
		t.Errorf("expected stage=command_issued, got %v", resp["stage"])
	}
	if exec.syncCalls != 1 {
		t.Errorf("expected 1 Sync call, got %d", exec.syncCalls)
	}
	if exec.rebootCalls != 1 {
		t.Errorf("expected 1 Reboot call (issued after the response was written), got %d", exec.rebootCalls)
	}
	if exec.powerCalls != 0 {
		t.Errorf("a confirmed reboot must never call PowerOff, got %d", exec.powerCalls)
	}

	marker, ok, err := power.ReadSessionMarker(powerSessionMarkerPath)
	if err != nil || !ok {
		t.Fatalf("expected a session marker to have been written, ok=%v err=%v", ok, err)
	}
	if !marker.ClosedCleanly || marker.ClosedReason != "controlled-reboot" {
		t.Errorf("expected a clean controlled-reboot marker, got %+v", marker)
	}
}

func TestRebootFlow_RequestBlockedByPrecondition(t *testing.T) {
	blocked := func() error { return errFakePrecondition }
	withTestRebootManager(t, []power.Precondition{blocked})

	req := httptest.NewRequest(http.MethodPost, "/requestReboot", nil)
	w := httptest.NewRecorder()
	handleRequestRebootRequest(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRebootFlow_ConfirmWithoutRequestIsRejected(t *testing.T) {
	withTestRebootManager(t, nil)
	body, _ := json.Marshal(confirmRebootRequest{Token: "not-a-real-token"})
	req := httptest.NewRequest(http.MethodPost, "/confirmReboot", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleConfirmRebootRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRebootFlow_ConfirmMissingTokenRejected(t *testing.T) {
	withTestRebootManager(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/confirmReboot", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	handleConfirmRebootRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a missing token, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRebootFlow_ConfirmMalformedBodyRejected(t *testing.T) {
	withTestRebootManager(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/confirmReboot", bytes.NewReader([]byte(`{not json`)))
	w := httptest.NewRecorder()
	handleConfirmRebootRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a malformed body, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRebootFlow_WrongMethodsRejected(t *testing.T) {
	withTestRebootManager(t, nil)
	for _, tc := range []struct {
		method  string
		handler http.HandlerFunc
		path    string
	}{
		{http.MethodGet, handleRequestRebootRequest, "/requestReboot"},
		{http.MethodGet, handleConfirmRebootRequest, "/confirmReboot"},
		{http.MethodPost, handleGetRebootStatusRequest, "/getRebootStatus"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		tc.handler(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: expected 405, got %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestHandleGetRebootStatusRequest_ReflectsManagerState(t *testing.T) {
	withTestRebootManager(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/getRebootStatus", nil)
	w := httptest.NewRecorder()
	handleGetRebootStatusRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["stage"] != string(power.StageIdle) {
		t.Errorf("expected idle, got %v", body["stage"])
	}
}

// TestShutdownAndRebootFlows_AreIndependent proves the two Manager
// instances (shutdownManager/rebootManager) never share state - preparing
// a shutdown must never satisfy a reboot confirmation, or vice versa.
func TestShutdownAndRebootFlows_AreIndependent(t *testing.T) {
	withTestShutdownManager(t, nil)
	withTestRebootManager(t, nil)

	shutdownTok := requestShutdownToken(t)
	rebootTok := requestRebootToken(t)
	if shutdownTok == rebootTok {
		t.Fatalf("shutdown and reboot tokens must never collide: both were %q", shutdownTok)
	}

	// A reboot token must never confirm a shutdown, and vice versa.
	body, _ := json.Marshal(confirmShutdownRequest{Token: rebootTok})
	req := httptest.NewRequest(http.MethodPost, "/confirmShutdown", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleConfirmShutdownRequest(w, req)
	if w.Code == http.StatusOK {
		t.Error("a reboot token must never be accepted by confirmShutdown")
	}

	body, _ = json.Marshal(confirmRebootRequest{Token: shutdownTok})
	req = httptest.NewRequest(http.MethodPost, "/confirmReboot", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handleConfirmRebootRequest(w, req)
	if w.Code == http.StatusOK {
		t.Error("a shutdown token must never be accepted by confirmReboot")
	}
}

func TestCurrentBootOrSessionID_NeverEmpty(t *testing.T) {
	preflightMu.Lock()
	origSessionID := preflightSessionID
	preflightSessionID = "preflight-fallback-test"
	preflightMu.Unlock()
	t.Cleanup(func() {
		preflightMu.Lock()
		preflightSessionID = origSessionID
		preflightMu.Unlock()
	})

	id := currentBootOrSessionID()
	if id == "" {
		t.Error("currentBootOrSessionID must never return an empty string")
	}
}

func TestMarkSessionClosed_WritesClosedMarker(t *testing.T) {
	withTestPowerSessionMarkerPath(t)
	ensureStratuxClockForTest()
	powerPreviousSessionMu.Lock()
	powerCurrentSessionID = "test-session-for-marker"
	powerPreviousSessionMu.Unlock()

	markSessionClosed("reboot")

	marker, ok, err := power.ReadSessionMarker(powerSessionMarkerPath)
	if err != nil || !ok {
		t.Fatalf("expected a marker to exist, ok=%v err=%v", ok, err)
	}
	if !marker.ClosedCleanly || marker.ClosedReason != "reboot" || marker.SessionID != "test-session-for-marker" {
		t.Errorf("unexpected marker contents: %+v", marker)
	}
}

func TestPowerDiagnosticsSummary_NeverNilAndHasNoSensitiveShutdownToken(t *testing.T) {
	withTestShutdownManager(t, nil)
	summary := powerDiagnosticsSummary()
	m, ok := summary.(map[string]interface{})
	if !ok {
		t.Fatalf("expected a map, got %T", summary)
	}
	if _, present := m["token"]; present {
		t.Error("power diagnostics summary must never include an outstanding shutdown token")
	}
	if _, present := m["severity"]; !present {
		t.Error("expected a severity field")
	}
}
