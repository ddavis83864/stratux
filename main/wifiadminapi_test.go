package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratux/stratux/wifiadmin"
)

// fakeWifiExecutorForAPI is main package's own fake Executor - never
// touches a real network interface. Used only by this test file.
type fakeWifiExecutorForAPI struct {
	live       wifiadmin.Config
	failSSID   string
	applyErrs  int
	applyCalls int
}

func (f *fakeWifiExecutorForAPI) Apply(cfg wifiadmin.Config) error {
	f.applyCalls++
	if f.failSSID != "" && cfg.SSID == f.failSSID {
		f.applyErrs++
		return errTestExecutorFailure
	}
	f.live = cfg
	return nil
}

// applyCount reports how many times Apply has been called - used by
// tests that must prove a code path never touches the executor at all
// (e.g. a clean restart with nothing pending), not merely that it left
// no visible side effect.
func (f *fakeWifiExecutorForAPI) applyCount() int { return f.applyCalls }

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
	// httptest.NewRequest does not go through the real server's
	// ConnContext hook (main/managementinterface.go) - simulate what it
	// would have populated: the request delivered to the newly-applied
	// AP's own configured address, from a client address in that same
	// /24, exactly as a real reconnecting client would look.
	req3 = req3.WithContext(context.WithValue(req3.Context(), connLocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(proposed.IPAddress), Port: 80}))
	req3.RemoteAddr = "192.168.10.77:54321"
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

// previewAndApplyForConfirmTest is a shared helper for the HTTP-level
// path-confirmation tests below - runs preview+apply and returns the
// reconnect token, without confirming.
func previewAndApplyForConfirmTest(t *testing.T, ssid string) (reconnectToken string, proposed wifiadmin.Config) {
	t.Helper()
	proposed = wifiadmin.DefaultConfig()
	proposed.SSID = ssid
	body, _ := json.Marshal(proposed)
	req := httptest.NewRequest(http.MethodPost, "/previewWifiAdminSettings", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handlePreviewWifiAdminSettingsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("preview status = %d: %s", w.Code, w.Body.String())
	}
	var previewResp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &previewResp)
	applyToken, _ := previewResp["applyToken"].(string)

	applyBody, _ := json.Marshal(applyWifiAdminRequest{ApplyToken: applyToken})
	req2 := httptest.NewRequest(http.MethodPost, "/applyWifiAdminSettings", bytes.NewReader(applyBody))
	w2 := httptest.NewRecorder()
	handleApplyWifiAdminSettingsRequest(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("apply status = %d: %s", w2.Code, w2.Body.String())
	}
	var applyResp map[string]interface{}
	json.Unmarshal(w2.Body.Bytes(), &applyResp)
	reconnectToken, _ = applyResp["reconnectToken"].(string)
	if reconnectToken == "" {
		t.Fatal("expected a non-empty reconnectToken")
	}
	return reconnectToken, proposed
}

func confirmRequestWithPath(reconnectToken, localAddr, remoteAddr string) *http.Request {
	body, _ := json.Marshal(confirmWifiAdminReconnectionRequest{ReconnectToken: reconnectToken})
	req := httptest.NewRequest(http.MethodPost, "/confirmWifiAdminReconnection", bytes.NewReader(body))
	if localAddr != "" {
		host, portStr, _ := net.SplitHostPort(localAddr)
		port := 0
		fmt.Sscanf(portStr, "%d", &port)
		req = req.WithContext(context.WithValue(req.Context(), connLocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(host), Port: port}))
	}
	req.RemoteAddr = remoteAddr
	return req
}

// TestWifiAdminAPI_ConfirmReconnection_RejectsUnrelatedNetworkPath is the
// HTTP-level proof that a confirmation cannot succeed merely because the
// daemon is reachable - it must arrive via the newly-applied AP's own
// address/subnet. Covers the specific unrelated-path scenarios named in
// this mission: loopback, an Ethernet-shaped address, and a client-mode-
// Wi-Fi-shaped address, all distinct from the wifiadmin package's own
// (more exhaustive) unit tests of the same property.
func TestWifiAdminAPI_ConfirmReconnection_RejectsUnrelatedNetworkPath(t *testing.T) {
	cases := []struct {
		name      string
		localAddr string
	}{
		{"loopback", "127.0.0.1:80"},
		{"ethernet-shaped", "10.0.0.5:80"},
		{"client-mode-wifi-shaped", "192.168.1.50:80"},
		{"connctext-missing", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTestWifiAdminManager(t)
			reconnectToken, _ := previewAndApplyForConfirmTest(t, "path-test-ssid")
			req := confirmRequestWithPath(reconnectToken, tc.localAddr, "192.168.10.77:54321")
			w := httptest.NewRecorder()
			handleConfirmWifiAdminReconnectionRequest(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403: %s", w.Code, w.Body.String())
			}
			if wifiAdminManager.Status().Stage != wifiadmin.StageAwaitingReconnection {
				t.Error("a rejected-path confirmation must not disturb the pending transaction")
			}
		})
	}
}

