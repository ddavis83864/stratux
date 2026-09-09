/*
configbackupapi.go: HTTP glue for the configuration backup/restore
subsystem. The bounded, versioned, checksummed document type and every
piece of pure validation/diff/token logic lives in the configbackup
package (hardware/I/O-free, like alerting/readiness/recording/calprofile);
this file reads globalSettings/AlertSettings/calprofile.Store, owns the
in-memory restore-operation state machine and confirmation tokens, and
wires the four HTTP endpoints. See docs/configuration-backup-restore.md
for the full design.

This is a supported-application-configuration backup, never SD-card
imaging, filesystem cloning, OS recovery, or credential backup: it never
reads or restores Wi-Fi credentials, SSH material, tokens, certificates,
OS configuration, recordings, or diagnostic bundles.
*/
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/stratux/stratux/autorecord"
	"github.com/stratux/stratux/calprofile"
	"github.com/stratux/stratux/configbackup"
	"github.com/stratux/stratux/ota"
)

// configBackupTokenTTLSeconds bounds how long a validated preview's
// confirmation token remains usable - long enough to actually read a
// preview, short enough to bound how long a stale confirmation can matter.
const configBackupTokenTTLSeconds = 300

// configBackupMaxRequestBytes bounds an uploaded backup body - generous
// above configbackup.MaxDocumentBytes to allow for JSON whitespace/
// indentation plus the small confirmationToken wrapper field, while still
// rejecting anything resembling a bulk upload outright.
const configBackupMaxRequestBytes = configbackup.MaxDocumentBytes + 8192

type restoreLifecycleState string

const (
	restoreStateIdle         restoreLifecycleState = "idle"
	restoreStateValidating   restoreLifecycleState = "validating"
	restoreStatePreviewReady restoreLifecycleState = "preview-ready"
	restoreStateApplying     restoreLifecycleState = "applying"
	restoreStateVerifying    restoreLifecycleState = "verifying"
	restoreStateRollingBack  restoreLifecycleState = "rolling-back"
	restoreStateComplete     restoreLifecycleState = "complete"
	restoreStateFailed       restoreLifecycleState = "failed"
)

// configBackupPendingRestore is the one outstanding validated preview, if
// any. A new /validateConfigurationBackup call replaces it outright -
// there is deliberately never more than one live token, which is also
// how "prevent concurrent restore operations" is enforced at the
// validate/token layer (the recording/OTA/in-progress-restore guards
// below are the apply-time layer).
type configBackupPendingRestore struct {
	Token   string
	Record  configbackup.ConfirmationToken
	Preview configbackup.Preview
	Doc     configbackup.Document
}

// configBackupStatus is served by GET /getConfigurationRestoreStatus and
// embedded (via configBackupDiagnosticsSummary) in diagnostics bundles -
// never the backup content, calibration values, or credentials.
type configBackupStatus struct {
	State                   restoreLifecycleState `json:"state"`
	LastError               string                `json:"lastError,omitempty"`
	LastResult              *configBackupResult   `json:"lastResult,omitempty"`
	PreviewPending          bool                  `json:"previewPending"`
	PreviewExpiresInSeconds float64               `json:"previewExpiresInSeconds,omitempty"`
}

type configBackupResult struct {
	Success            bool       `json:"success"`
	CompletedAtUTC     *time.Time `json:"completedAtUtc,omitempty"`
	SectionsApplied    []string   `json:"sectionsApplied,omitempty"`
	SectionsRolledBack []string   `json:"sectionsRolledBack,omitempty"`
	FailureCategory    string     `json:"failureCategory,omitempty"`
	RestartRequired    bool       `json:"restartRequired"`
	RollbackFailed     bool       `json:"rollbackFailed,omitempty"`
}

var (
	// configBackupMu guards every field below - the restore-operation
	// state machine, the single outstanding confirmation token, and the
	// last completed/failed result. Never held during file I/O or a
	// calprofile.Store call (those have their own guards -
	// profilesMu/the store's internal mutex/alertSettingsMu) - only
	// while reading or updating this bookkeeping itself, matching this
	// project's existing lock-scoping convention (see
	// main/recordingapi.go's recMu doc comment).
	configBackupMu      sync.Mutex
	configBackupState   restoreLifecycleState = restoreStateIdle
	configBackupLastErr string
	configBackupPending *configBackupPendingRestore
	configBackupLast    *configBackupResult
	// configBackupRestoredThisBoot latches true the moment any restore
	// ever completes successfully and is never reset until the next
	// process start - see configBackupSnapshotForRecording.
	configBackupRestoredThisBoot bool
)

func monotonicSeconds() float64 {
	return stratuxClock.Time.Sub(time.Time{}).Seconds()
}

func newConfigBackupToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("configbackup: crypto/rand failed: " + err.Error())
	}
	return "cfgrestore-" + hex.EncodeToString(b[:])
}

// --- globalSettings <-> configbackup.ConfigurationSection -------------

