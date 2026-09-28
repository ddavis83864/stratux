package main

import (
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

// --- Legacy /shutdown and /reboot endpoint safety -----------------------
//
// handleShutdownRequest/handleRebootRequest call exec.Command("systemctl",
// ...) directly with no injectable executor (unlike the confirmed flows
// in main/powerapi.go), so these tests deliberately exercise ONLY the
// blocked (precondition-failure) path - the one path that returns before
// any real system command would run. There is no safe way to test the
// unblocked path from an automated test; that remains a manual,
// physical-device verification step (see the final report).

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

func TestHandleShutdownRequest_BlockedByOTABusy(t *testing.T) {
	withTestOTABusy(t)
	withTestConfigBackupIdle(t)

	req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	w := httptest.NewRecorder()
	handleShutdownRequest(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 while an OTA update is in progress, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleShutdownRequest_BlockedByConfigBackupBusy(t *testing.T) {
	withTestOTAIdle(t)
	withTestConfigBackupBusy(t)

	req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	w := httptest.NewRecorder()
	handleShutdownRequest(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 while a configuration restore is in progress, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleRebootRequest_BlockedByOTABusy(t *testing.T) {
	withTestOTABusy(t)
	withTestConfigBackupIdle(t)

	req := httptest.NewRequest(http.MethodPost, "/reboot", nil)
	w := httptest.NewRecorder()
	handleRebootRequest(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 while an OTA update is in progress, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleRebootRequest_BlockedByConfigBackupBusy(t *testing.T) {
	withTestOTAIdle(t)
	withTestConfigBackupBusy(t)

	req := httptest.NewRequest(http.MethodPost, "/reboot", nil)
	w := httptest.NewRecorder()
	handleRebootRequest(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 while a configuration restore is in progress, got %d: %s", w.Code, w.Body.String())
	}
}
