/*
autorecordapi.go: HTTP endpoints for Automatic Flight Recording - status,
settings, and manual controls. See docs/automatic-flight-recording.md's
"HTTP API" section.

Endpoints:

	GET  /getAutoRecordStatus   - the Machine's current Snapshot plus the
	                              settings currently in effect.
	GET  /getAutoRecordSettings - persisted settings only.
	POST /setAutoRecordSettings - validate, persist, and apply new settings.
	POST /clearAutoRecordError  - explicit ERROR-state recovery.
*/
package main

import (
	"encoding/json"
	"net/http"

	"github.com/stratux/stratux/autorecord"
)

// maxAutoRecordRequestBytes mirrors maxAlertRequestBytes - Settings is a
// small, flat struct; there is no legitimate reason for a request body
// anywhere near this large.
const maxAutoRecordRequestBytes = 4096

type autoRecordStatusResponse struct {
	Snapshot autorecord.Snapshot `json:"snapshot"`
	Settings autorecord.Settings `json:"settings"`
}

func autoRecordStatusSnapshot() autoRecordStatusResponse {
	autoRecordMu.Lock()
	defer autoRecordMu.Unlock()
	resp := autoRecordStatusResponse{Settings: autoRecordSettingsCache}
	if autoRecordMachine != nil {
		resp.Snapshot = autoRecordMachine.Snapshot(monotonicSeconds())
	}
	return resp
}

func handleGetAutoRecordStatusRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(autoRecordStatusSnapshot())
}

func handleGetAutoRecordSettingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(loadAutoRecordSettings())
}

// handleSetAutoRecordSettingsRequest validates, persists, and applies new
// settings - the in-memory cache the detection tick reads is updated
// under the same autoRecordMu the tick itself uses, so a setting change
// is visible starting with the very next tick, never mid-tick.
func handleSetAutoRecordSettingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAutoRecordRequestBytes)
	var s autorecord.Settings
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "invalid JSON body: " + err.Error()})
		return
	}
	s.SchemaVersion = autorecord.SettingsSchemaVersion
	if err := s.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if err := saveAutoRecordSettings(s); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	autoRecordMu.Lock()
	autoRecordSettingsCache = s
	autoRecordMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "settings": s})
}

// handleClearAutoRecordErrorRequest serves the explicit ERROR-state
// recovery control - see autorecord.Machine.ClearError. A no-op-with-
// error-response if the Machine is not currently in ERROR, never a panic.
func handleClearAutoRecordErrorRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	autoRecordMu.Lock()
	defer autoRecordMu.Unlock()
	if autoRecordMachine == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "automatic recording not initialized"})
		return
	}
	now := monotonicSeconds()
	if err := autoRecordMachine.ClearError(autoRecordSettingsCache.Enabled, now); err != nil {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "snapshot": autoRecordMachine.Snapshot(now)})
}