// configurationSectionFromGlobalSettings reflects the device's actual
// current state, so PrivacySensitiveIncluded is always true here - the
// off-by-default opt-in only governs what a *portable export* discloses
// (see buildConfigBackupDocument), never what this function reports about
// the live device to itself (diffing, fingerprinting, rollback
// snapshots).
func configurationSectionFromGlobalSettings() configbackup.ConfigurationSection {
	return configbackup.ConfigurationSection{
		DarkMode:                 globalSettings.DarkMode,
		UATEnabled:               globalSettings.UAT_Enabled,
		ESEnabled:                globalSettings.ES_Enabled,
		OGNEnabled:               globalSettings.OGN_Enabled,
		APRSEnabled:              globalSettings.APRS_Enabled,
		AISEnabled:               globalSettings.AIS_Enabled,
		PingEnabled:              globalSettings.Ping_Enabled,
		PongEnabled:              globalSettings.Pong_Enabled,
		GPSEnabled:               globalSettings.GPS_Enabled,
		BMPSensorEnabled:         globalSettings.BMP_Sensor_Enabled,
		IMUSensorEnabled:         globalSettings.IMU_Sensor_Enabled,
		DisplayTrafficSource:     globalSettings.DisplayTrafficSource,
		PPM:                      globalSettings.PPM,
		Dump1090Gain:             globalSettings.Dump1090Gain,
		AltitudeOffset:           globalSettings.AltitudeOffset,
		GLimits:                  globalSettings.GLimits,
		EstimateBearinglessDist:  globalSettings.EstimateBearinglessDist,
		RadarLimits:              globalSettings.RadarLimits,
		RadarRange:               globalSettings.RadarRange,
		OGNI2CTXEnabled:          globalSettings.OGNI2CTXEnabled,
		OGNAddrType:              globalSettings.OGNAddrType,
		OGNAcftType:              globalSettings.OGNAcftType,
		OGNTxPower:               globalSettings.OGNTxPower,
		PWMDutyMin:               globalSettings.PWMDutyMin,
		GpsManualConfig:          globalSettings.GpsManualConfig,
		GpsManualDevice:          globalSettings.GpsManualDevice,
		GpsManualChip:            globalSettings.GpsManualChip,
		GpsManualTargetBaud:      globalSettings.GpsManualTargetBaud,
		RegionSelected:           globalSettings.RegionSelected,
		PrivacySensitiveIncluded: true,
		PrivacySensitive: configbackup.PrivacySensitiveSection{
			OwnshipModeS: globalSettings.OwnshipModeS,
			OGNAddr:      globalSettings.OGNAddr,
			OGNReg:       globalSettings.OGNReg,
			OGNPilot:     globalSettings.OGNPilot,
		},
	}
}

// applyConfigurationSectionToGlobalSettings overwrites every allowlisted
// field in globalSettings from c. Caller is responsible for calling
// saveSettings() afterward and for any locking this project's existing
// settings-mutation handlers already do (none, today - see
// docs/configuration-backup-restore.md's "Locking and concurrency" note
// on globalSettings' pre-existing lack of a dedicated mutex).
func applyConfigurationSectionToGlobalSettings(c configbackup.ConfigurationSection) {
	globalSettings.DarkMode = c.DarkMode
	globalSettings.UAT_Enabled = c.UATEnabled
	globalSettings.ES_Enabled = c.ESEnabled
	globalSettings.OGN_Enabled = c.OGNEnabled
	globalSettings.APRS_Enabled = c.APRSEnabled
	globalSettings.AIS_Enabled = c.AISEnabled
	globalSettings.Ping_Enabled = c.PingEnabled
	globalSettings.Pong_Enabled = c.PongEnabled
	globalSettings.GPS_Enabled = c.GPSEnabled
	globalSettings.BMP_Sensor_Enabled = c.BMPSensorEnabled
	globalSettings.IMU_Sensor_Enabled = c.IMUSensorEnabled
	globalSettings.DisplayTrafficSource = c.DisplayTrafficSource
	globalSettings.PPM = c.PPM
	globalSettings.Dump1090Gain = c.Dump1090Gain
	globalSettings.AltitudeOffset = c.AltitudeOffset
	globalSettings.GLimits = c.GLimits
	globalSettings.EstimateBearinglessDist = c.EstimateBearinglessDist
	globalSettings.RadarLimits = c.RadarLimits
	globalSettings.RadarRange = c.RadarRange
	globalSettings.OGNI2CTXEnabled = c.OGNI2CTXEnabled
	globalSettings.OGNAddrType = c.OGNAddrType
	globalSettings.OGNAcftType = c.OGNAcftType
	globalSettings.OGNTxPower = c.OGNTxPower
	globalSettings.PWMDutyMin = c.PWMDutyMin
	globalSettings.GpsManualConfig = c.GpsManualConfig
	globalSettings.GpsManualDevice = c.GpsManualDevice
	globalSettings.GpsManualChip = c.GpsManualChip
	globalSettings.GpsManualTargetBaud = c.GpsManualTargetBaud
	globalSettings.RegionSelected = c.RegionSelected
	// Privacy-sensitive fields are only ever applied when the document
	// deliberately included them (PrivacySensitiveIncluded) - an export
	// that omitted them (the default, sanitized case) must never clear or
	// overwrite the device's existing values. Rollback's own snapshot
	// (configurationSectionFromGlobalSettings) always has this true, so
	// rollback still fully restores privacy fields regardless of what the
	// failed backup claimed.
	if c.PrivacySensitiveIncluded {
		globalSettings.OwnshipModeS = c.PrivacySensitive.OwnshipModeS
		globalSettings.OGNAddr = c.PrivacySensitive.OGNAddr
		globalSettings.OGNReg = c.PrivacySensitive.OGNReg
		globalSettings.OGNPilot = c.PrivacySensitive.OGNPilot
	}
}

