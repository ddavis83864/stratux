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

	"github.com/stratux/stratux/preflight"
	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/recording"
)

// recordingsDir/exportsDir are vars, not consts, solely so tests can
// redirect them at a temp directory for the duration of one test (see
// withTestRecordingsDir) - every other recording-lifecycle test in this
// project previously had to be validated live on hardware because these
// were compile-time constants pointing at the real persistent-data path.
// Production code never reassigns them after startup.
var (
	recordingsDir = PersistentDataPath + "/recordings"
	exportsDir    = PersistentDataPath + "/exports"
)

const (
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

	// Calibration* fields identify which named aircraft calibration
	// profile (see the calprofile package) was active when this session
	// started - captured once, at session start, and never changed for
	// the life of the session (switching the active profile while a
	// recording is active is refused - see
	// handleActivateCalibrationProfileRequest). This is deliberately
	// session-level metadata, not a per-sample field: the profile does
	// not change mid-session, so repeating it into every 1Hz sample
	// would only duplicate an unchanging string. recording.Sample's own
	// per-sample AHRSCalibrationState field still conveys the moment-to-
	// moment READY/DEGRADED calibration quality; these fields answer the
	// separate question of *which* profile that quality was measured
	// against.
	CalibrationProfileID        string     `json:"calibrationProfileId,omitempty"`
	CalibrationProfileName      string     `json:"calibrationProfileName,omitempty"`
	CalibrationProfileKind      string     `json:"calibrationProfileKind,omitempty"`
	CalibrationRegistration     string     `json:"calibrationRegistration,omitempty"`
	CalibrationAircraftType     string     `json:"calibrationAircraftType,omitempty"`
	CalibrationMountingNote     string     `json:"calibrationMountingNote,omitempty"`
	CalibrationValid            bool       `json:"calibrationValid"`
	CalibrationLastCalibratedAt *time.Time `json:"calibrationLastCalibratedAt,omitempty"`
	// CalibrationProfileAvailable is false if no active profile could be
	// determined at session start (profile subsystem missing/corrupt) -
	// the recording still proceeds (a profile problem must never block
	// or interrupt recording), just without profile identity attached.
	CalibrationProfileAvailable bool `json:"calibrationProfileAvailable"`

	// Preflight* fields are a small, session-level snapshot of the
	// preflight report at the moment this session started - see
	// main/preflightapi.go's populateSessionPreflightSummary. Deliberately
	// not the full preflight.Report (which would duplicate an
	// effectively-unchanging summary into session metadata far beyond
	// what a recording needs) and never repeated into every 1Hz sample,
	// same rationale as the Calibration* fields above.
	PreflightOverallState         string `json:"preflightOverallState,omitempty"`
	PreflightRequiredActionCount  int    `json:"preflightRequiredActionCount"`
	PreflightCautionCount         int    `json:"preflightCautionCount"`
	PreflightManualChecksComplete bool   `json:"preflightManualChecksComplete"`

	// MetadataError reports the most recent recording.WriteInitialMetadata
	// or recording.FinalizeMetadata failure, if any - empty means no known
	// problem. This is purely observational: a metadata write failure
	// never stops or invalidates the recording itself (see
	// main/recordingmetadataapi.go's buildSessionSnapshot and this file's
	// handleStartRecordingRequest/stopActiveRecording), it only means the
	// durable session-metadata sidecar may be missing or stale.
	MetadataError string `json:"metadataError,omitempty"`

	dir             string
	store           *recording.Store
	stopCh          chan struct{}
	doneCh          chan struct{}
	lastHealthState string
	lastTimeState   string

	// autoRecordInitiated is set once, at creation, only by
	// main/autorecordrun.go's autoRecordPerformStart - never read or
	// written anywhere else. It is what lets stopActiveRecording decide
	// whether a SessionFinalization.AutoRecordStopMode belongs on this
	// session's metadata at all (see stopActiveRecording's own doc
	// comment) without a redundant metadata.json re-read.
	autoRecordInitiated bool
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

	CalibrationProfileID        string     `json:"calibrationProfileId,omitempty"`
	CalibrationProfileName      string     `json:"calibrationProfileName,omitempty"`
	CalibrationProfileKind      string     `json:"calibrationProfileKind,omitempty"`
	CalibrationRegistration     string     `json:"calibrationRegistration,omitempty"`
	CalibrationAircraftType     string     `json:"calibrationAircraftType,omitempty"`
	CalibrationMountingNote     string     `json:"calibrationMountingNote,omitempty"`
	CalibrationValid            bool       `json:"calibrationValid"`
	CalibrationLastCalibratedAt *time.Time `json:"calibrationLastCalibratedAt,omitempty"`
	CalibrationProfileAvailable bool       `json:"calibrationProfileAvailable"`

	MetadataError string `json:"metadataError,omitempty"`
}

