package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/storagelifecycle"
)

// withFISBCacheTestEnv wires a temp settings file, a temp cache-namespace
// directory (so a test that persists an entry never touches the real
// persistent-data partition), and a fresh, minimal in-memory Store for the
// duration of one test - mirrors withConfigBackupTestEnv's own
// scoped-redirect pattern. It never calls initFISBCache itself (that
// starts real goroutines); handler-level tests only need the same
// package-level state initFISBCache would have set up.
func withFISBCacheTestEnv(t *testing.T) {
	t.Helper()
	withTestFISBCacheSettingsPath(t)
	ensureStratuxClockForTest()

	origStore := fisbCacheStore
	origSettings := fisbCacheSettingsCache
	origQueue := fisbCacheQueue
	origRecovered := fisbCacheStartupRecovered
	origRecoveryErr := fisbCacheRecoveryError
	origNonFatal := fisbCacheNonFatalErrors
	origShuttingDown := fisbCacheShuttingDown
	origLastCleanupUTC := fisbCacheLastCleanupUTC
	origLastCleanupCount := fisbCacheLastCleanupCount
	origDir := fisbCacheDir
	origNamespace := fisbCacheNamespace
	origFS := fisbCacheFS
	origWriter := fisbCacheAtomicWriter

	tmpDir := filepath.Join(t.TempDir(), "fisb-weather-cache")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatalf("could not create temp cache directory: %v", err)
	}

	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = DefaultFISBCacheSettings()
	fisbCacheQueue = make(chan fisbCaptureItem, fisbCaptureQueueDepth)
	fisbCacheStartupRecovered = true
	fisbCacheRecoveryError = false
	fisbCacheNonFatalErrors = false
	fisbCacheShuttingDown = false
	fisbCacheDir = tmpDir
	fisbCacheNamespace.Root = tmpDir
	fisbCacheFS = storagelifecycle.NewOSFS()
	fisbCacheAtomicWriter = storagelifecycle.NewAtomicWriter(fisbCacheFS)
	fisbCacheMu.Unlock()

	t.Cleanup(func() {
		fisbCacheMu.Lock()
		fisbCacheStore = origStore
		fisbCacheSettingsCache = origSettings
		fisbCacheQueue = origQueue
		fisbCacheStartupRecovered = origRecovered
		fisbCacheRecoveryError = origRecoveryErr
		fisbCacheNonFatalErrors = origNonFatal
		fisbCacheShuttingDown = origShuttingDown
		fisbCacheLastCleanupUTC = origLastCleanupUTC
		fisbCacheLastCleanupCount = origLastCleanupCount
		fisbCacheDir = origDir
		fisbCacheNamespace = origNamespace
		fisbCacheFS = origFS
		fisbCacheAtomicWriter = origWriter
		fisbCacheMu.Unlock()
	})
}

func decodeFISBJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("could not decode response body %q: %v", rec.Body.String(), err)
	}
	return m
}

// --- status -----------------------------------------------------------

func TestHandleGetFISBCacheStatus_DefaultDisabled(t *testing.T) {
	withFISBCacheTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/getFISBCacheStatus", nil)
	rec := httptest.NewRecorder()
	handleGetFISBCacheStatusRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeFISBJSONBody(t, rec)
	if resp["state"] != "DISABLED" {
		t.Errorf("expected DISABLED state by default, got %+v", resp["state"])
	}
	if resp["enabled"] != false {
		t.Errorf("expected disabled by default, got %+v", resp["enabled"])
	}
	if resp["totalEntries"] != float64(0) {
		t.Errorf("expected zero entries on a fresh store, got %+v", resp["totalEntries"])
	}
	notes, ok := resp["notes"].([]interface{})
	if !ok || len(notes) == 0 {
		t.Error("expected a non-empty disclaimer notes list")
	}
}