// --- AlertSettings <-> configbackup.AlertSettingsSection ---------------

func alertSettingsSectionFromCurrent(s AlertSettings) configbackup.AlertSettingsSection {
	return configbackup.AlertSettingsSection{
		MasterEnabled:                s.MasterEnabled,
		VisualTrafficNoticesEnabled:  s.VisualTrafficNoticesEnabled,
		BrowserAudioEnabled:          s.BrowserAudioEnabled,
		SystemVisualEnabled:          s.SystemVisualEnabled,
		SystemAudioEnabled:           s.SystemAudioEnabled,
		AudioVolume:                  s.AudioVolume,
		MonitoringHorizontalNM:       s.MonitoringHorizontalNM,
		MonitoringVerticalFeet:       s.MonitoringVerticalFeet,
		NoticeHorizontalNM:           s.NoticeHorizontalNM,
		NoticeVerticalFeet:           s.NoticeVerticalFeet,
		CautionHorizontalNM:          s.CautionHorizontalNM,
		CautionVerticalFeet:          s.CautionVerticalFeet,
		HighCautionHorizontalNM:      s.HighCautionHorizontalNM,
		HighCautionVerticalFeet:      s.HighCautionVerticalFeet,
		AudioCooldownNoticeSeconds:   s.AudioCooldownNoticeSeconds,
		AudioCooldownCautionSeconds:  s.AudioCooldownCautionSeconds,
		GlobalMinAudioSpacingSeconds: s.GlobalMinAudioSpacingSeconds,
		SuppressGroundTraffic:        s.SuppressGroundTraffic,
	}
}

// applyAlertSettingsSection returns base (the CURRENT settings - so
// SchemaVersion and Muted/MutedIndefinitely/MuteUntilUnixSeconds are
// preserved exactly as-is; mute state is deliberately never touched by
// restore, see configbackup.AlertSettingsSection's doc comment) with
// every durable field overwritten from section.
func applyAlertSettingsSection(base AlertSettings, section configbackup.AlertSettingsSection) AlertSettings {
	base.MasterEnabled = section.MasterEnabled
	base.VisualTrafficNoticesEnabled = section.VisualTrafficNoticesEnabled
	base.BrowserAudioEnabled = section.BrowserAudioEnabled
	base.SystemVisualEnabled = section.SystemVisualEnabled
	base.SystemAudioEnabled = section.SystemAudioEnabled
	base.AudioVolume = section.AudioVolume
	base.MonitoringHorizontalNM = section.MonitoringHorizontalNM
	base.MonitoringVerticalFeet = section.MonitoringVerticalFeet
	base.NoticeHorizontalNM = section.NoticeHorizontalNM
	base.NoticeVerticalFeet = section.NoticeVerticalFeet
	base.CautionHorizontalNM = section.CautionHorizontalNM
	base.CautionVerticalFeet = section.CautionVerticalFeet
	base.HighCautionHorizontalNM = section.HighCautionHorizontalNM
	base.HighCautionVerticalFeet = section.HighCautionVerticalFeet
	base.AudioCooldownNoticeSeconds = section.AudioCooldownNoticeSeconds
	base.AudioCooldownCautionSeconds = section.AudioCooldownCautionSeconds
	base.GlobalMinAudioSpacingSeconds = section.GlobalMinAudioSpacingSeconds
	base.SuppressGroundTraffic = section.SuppressGroundTraffic
	return base
}

// --- autorecord.Settings <-> configbackup.AutoRecordSettingsSection ----

func autoRecordSettingsSectionFromCurrent(s autorecord.Settings) configbackup.AutoRecordSettingsSection {
	return configbackup.AutoRecordSettingsSection{
		Enabled:                         s.Enabled,
		StartGroundspeedKnots:           s.StartGroundspeedKnots,
		StartDwellSeconds:               s.StartDwellSeconds,
		StopGroundspeedKnots:            s.StopGroundspeedKnots,
		StopDwellSeconds:                s.StopDwellSeconds,
		GPSLossGraceSeconds:             s.GPSLossGraceSeconds,
		RestartCooldownSeconds:          s.RestartCooldownSeconds,
		MinimumRecordingDurationSeconds: s.MinimumRecordingDurationSeconds,
	}
}