func recordingStatusLocked() recordingStatusSnapshot {
	if recCurrent == nil {
		return recordingStatusSnapshot{State: recordingStateIdle}
	}
	return recordingStatusSnapshot{
		ID:                          recCurrent.ID,
		State:                       recCurrent.State,
		StartedAt:                   recCurrent.StartedAt,
		StoppedAt:                   recCurrent.StoppedAt,
		SampleCount:                 recCurrent.SampleCount,
		LastError:                   recCurrent.LastError,
		CalibrationProfileID:        recCurrent.CalibrationProfileID,
		CalibrationProfileName:      recCurrent.CalibrationProfileName,
		CalibrationProfileKind:      recCurrent.CalibrationProfileKind,
		CalibrationRegistration:     recCurrent.CalibrationRegistration,
		CalibrationAircraftType:     recCurrent.CalibrationAircraftType,
		CalibrationMountingNote:     recCurrent.CalibrationMountingNote,
		CalibrationValid:            recCurrent.CalibrationValid,
		CalibrationLastCalibratedAt: recCurrent.CalibrationLastCalibratedAt,
		CalibrationProfileAvailable: recCurrent.CalibrationProfileAvailable,
		MetadataError:               recCurrent.MetadataError,
	}
}

// availablePersistentBytes reports current free space on the persistent
// partition, reusing the same certification logic /getHealth's Storage
// tile is built from rather than a second, parallel df-equivalent.
//
// This deliberately uses FreeBytes, not AvailableBytes: stratuxrun (and
// therefore this recording process) always runs as root, so the
// ext4 reserved-blocks percentage that AvailableBytes excludes is space
// this process can actually still write. Gating the minimum-free-space
// guard on AvailableBytes would refuse recording (or stop one already
// running) while `df` and this same process's own writes still had real
// room to spare - see readiness.StatfsResult's doc comment for the
// measured evidence this is based on.
//
// A var, not a func, solely so tests can substitute a fake result for the
// duration of one test (see withFakePersistentStorageForTest) - this
// always checks the real PersistentDataPath in production, which does not
// exist in the sandboxed test environment. Never reassigned outside tests.
var availablePersistentBytes = func() (uint64, error) {
	storage := readiness.CertifyPersistentStorage(PersistentDataPath, globalSettings.PersistentDataUUID, readiness.DefaultPersistentStorageThresholds())
	if !storage.Mounted {
		return 0, fmt.Errorf("persistent storage not mounted")
	}
	if storage.ReadOnly {
		return 0, fmt.Errorf("persistent storage is read-only")
	}
	return storage.FreeBytes, nil
}

// startRecordingOutcome is startRecordingLocked's result - either a newly
// started session (Session != nil) or an already-encoded failure response
// (HTTPStatus/HTTPBody) to write verbatim.
type startRecordingOutcome struct {
	Session    *recordingSession
	Status     recordingStatusSnapshot
	HTTPStatus int
	HTTPBody   map[string]interface{}
}

