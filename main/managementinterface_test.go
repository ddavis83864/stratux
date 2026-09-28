package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stratux/stratux/ota"
)

// withTestWWWDir points STRATUX_WWW_DIR at a temp directory containing a
// couple of real files, for the duration of one test - defaultServer
// delegates to http.FileServer(http.Dir(STRATUX_WWW_DIR)), so a real
// directory (not a fake) is needed to exercise it end-to-end.
func withTestWWWDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/plates/js", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/plates/js/wifiadmin.js", []byte("// fake wifiadmin.js"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/other.js", []byte("// fake other.js"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := STRATUX_WWW_DIR
	STRATUX_WWW_DIR = dir
	t.Cleanup(func() { STRATUX_WWW_DIR = orig })
	return dir
}

// TestDefaultServer_WifiAdminAssetsForceRevalidation is the regression
// test for the caching defect found during PR #20 hardware validation:
// a browser's ordinary (non-hard-refresh) navigation back to the Wi-Fi
// Admin page could silently keep running JS fetched before an OTA
// update for up to defaultServer's own general 5-minute cache window -
// long enough to span this feature's entire reconnect-confirmation
// deadline. wifiadmin.js (and its plate) must be served with a
// Cache-Control that forces revalidation instead.
func TestDefaultServer_WifiAdminAssetsForceRevalidation(t *testing.T) {
	withTestWWWDir(t)

	req := httptest.NewRequest("GET", "/plates/js/wifiadmin.js", nil)
	w := httptest.NewRecorder()
	defaultServer(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control for wifiadmin.js = %q, want %q", got, "no-cache")
	}
}

