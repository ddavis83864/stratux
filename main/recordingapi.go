/*
recordingapi.go: wires the existing, unit-tested recording package into
the running daemon via a minimal, additive, on-demand control API.
Automatic flight recording remains disabled - nothing here starts a
recording unless explicitly requested through this API.

Endpoints (all new, none replace or rename an existing one):

	POST /startRecording                 - begin a new recording session
	POST /stopRecording                   - stop the active session, if any
	GET  /getRecordingStatus              - current/last session status
	GET  /getRecordings                   - list recording sessions
	POST /exportRecording?id=...&format=csv - export a session to a persisted file
	GET  /downloadRecording?id=...        - download a session's raw JSONL (zipped)
	GET  /downloadExport?name=...         - download a previously created export

Recording files live under recordingsDir (one subdirectory per session,
server-generated ID); exports live under exportsDir. Both are on the
persistent data partition, never the temporary root overlay. As with
diagnostics, path-traversal safety comes from validating the requested
id/name against a fresh directory listing before ever building a path
from it - never from scrubbing the input string.
*/
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/recording"
)

const (
	recordingsDir = PersistentDataPath + "/recordings"
	exportsDir    = PersistentDataPath + "/exports"

	// recordingSampleInterval is how often a sample is appended while a
	// recording is active.
	recordingSampleInterval = 1 * time.Second
	// recordingMaxFileBytes/recordingMaxFiles bound one session's on-disk
	// footprint the same way recording.Store already bounds any store.
	recordingMaxFileBytes = 8 << 20 // 8 MiB per rotated file within a session
	recordingMaxFiles     = 50      // per session
	// recordingMinFreeBytes is the documented minimum free space on the
	// persistent partition below which a recording refuses to start, and
	// an active recording stops itself rather than risk starving other
	// persistent-partition consumers (diagnostics, OTA staging, exports).
	recordingMinFreeBytes = 100 << 20 // 100 MiB
	// recordingIDPattern is the exact shape a server-generated session ID
	// takes - used to fast-reject obviously-invalid input before the real
	// safety check (exact match against a fresh directory listing).
	recordingIDPatternStr = `^rec-[0-9]{8}T[0-9]{6}Z$`
)

var recordingIDPattern = regexp.MustCompile(recordingIDPatternStr)
var exportNamePattern = regexp.MustCompile(`^rec-[0-9]{8}T[0-9]{6}Z\.(csv)$`)

type recordingLifecycleState string

const (
	recordingStateIdle   recordingLifecycleState = "idle"
	recordingStateActive recordingLifecycleState = "active"
	recordingStateError  recordingLifecycleState = "error"
)

// recordingSession tracks one start-to-stop recording. Only one may be
// active at a time (recMu + recCurrent enforce this).
type recordingSession struct {
	ID          string                  `json:"id"`
	State       recordingLifecycleState `json:"state"`
	StartedAt   time.Time               `json:"startedAt"`
	StoppedAt   time.Time               `json:"stoppedAt,omitempty"`
	SampleCount int64                   `json:"sampleCount"`
	LastError   string                  `json:"lastError,omitempty"`

	dir             string
	store           *recording.Store
	stopCh          chan struct{}
	doneCh          chan struct{}
	lastHealthState string
	lastTimeState   string
}

var (
	recMu      sync.Mutex
	recCurrent *recordingSession // nil until the first /startRecording ever
)

// recordingStatusSnapshot is what the API reports - a copy, never the live
// session pointer, so a caller can't observe or mutate internal state.
type recordingStatusSnapshot struct {
	ID          string                  `json:"id,omitempty"`
	State       recordingLifecycleState `json:"state"`
	StartedAt   time.Time               `json:"startedAt,omitempty"`
	StoppedAt   time.Time               `json:"stoppedAt,omitempty"`
	SampleCount int64                   `json:"sampleCount"`
	LastError   string                  `json:"lastError,omitempty"`
}

