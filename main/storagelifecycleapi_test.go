package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/storagelifecycle"
)

// withTestStorageManager installs a Manager backed by a temp directory
// for the duration of one test, and restores the original afterward.
func withTestStorageManager(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "diagnostics"), 0o755))
	must(t, os.MkdirAll(filepath.Join(dir, "recordings"), 0o755))

	registry := storagelifecycle.NewRegistry()
	must(t, registry.Register(storagelifecycle.Namespace{
		ID: "diagnostics", Root: filepath.Join(dir, "diagnostics"),
		Criticality: storagelifecycle.CriticalityBounded, ItemKind: storagelifecycle.ItemKindFile,
		AllowedExtensions: []string{".json"},
	}))
	must(t, registry.Register(storagelifecycle.Namespace{
		ID: "recordings", Root: filepath.Join(dir, "recordings"),
		Criticality: storagelifecycle.CriticalityImportant, ItemKind: storagelifecycle.ItemKindDirectory,
	}))

	ensureStratuxClockForTest()
	orig := storageManager
	storageManager = storagelifecycle.NewManager(storagelifecycle.ManagerConfig{
		Registry:             registry,
		Policy:               storagelifecycle.Policy{RequiredConsecutivePressureSamples: 1},
		FS:                   storagelifecycle.NewOSFS(),
		Clock:                monotonicSeconds,
		FilesystemThresholds: readiness.DefaultPersistentStorageThresholds(),
	})
	t.Cleanup(func() { storageManager = orig })
	return dir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandleGetStorageLifecycleRequest_BeforeAnyScan(t *testing.T) {
	withTestStorageManager(t)
	req := httptest.NewRequest(http.MethodGet, "/getStorageLifecycle", nil)
	w := httptest.NewRecorder()
	handleGetStorageLifecycleRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	must(t, json.Unmarshal(w.Body.Bytes(), &body))
	if body["hasInventory"] != false {
		t.Errorf("expected hasInventory=false before any scan, got %v", body["hasInventory"])
	}
	notes, _ := body["notes"].([]interface{})
	if len(notes) == 0 {
		t.Error("expected non-empty notes explaining automatic cleanup is not enabled")
	}
}

func TestHandleGetStorageLifecycleRequest_AfterScan(t *testing.T) {
	dir := withTestStorageManager(t)
	must(t, os.WriteFile(filepath.Join(dir, "diagnostics", "diag-1.json"), []byte("{}"), 0o644))
	storageManager.Scan()

	req := httptest.NewRequest(http.MethodGet, "/getStorageLifecycle", nil)
	w := httptest.NewRecorder()
	handleGetStorageLifecycleRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	must(t, json.Unmarshal(w.Body.Bytes(), &body))
	if body["hasInventory"] != true {
		t.Fatalf("expected hasInventory=true after a scan, got %v", body["hasInventory"])
	}
	ns, ok := body["namespaces"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a namespaces object, got %T", body["namespaces"])
	}
	diag, ok := ns["diagnostics"].(map[string]interface{})
	if !ok || diag["managedCount"] != float64(1) {
		t.Errorf("expected diagnostics managedCount=1, got %+v", diag)
	}
	if body["enforcementEnabled"] != false {
		t.Error("enforcementEnabled must always be false in this release")
	}
}

func TestHandleGetStorageLifecycleRequest_WrongMethodRejected(t *testing.T) {
	withTestStorageManager(t)
	req := httptest.NewRequest(http.MethodPost, "/getStorageLifecycle", nil)
	w := httptest.NewRecorder()
	handleGetStorageLifecycleRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleGetStorageLifecycleRequest_UninitializedManager(t *testing.T) {
	orig := storageManager
	storageManager = nil
	defer func() { storageManager = orig }()

	req := httptest.NewRequest(http.MethodGet, "/getStorageLifecycle", nil)
	w := httptest.NewRecorder()
	handleGetStorageLifecycleRequest(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when uninitialized, got %d", w.Code)
	}
}

func TestStorageLifecycleActiveChecker_OnlyMatchesRecordingsNamespace(t *testing.T) {
	origCurrent := recCurrent
	defer func() { recCurrent = origCurrent }()
	recMu.Lock()
	recCurrent = &recordingSession{ID: "rec-active", State: recordingStateActive}
	recMu.Unlock()

	if !storageLifecycleActiveChecker("recordings", "rec-active") {
		t.Error("expected the active recording to be reported active")
	}
	if storageLifecycleActiveChecker("diagnostics", "rec-active") {
		t.Error("expected the active checker to only ever match the recordings namespace")
	}
	if storageLifecycleActiveChecker("recordings", "rec-other") {
		t.Error("expected a non-matching recording name to report false")
	}
}

func TestStorageLifecycleActiveChecker_NoCurrentRecording(t *testing.T) {
	origCurrent := recCurrent
	defer func() { recCurrent = origCurrent }()
	recMu.Lock()
	recCurrent = nil
	recMu.Unlock()

	if storageLifecycleActiveChecker("recordings", "anything") {
		t.Error("expected false when no recording is current")
	}
}

func TestStorageLifecycleFilesystemPressure_ReflectsGlobalHealth(t *testing.T) {
	origHealth := globalHealth
	defer func() { globalHealth = origHealth }()

	globalHealthMutex.Lock()
	globalHealth.Storage.Present = true
	globalHealth.Storage.Mounted = true
	globalHealth.Storage.UtilizationPercent = 42.5
	globalHealthMutex.Unlock()

	pct, ok := storageLifecycleFilesystemPressure()
	if !ok || pct != 42.5 {
		t.Errorf("expected (42.5, true), got (%v, %v)", pct, ok)
	}
}

func TestStorageLifecycleFilesystemPressure_NotPresentReportsNotOK(t *testing.T) {
	origHealth := globalHealth
	defer func() { globalHealth = origHealth }()

	globalHealthMutex.Lock()
	globalHealth.Storage.Present = false
	globalHealthMutex.Unlock()

	_, ok := storageLifecycleFilesystemPressure()
	if ok {
		t.Error("expected ok=false when storage is not present")
	}
}

func TestStorageLifecycleDiagnosticsSummary_NeverNilMapWhenInitialized(t *testing.T) {
	withTestStorageManager(t)
	storageManager.Scan()
	summary := storageLifecycleDiagnosticsSummary()
	m, ok := summary.(map[string]interface{})
	if !ok {
		t.Fatalf("expected a map, got %T", summary)
	}
	if _, present := m["hasInventory"]; !present {
		t.Error("expected a hasInventory field")
	}
}

func TestBuildStorageLifecycleHealth_UsesManagerStatus(t *testing.T) {
	withTestStorageManager(t)
	storageManager.Scan()
	h := buildStorageLifecycleHealth()
	if !h.HasInventory {
		t.Error("expected HasInventory=true after a scan")
	}
	if h.State != readiness.StateReady {
		t.Errorf("expected READY for a fresh, empty, unpressured inventory, got %s (%s)", h.State, h.Reason)
	}
}

func TestBuildStorageLifecycleHealth_NilManagerReportsUnknown(t *testing.T) {
	orig := storageManager
	storageManager = nil
	defer func() { storageManager = orig }()
	h := buildStorageLifecycleHealth()
	if h.State != readiness.StateUnknown {
		t.Errorf("expected UNKNOWN when uninitialized, got %s", h.State)
	}
}