// applyAutoRecordSettingsSection returns base (the CURRENT settings - so
// SchemaVersion is preserved exactly as-is) with every field overwritten
// from section. Unlike applyAlertSettingsSection, there is no operational/
// time-bound field to exclude - see AutoRecordSettingsSection's own doc
// comment. Applying a restored section never changes any existing
// recording's own recorded origin (manual/automatic) - it only changes
// the going-forward configuration the Machine reads on its next tick.
func applyAutoRecordSettingsSection(base autorecord.Settings, section configbackup.AutoRecordSettingsSection) autorecord.Settings {
	base.Enabled = section.Enabled
	base.StartGroundspeedKnots = section.StartGroundspeedKnots
	base.StartDwellSeconds = section.StartDwellSeconds
	base.StopGroundspeedKnots = section.StopGroundspeedKnots
	base.StopDwellSeconds = section.StopDwellSeconds
	base.GPSLossGraceSeconds = section.GPSLossGraceSeconds
	base.RestartCooldownSeconds = section.RestartCooldownSeconds
	base.MinimumRecordingDurationSeconds = section.MinimumRecordingDurationSeconds
	return base
}

// --- FISBCacheSettings <-> configbackup.FISBCacheSettingsSection -------

func fisbCacheSettingsSectionFromCurrent(s FISBCacheSettings) configbackup.FISBCacheSettingsSection {
	return configbackup.FISBCacheSettingsSection{
		Enabled:            s.Enabled,
		PersistenceEnabled: s.PersistenceEnabled,
		ReplayEnabled:      s.ReplayEnabled,
		MaxCacheBytes:      s.MaxCacheBytes,
		MaxEntries:         s.MaxEntries,
	}
}

// applyFISBCacheSettingsSection returns base (the CURRENT settings - so
// SchemaVersion is preserved exactly as-is) with every field overwritten
// from section. Never touches the cache's own stored entries - this is
// settings only, mirroring applyAutoRecordSettingsSection exactly. A
// restored section carrying ReplayEnabled:true is still rejected by
// configbackup.Validate before this function is ever reached (see
// validateFISBCacheSettings), so this function itself does not need to
// re-check it.
func applyFISBCacheSettingsSection(base FISBCacheSettings, section configbackup.FISBCacheSettingsSection) FISBCacheSettings {
	base.Enabled = section.Enabled
	base.PersistenceEnabled = section.PersistenceEnabled
	base.ReplayEnabled = section.ReplayEnabled
	base.MaxCacheBytes = section.MaxCacheBytes
	base.MaxEntries = section.MaxEntries
	return base
}

// gatherConfigBackupCurrentState takes a coherent-enough snapshot of
// every section this subsystem exports/restores. It never holds
// profilesMu across the AlertSettings/globalSettings reads - only around
// the calprofile.Store calls themselves, matching every existing handler
// in main/calprofilesapi.go.
func gatherConfigBackupCurrentState() (configbackup.CurrentState, error) {
	profilesMu.Lock()
	store := profilesStore
	initErr := profilesInitError
	profilesMu.Unlock()
	if store == nil {
		return configbackup.CurrentState{}, fmt.Errorf("calibration-profile subsystem not initialized")
	}
	if initErr != nil {
		return configbackup.CurrentState{}, initErr
	}
	profiles, err := store.List()
	if err != nil {
		return configbackup.CurrentState{}, fmt.Errorf("listing calibration profiles: %w", err)
	}
	activeID, err := store.ActiveID()
	if err != nil {
		return configbackup.CurrentState{}, fmt.Errorf("reading active calibration profile: %w", err)
	}
	return configbackup.CurrentState{
		Version:             stratuxVersion,
		Commit:              stratuxBuild,
		Configuration:       configurationSectionFromGlobalSettings(),
		CalibrationProfiles: profiles,
		ActiveProfileID:     activeID,
		AlertSettings:       alertSettingsSectionFromCurrent(loadAlertSettings()),
		AutoRecordSettings:  autoRecordSettingsSectionFromCurrent(loadAutoRecordSettings()),
		FISBCacheSettings:   fisbCacheSettingsSectionFromCurrent(loadFISBCacheSettings()),
	}, nil
}

// configBackupCreatedAtUTC returns the current trusted (GNSS-corrected)
// UTC time, or nil if trusted time is not currently available - the same
// OptionalTime check main/preflightapi.go's buildPreflightReport already
// uses, so "no trusted time yet" is reported the same honest way
// everywhere in this codebase.
func configBackupCreatedAtUTC() *time.Time {
	globalHealthMutex.Lock()
	health := globalHealth
	globalHealthMutex.Unlock()
	if health.Time.CurrentUTC.Valid {
		t := health.Time.CurrentUTC.Time
		return &t
	}
	return nil
}

