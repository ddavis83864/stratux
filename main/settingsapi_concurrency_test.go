package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// This file covers the globalSettingsMu regression scenarios this
// hotfix's own mission specifically named, beyond what
// TestHandleSettingsSetRequest_ConcurrentValidAndInvalidRequests
// (settingsapi_test.go, pre-existing) already proves for concurrent
// valid+invalid /setSettings requests: simultaneous GET+PATCH,
// simultaneous valid PATCHes against different fields, Configuration
// Backup's snapshot/apply paths racing against live settings traffic,
// repeated snapshot reads, and an explicit no-deadlock-under-timeout
// proof for the whole mixed load. Every test here runs under
// `go test -race`; on the pre-hotfix code (globalSettings mutated with
// no lock at all) these reliably fault - after the fix they must pass
// cleanly, every run.

// TestGlobalSettingsMu_ConcurrentGetAndSetAreRaceSafe covers "simultaneous
// GET+valid-PATCH": many concurrent /getSettings reads interleaved with
// many concurrent valid /setSettings writes.
func TestGlobalSettingsMu_ConcurrentGetAndSetAreRaceSafe(t *testing.T) {
	withSettingsAPITestEnv(t)

	const n = 25
	var wg sync.WaitGroup
	wg.Add(2 * n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/getSettings", nil)
			rr := httptest.NewRecorder()
			handleSettingsGetRequest(rr, req)
			if rr.Code != http.StatusOK {
				t.Errorf("expected 200 from a concurrent GET, got %d", rr.Code)
			}
		}()
		go func(i int) {
			defer wg.Done()
			body := `{"DarkMode": true}`
			if i%2 == 0 {
				body = `{"DarkMode": false}`
			}
			rr := doSettingsPost(t, body)
			if rr.Code != http.StatusOK {
				t.Errorf("expected 200 from a concurrent valid PATCH, got %d: %s", rr.Code, rr.Body.String())
			}
		}(i)
	}
	wg.Wait()
}

// TestGlobalSettingsMu_ConcurrentValidPatchesToDifferentFieldsAreRaceSafe
// covers "simultaneous valid PATCHes" specifically - unlike
// TestHandleSettingsSetRequest_ConcurrentValidAndInvalidRequests, every
// request here is independently valid and touches a different field, so a
// failure here could only be the mutation race itself, never a rejection
// path.
func TestGlobalSettingsMu_ConcurrentValidPatchesToDifferentFieldsAreRaceSafe(t *testing.T) {
	withSettingsAPITestEnv(t)

	bodies := []string{
		`{"DarkMode": true}`,
		`{"PPM": 12}`,
		`{"Dump1090Gain": 30.5}`,
		`{"WiFiChannel": 6}`,
		`{"RadarRange": 20}`,
	}
	const rounds = 10
	var wg sync.WaitGroup
	for r := 0; r < rounds; r++ {
		for _, b := range bodies {
			wg.Add(1)
			go func(body string) {
				defer wg.Done()
				rr := doSettingsPost(t, body)
				if rr.Code != http.StatusOK {
					t.Errorf("expected 200 for a valid single-field PATCH, got %d: %s", rr.Code, rr.Body.String())
				}
			}(b)
		}
	}
	wg.Wait()
}

// TestGlobalSettingsMu_ConfigBackupSnapshotDuringConcurrentSettingsPatches
// covers "Config Backup preview during update" and "repeated snapshot
// reads": gatherConfigBackupCurrentState (what
// handleValidateConfigurationBackupRequest calls to build a preview)
// racing against a stream of concurrent, live /setSettings PATCHes.
func TestGlobalSettingsMu_ConfigBackupSnapshotDuringConcurrentSettingsPatches(t *testing.T) {
	withConfigBackupTestEnv(t)

	const patchers = 10
	const snapshotters = 10
	var wg sync.WaitGroup
	wg.Add(patchers + snapshotters)
	for i := 0; i < patchers; i++ {
		go func(i int) {
			defer wg.Done()
			body := `{"DarkMode": true}`
			if i%2 == 0 {
				body = `{"DarkMode": false}`
			}
			rr := doSettingsPost(t, body)
			if rr.Code != http.StatusOK {
				t.Errorf("expected 200 for a concurrent PATCH during snapshotting, got %d: %s", rr.Code, rr.Body.String())
			}
		}(i)
	}
	for i := 0; i < snapshotters; i++ {
		go func() {
			defer wg.Done()
			if _, err := gatherConfigBackupCurrentState(); err != nil {
				t.Errorf("gatherConfigBackupCurrentState failed during concurrent settings patches: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestGlobalSettingsMu_ConfigBackupApplyDuringConcurrentSettingsReads
// covers "Config Backup apply during reads": a full, real
// /applyConfigurationBackup restore (exercising
// applyConfigurationSectionToGlobalSettings through
// applyConfigBackupTransaction, nested inside profilesMu exactly as
// production does) running concurrently with a stream of live
// /getSettings reads.
func TestGlobalSettingsMu_ConfigBackupApplyDuringConcurrentSettingsReads(t *testing.T) {
	withConfigBackupTestEnv(t)

	body := downloadTestBackup(t)
	token, backup := validateAndGetToken(t, body)
	applyBody := applyRequestBody(t, token, backup)

	const readers = 20
	var wg sync.WaitGroup
	wg.Add(readers + 1)

	readersDone := make(chan struct{})
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-readersDone:
					return
				default:
				}
				req := httptest.NewRequest(http.MethodGet, "/getSettings", nil)
				rr := httptest.NewRecorder()
				handleSettingsGetRequest(rr, req)
				if rr.Code != http.StatusOK {
					t.Errorf("expected 200 from a concurrent GET during apply, got %d", rr.Code)
					return
				}
			}
		}()
	}
	go func() {
		defer wg.Done()
		defer close(readersDone)
		req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyBody))
		rec := httptest.NewRecorder()
		handleApplyConfigurationBackupRequest(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 from apply, got %d: %s", rec.Code, rec.Body.String())
		}
	}()
	wg.Wait()
}

