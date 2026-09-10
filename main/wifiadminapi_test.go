package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratux/stratux/wifiadmin"
)

// fakeWifiExecutorForAPI is main package's own fake Executor - never
// touches a real network interface. Used only by this test file.
type fakeWifiExecutorForAPI struct {
	live      wifiadmin.Config
	failSSID  string
	applyErrs int
}

func (f *fakeWifiExecutorForAPI) Apply(cfg wifiadmin.Config) error {
	if f.failSSID != "" && cfg.SSID == f.failSSID {
		f.applyErrs++
		return errTestExecutorFailure
	}
	f.live = cfg
	return nil
}

func (f *fakeWifiExecutorForAPI) HealthCheck(cfg wifiadmin.Config) (wifiadmin.Health, error) {
	return wifiadmin.Health{InterfacePresent: true, InterfaceAddress: f.live.IPAddress, AddressMatches: f.live.IPAddress == cfg.IPAddress}, nil
}

var errTestExecutorFailure = &testExecErr{"fake executor failure"}

type testExecErr struct{ s string }

func (e *testExecErr) Error() string { return e.s }

// withTestWifiAdminManager points wifiAdminManager at a fresh Manager
// backed by fakeWifiExecutorForAPI and the real filePersistence
// redirected to a temp directory, for the duration of one test.
func withTestWifiAdminManager(t *testing.T) *fakeWifiExecutorForAPI {
	t.Helper()
	ensureStratuxClockForTest()
	dir := t.TempDir()
	origGood, origPending := wifiAdminLastKnownGoodPath, wifiAdminPendingPath
	wifiAdminLastKnownGoodPath = dir + "/last-known-good.json"
	wifiAdminPendingPath = dir + "/pending.json"

	origMgr := wifiAdminManager
	exec := &fakeWifiExecutorForAPI{live: wifiadmin.DefaultConfig()}
	mgr, err := wifiadmin.NewManager("test-boot-session", monotonicSeconds, exec, filePersistence{}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mgr.SetReconnectTimeoutSeconds(30)
	wifiAdminManager = mgr

	t.Cleanup(func() {
		wifiAdminManager = origMgr
		wifiAdminLastKnownGoodPath = origGood
		wifiAdminPendingPath = origPending
	})
	return exec
}

func TestHandleGetWifiAdminStatusRequest_NotInitialized(t *testing.T) {
	orig := wifiAdminManager
	wifiAdminManager = nil
	defer func() { wifiAdminManager = orig }()

	req := httptest.NewRequest(http.MethodGet, "/getWifiAdminStatus", nil)
	w := httptest.NewRecorder()
	handleGetWifiAdminStatusRequest(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandleGetWifiAdminStatusRequest_Valid(t *testing.T) {
	withTestWifiAdminManager(t)
	req := httptest.NewRequest(http.MethodGet, "/getWifiAdminStatus", nil)
	w := httptest.NewRecorder()
	handleGetWifiAdminStatusRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var status wifiadmin.ManagerStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if status.Stage != wifiadmin.StageIdle {
		t.Errorf("stage = %v, want idle", status.Stage)
	}
}

func TestHandleGetWifiAdminStatusRequest_WrongMethod(t *testing.T) {
	withTestWifiAdminManager(t)
	req := httptest.NewRequest(http.MethodPost, "/getWifiAdminStatus", nil)
	w := httptest.NewRecorder()
	handleGetWifiAdminStatusRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestWifiAdminAPI_FullPreviewApplyConfirmRoundTrip(t *testing.T) {
	withTestWifiAdminManager(t)

	proposed := wifiadmin.DefaultConfig()
	proposed.SSID = "new-plane-ssid"
	body, _ := json.Marshal(proposed)

	req := httptest.NewRequest(http.MethodPost, "/previewWifiAdminSettings", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var previewResp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &previewResp)
	applyToken, _ := previewResp["applyToken"].(string)
	if applyToken == "" {
		t.Fatal("expected a non-empty applyToken")
	}
	// The response must never contain a raw passphrase, even though none
	// was set in this proposed config - assert the general shape by
	// checking the preview object itself has no "passphrase" field with
	// a real value key at all (ComputePreview only ever emits markers).
	if strings.Contains(w.Body.String(), `"passphrase":"`) {
		t.Error("preview response must never contain a literal passphrase field with a real value")
	}

	applyBody, _ := json.Marshal(applyWifiAdminRequest{ApplyToken: applyToken})
	req2 := httptest.NewRequest(http.MethodPost, "/applyWifiAdminSettings", bytes.NewReader(applyBody))
	w2 := httptest.NewRecorder()
	handleApplyWifiAdminSettingsRequest(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("apply status = %d, want 200: %s", w2.Code, w2.Body.String())
	}
	var applyResp map[string]interface{}
	json.Unmarshal(w2.Body.Bytes(), &applyResp)
	reconnectToken, _ := applyResp["reconnectToken"].(string)
	if reconnectToken == "" {
		t.Fatal("expected a non-empty reconnectToken")
	}
	if applyResp["stage"] != string(wifiadmin.StageAwaitingReconnection) {
		t.Errorf("stage = %v, want awaiting_reconnection", applyResp["stage"])
	}

	confirmBody, _ := json.Marshal(confirmWifiAdminReconnectionRequest{ReconnectToken: reconnectToken})
	req3 := httptest.NewRequest(http.MethodPost, "/confirmWifiAdminReconnection", bytes.NewReader(confirmBody))
	w3 := httptest.NewRecorder()
	handleConfirmWifiAdminReconnectionRequest(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, want 200: %s", w3.Code, w3.Body.String())
	}

	status := wifiAdminManager.Status()
	if status.Stage != wifiadmin.StageIdle || status.LastResult != wifiadmin.ResultCommitted {
		t.Errorf("final status = %+v, want idle/committed", status)
	}
	if status.LastKnownGood.SSID != "new-plane-ssid" {
		t.Errorf("last known good SSID = %q", status.LastKnownGood.SSID)
	}
}

func TestWifiAdminAPI_PreviewRejectsInvalidConfig(t *testing.T) {
	withTestWifiAdminManager(t)
	proposed := wifiadmin.DefaultConfig()
	proposed.SSID = "" // invalid
	body, _ := json.Marshal(proposed)

	req := httptest.NewRequest(http.MethodPost, "/previewWifiAdminSettings", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for invalid SSID", w.Code)
	}
}

func TestWifiAdminAPI_PreviewRejectsMalformedJSON(t *testing.T) {
	withTestWifiAdminManager(t)
	req := httptest.NewRequest(http.MethodPost, "/previewWifiAdminSettings", strings.NewReader(`{bad`))
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestWifiAdminAPI_PreviewRejectsUnknownField(t *testing.T) {
	withTestWifiAdminManager(t)
	req := httptest.NewRequest(http.MethodPost, "/previewWifiAdminSettings", strings.NewReader(`{"ssid":"x","bogusField":1}`))
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown field", w.Code)
	}
}

func TestWifiAdminAPI_PreviewRejectsMultiValueBody(t *testing.T) {
	withTestWifiAdminManager(t)
	proposed := wifiadmin.DefaultConfig()
	body, _ := json.Marshal(proposed)
	doubled := string(body) + string(body)
	req := httptest.NewRequest(http.MethodPost, "/previewWifiAdminSettings", strings.NewReader(doubled))
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a multi-value JSON body", w.Code)
	}
}

func TestWifiAdminAPI_PreviewRejectsOversizedBody(t *testing.T) {
	withTestWifiAdminManager(t)
	huge := `{"ssid":"` + strings.Repeat("a", maxWifiAdminRequestBytes+1000) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/previewWifiAdminSettings", strings.NewReader(huge))
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized body", w.Code)
	}
}

func TestWifiAdminAPI_PreviewWrongMethod(t *testing.T) {
	withTestWifiAdminManager(t)
	req := httptest.NewRequest(http.MethodGet, "/previewWifiAdminSettings", nil)
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestWifiAdminAPI_ApplyMissingTokenRejected(t *testing.T) {
	withTestWifiAdminManager(t)
	req := httptest.NewRequest(http.MethodPost, "/applyWifiAdminSettings", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handleApplyWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a missing applyToken", w.Code)
	}
}

func TestWifiAdminAPI_ApplyWithUnknownTokenGone(t *testing.T) {
	withTestWifiAdminManager(t)
	body, _ := json.Marshal(applyWifiAdminRequest{ApplyToken: "does-not-exist"})
	req := httptest.NewRequest(http.MethodPost, "/applyWifiAdminSettings", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleApplyWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusGone {
		t.Errorf("status = %d, want 410 for an unknown token", w.Code)
	}
}

func TestWifiAdminAPI_ConflictWhenPreconditionBlocks(t *testing.T) {
	ensureStratuxClockForTest()
	dir := t.TempDir()
	origGood, origPending := wifiAdminLastKnownGoodPath, wifiAdminPendingPath
	wifiAdminLastKnownGoodPath = dir + "/last-known-good.json"
	wifiAdminPendingPath = dir + "/pending.json"
	defer func() {
		wifiAdminLastKnownGoodPath, wifiAdminPendingPath = origGood, origPending
	}()

	blocked := func() error { return &testExecErr{"blocked for test"} }
	exec := &fakeWifiExecutorForAPI{live: wifiadmin.DefaultConfig()}
	mgr, err := wifiadmin.NewManager("test-boot", monotonicSeconds, exec, filePersistence{}, []wifiadmin.Precondition{blocked})
	if err != nil {
		t.Fatal(err)
	}
	origMgr := wifiAdminManager
	wifiAdminManager = mgr
	defer func() { wifiAdminManager = origMgr }()

	proposed := wifiadmin.DefaultConfig()
	body, _ := json.Marshal(proposed)
	req := httptest.NewRequest(http.MethodPost, "/previewWifiAdminSettings", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 when a precondition blocks", w.Code)
	}
}

func TestWifiAdminAPI_ApplyFailureAutoRollsBackViaAPI(t *testing.T) {
	exec := withTestWifiAdminManager(t)
	exec.failSSID = "bad-ssid"

	proposed := wifiadmin.DefaultConfig()
	proposed.SSID = "bad-ssid"
	body, _ := json.Marshal(proposed)
	req := httptest.NewRequest(http.MethodPost, "/previewWifiAdminSettings", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)
	var previewResp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &previewResp)
	applyToken, _ := previewResp["applyToken"].(string)

	applyBody, _ := json.Marshal(applyWifiAdminRequest{ApplyToken: applyToken})
	req2 := httptest.NewRequest(http.MethodPost, "/applyWifiAdminSettings", bytes.NewReader(applyBody))
	w2 := httptest.NewRecorder()
	handleApplyWifiAdminSettingsRequest(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when the apply itself fails", w2.Code)
	}
	status := wifiAdminManager.Status()
	if status.Stage != wifiadmin.StageIdle {
		t.Errorf("stage after auto-rollback = %v, want idle", status.Stage)
	}
}

func TestWifiAdminAPI_CancelAndRollbackEndpoints(t *testing.T) {
	withTestWifiAdminManager(t)
	proposed := wifiadmin.DefaultConfig()
	proposed.SSID = "temp-ssid"
	body, _ := json.Marshal(proposed)
	req := httptest.NewRequest(http.MethodPost, "/previewWifiAdminSettings", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)

	req2 := httptest.NewRequest(http.MethodPost, "/cancelWifiAdminChange", nil)
	w2 := httptest.NewRecorder()
	handleCancelWifiAdminChangeRequest(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200: %s", w2.Code, w2.Body.String())
	}
	if wifiAdminManager.Status().Stage != wifiadmin.StageIdle {
		t.Error("expected idle after cancel")
	}

	req3 := httptest.NewRequest(http.MethodPost, "/rollbackWifiAdminChange", nil)
	w3 := httptest.NewRecorder()
	handleRollbackWifiAdminChangeRequest(w3, req3)
	// No pending transaction to roll back - must be a clean error, not a
	// panic or a false "success".
	if w3.Code == http.StatusOK {
		t.Error("rollback with nothing pending should not report success")
	}
}

func TestWifiAdminModeConstants_MatchExistingSettings(t *testing.T) {
	if !wifiAdminModeConstantsMatch() {
		t.Error("wifiadmin.Mode constants have drifted from the existing WifiModeAp/WifiModeDirect/WifiModeApClient constants")
	}
}

func TestWifiAdminDiagnosticsSummary_ReflectsCurrentStateWhenInitialized(t *testing.T) {
	withTestWifiAdminManager(t)
	s := wifiAdminDiagnosticsSummaryFor()
	if s.Stage != string(wifiadmin.StageIdle) {
		t.Errorf("stage = %q, want idle", s.Stage)
	}
}

func TestWifiAdminDiagnosticsSummary_ZeroValueWhenNotInitialized(t *testing.T) {
	orig := wifiAdminManager
	wifiAdminManager = nil
	defer func() { wifiAdminManager = orig }()
	s := wifiAdminDiagnosticsSummaryFor()
	if s.Stage != "" || s.LastResult != "" || s.LastError != "" {
		t.Errorf("expected a zero-value summary when wifiadmin is not initialized, got %+v", s)
	}
}

func TestToNetworkTemplateParams_DerivesConsistentFields(t *testing.T) {
	cfg := wifiadmin.DefaultConfig()
	cfg.SSID = "TestSSID"
	cfg.IPAddress = "192.168.10.1"
	params, err := toNetworkTemplateParams(cfg)
	if err != nil {
		t.Fatalf("toNetworkTemplateParams: %v", err)
	}
	if params.WiFiSSID != "TestSSID" {
		t.Errorf("SSID = %q", params.WiFiSSID)
	}
	if params.IpPrefix != "192.168.10" {
		t.Errorf("IpPrefix = %q", params.IpPrefix)
	}
	if params.DhcpRangeStart == "" || params.DhcpRangeEnd == "" {
		t.Error("expected a derived DHCP range")
	}
}

func TestToNetworkTemplateParams_RejectsUnparseableAddress(t *testing.T) {
	cfg := wifiadmin.DefaultConfig()
	cfg.IPAddress = "not-an-ip"
	if _, err := toNetworkTemplateParams(cfg); err == nil {
		t.Error("expected an error for an unparseable IP address (should never reach this point past Validate, but must fail safely if it does)")
	}
}