// buildConfigBackupDocument assembles a portable export. includePrivacySensitive
// is the owner's explicit, off-by-default opt-in (the dashboard's "Include
// aircraft/owner identification fields" checkbox, or
// ?includePrivacySensitive=true) - false is the ordinary, sanitized
// export: the privacy section is zeroed and marked not-included, never
// silently populated from the live device state
// (configurationSectionFromGlobalSettings always returns it populated,
// since that function separately serves the "what is the device's actual
// current state" question - see its own doc comment).
func buildConfigBackupDocument(includePrivacySensitive bool) (configbackup.Document, error) {
	current, err := gatherConfigBackupCurrentState()
	if err != nil {
		return configbackup.Document{}, err
	}
	cfg := current.Configuration
	if !includePrivacySensitive {
		cfg.PrivacySensitiveIncluded = false
		cfg.PrivacySensitive = configbackup.PrivacySensitiveSection{}
	}
	return configbackup.BuildDocument(configbackup.BuildInputs{
		SourceVersion:              current.Version,
		SourceCommit:               current.Commit,
		CreatedAtUTC:               configBackupCreatedAtUTC(),
		Configuration:              cfg,
		CalibrationProfiles:        current.CalibrationProfiles,
		ActiveCalibrationProfileID: current.ActiveProfileID,
		AlertSettings:              current.AlertSettings,
		AutoRecordSettings:         current.AutoRecordSettings,
		FISBCacheSettings:          current.FISBCacheSettings,
	})
}

// parseIncludePrivacySensitive reads the includePrivacySensitive query
// parameter: absent means false (the safe default - "absence of the
// option means false"); present, it must be exactly "true" or "false" -
// any other value is rejected rather than silently coerced, so a caller's
// typo can never accidentally enable it.
func parseIncludePrivacySensitive(r *http.Request) (bool, error) {
	v := r.URL.Query().Get("includePrivacySensitive")
	switch v {
	case "":
		return false, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("includePrivacySensitive must be \"true\" or \"false\", got %q", v)
	}
}

// --- HTTP handlers -------------------------------------------------

// handleDownloadConfigurationBackupRequest serves
// GET /downloadConfigurationBackup.
func handleDownloadConfigurationBackupRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	includePrivacy, err := parseIncludePrivacySensitive(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	doc, err := buildConfigBackupDocument(includePrivacy)
	if err != nil {
		http.Error(w, "could not build configuration backup: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		http.Error(w, "could not encode configuration backup", http.StatusInternalServerError)
		return
	}
	setNoCache(w)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", configbackup.SanitizedFilename(includePrivacy)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// readBoundedBody reads at most limit+1 bytes so an oversized body is
// detected exactly (not masked by a reader that silently truncates).
func readBoundedBody(r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// handleValidateConfigurationBackupRequest serves
// POST /validateConfigurationBackup. Performs no writes - see
// configbackup.Validate's own doc comment.
func handleValidateConfigurationBackupRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := readBoundedBody(r, configBackupMaxRequestBytes)
	if err != nil {
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > configBackupMaxRequestBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	var doc configbackup.Document
	if err := json.Unmarshal(body, &doc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "malformed JSON: " + err.Error()})
		return
	}

	res := configbackup.Validate(doc, len(body))
	if !res.OK() {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "errors": res.Errors, "warnings": res.Warnings})
		return
	}
	// Only after Validate has reported doc OK: fill in any historical-
	// shape-only defaulting (currently: a verified pre-autoRecordSettings
	// legacy document's AutoRecordSettings, defaulted disabled) so the
	// preview shown below reflects what will actually be applied, not a
	// hysteresis-violating zero value. A no-op for every current-format
	// document. doc.ContentChecksum/SectionChecksums (used below for the
	// confirmation token) are untouched by this - they still name
	// exactly the bytes the caller uploaded.
	doc = configbackup.NormalizeDocument(doc)

	current, err := gatherConfigBackupCurrentState()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	preview := configbackup.ComputePreview(doc, current)
	preview.Warnings = append(preview.Warnings, res.Warnings...)

	fingerprint, err := configbackup.Fingerprint(current)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": "could not fingerprint current configuration"})
		return
	}

	now := monotonicSeconds()
	record := configbackup.ConfirmationToken{
		ContentChecksum:         doc.ContentChecksum,
		CurrentStateFingerprint: fingerprint,
		BootSessionID:           preflightSessionID,
		IssuedAtMonotonic:       now,
		ExpiresAtMonotonic:      now + configBackupTokenTTLSeconds,
	}
	token := newConfigBackupToken()

	configBackupMu.Lock()
	if configBackupState == restoreStateApplying || configBackupState == restoreStateVerifying || configBackupState == restoreStateRollingBack {
		configBackupMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]interface{}{"success": false, "error": "a restore is currently being applied"})
		return
	}
	configBackupPending = &configBackupPendingRestore{Token: token, Record: record, Preview: preview, Doc: doc}
	configBackupState = restoreStatePreviewReady
	configBackupMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":           true,
		"preview":           preview,
		"confirmationToken": token,
		"expiresInSeconds":  configBackupTokenTTLSeconds,
	})
}

// configBackupApplyRequest is the body POST /applyConfigurationBackup
// expects: the confirmation token plus the exact same backup document
// validated to obtain it (re-uploaded, not cached by token alone) - see
// docs/configuration-backup-restore.md's "Confirmation token" section for
// why apply re-checks the content rather than trusting the token in
// isolation.
type configBackupApplyRequest struct {
	ConfirmationToken string                `json:"confirmationToken"`
	Backup            configbackup.Document `json:"backup"`
}