func TestDefaultServer_WifiAdminPlateForcesRevalidation(t *testing.T) {
	dir := withTestWWWDir(t) // already creates dir/plates/js
	if err := os.WriteFile(dir+"/plates/wifiadmin.html", []byte("<div></div>"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/plates/wifiadmin.html", nil)
	w := httptest.NewRecorder()
	defaultServer(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control for wifiadmin.html plate = %q, want %q", got, "no-cache")
	}
}

// TestDefaultServer_OtherAssetsKeepGeneralCache proves this fix is
// narrowly scoped - every other file under STRATUX_WWW_DIR must keep the
// existing, deliberate 5-minute cache (see defaultServer's own comment);
// this is not a blanket cache-control change for the whole dashboard.
func TestDefaultServer_OtherAssetsKeepGeneralCache(t *testing.T) {
	withTestWWWDir(t)

	req := httptest.NewRequest("GET", "/other.js", nil)
	w := httptest.NewRecorder()
	defaultServer(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "max-age=360" {
		t.Errorf("Cache-Control for an unrelated asset = %q, want unchanged %q", got, "max-age=360")
	}
}

// --- Legacy /shutdown and /reboot endpoint retirement --------------------
//
// handleShutdownRequest/handleRebootRequest used to call exec.Command
// ("systemctl", ...) directly - first unconditionally, then (a prior,
// still-insufficient fix) gated only by the OTA/restore-busy
// preconditions. Both are retired now: neither endpoint performs any
// action anymore, under any precondition state - a single POST can never
// power the device off or reboot it, confirmed/token or not. These tests
// prove that invariance directly: the response is identical (410, no
// action) whether OTA/config-restore is busy or idle, because the
// handler no longer even inspects that state - there is no remaining
// conditional path that could ever fall through to a real systemctl
// command from either endpoint.

// withTestOTABusy points otaDir at a temp dir holding a real, non-idle,
// non-terminal OTA state - the same shape otaNotBusyPrecondition reads -
// so it reports "an OTA update is in progress", restoring the original
// otaDir afterward.
func withTestOTABusy(t *testing.T) {
	t.Helper()
	orig := otaDir
	otaDir = t.TempDir()
	t.Cleanup(func() { otaDir = orig })
	if err := ota.SaveState(otaDir, ota.State{Stage: ota.StageInstalling}, time.Now()); err != nil {
		t.Fatalf("ota.SaveState: %v", err)
	}
}

// withTestOTAIdle points otaDir at an empty temp dir - ota.LoadState
// reports StageIdle for a directory with no state file at all, so
// otaNotBusyPrecondition passes.
func withTestOTAIdle(t *testing.T) {
	t.Helper()
	orig := otaDir
	otaDir = filepath.Join(t.TempDir(), "updates")
	t.Cleanup(func() { otaDir = orig })
}

func withTestConfigBackupBusy(t *testing.T) {
	t.Helper()
	orig := configBackupState
	configBackupState = restoreStateApplying
	t.Cleanup(func() { configBackupState = orig })
}

func withTestConfigBackupIdle(t *testing.T) {
	t.Helper()
	orig := configBackupState
	configBackupState = restoreStateIdle
	t.Cleanup(func() { configBackupState = orig })
}

// legacyRouteResponse posts to a legacy handler and decodes its JSON body.
func legacyRouteResponse(t *testing.T, path string, handler http.HandlerFunc) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	w := httptest.NewRecorder()
	handler(w, req)
	var body map[string]interface{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: response was not valid JSON: %v (body: %s)", path, err, w.Body.String())
		}
	}
	return w.Code, body
}

func assertLegacyRouteNeverActs(t *testing.T, path string, handler http.HandlerFunc) {
	t.Helper()
	code, body := legacyRouteResponse(t, path, handler)
	if code != http.StatusGone {
		t.Errorf("%s: expected 410 Gone, got %d (body: %v)", path, code, body)
	}
	if body["success"] != false {
		t.Errorf("%s: expected success:false, got %v", path, body)
	}
	if _, hasError := body["error"]; !hasError {
		t.Errorf("%s: expected a non-empty error explaining the retirement, got %v", path, body)
	}
}

func TestHandleShutdownRequest_NeverActs_RegardlessOfPreconditionState(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T)
	}{
		{"OTA busy", func(t *testing.T) { withTestOTABusy(t); withTestConfigBackupIdle(t) }},
		{"config restore busy", func(t *testing.T) { withTestOTAIdle(t); withTestConfigBackupBusy(t) }},
		{"idle", func(t *testing.T) { withTestOTAIdle(t); withTestConfigBackupIdle(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.set(t)
			assertLegacyRouteNeverActs(t, "/shutdown", handleShutdownRequest)
		})
	}
}

func TestHandleRebootRequest_NeverActs_RegardlessOfPreconditionState(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T)
	}{
		{"OTA busy", func(t *testing.T) { withTestOTABusy(t); withTestConfigBackupIdle(t) }},
		{"config restore busy", func(t *testing.T) { withTestOTAIdle(t); withTestConfigBackupBusy(t) }},
		{"idle", func(t *testing.T) { withTestOTAIdle(t); withTestConfigBackupIdle(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.set(t)
			assertLegacyRouteNeverActs(t, "/reboot", handleRebootRequest)
		})
	}
}

// TestHandleShutdownAndRebootRequest_WrongMethodAndAnyMethod_NeverAct proves
// the retirement response is unconditional on method too - not just that
// GET/POST are both harmless, but that there is no code path in either
// handler that could ever reach a real systemctl command.
func TestHandleShutdownAndRebootRequest_AnyMethodNeverActs(t *testing.T) {
	withTestOTAIdle(t)
	withTestConfigBackupIdle(t)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/shutdown", nil)
		w := httptest.NewRecorder()
		handleShutdownRequest(w, req)
		if w.Code != http.StatusGone {
			t.Errorf("/shutdown %s: expected 410, got %d", method, w.Code)
		}

		req = httptest.NewRequest(method, "/reboot", nil)
		w = httptest.NewRecorder()
		handleRebootRequest(w, req)
		if w.Code != http.StatusGone {
			t.Errorf("/reboot %s: expected 410, got %d", method, w.Code)
		}
	}
}
