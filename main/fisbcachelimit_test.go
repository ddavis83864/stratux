package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stratux/stratux/configbackup"
)

// The FIS-B cache maximum entry count is 10,000: every accepted capture copies the store
// snapshot, and 10,000 is the largest size measured on the target Pi. These tests pin the
// boundary in every place a value can enter: Validate, the settings API, the persisted file,
// Configuration Backup and the dashboard form.

func TestFISBCacheMaxEntriesLimit_IsTenThousandAndDefaultUnchanged(t *testing.T) {
	if FISBCacheMaxEntriesLimit != 10000 {
		t.Fatalf("FISBCacheMaxEntriesLimit = %d, want 10000", FISBCacheMaxEntriesLimit)
	}
	if got := DefaultFISBCacheSettings().MaxEntries; got != 2000 {
		t.Fatalf("the default maxEntries changed to %d; the owner kept it at 2000", got)
	}
	if configbackup.FISBCacheMaxEntries != FISBCacheMaxEntriesLimit {
		t.Fatalf("configbackup.FISBCacheMaxEntries = %d but main's limit is %d - keep them equal", configbackup.FISBCacheMaxEntries, FISBCacheMaxEntriesLimit)
	}
}

func TestFISBCacheSettings_MaxEntriesBoundaryTable(t *testing.T) {
	cases := []struct {
		v  int
		ok bool
	}{
		{1, true}, {2, true}, {2000, true}, {9999, true}, {10000, true},
		{0, false}, {-1, false}, {math.MinInt32, false},
		{10001, false}, {10002, false}, {100000, false}, {100001, false}, {1000000, false},
		{math.MaxInt32, false}, {math.MaxInt64, false},
	}
	for _, c := range cases {
		s := DefaultFISBCacheSettings()
		s.MaxEntries = c.v
		err := s.Validate()
		if (err == nil) != c.ok {
			t.Errorf("maxEntries=%d: err=%v, want ok=%v", c.v, err, c.ok)
		}
		if err != nil && c.v > FISBCacheMaxEntriesLimit && !strings.Contains(err.Error(), "10000") {
			t.Errorf("maxEntries=%d: the error should state the limit, got %q", c.v, err)
		}
	}
}

func postFISBSettingsBody(t *testing.T, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/setFISBCacheSettings", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handleSetFISBCacheSettingsRequest(rec, req)
	return rec.Code, rec.Body.String()
}

func TestHandleSetFISBCacheSettings_MaxEntriesBoundaryOverHTTP(t *testing.T) {
	withFISBCacheTestEnv(t)

	body := func(v string) string {
		return `{"schemaVersion":1,"enabled":true,"persistenceEnabled":false,"replayEnabled":false,"maxCacheBytes":16777216,"maxEntries":` + v + `}`
	}
	// Establish a known-good persisted setting first.
	if code, out := postFISBSettingsBody(t, body("2000")); code != http.StatusOK {
		t.Fatalf("baseline 2000: %d %s", code, out)
	}
	for _, v := range []string{"9999", "10000"} {
		if code, out := postFISBSettingsBody(t, body(v)); code != http.StatusOK {
			t.Errorf("maxEntries %s must be accepted, got %d %s", v, code, out)
		}
	}
	if got := loadFISBCacheSettings().MaxEntries; got != 10000 {
		t.Fatalf("accepted 10000 was not persisted: %d", got)
	}
	// Rejected values: HTTP 400, and the currently valid setting (10000) neither changes nor is corrupted.
	before, _ := os.ReadFile(fisbCacheSettingsPath)
	for _, v := range []string{"10001", "100000", "1000000", "0", "-5", "2147483648", "9223372036854775807", "99999999999999999999999", `"10000"`, "10000.5", "null", "true", "[10]"} {
		code, out := postFISBSettingsBody(t, body(v))
		if code != http.StatusBadRequest {
			t.Errorf("maxEntries %s must be rejected with 400, got %d %s", v, code, out)
		}
		after, _ := os.ReadFile(fisbCacheSettingsPath)
		if !bytes.Equal(before, after) {
			t.Fatalf("a rejected maxEntries %s changed the persisted settings file", v)
		}
		if got := loadFISBCacheSettings().MaxEntries; got != 10000 {
			t.Fatalf("after rejecting %s the active setting is %d, want 10000 untouched", v, got)
		}
		if _, err := os.Stat(fisbCacheSettingsPath + ".tmp"); err == nil {
			t.Fatalf("a rejected %s left a temp settings file behind", v)
		}
	}
	// A missing maxEntries decodes to 0 and is rejected by the existing rule (no coercion).
	if code, _ := postFISBSettingsBody(t, `{"schemaVersion":1,"enabled":true,"maxCacheBytes":16777216}`); code != http.StatusBadRequest {
		t.Errorf("a missing maxEntries must be rejected, got %d", code)
	}
	// Replay stays rejected.
	if code, _ := postFISBSettingsBody(t, `{"schemaVersion":1,"enabled":true,"replayEnabled":true,"maxCacheBytes":16777216,"maxEntries":2000}`); code != http.StatusBadRequest {
		t.Errorf("replayEnabled:true must still be rejected with 400, got %d", code)
	}
	if got := loadFISBCacheSettings(); got.MaxEntries != 10000 || got.ReplayEnabled {
		t.Fatalf("settings after all rejections: %+v", got)
	}
}