// handleApplyConfigurationBackupRequest serves
// POST /applyConfigurationBackup.
func handleApplyConfigurationBackupRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	recMu.Lock()
	recordingActive := recCurrent != nil && recCurrent.State == recordingStateActive
	recMu.Unlock()
	if recordingActive {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"success": false, "error": "cannot restore configuration while a recording is active"})
		return
	}
	// ota.Stage.Terminal() only reports true for StageComplete/
	// StageRolledBack (a *finished* update) - StageIdle ("no update ever
	// staged") is a separate, equally-safe state Terminal() does not
	// cover, so "an OTA update is genuinely in progress" is neither idle
	// nor terminal.
	if otaState, err := ota.LoadState(otaDir); err == nil && otaState.Stage != ota.StageIdle && !otaState.Stage.Terminal() {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"success": false, "error": "cannot restore configuration while an OTA update is in progress"})
		return
	}

	configBackupMu.Lock()
	if configBackupState == restoreStateApplying || configBackupState == restoreStateVerifying || configBackupState == restoreStateRollingBack {
		configBackupMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]interface{}{"success": false, "error": "a restore is already in progress"})
		return
	}
	pending := configBackupPending
	configBackupMu.Unlock()

	body, err := readBoundedBody(r, configBackupMaxRequestBytes)
	if err != nil {
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > configBackupMaxRequestBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req configBackupApplyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "malformed JSON: " + err.Error()})
		return
	}
	if req.ConfirmationToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "confirmationToken is required"})
		return
	}
	if pending == nil || pending.Token != req.ConfirmationToken {
		writeJSON(w, http.StatusGone, map[string]interface{}{"success": false, "error": "confirmation token not found or expired"})
		return
	}

	// Revalidate the complete re-uploaded document from scratch - never
	// trust the token alone, and never trust the preview computed at
	// validate time as still describing reality.
	res := configbackup.Validate(req.Backup, len(mustMarshalForApply(req.Backup)))
	if !res.OK() {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "errors": res.Errors})
		return
	}
	// Same historical-shape defaulting as the validate/preview path,
	// applied here too so a legacy document actually being applied gets
	// the same disabled default rather than a hysteresis-violating zero
	// value. req.Backup.ContentChecksum (used by VerifyToken below) is
	// untouched - it still names exactly the bytes that were validated
	// and bound into the confirmation token.
	req.Backup = configbackup.NormalizeDocument(req.Backup)

	current, err := gatherConfigBackupCurrentState()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	fingerprint, err := configbackup.Fingerprint(current)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": "could not fingerprint current configuration"})
		return
	}

	if err := configbackup.VerifyToken(pending.Record, req.Backup.ContentChecksum, fingerprint, preflightSessionID, monotonicSeconds()); err != nil {
		status := http.StatusConflict
		switch err {
		case configbackup.ErrTokenExpired, configbackup.ErrTokenAlreadyUsed:
			status = http.StatusGone
		}
		writeJSON(w, status, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}

	// Pre-flight environment check - never start mutating persistent
	// state on storage that can't reliably accept the write. Reuses the
	// same swappable check main/recordingapi.go's startRecordingLocked
	// already relies on (see availablePersistentBytes's doc comment).
	if _, err := availablePersistentBytes(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "persistent storage is not writable: " + err.Error()})
		return
	}

	configBackupMu.Lock()
	if configBackupState == restoreStateApplying || configBackupState == restoreStateVerifying || configBackupState == restoreStateRollingBack {
		configBackupMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]interface{}{"success": false, "error": "a restore is already in progress"})
		return
	}
	configBackupState = restoreStateApplying
	configBackupMu.Unlock()

	sectionsApplied, rollbackAttempted, rollbackFailed, applyErr := applyConfigBackupTransaction(req.Backup)

	now := time.Now().UTC()
	result := &configBackupResult{
		CompletedAtUTC:  &now,
		SectionsApplied: sectionsApplied,
		RestartRequired: pending.Preview.RestartRequired,
	}

	configBackupMu.Lock()
	defer configBackupMu.Unlock()
	// Mark the token used regardless of outcome - a failed apply must
	// never be retryable with the same token (the caller must validate
	// again against then-current state).
	pending.Record.Used = true
	if configBackupPending != nil && configBackupPending.Token == pending.Token {
		configBackupPending.Record.Used = true
	}

	if applyErr == nil {
		configBackupState = restoreStateComplete
		configBackupLastErr = ""
		configBackupRestoredThisBoot = true
		result.Success = true
		configBackupLast = result
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "result": result})
		return
	}

	result.Success = false
	result.FailureCategory = applyErr.Error()
	if rollbackAttempted {
		result.SectionsRolledBack = sectionsApplied
	}
	result.RollbackFailed = rollbackFailed
	configBackupLast = result
	configBackupLastErr = applyErr.Error()
	if rollbackFailed {
		configBackupState = restoreStateFailed
		log.Printf("configbackup: APPLY FAILED AND ROLLBACK FAILED - manual recovery may be required: %s\n", applyErr)
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   "restore failed and automatic rollback also failed - manual recovery may be required",
			"detail":  applyErr.Error(),
			"result":  result,
		})
		return
	}
	configBackupState = restoreStateFailed
	log.Printf("configbackup: restore failed, rolled back cleanly: %s\n", applyErr)
	writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
		"success": false,
		"error":   "restore failed and was fully rolled back: " + applyErr.Error(),
		"result":  result,
	})
}

