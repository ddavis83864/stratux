package readiness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeSettings_RemovesKnownSensitiveKeys(t *testing.T) {
	raw := map[string]interface{}{
		"WiFiSSID":       "Stratux",
		"WiFiPassphrase": "supersecret",
		"DEBUG":          false,
		"OwnshipModeS":   "F00000",
		"SomeAPIToken":   "abc123",
		"AuthorizedKeys": "ssh-ed25519 ...",
	}
	got := SanitizeSettings(raw)
	for _, sensitive := range []string{"WiFiPassphrase", "SomeAPIToken", "AuthorizedKeys"} {
		if _, present := got[sensitive]; present {
			t.Errorf("sanitized output must not contain %q", sensitive)
		}
	}
	for _, safe := range []string{"WiFiSSID", "DEBUG", "OwnshipModeS"} {
		if _, present := got[safe]; !present {
			t.Errorf("sanitized output should retain non-sensitive field %q", safe)
		}
	}
}

func TestSanitizeSettings_RecursesIntoNestedStructures(t *testing.T) {
	raw := map[string]interface{}{
		"WiFiClientNetworks": []interface{}{
			map[string]interface{}{
				"Ssid":     "SomeNetwork",
				"Password": "hunter2",
			},
		},
	}
	got := SanitizeSettings(raw)
	networks, ok := got["WiFiClientNetworks"].([]interface{})
	if !ok || len(networks) != 1 {
		t.Fatalf("expected WiFiClientNetworks to survive sanitization as a 1-element slice, got %#v", got["WiFiClientNetworks"])
	}
	network, ok := networks[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected nested network entry to remain a map, got %#v", networks[0])
	}
	if _, present := network["Password"]; present {
		t.Error("nested Password field must be removed, not just top-level ones")
	}
	if _, present := network["Ssid"]; !present {
		t.Error("nested non-sensitive field should survive")
	}
}

func TestSanitizeSettings_DoesNotMutateInput(t *testing.T) {
	raw := map[string]interface{}{"WiFiPassphrase": "secret"}
	_ = SanitizeSettings(raw)
	if _, present := raw["WiFiPassphrase"]; !present {
		t.Error("SanitizeSettings must not mutate its input map")
	}
}

func TestBuildDiagnosticBundle_TruncatesLogLines(t *testing.T) {
	lines := make([]string, maxDiagnosticLogLines+50)
	for i := range lines {
		lines[i] = "log line"
	}
	b := BuildDiagnosticBundle(time.Now(), "2.0-pre5", "abc123", HealthReport{}, nil, lines, nil, "")
	if len(b.RecentLogLines) != maxDiagnosticLogLines {
		t.Errorf("expected log lines truncated to %d, got %d", maxDiagnosticLogLines, len(b.RecentLogLines))
	}
}

func TestWriteDiagnosticBundle_WritesReadableJSON(t *testing.T) {
	dir := t.TempDir()
	b := BuildDiagnosticBundle(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC), "2.0-pre5", "abc123", HealthReport{Overall: StateReady}, map[string]interface{}{"DEBUG": false}, nil, nil, "")
	path, err := WriteDiagnosticBundle(dir, b, 10)
	if err != nil {
		t.Fatalf("WriteDiagnosticBundle error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read written bundle: %v", err)
	}
	var roundTrip DiagnosticBundle
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("written bundle is not valid JSON: %v", err)
	}
	if roundTrip.Version != "2.0-pre5" {
		t.Errorf("round-tripped Version = %q, want 2.0-pre5", roundTrip.Version)
	}
	if !strings.Contains(filepath.Base(path), "20260601") {
		t.Errorf("bundle filename %q should embed its generation date", path)
	}
}

func TestWriteDiagnosticBundle_PrunesOldBundles(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		b := BuildDiagnosticBundle(time.Date(2026, 6, 1, 12, i, 0, 0, time.UTC), "v", "c", HealthReport{}, nil, nil, nil, "")
		if _, err := WriteDiagnosticBundle(dir, b, 3); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("expected retention to prune down to 3 bundles, found %d", len(entries))
	}
	// The three that remain should be the three most recent (0m2, 0m3, 0m4).
	for _, e := range entries {
		if strings.Contains(e.Name(), "120000") || strings.Contains(e.Name(), "120100") {
			t.Errorf("pruning kept an old bundle it should have removed: %s", e.Name())
		}
	}
}

func TestWriteDiagnosticBundle_RetentionZeroMeansNoPruning(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		b := BuildDiagnosticBundle(time.Date(2026, 6, 1, 12, i, 0, 0, time.UTC), "v", "c", HealthReport{}, nil, nil, nil, "")
		if _, err := WriteDiagnosticBundle(dir, b, 0); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 5 {
		t.Errorf("maxRetain=0 should disable pruning, got %d files", len(entries))
	}
}

func TestWriteDiagnosticBundle_FailureDoesNotPanic(t *testing.T) {
	// A path that cannot be created (parent is actually a file, not a
	// directory) must return an error, not panic - "a failed diagnostic
	// write must not disrupt ADS-B/GDL90 operation".
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	badDir := filepath.Join(blocker, "diagnostics")
	b := BuildDiagnosticBundle(time.Now(), "v", "c", HealthReport{}, nil, nil, nil, "")
	if _, err := WriteDiagnosticBundle(badDir, b, 10); err == nil {
		t.Error("expected an error writing under a non-directory path, got nil")
	}
}
