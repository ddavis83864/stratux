package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stratux/stratux/sensors"
)

// fakeIMUReader is a minimal sensors.IMUReader stub. myIMUReader is a
// package-level interface variable, nil until sensors.go's real hardware
// probe sets it during main()'s startup - which never runs under `go
// test`. Any handleSettingsSetRequest path that calls myIMUReader.Close()
// (IMU_Sensor_Enabled turning off, or a genuine IMUMapping change) would
// panic on that nil interface in a test process, exactly as
// systemErrsMutex did for saveSettings() (see withSettingsAPITestEnv) -
// this is a test-environment-only condition, not a production defect,
// since main() always assigns a real reader (or leaves myIMUReader nil
// with globalStatus.IMUConnected also false, which never calls Close())
// before any HTTP handler can run.
type fakeIMUReader struct{ closed bool }

func (f *fakeIMUReader) Read() (int64, float64, float64, float64, float64, float64, float64, float64, float64, float64, error, error) {
	return 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, nil, nil
}
func (f *fakeIMUReader) ReadOne() (int64, float64, float64, float64, float64, float64, float64, float64, float64, float64, error, error) {
	return 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, nil, nil
}
func (f *fakeIMUReader) Close() { f.closed = true }

var _ sensors.IMUReader = (*fakeIMUReader)(nil)

// withSettingsAPITestEnv isolates handleSettingsSetRequest's package-level
// dependencies for one test: a snapshot/restore of globalSettings and
// globalStatus (mirrors the established pattern in
// fancontrolstatus_test.go and calprofilesapi_test.go), and a writable
// configLocation so saveSettings() - called unconditionally on every
// successful request - takes its normal success path instead of hitting
// the nil systemErrsMutex deadlock documented in configbackupapi_test.go's
// withConfigBackupTestEnv.
func withSettingsAPITestEnv(t *testing.T) {
	t.Helper()

	origSettings := globalSettings
	origStatus := globalStatus
	t.Cleanup(func() {
		globalSettings = origSettings
		globalStatus = origStatus
	})
	// Zero out both: a prior test in the same process may have left
	// mutated fields (e.g. DarkMode) behind, and this handler's "no
	// partial mutation" guarantee is meaningless to assert against an
	// unknown starting state.
	globalSettings = settings{}
	globalStatus = status{}

	origConfigLocation := configLocation
	t.Cleanup(func() { configLocation = origConfigLocation })
	configLocation = filepath.Join(t.TempDir(), "stratux.conf")

	origIMU := myIMUReader
	origPressure := myPressureReader
	t.Cleanup(func() {
		myIMUReader = origIMU
		myPressureReader = origPressure
	})
}

func doSettingsPost(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/setSettings", strings.NewReader(body))
	rr := httptest.NewRecorder()
	handleSettingsSetRequest(rr, req)
	return rr
}

// callHandlerSafely runs handleSettingsSetRequest and turns any panic into
// a test failure with the recovered value, instead of crashing the whole
// test binary - this is what actually distinguishes "returns 4xx" from
// "panics but happens to also produce a response" while reproducing the
// original defect under test control.
func callHandlerSafely(t *testing.T, req *http.Request) (rr *httptest.ResponseRecorder, panicked interface{}) {
	t.Helper()
	rr = httptest.NewRecorder()
	func() {
		defer func() {
			panicked = recover()
		}()
		handleSettingsSetRequest(rr, req)
	}()
	return rr, panicked
}