// startRecordingLocked performs every recMu-guarded step of starting a
// recording (conflict check, storage check, store/session creation) inside
// its own recMu.Lock/defer Unlock, then returns. It is a separate function
// specifically so recMu is always released - via its own defer, not
// scattered manual Unlock calls on every return path - before
// handleStartRecordingRequest does any file I/O (writing the initial
// recording-metadata sidecar) or writes its HTTP response. See
// docs/recording.md's locking-strategy note: metadata persistence, like
// Preflight report construction before it, must never happen while recMu
// is held.
func startRecordingLocked(preflightSnapshot preflight.Report) startRecordingOutcome {
	recMu.Lock()
	defer recMu.Unlock()

	if recCurrent != nil && recCurrent.State == recordingStateActive {
		// Double start: a clear conflict response, not a silent no-op -
		// starting a second session while one is active would either
		// orphan the first or silently merge two unrelated recordings.
		return startRecordingOutcome{
			HTTPStatus: http.StatusConflict,
			HTTPBody: map[string]interface{}{
				"success": false,
				"error":   "a recording is already active",
				"status":  recordingStatusLocked(),
			},
		}
	}

	if _, err := availablePersistentBytes(); err != nil {
		return startRecordingOutcome{
			HTTPStatus: http.StatusServiceUnavailable,
			HTTPBody:   map[string]interface{}{"success": false, "error": err.Error()},
		}
	}
	if avail, err := availablePersistentBytes(); err == nil && avail < recordingMinFreeBytes {
		return startRecordingOutcome{
			HTTPStatus: http.StatusInsufficientStorage,
			HTTPBody: map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("persistent storage below minimum free space (%d MiB) required to start a recording", recordingMinFreeBytes>>20),
			},
		}
	}

	now := time.Now().UTC()
	id := "rec-" + now.Format("20060102T150405Z")
	dir := filepath.Join(recordingsDir, id)
	store, err := recording.NewStore(dir, recordingMaxFileBytes, recordingMaxFiles)
	if err != nil {
		return startRecordingOutcome{
			HTTPStatus: http.StatusInternalServerError,
			HTTPBody:   map[string]interface{}{"success": false, "error": err.Error()},
		}
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
	populateSessionCalibrationProfile(session)
	applyPreflightSummaryToSession(session, preflightSnapshot)
	recCurrent = session
	go recordingSamplerLoop(session)

	log.Printf("recording: started session %s\n", id)
	return startRecordingOutcome{Session: session, Status: recordingStatusLocked(), HTTPStatus: http.StatusOK}
}

// handleStartRecordingRequest serves POST /startRecording.
func handleStartRecordingRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	// Computed before acquiring recMu below, not after: buildPreflightReport()
	// transitively locks recMu itself (see main/preflightapi.go's
	// recordingReadinessForPreflight), and sync.Mutex is not reentrant -
	// calling it from inside this function's own locked section
	// self-deadlocked the entire recording subsystem (confirmed live
	// during hardware validation: /startRecording never returned, and
	// every subsequent recording endpoint hung waiting on recMu forever).
	preflightSnapshot := buildPreflightReport()

	outcome := startRecordingLocked(preflightSnapshot)
	if outcome.Session == nil {
		w.WriteHeader(outcome.HTTPStatus)
		json.NewEncoder(w).Encode(outcome.HTTPBody)
		return
	}

	// recMu has already been released (startRecordingLocked's own defer
	// unlocked it before returning) - the initial metadata write happens
	// here, entirely outside the lock. Session.Calibration*/ID/dir are
	// safe to read without the lock: they are set once at creation, above,
	// and never mutated again for the life of the session.
	snapshot := buildSessionSnapshot(preflightSnapshot, outcome.Session, nil)
	if err := recording.WriteInitialMetadata(outcome.Session.dir, outcome.Session.ID, snapshot); err != nil {
		log.Printf("recording: could not write initial metadata for session %s: %s\n", outcome.Session.ID, err)
		recMu.Lock()
		if recCurrent == outcome.Session {
			outcome.Session.MetadataError = err.Error()
		}
		recMu.Unlock()
		outcome.Status.MetadataError = err.Error()
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"status":  outcome.Status,
	})
}