func TestHandleGetFISBCacheStatus_WrongMethod(t *testing.T) {
	withFISBCacheTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/getFISBCacheStatus", nil)
	rec := httptest.NewRecorder()
	handleGetFISBCacheStatusRequest(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestHandleGetFISBCacheStatus_ReflectsAdmittedEntry(t *testing.T) {
	withFISBCacheTestEnv(t)
	fisbCacheStore.Admit(fisbcache.Entry{
		Key:                 fisbcache.TextKey(fisbcache.TextProductMETAR, "KSEA"),
		ReceivedAtMonotonic: monotonicSeconds(),
		SizeBytes:           42,
	})

	req := httptest.NewRequest(http.MethodGet, "/getFISBCacheStatus", nil)
	rec := httptest.NewRecorder()
	handleGetFISBCacheStatusRequest(rec, req)
	resp := decodeFISBJSONBody(t, rec)
	if resp["totalEntries"] != float64(1) {
		t.Errorf("expected one entry reflected in status, got %+v", resp["totalEntries"])
	}
	if resp["totalBytes"] != float64(42) {
		t.Errorf("expected the admitted entry's size reflected, got %+v", resp["totalBytes"])
	}
}

// --- inventory ----------------------------------------------------------

func TestHandleGetFISBCacheInventory_EmptyByDefault(t *testing.T) {
	withFISBCacheTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/getFISBCacheInventory", nil)
	rec := httptest.NewRecorder()
	handleGetFISBCacheInventoryRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var items []fisbCacheInventoryItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("could not decode inventory: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected an empty inventory on a fresh store, got %+v", items)
	}
}

func TestHandleGetFISBCacheInventory_NeverIncludesRawPayload(t *testing.T) {
	withFISBCacheTestEnv(t)
	const secretPayload = "METAR KSEA 061853Z SENSITIVE-PAYLOAD-MARKER"
	fisbCacheStore.Admit(fisbcache.Entry{
		Key:                 fisbcache.TextKey(fisbcache.TextProductMETAR, "KSEA"),
		ReceivedAtMonotonic: monotonicSeconds(),
		SizeBytes:           int64(len(secretPayload)),
	})

	req := httptest.NewRequest(http.MethodGet, "/getFISBCacheInventory", nil)
	rec := httptest.NewRecorder()
	handleGetFISBCacheInventoryRequest(rec, req)
	if bytes.Contains(rec.Body.Bytes(), []byte("SENSITIVE-PAYLOAD-MARKER")) {
		t.Fatalf("inventory must never include raw payload content, got: %s", rec.Body.String())
	}

	var items []fisbCacheInventoryItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("could not decode inventory: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected exactly one inventory item, got %d", len(items))
	}
	if items[0].ProductClass != string(fisbcache.ClassText) {
		t.Errorf("unexpected productClass %q", items[0].ProductClass)
	}
	if items[0].Identity != "METAR KSEA" {
		t.Errorf("unexpected identity %q", items[0].Identity)
	}
}