func recordingStatusLocked() recordingStatusSnapshot {
	if recCurrent == nil {
		return recordingStatusSnapshot{State: recordingStateIdle}
	}
	return recordingStatusSnapshot{
		ID:          recCurrent.ID,
		State:       recCurrent.State,
		StartedAt:   recCurrent.StartedAt,
		StoppedAt:   recCurrent.StoppedAt,
		SampleCount: recCurrent.SampleCount,
		LastError:   recCurrent.LastError,
	}
}

// availablePersistentBytes reports current free space on the persistent
// partition, reusing the same certification logic /getHealth's Storage
// tile is built from rather than a second, parallel df-equivalent.
func availablePersistentBytes() (uint64, error) {
	storage := readiness.CertifyPersistentStorage(PersistentDataPath, globalSettings.PersistentDataUUID, readiness.DefaultPersistentStorageThresholds())
	if !storage.Mounted {
		return 0, fmt.Errorf("persistent storage not mounted")
	}
	if storage.ReadOnly {
		return 0, fmt.Errorf("persistent storage is read-only")
	}
	return uint64(storage.AvailableBytes), nil
}

// handleStartRecordingRequest serves POST /startRecording.
func handleStartRecordingRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	recMu.Lock()
	defer recMu.Unlock()

	if recCurrent != nil && recCurrent.State == recordingStateActive {
		// Double start: a clear conflict response, not a silent no-op -
		// starting a second session while one is active would either
		// orphan the first or silently merge two unrelated recordings.
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "a recording is already active",
			"status":  recordingStatusLocked(),
		})
		return
	}

	if _, err := availablePersistentBytes(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if avail, err := availablePersistentBytes(); err == nil && avail < recordingMinFreeBytes {
		w.WriteHeader(http.StatusInsufficientStorage)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("persistent storage below minimum free space (%d MiB) required to start a recording", recordingMinFreeBytes>>20),
		})
		return
	}

	now := time.Now().UTC()
	id := "rec-" + now.Format("20060102T150405Z")
	dir := filepath.Join(recordingsDir, id)
	store, err := recording.NewStore(dir, recordingMaxFileBytes, recordingMaxFiles)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}

	session := &recordingSession{
		ID:        id,
		State:     recordingStateActive,
		StartedAt: now,
		dir:       dir,
		store:     store,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
	recCurrent = session
	go recordingSamplerLoop(session)

	log.Printf("recording: started session %s\n", id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"status":  recordingStatusLocked(),
	})
}

// recordingSamplerLoop appends one Sample every recordingSampleInterval
// until told to stop. It runs in its own goroutine and only ever reads
// shared state under that state's own existing locks, briefly - it never
// blocks the decode or GDL90 send paths, which share none of its locks.
func recordingSamplerLoop(s *recordingSession) {
	defer close(s.doneCh)
	ticker := time.NewTicker(recordingSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			if err := appendRecordingSample(s); err != nil {
				recMu.Lock()
				if recCurrent == s {
					s.State = recordingStateError
					s.LastError = err.Error()
				}
				recMu.Unlock()
				log.Printf("recording: session %s entering error state: %s\n", s.ID, err)
				return
			}
		}
	}
}