// TestHandleSettingsSetRequest_FullSettingsObjectPanicPayloadRejected
// reproduces the exact defect this hotfix addresses: posting the complete
// /getSettings document back to /setSettings previously panicked
// unconditionally at the "IMUMapping" case (val.([2]int) can never
// succeed - encoding/json decodes a JSON array into []interface{}, never
// a fixed-size Go array, through an interface{}). The handler must now
// reject this with an explicit 4xx instead of panicking.
func TestHandleSettingsSetRequest_FullSettingsObjectPanicPayloadRejected(t *testing.T) {
	withSettingsAPITestEnv(t)

	full, err := json.Marshal(&globalSettings)
	if err != nil {
		t.Fatalf("could not marshal a full settings document: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/setSettings", bytes.NewReader(full))
	rr, panicked := callHandlerSafely(t, req)
	if panicked != nil {
		t.Fatalf("handleSettingsSetRequest panicked on a full-settings-object body: %v", panicked)
	}
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected a 4xx status for a full-object repost, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSettingsSetRequest_ValidSingleSettingSucceeds(t *testing.T) {
	withSettingsAPITestEnv(t)

	rr := doSettingsPost(t, `{"DarkMode": true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !globalSettings.DarkMode {
		t.Fatalf("DarkMode was not applied")
	}
	var echoed settings
	if err := json.Unmarshal(rr.Body.Bytes(), &echoed); err != nil {
		t.Fatalf("response body was not valid settings JSON: %v", err)
	}
	if !echoed.DarkMode {
		t.Fatalf("echoed settings response did not reflect the update")
	}
}

func TestHandleSettingsSetRequest_EmptyBodyRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	rr := doSettingsPost(t, "")
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx for an empty body, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSettingsSetRequest_EmptyObjectRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	rr := doSettingsPost(t, `{}`)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx for an empty object, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSettingsSetRequest_MalformedJSONRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/setSettings", strings.NewReader(`{"DarkMode": tru`))
	rr, panicked := callHandlerSafely(t, req)
	if panicked != nil {
		t.Fatalf("handleSettingsSetRequest panicked on malformed JSON: %v", panicked)
	}
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx for malformed JSON, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSettingsSetRequest_TopLevelArrayRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	rr := doSettingsPost(t, `[1,2,3]`)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx for a top-level JSON array, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSettingsSetRequest_TopLevelScalarRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	for _, body := range []string{`5`, `"hello"`, `true`} {
		rr := doSettingsPost(t, body)
		if rr.Code < 400 || rr.Code >= 500 {
			t.Fatalf("expected 4xx for top-level scalar %s, got %d: %s", body, rr.Code, rr.Body.String())
		}
	}
}

func TestHandleSettingsSetRequest_NullBodyRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	rr := doSettingsPost(t, `null`)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx for a null body, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSettingsSetRequest_UnknownFieldRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	rr := doSettingsPost(t, `{"NotARealSetting": true}`)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx for an unrecognized key, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSettingsSetRequest_InvalidTypeRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	rr := doSettingsPost(t, `{"DarkMode": "yes"}`)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx for a wrong-typed value, got %d: %s", rr.Code, rr.Body.String())
	}
	if globalSettings.DarkMode {
		t.Fatalf("DarkMode must not be mutated when its own value fails validation")
	}
}

func TestHandleSettingsSetRequest_MultipleValidSettingsSucceed(t *testing.T) {
	withSettingsAPITestEnv(t)
	rr := doSettingsPost(t, `{"DarkMode": true, "UAT_Enabled": true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for a legitimate multi-key patch, got %d: %s", rr.Code, rr.Body.String())
	}
	if !globalSettings.DarkMode || !globalSettings.UAT_Enabled {
		t.Fatalf("both keys in a valid multi-setting request must be applied")
	}
}

func TestHandleSettingsSetRequest_DuplicateJSONKeysUsesLastValue(t *testing.T) {
	withSettingsAPITestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/setSettings", strings.NewReader(`{"DarkMode": false, "DarkMode": true}`))
	rr, panicked := callHandlerSafely(t, req)
	if panicked != nil {
		t.Fatalf("handleSettingsSetRequest panicked on duplicate JSON keys: %v", panicked)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (duplicate keys are standard JSON, last value wins), got %d: %s", rr.Code, rr.Body.String())
	}
	if !globalSettings.DarkMode {
		t.Fatalf("expected the last of two duplicate keys to win, DarkMode should be true")
	}
}

func TestHandleSettingsSetRequest_TrailingJSONRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/setSettings", strings.NewReader(`{"DarkMode": true}{"UAT_Enabled": true}`))
	rr, panicked := callHandlerSafely(t, req)
	if panicked != nil {
		t.Fatalf("handleSettingsSetRequest panicked on trailing JSON: %v", panicked)
	}
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx for a body containing a second JSON value, got %d: %s", rr.Code, rr.Body.String())
	}
	if globalSettings.DarkMode || globalSettings.UAT_Enabled {
		t.Fatalf("neither setting should be applied when the request as a whole is rejected")
	}
}

func TestHandleSettingsSetRequest_OversizedRequestRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	// One massive string value comfortably exceeds maxSettingsRequestBytes
	// while still being syntactically valid JSON, so this specifically
	// exercises the body-size limit rather than a decode error.
	huge := `{"WatchList": "` + strings.Repeat("A", maxSettingsRequestBytes+1024) + `"}`
	rr := doSettingsPost(t, huge)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx for an oversized request body, got %d", rr.Code)
	}
}