// A device upgraded from the previous build may already hold a persisted maxEntries between
// 10,001 and 100,000. The established convention for a persisted file that fails validation is
// to log and fall back to the documented (disabled) defaults - never to clamp it silently, never
// to crash, never to keep an out-of-envelope cache. Verified here; the file itself is left alone.
func TestFISBCacheSettings_PersistedValueAboveTheNewLimitFallsBackToDisabledDefaults(t *testing.T) {
	withTestFISBCacheSettingsPath(t)
	for _, v := range []int{10001, 50000, 100000} {
		raw := fmt.Sprintf(`{"schemaVersion":1,"enabled":true,"persistenceEnabled":true,"replayEnabled":false,"maxCacheBytes":16777216,"maxEntries":%d}`, v)
		if err := os.WriteFile(fisbCacheSettingsPath, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		got := loadFISBCacheSettings()
		if got != DefaultFISBCacheSettings() {
			t.Errorf("persisted maxEntries %d: loaded %+v, want the disabled defaults", v, got)
		}
		if got.Enabled {
			t.Errorf("persisted maxEntries %d left the cache enabled", v)
		}
		after, _ := os.ReadFile(fisbCacheSettingsPath)
		if string(after) != raw {
			t.Errorf("loading must not rewrite the persisted file")
		}
	}
	// A value at the limit is still honored.
	raw := `{"schemaVersion":1,"enabled":true,"persistenceEnabled":false,"replayEnabled":false,"maxCacheBytes":16777216,"maxEntries":10000}`
	os.WriteFile(fisbCacheSettingsPath, []byte(raw), 0o644)
	if got := loadFISBCacheSettings(); got.MaxEntries != 10000 || !got.Enabled {
		t.Errorf("a persisted 10000 must load as is, got %+v", got)
	}
}

func TestDashboardMaxEntriesInputMatchesTheServerLimit(t *testing.T) {
	html, err := os.ReadFile("../web/plates/fisbcache.html")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`ng-model="Settings.maxEntries" min="1" max="%d"`, FISBCacheMaxEntriesLimit)
	if !strings.Contains(string(html), want) {
		t.Fatalf("the dashboard's maxEntries input must be limited to %d (server-side validation stays authoritative); want %q", FISBCacheMaxEntriesLimit, want)
	}
	if strings.Contains(string(html), "100000") {
		t.Fatal("the dashboard still mentions 100000")
	}
}

// Configuration Backup through the real validate handler: the limit applies to restore too.
func TestHandleValidateConfigurationBackup_FISBCacheMaxEntries(t *testing.T) {
	withConfigBackupTestEnv(t)
	base := downloadTestBackup(t)
	var cur configbackup.Document
	if err := json.Unmarshal(base, &cur); err != nil {
		t.Fatal(err)
	}
	build := func(entries int) []byte {
		doc, err := configbackup.BuildDocument(configbackup.BuildInputs{
			SourceVersion: cur.SourceVersion, SourceCommit: cur.SourceCommit, CreatedAtUTC: cur.CreatedAtUTC,
			Configuration: cur.Configuration, CalibrationProfiles: cur.CalibrationProfiles, ActiveCalibrationProfileID: cur.ActiveCalibrationProfileID,
			AlertSettings: cur.AlertSettings, AutoRecordSettings: cur.AutoRecordSettings, TrafficCPASettings: cur.TrafficCPASettings,
			EpaperSettings:    cur.EpaperSettings,
			FISBCacheSettings: configbackup.FISBCacheSettingsSection{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 16 << 20, MaxEntries: entries},
		})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(doc)
		return b
	}
	for _, c := range []struct {
		entries int
		want    int
	}{{10000, http.StatusOK}, {9999, http.StatusOK}, {10001, http.StatusBadRequest}, {100000, http.StatusBadRequest}} {
		req := httptest.NewRequest(http.MethodPost, "/validateConfigurationBackup", bytes.NewReader(build(c.entries)))
		rec := httptest.NewRecorder()
		handleValidateConfigurationBackupRequest(rec, req)
		if rec.Code != c.want {
			t.Errorf("backup with maxEntries %d: HTTP %d, want %d: %s", c.entries, rec.Code, c.want, rec.Body.String())
		}
	}
	// The section is present in every downloaded backup (seven sections) and the legacy defaults stay valid.
	if _, ok := cur.SectionChecksums["fisbCacheSettings"]; !ok || len(cur.SectionChecksums) != 7 {
		t.Errorf("expected a seven-section backup including fisbCacheSettings, got %v", cur.SectionChecksums)
	}
	def := configbackup.LegacyDefaultFISBCacheSettings()
	if def.MaxEntries > FISBCacheMaxEntriesLimit || def.MaxEntries != DefaultFISBCacheSettings().MaxEntries {
		t.Errorf("legacy default maxEntries %d must be the unchanged default and within the limit", def.MaxEntries)
	}
}