func appendRecordingSample(s *recordingSession) error {
	if avail, err := availablePersistentBytes(); err != nil {
		return fmt.Errorf("persistent storage unavailable: %w", err)
	} else if avail < recordingMinFreeBytes {
		return fmt.Errorf("persistent storage below minimum free space (%d MiB); recording stopped to protect other consumers", recordingMinFreeBytes>>20)
	}

	mySituation.muGPS.Lock()
	lat := float64(mySituation.GPSLatitude)
	lon := float64(mySituation.GPSLongitude)
	alt := float64(mySituation.GPSAltitudeMSL)
	acc := float64(mySituation.GPSHorizontalAccuracy)
	gs := mySituation.GPSGroundSpeed
	course := float64(mySituation.GPSTrueCourse)
	mySituation.muGPS.Unlock()

	ADSBTowerMutex.Lock()
	towerCount := len(ADSBTowers)
	ADSBTowerMutex.Unlock()

	globalHealthMutex.Lock()
	health := globalHealth
	globalHealthMutex.Unlock()

	healthTransition := ""
	if string(health.Overall) != s.lastHealthState {
		healthTransition = fmt.Sprintf("%s -> %s", s.lastHealthState, health.Overall)
		s.lastHealthState = string(health.Overall)
	}
	timeState := string(health.Time.State)
	timeTransition := ""
	if timeState != s.lastTimeState {
		timeTransition = fmt.Sprintf("%s -> %s", s.lastTimeState, timeState)
		s.lastTimeState = timeState
	}

	sample := recording.Sample{
		UTC:                         time.Now().UTC(),
		TimeTrustState:              timeState,
		Latitude:                    lat,
		Longitude:                   lon,
		GPSAltitudeFt:               alt,
		GPSAccuracyMeters:           acc,
		GroundspeedKt:               gs,
		CourseDeg:                   course,
		UAT978MessageRateLastMinute: float64(globalStatus.UAT_messages_last_minute),
		ES1090MessageRateLastMinute: float64(globalStatus.ES_messages_last_minute),
		FISBTowerCount:              towerCount,
		FISBProductCounts: map[string]int{
			"METAR":  int(globalStatus.UAT_METAR_total),
			"TAF":    int(globalStatus.UAT_TAF_total),
			"NEXRAD": int(globalStatus.UAT_NEXRAD_total),
			"SIGMET": int(globalStatus.UAT_SIGMET_total),
			"PIREP":  int(globalStatus.UAT_PIREP_total),
			"NOTAM":  int(globalStatus.UAT_NOTAM_total),
			"OTHER":  int(globalStatus.UAT_OTHER_total),
		},
		SystemHealthTransition: healthTransition,
		TimeSourceTransition:   timeTransition,
		// AHRS/pressure-altitude fields are intentionally left nil: no
		// AHRS/barometer board exists on this hardware revision. They
		// must never be synthesized as zero.
	}
	if err := s.store.Append(sample); err != nil {
		return err
	}
	recMu.Lock()
	if recCurrent == s {
		s.SampleCount++
	}
	recMu.Unlock()
	return nil
}

// handleStopRecordingRequest serves POST /stopRecording. Stopping when
// nothing is active is a safe no-op (idempotent), not an error - a client
// that isn't sure whether a previous stop actually landed should be able
// to call this again freely.
func handleStopRecordingRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	stopActiveRecording()
	recMu.Lock()
	status := recordingStatusLocked()
	recMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "status": status})
}

// stopActiveRecording stops the current session, if one is active. Safe
// to call from the HTTP handler or from gracefulShutdown.
func stopActiveRecording() {
	recMu.Lock()
	s := recCurrent
	if s == nil || s.State != recordingStateActive {
		recMu.Unlock()
		return
	}
	recMu.Unlock()

	close(s.stopCh)
	<-s.doneCh // wait for the sampler goroutine to actually exit before closing the store
	s.store.Close()

	recMu.Lock()
	if recCurrent == s && s.State == recordingStateActive {
		s.State = recordingStateIdle
		s.StoppedAt = time.Now().UTC()
	}
	recMu.Unlock()
	log.Printf("recording: stopped session %s (%d samples)\n", s.ID, s.SampleCount)
}

// stopRecordingForShutdown is called from gracefulShutdown so an active
// recording is flushed and closed cleanly on daemon exit, not left with an
// unflushed final file.
func stopRecordingForShutdown() {
	stopActiveRecording()
}

// handleRecordingStatusRequest serves GET /getRecordingStatus.
func handleRecordingStatusRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	recMu.Lock()
	status := recordingStatusLocked()
	recMu.Unlock()
	json.NewEncoder(w).Encode(status)
}

type recordingListEntry struct {
	ID        string    `json:"id"`
	SizeBytes int64     `json:"sizeBytes"`
	FileCount int       `json:"fileCount"`
	StartedAt time.Time `json:"startedAt"`
}