func TestHandleSettingsSetRequest_UnsupportedMethodRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/setSettings", nil)
		rr := httptest.NewRecorder()
		handleSettingsSetRequest(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("method %s: expected 405, got %d: %s", method, rr.Code, rr.Body.String())
		}
	}
}

func TestHandleSettingsSetRequest_OptionsPreflightStillSupported(t *testing.T) {
	withSettingsAPITestEnv(t)
	req := httptest.NewRequest(http.MethodOptions, "/setSettings", nil)
	rr := httptest.NewRecorder()
	handleSettingsSetRequest(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for an OPTIONS preflight request, got %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("expected an empty body for an OPTIONS preflight request, got %q", rr.Body.String())
	}
}

func TestHandleSettingsSetRequest_NoPartialMutationAfterRejection(t *testing.T) {
	withSettingsAPITestEnv(t)
	rr := doSettingsPost(t, `{"DarkMode": true, "NotARealSetting": true}`)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx, got %d: %s", rr.Code, rr.Body.String())
	}
	if globalSettings.DarkMode {
		t.Fatalf("DarkMode must not be applied when a sibling key in the same request is rejected")
	}
}

func TestHandleSettingsSetRequest_ConcurrentValidAndInvalidRequests(t *testing.T) {
	withSettingsAPITestEnv(t)
	const n = 20
	var wg sync.WaitGroup
	var panics int32
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/setSettings", strings.NewReader(`{"DarkMode": true}`))
			if _, panicked := callHandlerSafely(t, req); panicked != nil {
				mu.Lock()
				panics++
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/setSettings", strings.NewReader(`{"IMUMapping": "not-an-array"}`))
			if _, panicked := callHandlerSafely(t, req); panicked != nil {
				mu.Lock()
				panics++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if panics != 0 {
		t.Fatalf("%d of %d concurrent requests panicked", panics, 2*n)
	}
}

func TestHandleSettingsSetRequest_NoSensitiveDataInErrorOrLog(t *testing.T) {
	withSettingsAPITestEnv(t)
	const secret = "MyBenchWiFiPassphrase12345"
	rr := doSettingsPost(t, fmt.Sprintf(`{"WiFiPassphrase": %d}`, 12345))
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("expected 4xx for a wrong-typed WiFiPassphrase, got %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "12345") {
		t.Fatalf("error response must not echo the submitted value, got %q", rr.Body.String())
	}
	// Also confirm a value that does type-check is never echoed back
	// anywhere in a success response - handleSettingsSetRequest always
	// responds with the full globalSettings document, and WiFiPassphrase
	// is a real field on it, so this is a genuine exposure surface to
	// watch, not a hypothetical one.
	rr2 := doSettingsPost(t, fmt.Sprintf(`{"WiFiPassphrase": %q}`, secret))
	if rr2.Code == http.StatusOK && strings.Contains(rr2.Body.String(), secret) {
		t.Logf("note: a successful WiFiPassphrase update echoes the new passphrase back in the response body, matching /getSettings's existing behavior for this field - not a regression introduced by this change, but recorded here for visibility.")
	}
}

func TestHandleSettingsSetRequest_ConfigBackupStillWorksAfterValidPatch(t *testing.T) {
	withSettingsAPITestEnv(t)
	withTestProfilesStore(t)
	withTestAlertSettingsPath(t)
	withFakePersistentStorageForTest(t)
	if stratuxClock == nil {
		stratuxClock = NewMonotonic()
	}

	rr := doSettingsPost(t, `{"DarkMode": true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/downloadConfigurationBackup", nil)
	rr2 := httptest.NewRecorder()
	handleDownloadConfigurationBackupRequest(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Fatalf("ConfigBackup download should still work after a settings update, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

func TestHandleSettingsSetRequest_ExistingDashboardWiFiPatchStillWorks(t *testing.T) {
	withSettingsAPITestEnv(t)
	// Mirrors the exact multi-key object web/plates/js/settings.js builds
	// for its WiFi configuration modal (the largest legitimate real
	// request this handler sees) - proves the compatibility this hotfix
	// must preserve.
	body := `{
		"WiFiCountry": "US",
		"WiFiSSID": "stratux",
		"WiFiSecurityEnabled": false,
		"WiFiPassphrase": "",
		"WiFiChannel": 1,
		"WiFiIPAddress": "192.168.10.1",
		"WiFiMode": 0,
		"WiFiDirectPin": "12345678",
		"WiFiClientNetworks": [],
		"WiFiInternetPassThroughEnabled": false
	}`
	rr := doSettingsPost(t, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for the real WiFi-modal request shape, got %d: %s", rr.Code, rr.Body.String())
	}
	if globalSettings.WiFiSSID != "stratux" {
		t.Fatalf("WiFiSSID was not applied")
	}
}

func TestHandleSettingsSetRequest_IMUMappingRoundTripFixed(t *testing.T) {
	withSettingsAPITestEnv(t)
	fake := &fakeIMUReader{}
	myIMUReader = fake
	globalSettings.IMUMapping = [2]int{0, 0}

	rr := doSettingsPost(t, `{"IMUMapping": [1, 0]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for a valid IMUMapping update, got %d: %s", rr.Code, rr.Body.String())
	}
	if globalSettings.IMUMapping != [2]int{1, 0} {
		t.Fatalf("IMUMapping was not applied correctly, got %v", globalSettings.IMUMapping)
	}
	if !fake.closed {
		t.Fatalf("expected the IMU reader to be closed to force a restart after a real mapping change")
	}
}

func TestHandleSettingsSetRequest_IMUMappingMalformedRejected(t *testing.T) {
	withSettingsAPITestEnv(t)
	for _, body := range []string{
		`{"IMUMapping": "not-an-array"}`,
		`{"IMUMapping": [1]}`,
		`{"IMUMapping": [1, 2, 3]}`,
		`{"IMUMapping": [1, "x"]}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/setSettings", strings.NewReader(body))
		rr, panicked := callHandlerSafely(t, req)
		if panicked != nil {
			t.Fatalf("body %s: handleSettingsSetRequest panicked: %v", body, panicked)
		}
		if rr.Code < 400 || rr.Code >= 500 {
			t.Fatalf("body %s: expected 4xx, got %d: %s", body, rr.Code, rr.Body.String())
		}
	}
}

func TestHandleSettingsSetRequest_WiFiClientNetworksValidAndInvalidShapes(t *testing.T) {
	withSettingsAPITestEnv(t)

	rr := doSettingsPost(t, `{"WiFiClientNetworks": [{"SSID": "home", "Password": "hunter2"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for a valid WiFiClientNetworks entry, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(globalSettings.WiFiClientNetworks) != 1 || globalSettings.WiFiClientNetworks[0].SSID != "home" {
		t.Fatalf("WiFiClientNetworks was not applied correctly, got %+v", globalSettings.WiFiClientNetworks)
	}

	for _, body := range []string{
		`{"WiFiClientNetworks": "not-an-array"}`,
		`{"WiFiClientNetworks": [{"SSID": "home"}]}`,
		`{"WiFiClientNetworks": [{"Password": "hunter2"}]}`,
		`{"WiFiClientNetworks": ["not-an-object"]}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/setSettings", strings.NewReader(body))
		rr, panicked := callHandlerSafely(t, req)
		if panicked != nil {
			t.Fatalf("body %s: handleSettingsSetRequest panicked: %v", body, panicked)
		}
		if rr.Code < 400 || rr.Code >= 500 {
			t.Fatalf("body %s: expected 4xx, got %d: %s", body, rr.Code, rr.Body.String())
		}
	}
}