func TestWifiAdminAPI_ConfirmReconnection_RejectsRemoteFromWrongSubnet(t *testing.T) {
	withTestWifiAdminManager(t)
	reconnectToken, proposed := previewAndApplyForConfirmTest(t, "path-test-ssid")
	// Correct local (AP's own) address, but a client address from an
	// entirely different subnet.
	req := confirmRequestWithPath(reconnectToken, proposed.IPAddress+":80", "203.0.113.5:54321")
	w := httptest.NewRecorder()
	handleConfirmWifiAdminReconnectionRequest(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
}

func TestWifiAdminAPI_ConfirmReconnection_ValidPathSucceeds(t *testing.T) {
	withTestWifiAdminManager(t)
	reconnectToken, proposed := previewAndApplyForConfirmTest(t, "path-test-ssid")
	req := confirmRequestWithPath(reconnectToken, proposed.IPAddress+":80", "192.168.10.77:54321")
	w := httptest.NewRecorder()
	handleConfirmWifiAdminReconnectionRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if wifiAdminManager.Status().Stage != wifiadmin.StageIdle {
		t.Error("expected idle after a valid-path confirmation")
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

// TestWifiAdminDefaultConfig_MatchesExistingDefaultSettings is a
// regression test for a real defect found during this mission's own
// artifact verification: wifiadmin.DefaultConfig() originally used SSID
// "stratux" (lowercase), while main/gen_gdl90.go's defaultSettings()
// (line ~1472: `globalSettings.WiFiSSID = "Stratux"`) and
// main/networksettings.go's own applyNetworkSettings fallback both use
// "Stratux" (capital S) - a genuine default-compatibility mismatch this
// feature's own design explicitly promises never to introduce (see
// wifiadmin.DefaultConfig's own doc comment). Caught by inspecting the
// CI-built artifact's own embedded strings, not by a pre-existing test -
// this test exists so it cannot regress silently again.
func TestWifiAdminDefaultConfig_MatchesExistingDefaultSettings(t *testing.T) {
	if got, want := wifiadmin.DefaultConfig().SSID, "Stratux"; got != want {
		t.Errorf("wifiadmin.DefaultConfig().SSID = %q, want %q (must match main/gen_gdl90.go's defaultSettings() exactly)", got, want)
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

// --- wifiAdminTransactionActive: the reverse-direction guard main/ota.go
// uses so a fresh OTA install waits for an in-flight Wi-Fi transaction
// rather than rebooting out from under it (see main/ota.go's otaAdvance,
// ActionRequestDisable). ---

func TestWifiAdminTransactionActive_NilManagerIsNotActive(t *testing.T) {
	orig := wifiAdminManager
	wifiAdminManager = nil
	defer func() { wifiAdminManager = orig }()

	if wifiAdminTransactionActive() {
		t.Error("a nil (uninitialized) manager must never be reported as an active transaction")
	}
}

func TestWifiAdminTransactionActive_IdleIsNotActive(t *testing.T) {
	withTestWifiAdminManager(t)
	if wifiAdminTransactionActive() {
		t.Error("an idle manager must not be reported as an active transaction")
	}
}

func TestWifiAdminTransactionActive_PreviewedIsNotActive(t *testing.T) {
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
	// Nothing live has been touched yet at StagePreviewed - OTA should
	// not need to wait for a preview nobody has applied.
	if wifiAdminTransactionActive() {
		t.Error("a merely-previewed (not yet applied) transaction must not be reported as active")
	}
}

func TestWifiAdminTransactionActive_AwaitingReconnectionIsActive(t *testing.T) {
	withTestWifiAdminManager(t)
	previewAndApplyForConfirmTest(t, "new-plane-ssid")
	if wifiAdminManager.Status().Stage != wifiadmin.StageAwaitingReconnection {
		t.Fatalf("setup: expected StageAwaitingReconnection, got %s", wifiAdminManager.Status().Stage)
	}

	if !wifiAdminTransactionActive() {
		t.Error("a transaction awaiting reconnection confirmation must be reported as active - this is exactly the window OTA must not reboot through")
	}
}

func TestWifiAdminTransactionActive_ConfirmedIsNotActive(t *testing.T) {
	withTestWifiAdminManager(t)
	reconnectToken, proposed := previewAndApplyForConfirmTest(t, "new-plane-ssid")
	req := confirmRequestWithPath(reconnectToken, proposed.IPAddress+":80", "192.168.10.77:54321")
	w := httptest.NewRecorder()
	handleConfirmWifiAdminReconnectionRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, want 200: %s", w.Code, w.Body.String())
	}

	if wifiAdminTransactionActive() {
		t.Error("a confirmed (settled) transaction must not be reported as active once more")
	}
}