// handleListRecordingsRequest serves GET /getRecordings.
func handleListRecordingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	entries, err := os.ReadDir(recordingsDir)
	if err != nil {
		if os.IsNotExist(err) {
			json.NewEncoder(w).Encode([]recordingListEntry{})
			return
		}
		http.Error(w, fmt.Sprintf("could not list recordings: %s", err), http.StatusInternalServerError)
		return
	}
	var out []recordingListEntry
	for _, e := range entries {
		if !e.IsDir() || !recordingIDPattern.MatchString(e.Name()) {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(recordingsDir, e.Name()))
		if err != nil {
			continue
		}
		var size int64
		for _, f := range sub {
			if info, err := f.Info(); err == nil {
				size += info.Size()
			}
		}
		startedAt, _ := time.Parse("20060102T150405Z", strings.TrimPrefix(e.Name(), "rec-"))
		out = append(out, recordingListEntry{ID: e.Name(), SizeBytes: size, FileCount: len(sub), StartedAt: startedAt.UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if out == nil {
		out = []recordingListEntry{}
	}
	json.NewEncoder(w).Encode(out)
}

// validRecordingDir resolves id to its directory only if id exactly
// matches an existing, well-formed entry in recordingsDir.
func validRecordingDir(id string) (string, bool) {
	return resolveSubdirInDir(recordingsDir, id, recordingIDPattern)
}

// handleExportRecordingRequest serves POST /exportRecording?id=...&format=csv,
// writing a persisted export file under exportsDir and returning its
// metadata. GPX/KML are accepted as format values but honestly report
// ErrExportNotImplemented rather than silently producing CSV or nothing.
func handleExportRecordingRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "csv"
	}
	dir, ok := validRecordingDir(id)
	if !ok {
		http.Error(w, "recording not found", http.StatusNotFound)
		return
	}

	var exporter recording.Exporter
	var ext string
	switch format {
	case "csv":
		exporter, ext = recording.CSVExporter{}, "csv"
	case "gpx":
		exporter, ext = recording.GPXExporter{}, "gpx"
	case "kml":
		exporter, ext = recording.KMLExporter{}, "kml"
	default:
		http.Error(w, "unsupported format (use csv, gpx, or kml)", http.StatusBadRequest)
		return
	}

	samples, err := recording.ReadAll(dir)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not read recording: %s", err), http.StatusInternalServerError)
		return
	}

	if err := os.MkdirAll(exportsDir, 0o755); err != nil {
		http.Error(w, fmt.Sprintf("could not create exports directory: %s", err), http.StatusInternalServerError)
		return
	}
	name := fmt.Sprintf("%s.%s", id, ext)
	path := filepath.Join(exportsDir, name)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not create export file: %s", err), http.StatusInternalServerError)
		return
	}
	exportErr := exporter.Export(f, samples)
	f.Close()
	if exportErr != nil {
		os.Remove(tmp)
		if exportErr == recording.ErrExportNotImplemented {
			http.Error(w, fmt.Sprintf("%s export is not yet implemented", format), http.StatusNotImplemented)
			return
		}
		http.Error(w, fmt.Sprintf("export failed: %s", exportErr), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		http.Error(w, fmt.Sprintf("could not finalize export: %s", err), http.StatusInternalServerError)
		return
	}
	info, _ := os.Stat(path)
	var size int64
	if info != nil {
		size = info.Size()
	}
	log.Printf("recording: exported session %s to %s (%d samples)\n", id, name, len(samples))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"name":        name,
		"sizeBytes":   size,
		"sampleCount": len(samples),
	})
}

// handleDownloadRecordingRequest serves GET /downloadRecording?id=...,
// streaming the session's raw JSONL file(s) as a zip.
func handleDownloadRecordingRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	id := r.URL.Query().Get("id")
	dir, ok := validRecordingDir(id)
	if !ok {
		http.Error(w, "recording not found", http.StatusNotFound)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not read recording: %s", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.zip", id))
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		src, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		dst, err := zw.Create(e.Name())
		if err == nil {
			io.Copy(dst, src)
		}
		src.Close()
	}
}

// handleDownloadExportRequest serves GET /downloadExport?name=..., the same
// listing-validated pattern as diagnostics downloads.
func handleDownloadExportRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	requested := r.URL.Query().Get("name")
	path, ok := resolveNameInDir(exportsDir, requested, exportNamePattern)
	if !ok {
		http.Error(w, "export not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", requested))
	http.ServeFile(w, r, path)
}