func TestHandleGetFISBCacheInventory_WrongMethod(t *testing.T) {
	withFISBCacheTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/getFISBCacheInventory", nil)
	rec := httptest.NewRecorder()
	handleGetFISBCacheInventoryRequest(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

// --- settings -----------------------------------------------------------

func TestHandleGetFISBCacheSettings_DefaultDisabled(t *testing.T) {
	withFISBCacheTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/getFISBCacheSettings", nil)
	rec := httptest.NewRecorder()
	handleGetFISBCacheSettingsRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var s FISBCacheSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Enabled {
		t.Error("expected disabled by default")
	}
}

func TestHandleSetFISBCacheSettings_ValidUpdateAppliesAndPersists(t *testing.T) {
	withFISBCacheTestEnv(t)
	body, _ := json.Marshal(FISBCacheSettings{
		Enabled:            true,
		PersistenceEnabled: true,
		MaxCacheBytes:      8 * 1024 * 1024,
		MaxEntries:         500,
	})
	req := httptest.NewRequest(http.MethodPost, "/setFISBCacheSettings", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSetFISBCacheSettingsRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	loaded := loadFISBCacheSettings()
	if !loaded.Enabled || !loaded.PersistenceEnabled || loaded.MaxCacheBytes != 8*1024*1024 || loaded.MaxEntries != 500 {
		t.Errorf("expected the new settings persisted, got %+v", loaded)
	}

	fisbCacheMu.Lock()
	cached := fisbCacheSettingsCache
	fisbCacheMu.Unlock()
	if !cached.Enabled {
		t.Error("expected the in-memory settings cache updated immediately")
	}
}

func TestHandleSetFISBCacheSettings_ReplayEnabledRejected(t *testing.T) {
	withFISBCacheTestEnv(t)
	body, _ := json.Marshal(FISBCacheSettings{
		Enabled:       true,
		ReplayEnabled: true,
		MaxCacheBytes: 1024,
		MaxEntries:    10,
	})
	req := httptest.NewRequest(http.MethodPost, "/setFISBCacheSettings", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSetFISBCacheSettingsRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for replayEnabled=true, got %d: %s", rec.Code, rec.Body.String())
	}
	if loadFISBCacheSettings().Enabled {
		t.Error("a rejected update must never have been persisted")
	}
}

func TestHandleSetFISBCacheSettings_OutOfBoundsRejected(t *testing.T) {
	withFISBCacheTestEnv(t)
	body, _ := json.Marshal(FISBCacheSettings{Enabled: true, MaxCacheBytes: 0, MaxEntries: 10})
	req := httptest.NewRequest(http.MethodPost, "/setFISBCacheSettings", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSetFISBCacheSettingsRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-positive maxCacheBytes, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSetFISBCacheSettings_UnknownFieldRejected(t *testing.T) {
	withFISBCacheTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/setFISBCacheSettings", bytes.NewReader([]byte(`{"enabled":true,"bogusField":1}`)))
	rec := httptest.NewRecorder()
	handleSetFISBCacheSettingsRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown field, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSetFISBCacheSettings_MultipleJSONValuesRejected(t *testing.T) {
	withFISBCacheTestEnv(t)
	// A second, concatenated JSON object after an otherwise-valid one -
	// json.Decoder.Decode by itself only consumes the first value and
	// silently ignores the rest; this must not be treated as success.
	body := []byte(`{"enabled":true,"maxCacheBytes":1024,"maxEntries":10}{"enabled":false}`)
	req := httptest.NewRequest(http.MethodPost, "/setFISBCacheSettings", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSetFISBCacheSettingsRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a body containing more than one JSON value, got %d: %s", rec.Code, rec.Body.String())
	}
	if loadFISBCacheSettings().Enabled {
		t.Error("a rejected multi-value body must never have been persisted")
	}
}

func TestHandleConfirmFISBCachePurge_MultipleJSONValuesRejected(t *testing.T) {
	withFISBCacheTestEnv(t)
	withFISBCachePurgeStateReset(t)
	body := []byte(`{"token":"fisbpurge-whatever"}{"token":"fisbpurge-second"}`)
	req := httptest.NewRequest(http.MethodPost, "/confirmFISBCachePurge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleConfirmFISBCachePurgeRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a body containing more than one JSON value, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSetFISBCacheSettings_WrongMethod(t *testing.T) {
	withFISBCacheTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/setFISBCacheSettings", nil)
	rec := httptest.NewRecorder()
	handleSetFISBCacheSettingsRequest(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

// --- purge (two-step confirmed) -----------------------------------------

// withFISBCachePurgeStateReset clears the package-level purge token state
// before and after a test - this state is not covered by
// withFISBCacheTestEnv since most tests never touch it.
func withFISBCachePurgeStateReset(t *testing.T) {
	t.Helper()
	fisbPurgeMu.Lock()
	origToken, origUntil, origUsed := fisbPurgeToken, fisbPurgeUntil, fisbPurgeUsed
	fisbPurgeToken, fisbPurgeUsed = "", false
	fisbPurgeMu.Unlock()
	t.Cleanup(func() {
		fisbPurgeMu.Lock()
		fisbPurgeToken, fisbPurgeUntil, fisbPurgeUsed = origToken, origUntil, origUsed
		fisbPurgeMu.Unlock()
	})
}

func TestFISBCachePurge_PrepareThenConfirmDeletesEverything(t *testing.T) {
	withFISBCacheTestEnv(t)
	withFISBCachePurgeStateReset(t)
	entry1 := fisbcache.Entry{Key: fisbcache.TextKey(fisbcache.TextProductMETAR, "KSEA"), ReceivedAtMonotonic: monotonicSeconds()}
	entry2 := fisbcache.Entry{Key: fisbcache.TextKey(fisbcache.TextProductTAF, "KPDX"), ReceivedAtMonotonic: monotonicSeconds()}
	fisbCacheStore.Admit(entry1)
	fisbCacheStore.Admit(entry2)
	if fisbCacheStore.Len() != 2 {
		t.Fatalf("test precondition failed: expected 2 entries, got %d", fisbCacheStore.Len())
	}
	// Actually persist both, through the real write path, so a confirmed
	// purge has real files to delete - a purely in-memory (never
	// persisted) entry has nothing on disk for eviction to remove, which
	// is a distinct, already-covered scenario, not this test's own.
	if err := fisbCachePersist(entry1, "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
		t.Fatalf("could not persist entry1 for test setup: %v", err)
	}
	if err := fisbCachePersist(entry2, "TAF KPDX 091730Z 0918/1024 00000KT P6SM SKC"); err != nil {
		t.Fatalf("could not persist entry2 for test setup: %v", err)
	}

	prepReq := httptest.NewRequest(http.MethodPost, "/prepareFISBCachePurge", nil)
	prepRec := httptest.NewRecorder()
	handlePrepareFISBCachePurgeRequest(prepRec, prepReq)
	if prepRec.Code != http.StatusOK {
		t.Fatalf("expected 200 preparing a purge, got %d: %s", prepRec.Code, prepRec.Body.String())
	}
	prepResp := decodeFISBJSONBody(t, prepRec)
	token, _ := prepResp["token"].(string)
	if token == "" {
		t.Fatal("expected a non-empty purge confirmation token")
	}

	confirmBody, _ := json.Marshal(map[string]string{"token": token})
	confirmReq := httptest.NewRequest(http.MethodPost, "/confirmFISBCachePurge", bytes.NewReader(confirmBody))
	confirmRec := httptest.NewRecorder()
	handleConfirmFISBCachePurgeRequest(confirmRec, confirmReq)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("expected 200 confirming a purge, got %d: %s", confirmRec.Code, confirmRec.Body.String())
	}
	confirmResp := decodeFISBJSONBody(t, confirmRec)
	if confirmResp["deletedCount"] != float64(2) {
		t.Errorf("expected deletedCount=2, got %+v", confirmResp["deletedCount"])
	}
	if fisbCacheStore.Len() != 0 {
		t.Errorf("expected the store empty after a confirmed purge, got %d entries", fisbCacheStore.Len())
	}
}

func TestFISBCachePurge_ConfirmWithoutPrepareRejected(t *testing.T) {
	withFISBCacheTestEnv(t)
	withFISBCachePurgeStateReset(t)
	confirmBody, _ := json.Marshal(map[string]string{"token": "fisbpurge-doesnotexist"})
	req := httptest.NewRequest(http.MethodPost, "/confirmFISBCachePurge", bytes.NewReader(confirmBody))
	rec := httptest.NewRecorder()
	handleConfirmFISBCachePurgeRequest(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("expected 410 for an unknown token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFISBCachePurge_ReusedTokenRejected(t *testing.T) {
	withFISBCacheTestEnv(t)
	withFISBCachePurgeStateReset(t)

	prepRec := httptest.NewRecorder()
	handlePrepareFISBCachePurgeRequest(prepRec, httptest.NewRequest(http.MethodPost, "/prepareFISBCachePurge", nil))
	token, _ := decodeFISBJSONBody(t, prepRec)["token"].(string)

	confirmBody, _ := json.Marshal(map[string]string{"token": token})
	rec1 := httptest.NewRecorder()
	handleConfirmFISBCachePurgeRequest(rec1, httptest.NewRequest(http.MethodPost, "/confirmFISBCachePurge", bytes.NewReader(confirmBody)))
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected the first confirm to succeed, got %d: %s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	handleConfirmFISBCachePurgeRequest(rec2, httptest.NewRequest(http.MethodPost, "/confirmFISBCachePurge", bytes.NewReader(confirmBody)))
	if rec2.Code != http.StatusGone {
		t.Fatalf("expected 410 for a reused token, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestFISBCachePurge_CancelInvalidatesToken(t *testing.T) {
	withFISBCacheTestEnv(t)
	withFISBCachePurgeStateReset(t)

	prepRec := httptest.NewRecorder()
	handlePrepareFISBCachePurgeRequest(prepRec, httptest.NewRequest(http.MethodPost, "/prepareFISBCachePurge", nil))
	token, _ := decodeFISBJSONBody(t, prepRec)["token"].(string)

	cancelRec := httptest.NewRecorder()
	handleCancelFISBCachePurgeRequest(cancelRec, httptest.NewRequest(http.MethodPost, "/cancelFISBCachePurge", nil))
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("expected 200 canceling a purge, got %d: %s", cancelRec.Code, cancelRec.Body.String())
	}

	confirmBody, _ := json.Marshal(map[string]string{"token": token})
	confirmRec := httptest.NewRecorder()
	handleConfirmFISBCachePurgeRequest(confirmRec, httptest.NewRequest(http.MethodPost, "/confirmFISBCachePurge", bytes.NewReader(confirmBody)))
	if confirmRec.Code != http.StatusGone {
		t.Fatalf("expected 410 confirming a canceled token, got %d: %s", confirmRec.Code, confirmRec.Body.String())
	}
}

// --- diagnostics summary --------------------------------------------

func TestFISBCacheDiagnosticsSummary_NeverIncludesRawPayload(t *testing.T) {
	withFISBCacheTestEnv(t)
	fisbCacheStore.Admit(fisbcache.Entry{Key: fisbcache.TextKey(fisbcache.TextProductMETAR, "KSEA"), ReceivedAtMonotonic: monotonicSeconds()})

	summary := fisbCacheDiagnosticsSummary()
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"KSEA", "payload", "identity"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Errorf("diagnostics summary must never include %q, got %s", forbidden, encoded)
		}
	}
}