func mustMarshalForApply(doc configbackup.Document) []byte {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	return b
}

// applyConfigBackupTransaction performs the transactional apply described
// in docs/configuration-backup-restore.md's "Transactional restore"
// section. On any failure it attempts a full rollback to the exact
// pre-apply snapshot and reports whether that rollback itself succeeded -
// it never claims success after a partial application.
func applyConfigBackupTransaction(doc configbackup.Document) (sectionsApplied []string, rollbackAttempted bool, rollbackFailed bool, err error) {
	profilesMu.Lock()
	store := profilesStore
	initErr := profilesInitError
	profilesMu.Unlock()
	if store == nil {
		return nil, false, false, fmt.Errorf("calibration-profile subsystem not initialized")
	}
	if initErr != nil {
		return nil, false, false, initErr
	}

	profilesMu.Lock()
	originalProfiles, listErr := store.List()
	originalActiveID, activeErr := store.ActiveID()
	profilesMu.Unlock()
	if listErr != nil {
		return nil, false, false, fmt.Errorf("reading current calibration profiles: %w", listErr)
	}
	if activeErr != nil {
		return nil, false, false, fmt.Errorf("reading active calibration profile: %w", activeErr)
	}
	originalByID := make(map[string]calprofile.Profile, len(originalProfiles))
	for _, p := range originalProfiles {
		originalByID[p.ID] = p
	}
	originalConfig := configurationSectionFromGlobalSettings()
	originalAlertSettings := loadAlertSettings()
	originalAutoRecordSettings := loadAutoRecordSettings()
	originalFISBCacheSettings := loadFISBCacheSettings()

	rollback := func() bool {
		clean := true
		profilesMu.Lock()
		if originalActiveID != "" {
			if err := store.SetActiveID(originalActiveID, time.Now().UTC()); err != nil {
				clean = false
				log.Printf("configbackup: rollback could not restore active profile pointer: %s\n", err)
			}
		}
		for _, p := range doc.CalibrationProfiles {
			if orig, existed := originalByID[p.ID]; existed {
				if err := store.Save(orig); err != nil {
					clean = false
					log.Printf("configbackup: rollback could not restore profile %s: %s\n", p.ID, err)
				}
			} else {
				if err := store.Delete(p.ID); err != nil {
					clean = false
					log.Printf("configbackup: rollback could not remove newly-added profile %s: %s\n", p.ID, err)
				}
			}
		}
		applyConfigurationSectionToGlobalSettings(originalConfig)
		if active, aerr := store.Active(); aerr == nil {
			applyProfileToGlobalSettingsLocked(active)
		}
		saveSettings()
		profilesMu.Unlock()
		if err := saveAlertSettings(originalAlertSettings); err != nil {
			clean = false
			log.Printf("configbackup: rollback could not restore alert settings: %s\n", err)
		}
		if err := saveAutoRecordSettings(originalAutoRecordSettings); err != nil {
			clean = false
			log.Printf("configbackup: rollback could not restore automatic-recording settings: %s\n", err)
		}
		if err := saveFISBCacheSettings(originalFISBCacheSettings); err != nil {
			clean = false
			log.Printf("configbackup: rollback could not restore fisb weather cache settings: %s\n", err)
		} else {
			fisbCacheMu.Lock()
			fisbCacheSettingsCache = originalFISBCacheSettings
			fisbCacheMu.Unlock()
		}
		return clean
	}

	// Alert settings first - its own atomic write, cheapest to detect
	// failure on before touching calibration profiles at all.
	newAlertSettings := applyAlertSettingsSection(originalAlertSettings, doc.AlertSettings)
	if err := saveAlertSettings(newAlertSettings); err != nil {
		return nil, false, false, fmt.Errorf("applying alert settings: %w", err)
	}
	sectionsApplied = append(sectionsApplied, "alertSettings")

	// Automatic-recording settings - same atomic-write pattern. Applying
	// this never changes any existing recording's own recorded origin
	// (manual/automatic), only the going-forward configuration the
	// Machine reads on its next detection tick.
	newAutoRecordSettings := applyAutoRecordSettingsSection(originalAutoRecordSettings, doc.AutoRecordSettings)
	if err := saveAutoRecordSettings(newAutoRecordSettings); err != nil {
		clean := rollback()
		return sectionsApplied, true, !clean, fmt.Errorf("applying automatic-recording settings: %w", err)
	}
	autoRecordMu.Lock()
	autoRecordSettingsCache = newAutoRecordSettings
	autoRecordMu.Unlock()
	sectionsApplied = append(sectionsApplied, "autoRecordSettings")

	// Rolling FIS-B Weather Cache settings - same atomic-write pattern.
	// Applying this never touches the cache's own stored entries, only
	// the going-forward configuration (see applyFISBCacheSettingsSection).
	newFISBCacheSettings := applyFISBCacheSettingsSection(originalFISBCacheSettings, doc.FISBCacheSettings)
	if err := saveFISBCacheSettings(newFISBCacheSettings); err != nil {
		clean := rollback()
		return sectionsApplied, true, !clean, fmt.Errorf("applying fisb weather cache settings: %w", err)
	}
	fisbCacheMu.Lock()
	fisbCacheSettingsCache = newFISBCacheSettings
	fisbCacheMu.Unlock()
	sectionsApplied = append(sectionsApplied, "fisbCacheSettings")

	profilesMu.Lock()
	for _, p := range doc.CalibrationProfiles {
		if err := store.Save(p); err != nil {
			profilesMu.Unlock()
			clean := rollback()
			return sectionsApplied, true, !clean, fmt.Errorf("saving calibration profile %s: %w", p.ID, err)
		}
	}
	if len(doc.CalibrationProfiles) > 0 {
		sectionsApplied = append(sectionsApplied, "calibrationProfiles")
	}

	applyConfigurationSectionToGlobalSettings(doc.Configuration)
	sectionsApplied = append(sectionsApplied, "configuration")

	if doc.ActiveCalibrationProfileID != "" && doc.ActiveCalibrationProfileID != originalActiveID {
		target, err := store.Get(doc.ActiveCalibrationProfileID)
		if err != nil {
			profilesMu.Unlock()
			clean := rollback()
			return sectionsApplied, true, !clean, fmt.Errorf("resolving restored active profile: %w", err)
		}
		if err := store.SetActiveID(target.ID, time.Now().UTC()); err != nil {
			profilesMu.Unlock()
			clean := rollback()
			return sectionsApplied, true, !clean, fmt.Errorf("activating restored profile: %w", err)
		}
		applyProfileToGlobalSettingsLocked(target)
		sectionsApplied = append(sectionsApplied, "activeCalibrationProfile")
	}

	saveSettings()
	profilesMu.Unlock()

	return sectionsApplied, false, false, nil
}