// populateSessionCalibrationProfile captures the currently active
// calibration profile's identity into session, once, at session start -
// see recordingSession's Calibration* fields' doc comment for why this is
// session-level, not per-sample. A missing/errored profile subsystem
// leaves every Calibration* field at its zero value and
// CalibrationProfileAvailable false - it must never block or interrupt
// starting a recording.
func populateSessionCalibrationProfile(session *recordingSession) {
	if profilesStore == nil {
		return
	}
	active, err := profilesStore.Active()
	if err != nil {
		return
	}
	session.CalibrationProfileAvailable = true
	session.CalibrationProfileID = active.ID
	session.CalibrationProfileName = active.Name
	session.CalibrationProfileKind = active.Kind
	session.CalibrationRegistration = active.Registration
	session.CalibrationAircraftType = active.AircraftType
	session.CalibrationMountingNote = active.MountingNote
	session.CalibrationValid = active.CalibrationComplete()
	session.CalibrationLastCalibratedAt = active.LastCalibratedAt
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

	// AHRS/barometer fields are gathered fresh every sample (not read back
	// from the cached globalHealth above, which only recomputes every
	// healthUpdateInterval) via the same buildAHRSHealth/buildBaroHealth
	// glue main/health.go itself uses - one mutex-protected read of
	// mySituation, immediately released, matching the nonblocking pattern
	// already used for GPS/tower-count above. A nil pointer here means
	// exactly what it means on the readiness dashboard: disabled,
	// disconnected, or not yet a valid measurement - never a fabricated 0.
	now := time.Now().UTC()
	mono := stratuxClock.Time
	ahrsHealth := buildAHRSHealth(mono, now)
	baroHealth := buildBaroHealth(mono, now)

	mySituation.muAttitude.Lock()
	gLoadMinRaw := mySituation.AHRSGLoadMin
	gLoadMaxRaw := mySituation.AHRSGLoadMax
	mySituation.muAttitude.Unlock()
	var gLoadMin, gLoadMax *float64
	if !isAHRSInvalidValue(gLoadMinRaw) {
		gLoadMin = &gLoadMinRaw
	}
	if !isAHRSInvalidValue(gLoadMaxRaw) {
		gLoadMax = &gLoadMaxRaw
	}
	var ahrsStatus *uint8
	var ahrsCalState *string
	if ahrsHealth.Connected {
		st := ahrsHealth.RawStatus
		ahrsStatus = &st
		cs := string(ahrsHealth.State)
		ahrsCalState = &cs
	}

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
		UTC:                         now,
		TimeTrustState:              timeState,
		Latitude:                    lat,
		Longitude:                   lon,
		GPSAltitudeFt:               alt,
		GPSAccuracyMeters:           acc,
		GroundspeedKt:               gs,
		CourseDeg:                   course,
		PressureAltitudeFt:          baroHealth.PressureAltitudeFt,
		PitchDeg:                    ahrsHealth.PitchDeg,
		BankDeg:                     ahrsHealth.RollDeg,
		GLoad:                       ahrsHealth.GLoad,
		GLoadMin:                    gLoadMin,
		GLoadMax:                    gLoadMax,
		BaroVerticalSpeedFPM:        baroHealth.VerticalSpeedFPM,
		AHRSStatus:                  ahrsStatus,
		AHRSCalibrationState:        ahrsCalState,
		AHRSMeasurementAgeSeconds:   ahrsHealth.LastMeasurementAgeSeconds,
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
		// VerticalAccelG has no source anywhere in this codebase (only
		// GLoad, the total accel magnitude, is computed - see
		// main/sensors.go's s.GLoad()) and is intentionally left nil
		// rather than approximated.
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
	stopActiveRecording("manual")
	// Reconcile Automatic Flight Recording's own state if the recording
	// just stopped here was one it started - see
	// main/autorecordrun.go's autoRecordNotifyManualStopIfOwned doc
	// comment. A no-op whenever it wasn't (including: manual recording,
	// or nothing was active).
	autoRecordNotifyManualStopIfOwned()
	recMu.Lock()
	status := recordingStatusLocked()
	recMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "status": status})
}