// TestGlobalSettingsMu_NoDeadlockUnderConcurrentMixedLoad is the explicit
// no-deadlock-under-timeout proof: GET, PATCH, Configuration Backup
// snapshot, and a full apply all running concurrently, with the whole
// test required to finish well inside a generous timeout. A lock-order
// cycle or a forgotten Unlock would hang this test instead of failing it
// cleanly, which is exactly what the timeout here is for.
func TestGlobalSettingsMu_NoDeadlockUnderConcurrentMixedLoad(t *testing.T) {
	withConfigBackupTestEnv(t)

	body := downloadTestBackup(t)
	token, backup := validateAndGetToken(t, body)
	applyBody := applyRequestBody(t, token, backup)

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		const n = 8
		wg.Add(3*n + 1)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodGet, "/getSettings", nil)
				rr := httptest.NewRecorder()
				handleSettingsGetRequest(rr, req)
			}()
			go func(i int) {
				defer wg.Done()
				b := `{"DarkMode": true}`
				if i%2 == 0 {
					b = `{"DarkMode": false}`
				}
				doSettingsPost(t, b)
			}(i)
			go func() {
				defer wg.Done()
				gatherConfigBackupCurrentState()
			}()
		}
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyBody))
			rec := httptest.NewRecorder()
			handleApplyConfigurationBackupRequest(rec, req)
		}()
		wg.Wait()
	}()

	select {
	case <-done:
		// completed - no deadlock.
	case <-time.After(10 * time.Second):
		t.Fatal("mixed concurrent settings/config-backup load did not complete within 10s - suspected deadlock")
	}
}

// TestGlobalSettingsMu_PersistenceFailureStillReleasesLock proves that
// handleSettingsSetRequest's globalSettingsMu is released even when
// saveSettings() itself hits trouble writing to disk (an unwritable
// configLocation), and that a subsequent, independent request still
// succeeds - i.e. a persistence failure can never leave globalSettingsMu
// permanently held.
func TestGlobalSettingsMu_PersistenceFailureStillReleasesLock(t *testing.T) {
	withSettingsAPITestEnv(t)

	// saveSettings()'s own error path calls addSingleSystemErrorf, which
	// locks the package-level systemErrsMutex - a *sync.Mutex only ever
	// initialized inside main() (see gen_gdl90.go), nil under `go test`.
	// Forcing a persistence failure without first giving that path a
	// real mutex/map to use would corrupt this whole test binary, not
	// just this one test - see withConfigBackupTestEnv's own note on the
	// exact same hazard. Initialize both here, scoped to this test.
	origMu := systemErrsMutex
	origErrs := systemErrs
	t.Cleanup(func() {
		systemErrsMutex = origMu
		systemErrs = origErrs
	})
	systemErrsMutex = &sync.Mutex{}
	systemErrs = map[string]string{}

	// A directory in place of the settings file: saveSettings()'s
	// os.OpenFile write will fail, but the handler must still return
	// (successfully or not) having released the lock.
	origConfigLocation := configLocation
	t.Cleanup(func() { configLocation = origConfigLocation })
	configLocation = t.TempDir() // a directory, not a writable regular file

	rr := doSettingsPost(t, `{"DarkMode": true}`)
	_ = rr // handler is expected to respond one way or another, not hang

	// The real proof: the lock is actually free afterward. If the first
	// request's defer never ran, this call hangs forever and the test
	// times out instead of failing cleanly - `go test` itself provides
	// that timeout.
	done := make(chan struct{})
	go func() {
		defer close(done)
		globalSettingsMu.Lock()
		globalSettingsMu.Unlock()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("globalSettingsMu still held after a persistence failure - handler must always release it")
	}
}
