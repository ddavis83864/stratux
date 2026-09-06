/*
recordingmetadataapi.go: the durable, versioned session-level Preflight
metadata that main/recordingapi.go captures once at recording start and
finalizes once at a normal stop - see docs/recording.md for the full
design.

Endpoints (both new, neither replaces or renames an existing one):

	GET /getRecordingMetadata?id=...      - a recording's metadata, if any
	GET /downloadRecordingMetadata?id=... - the same, as a JSON download

All file I/O here (recording.WriteInitialMetadata/FinalizeMetadata/
ReadMetadata) happens outside recMu - see main/recordingapi.go's
startRecordingLocked/stopActiveRecording for exactly where each call site
sits relative to the lock, and docs/recording.md's locking-strategy note
for why that matters.
*/
package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/stratux/stratux/preflight"
	"github.com/stratux/stratux/recording"
)

// buildSessionSnapshot derives a recording.SessionSnapshot from an
// already-built preflight.Report and a recording session whose
// Calibration* fields have already been populated (see
// populateSessionCalibrationProfile). Pure - no locks, no I/O - so it is
// safe to call at any point relative to recMu.
//
// CapturedAtUTC/CapturedAtMonoSeconds deliberately reuse r's own
// GeneratedAt/GeneratedAtMonoSeconds rather than taking a fresh
// time.Now()/stratuxClock.Time reading: r is the exact Preflight report
// this snapshot summarizes, so tying "when was this captured" to "when was
// this Preflight data generated" keeps the two internally consistent, and
// preserves r.GeneratedAt's existing nil-unless-trusted rule (see
// main/preflightapi.go's buildPreflightReport) without re-deriving it.
func buildSessionSnapshot(r preflight.Report, session *recordingSession) recording.SessionSnapshot {
	gpsFixAvailable := false
	for _, c := range r.Automated {
		if c.CheckID == "gps_fix" && c.State == preflight.StateReady {
			gpsFixAvailable = true
			break
		}
	}
	return recording.SessionSnapshot{
		CapturedAtUTC:                   r.GeneratedAt,
		CapturedAtMonoSeconds:           r.GeneratedAtMonoSeconds,
		StratuxVersion:                  globalStatus.Version,
		StratuxCommit:                   globalStatus.Build,
		PreflightBootSessionID:          r.BootSessionID,
		PreflightGeneratedAt:            r.GeneratedAt,
		PreflightGeneratedAtMonoSeconds: r.GeneratedAtMonoSeconds,
		PreflightOverallState:           string(r.Overall),
		PreflightRequiredActionCount:    r.RequiredActionCount,
		PreflightCautionCount:           r.CautionCount,
		PreflightAutomated:              r.Automated,
		PreflightManual:                 r.Manual,
		TrustedTimeAvailable:            r.GeneratedAt != nil,
		GPSFixAvailable:                 gpsFixAvailable,
		CalibrationProfileID:            session.CalibrationProfileID,
		CalibrationProfileName:          session.CalibrationProfileName,
		CalibrationProfileKind:          session.CalibrationProfileKind,
		CalibrationValid:                session.CalibrationValid,
		CalibrationProfileAvailable:     session.CalibrationProfileAvailable,
	}
}

// handleRecordingMetadataRequest serves GET /getRecordingMetadata?id=... -
// 200 with the metadata for a recording that has one (available:true),
// a distinct 200 response for a legacy recording with none
// (available:false, legacy:true) or one whose metadata failed to parse
// (available:false, corrupt:true) - corruption is never silently reported
// as absence, and absence is never treated as an error. 400 for a
// malformed/unsafe id, 404 for a well-formed id that does not name an
// existing recording, 405 for any method but GET.
func handleRecordingMetadataRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" || !recordingIDPattern.MatchString(id) {
		http.Error(w, "invalid recording id", http.StatusBadRequest)
		return
	}
	dir, ok := validRecordingDir(id)
	if !ok {
		http.Error(w, "recording not found", http.StatusNotFound)
		return
	}
	result := recording.ReadMetadata(dir)
	switch result.Status {
	case recording.MetadataOK:
		json.NewEncoder(w).Encode(map[string]interface{}{
			"available": true,
			"metadata":  result.Metadata,
		})
	case recording.MetadataCorrupt:
		// The recording itself was found and the request was entirely
		// valid - it is specifically this recording's metadata that is
		// unreadable, an honest 200 carrying that fact, not a 4xx/5xx
		// that would suggest the request itself was at fault.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"available": false,
			"corrupt":   true,
			"error":     result.Error,
		})
	default: // recording.MetadataUnavailable
		json.NewEncoder(w).Encode(map[string]interface{}{
			"available": false,
			"legacy":    true,
		})
	}
}

// handleDownloadRecordingMetadataRequest serves
// GET /downloadRecordingMetadata?id=..., the same JSON
// handleRecordingMetadataRequest's "available" case returns, as a named
// file download. Only ever serves a recording that actually has valid
// metadata - a legacy or corrupt recording gets 404, matching the existing
// download endpoints' "not found" semantics for anything they cannot
// serve.
func handleDownloadRecordingMetadataRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" || !recordingIDPattern.MatchString(id) {
		http.Error(w, "invalid recording id", http.StatusBadRequest)
		return
	}
	dir, ok := validRecordingDir(id)
	if !ok {
		http.Error(w, "recording not found", http.StatusNotFound)
		return
	}
	result := recording.ReadMetadata(dir)
	if result.Status != recording.MetadataOK {
		http.Error(w, "no metadata available for this recording", http.StatusNotFound)
		return
	}
	data, err := json.MarshalIndent(&result.Metadata, "", "  ")
	if err != nil {
		http.Error(w, "could not encode metadata", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.metadata.json", id))
	w.Write(data)
}