// handleGetConfigurationRestoreStatusRequest serves
// GET /getConfigurationRestoreStatus.
func handleGetConfigurationRestoreStatusRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, configBackupStatusSnapshot())
}

func configBackupStatusSnapshot() configBackupStatus {
	configBackupMu.Lock()
	defer configBackupMu.Unlock()
	status := configBackupStatus{
		State:      configBackupState,
		LastError:  configBackupLastErr,
		LastResult: configBackupLast,
	}
	if configBackupPending != nil && !configBackupPending.Record.Used {
		remaining := configBackupPending.Record.ExpiresAtMonotonic - monotonicSeconds()
		if remaining > 0 {
			status.PreviewPending = true
			status.PreviewExpiresInSeconds = remaining
		}
	}
	return status
}

// configBackupSnapshotForRecording returns the small, session-level
// fingerprint main/recordingmetadataapi.go's buildSessionSnapshot embeds
// in a recording's SessionSnapshot - see recording.SessionSnapshot's
// ConfigBackup* fields. fingerprint is best-effort: an error gathering
// current state (subsystem not yet initialized, most likely at very
// early startup) yields an empty fingerprint rather than blocking
// recording start on this subsystem.
func configBackupSnapshotForRecording() (schemaVersion int, fingerprint string, restoredThisBoot bool) {
	schemaVersion = configbackup.SchemaVersion
	configBackupMu.Lock()
	restoredThisBoot = configBackupRestoredThisBoot
	configBackupMu.Unlock()
	current, err := gatherConfigBackupCurrentState()
	if err != nil {
		return schemaVersion, "", restoredThisBoot
	}
	fp, err := configbackup.Fingerprint(current)
	if err != nil {
		return schemaVersion, "", restoredThisBoot
	}
	return schemaVersion, fp, restoredThisBoot
}

// configBackupDiagnosticsSummary returns a bounded, sanitized summary for
// diagnostics bundles - see readiness/diagnostics.go's
// DiagnosticBundle.ConfigBackupSummary. Never includes backup contents,
// calibration values, credentials, or filesystem paths.
func configBackupDiagnosticsSummary() interface{} {
	profilesMu.Lock()
	available := profilesStore != nil && profilesInitError == nil
	profilesMu.Unlock()

	status := configBackupStatusSnapshot()
	summary := map[string]interface{}{
		"available":        available,
		"schemaVersion":    configbackup.SchemaVersion,
		"lastRestoreState": status.State,
	}
	if status.LastResult != nil {
		summary["lastRestoreSuccess"] = status.LastResult.Success
		summary["lastRestoreSectionsApplied"] = status.LastResult.SectionsApplied
		summary["lastRestoreSectionsRolledBack"] = status.LastResult.SectionsRolledBack
		summary["lastRestoreRestartRequired"] = status.LastResult.RestartRequired
		if status.LastResult.FailureCategory != "" {
			summary["lastRestoreFailureCategory"] = status.LastResult.FailureCategory
		}
		if status.LastResult.RollbackFailed {
			summary["lastRestoreRollbackFailed"] = true
		}
	}
	return summary
}