// stopActiveRecording stops the current session, if one is active. Safe
// to call from the HTTP handler or from gracefulShutdown. autoRecordStopMode
// is recorded on the finalization (recording.SessionFinalization.AutoRecordStopMode)
// only for a session that was started by Automatic Flight Recording
// (SessionSnapshot.AutoRecordInitiationMode == "automatic") - it is
// otherwise silently ignored, so every existing manual-stop call site can
// keep passing a fixed, self-describing value without needing to first
// check who started the session.
func stopActiveRecording(autoRecordStopMode string) {
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

	// Metadata finalization is file I/O (a read-modify-write of
	// metadata.json) and happens here, entirely outside recMu, same as
	// the initial write in handleStartRecordingRequest - see
	// docs/recording.md. s.SampleCount is safe to read without the lock:
	// the sampler goroutine that mutates it has already exited (<-s.doneCh
	// above), so nothing else writes it concurrently.
	stoppedAt := time.Now().UTC()
	finalization := recording.SessionFinalization{
		Complete:        true,
		StoppedAtUTC:    &stoppedAt,
		DurationSeconds: stoppedAt.Sub(s.StartedAt).Seconds(),
		SampleCount:     s.SampleCount,
		AlertEvents:     recentAlertEventsForRecording(),
	}
	if s.autoRecordInitiated {
		finalization.AutoRecordStopMode = autoRecordStopMode
	}
	var metaErr error
	if err := recording.FinalizeMetadata(s.dir, finalization); err != nil {
		metaErr = err
		log.Printf("recording: could not finalize metadata for session %s: %s\n", s.ID, err)
	}

	recMu.Lock()
	if recCurrent == s && s.State == recordingStateActive {
		s.State = recordingStateIdle
		s.StoppedAt = stoppedAt
		if metaErr != nil {
			s.MetadataError = metaErr.Error()
		}
	}
	recMu.Unlock()
	log.Printf("recording: stopped session %s (%d samples)\n", s.ID, s.SampleCount)
}

// stopRecordingForShutdown is called from gracefulShutdown so an active
// recording is flushed and closed cleanly on daemon exit, not left with an
// unflushed final file. Always runs after autoRecordHandleShutdown (see
// gen_gdl90.go's gracefulShutdown), so by the time this executes, an
// automatic recording has already been finalized through the Machine's
// own shutdown path (with AutoRecordStopMode "shutdown") and this call is
// a harmless idempotent no-op for it - this function's own "shutdown"
// mode only ever actually lands on a still-active MANUAL recording.
func stopRecordingForShutdown() {
	stopActiveRecording("shutdown")
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

	// Metadata* fields are additive - a client that ignores unknown JSON
	// fields continues to work unmodified. Deliberately a small summary,
	// not the full recording.SessionMetadata (which would embed every
	// Preflight check result into a list response for every recording) -
	// see GET /getRecordingMetadata for the full record.
	MetadataAvailable     bool   `json:"metadataAvailable"`
	MetadataCorrupt       bool   `json:"metadataCorrupt,omitempty"`
	MetadataSchemaVersion int    `json:"metadataSchemaVersion,omitempty"`
	PreflightStateAtStart string `json:"preflightStateAtStart,omitempty"`
	ProfileNameAtStart    string `json:"profileNameAtStart,omitempty"`
	Complete              bool   `json:"complete"`
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
		entry := recordingListEntry{ID: e.Name(), SizeBytes: size, FileCount: len(sub), StartedAt: startedAt.UTC()}
		switch result := recording.ReadMetadata(filepath.Join(recordingsDir, e.Name())); result.Status {
		case recording.MetadataOK:
			entry.MetadataAvailable = true
			entry.MetadataSchemaVersion = result.Metadata.SchemaVersion
			entry.PreflightStateAtStart = result.Metadata.Snapshot.PreflightOverallState
			entry.ProfileNameAtStart = result.Metadata.Snapshot.CalibrationProfileName
			entry.Complete = result.Metadata.Finalization.Complete
		case recording.MetadataCorrupt:
			entry.MetadataCorrupt = true
		} // MetadataUnavailable: legacy recording - entry's Metadata* fields stay at their zero values
		out = append(out, entry)
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

// listRecordingRefs enumerates every recording directory, newest-first by
// ID (recording IDs embed a sortable UTC timestamp - see
// recordingIDPatternStr) - the same order handleListRecordingsRequest's own
// listing already uses. Used by diagnostics generation
// (recording.SummarizeMetadata needs newest-first order to identify "the
// most recent recording" correctly); a listing failure yields an empty
// slice rather than an error, matching handleListRecordingsRequest's own
// "no recordings directory yet" tolerance.
func listRecordingRefs() []recording.RecordingRef {
	entries, err := os.ReadDir(recordingsDir)
	if err != nil {
		return nil
	}
	var refs []recording.RecordingRef
	for _, e := range entries {
		if !e.IsDir() || !recordingIDPattern.MatchString(e.Name()) {
			continue
		}
		refs = append(refs, recording.RecordingRef{ID: e.Name(), Dir: filepath.Join(recordingsDir, e.Name())})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID > refs[j].ID })
	return refs
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
