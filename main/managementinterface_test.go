package main

import (
	"net/http/httptest"
	"os"
	"testing"
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
