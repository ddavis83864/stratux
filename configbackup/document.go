// Package configbackup implements a bounded, versioned, checksummed
// configuration backup-and-restore document for Stratux's supported
// application settings.
//
// This is supported-application-configuration backup, not SD-card
// imaging, filesystem cloning, operating-system recovery, or credential
// backup. A backup document never contains Wi-Fi credentials, SSH
// material, tokens, certificates, operating-system configuration,
// recordings, or diagnostic bundles - see docs/configuration-backup-
// restore.md's "Exported sections" / "Excluded sections" tables for the
// full, explicit allowlist this package's types enforce by construction
// (there is no general-purpose "export everything" path).
//
// Like alerting/readiness/recording/calprofile, this package is pure:
// every function here takes already-gathered values and returns a
// derived document, validation result, diff, or token judgment - no file
// I/O, no HTTP, no locks, no clock reads beyond an explicit parameter.
// The thin glue that reads globalSettings/AlertSettings/calprofile.Store,
// serves HTTP, and owns the in-memory restore-operation state lives in
// main/configbackupapi.go.
//
// Checksum limitation: every SHA-256 in a Document (SectionChecksums,
// ContentChecksum) detects accidental corruption and incomplete
// modification - a truncated download, a byte flipped in transit, a
// half-written file. It is NOT authentication and provides NO protection
// against deliberate modification: anyone able to edit the document can
// recompute a matching checksum for their edited content just as this
// package does, so a passing checksum proves the document is internally
// self-consistent, never that it came from a trusted source or wasn't
// deliberately altered. This package implements no signing, encryption,
// or key management. Restore only a backup you trust.
package configbackup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/stratux/stratux/calprofile"
)

// SchemaVersion is the current backup document schema. Bump this and add
// an explicit compatibility/migration note whenever the document shape
// changes in a way an older reader could misinterpret.
//
// Schema 2 (current) changed the meaning of the privacy-sensitive fields:
// schema 1 always populated them; schema 2 adds the explicit
// PrivacySensitiveIncluded flag and requires it be true before those
// fields may be treated as anything but absent (see
// ConfigurationSection.PrivacySensitiveIncluded and Validate). This is a
// deliberate, documented, non-additive break, made while this feature was
// still unmerged and had never shipped a schema-1 document from a real
// release - MinimumCompatibleSchemaVersion is raised alongside it rather
// than carrying schema 1's unsafe-by-default meaning forward.
const SchemaVersion = 2

// MinimumCompatibleSchemaVersion is the oldest schemaVersion this build
// will accept for restore. Bump only alongside a deliberate, documented
// migration path - never silently.
const MinimumCompatibleSchemaVersion = 2

// Bounds enforced by Validate. All deliberately conservative: this
// document holds small, bounded configuration, never bulk data.
const (
	// MaxDocumentBytes bounds the raw uploaded/downloaded JSON. Chosen
	// generously above any realistic configuration size (a handful of
	// settings plus up to MaxProfiles calibration profiles) while still
	// ruling out anything resembling an accidental or malicious bulk
	// upload.
	MaxDocumentBytes = 262144 // 256 KiB

	// MaxProfiles mirrors calprofile.Store's own cap - a backup can never
	// describe more profiles than the store could ever hold.
	MaxProfiles = calprofile.MaxProfiles

	// MaxShortStringRunes/MaxLongStringRunes bound the free-text
	// configuration fields this package carries (GPS device/chip names,
	// ownship identifiers). Calibration-profile string fields are
	// instead bounded by calprofile.ValidateProfile's own limits.
	MaxShortStringRunes = 32
	MaxLongStringRunes  = 128
)

// Sentinel errors, wrapped with context by Validate - callers can
// distinguish categories with errors.Is without string-matching.
var (
	ErrTooLarge               = errors.New("configbackup: document exceeds maximum size")
	ErrUnsupportedSchema      = errors.New("configbackup: unsupported schema version")
	ErrChecksumMismatch       = errors.New("configbackup: checksum mismatch")
	ErrInvalidField           = errors.New("configbackup: invalid field")
	ErrTooManyProfiles        = errors.New("configbackup: too many calibration profiles")
	ErrDuplicateProfileID     = errors.New("configbackup: duplicate calibration profile id")
	ErrDanglingActiveProfile  = errors.New("configbackup: active profile id does not match any included profile")
	ErrPrivacySectionMismatch = errors.New("configbackup: privacy-sensitive fields present but privacySensitiveIncluded is false")
)

// PrivacySensitiveSection holds ownship-identifying fields that are
// technically supported application configuration but can identify the
// aircraft or pilot. Excluded by default - see
// ConfigurationSection.PrivacySensitiveIncluded, the explicit,
// off-by-default opt-in an export must set before this section carries
// real values. When it is included, every non-empty field here is always
// called out distinctly by name in Preview, never silently folded into an
// undifferentiated settings diff, so an owner never restores them without
// seeing them named.
type PrivacySensitiveSection struct {
	// OwnshipModeS is globalSettings.OwnshipModeS - the ICAO/Mode S
	// address this receiver treats as its own aircraft for self-alert
	// exclusion.
	OwnshipModeS string `json:"ownshipModeS,omitempty"`
	// OGNAddr/OGNReg/OGNPilot are the external-tracker (OGN/FLARM-style)
	// address, aircraft registration, and pilot name - see
	// globalSettings' "External Tracker config" fields.
	OGNAddr  string `json:"ognAddr,omitempty"`
	OGNReg   string `json:"ognReg,omitempty"`
	OGNPilot string `json:"ognPilot,omitempty"`
}

// Empty reports whether no privacy-sensitive field is set.
func (p PrivacySensitiveSection) Empty() bool {
	return p.OwnshipModeS == "" && p.OGNAddr == "" && p.OGNReg == "" && p.OGNPilot == ""
}

// ConfigurationSection is the explicit allowlist of core application
// settings this package will export and restore - never the complete
// settings struct. Every field here is named after, and holds exactly the
// same meaning as, the corresponding globalSettings field in
// main/gen_gdl90.go; main/configbackupapi.go is responsible for the
// field-by-field copy in both directions since this package cannot import
// main.
//
// Deliberately excluded (see docs/configuration-backup-restore.md for the
// full inventory and rationale): Wi-Fi/network/BLE/serial output
// configuration (network/credential surface, out of scope); debug/
// developer-mode toggles (DEBUG, ReplayLog, TraceLog, AHRSLog,
// PersistentLogging, ClearLogOnStart, DeveloperMode); the legacy top-level
// IMUMapping/SensorQuaternion/C/D calibration fields (superseded by
// CalibrationProfiles - restoring both would create two sources of
// truth); PersistentDataUUID (pins this specific device's storage and
// must never travel in a portable backup); WatchList (free-text, may
// reference other aircraft).
type ConfigurationSection struct {
	DarkMode             bool `json:"darkMode"`
	UATEnabled           bool `json:"uatEnabled"`
	ESEnabled            bool `json:"esEnabled"`
	OGNEnabled           bool `json:"ognEnabled"`
	APRSEnabled          bool `json:"aprsEnabled"`
	AISEnabled           bool `json:"aisEnabled"`
	PingEnabled          bool `json:"pingEnabled"`
	PongEnabled          bool `json:"pongEnabled"`
	GPSEnabled           bool `json:"gpsEnabled"`
	BMPSensorEnabled     bool `json:"bmpSensorEnabled"`
	IMUSensorEnabled     bool `json:"imuSensorEnabled"`
	DisplayTrafficSource bool `json:"displayTrafficSource"`

	PPM            int     `json:"ppm"`
	Dump1090Gain   float64 `json:"dump1090Gain"`
	AltitudeOffset int     `json:"altitudeOffset"`
	GLimits        string  `json:"gLimits"`

	EstimateBearinglessDist bool `json:"estimateBearinglessDist"`
	RadarLimits             int  `json:"radarLimits"`
	RadarRange              int  `json:"radarRange"`

	OGNI2CTXEnabled bool `json:"ognI2cTxEnabled"`
	OGNAddrType     int  `json:"ognAddrType"`
	OGNAcftType     int  `json:"ognAcftType"`
	OGNTxPower      int  `json:"ognTxPower"`

	PWMDutyMin int `json:"pwmDutyMin"`

	GpsManualConfig     bool   `json:"gpsManualConfig"`
	GpsManualDevice     string `json:"gpsManualDevice"`
	GpsManualChip       string `json:"gpsManualChip"`
	GpsManualTargetBaud int    `json:"gpsManualTargetBaud"`

	RegionSelected int `json:"regionSelected"`

	// PrivacySensitiveIncluded records whether PrivacySensitive was
	// deliberately captured for this export - the owner-facing "Include
	// aircraft/owner identification fields" checkbox, off by default.
	// False is the ordinary, sanitized export: PrivacySensitive is the
	// zero value, and restore must treat the section as *absent*
	// (preserve whatever the device already has), never as "the owner
	// wants these fields cleared." True means PrivacySensitive holds the
	// real values (which may themselves still be empty strings, if the
	// device simply has none set - that is a real, deliberately-captured
	// empty, not an omission) and restore may propose changing them, with
	// every affected field named in preview. See Validate, which rejects
	// a document claiming false while PrivacySensitive is non-empty - an
	// export cannot honestly disclaim inclusion while leaking the values.
	PrivacySensitiveIncluded bool                    `json:"privacySensitiveIncluded"`
	PrivacySensitive         PrivacySensitiveSection `json:"privacySensitive"`
}

// AlertSettingsSection mirrors main.AlertSettings's own persisted fields,
// EXCEPT SchemaVersion (this document has its own) and Muted/
// MutedIndefinitely/MuteUntilUnixSeconds. Mute state is deliberately
// excluded even though it is technically persisted across restarts: it is
// operational/time-bound, not durable configuration, and silently
// re-muting alerts from a stale backup is a real safety footgun - see
// docs/configuration-backup-restore.md's "Excluded: mute state".
type AlertSettingsSection struct {
	MasterEnabled               bool `json:"masterEnabled"`
	VisualTrafficNoticesEnabled bool `json:"visualTrafficNoticesEnabled"`
	BrowserAudioEnabled         bool `json:"browserAudioEnabled"`
	SystemVisualEnabled         bool `json:"systemVisualEnabled"`
	SystemAudioEnabled          bool `json:"systemAudioEnabled"`

	AudioVolume float64 `json:"audioVolume"`

	MonitoringHorizontalNM  float64 `json:"monitoringHorizontalNm"`
	MonitoringVerticalFeet  float64 `json:"monitoringVerticalFeet"`
	NoticeHorizontalNM      float64 `json:"noticeHorizontalNm"`
	NoticeVerticalFeet      float64 `json:"noticeVerticalFeet"`
	CautionHorizontalNM     float64 `json:"cautionHorizontalNm"`
	CautionVerticalFeet     float64 `json:"cautionVerticalFeet"`
	HighCautionHorizontalNM float64 `json:"highCautionHorizontalNm"`
	HighCautionVerticalFeet float64 `json:"highCautionVerticalFeet"`

	AudioCooldownNoticeSeconds   float64 `json:"audioCooldownNoticeSeconds"`
	AudioCooldownCautionSeconds  float64 `json:"audioCooldownCautionSeconds"`
	GlobalMinAudioSpacingSeconds float64 `json:"globalMinAudioSpacingSeconds"`

	SuppressGroundTraffic bool `json:"suppressGroundTraffic"`
}

// AutoRecordSettingsSection mirrors autorecord.Settings's own persisted
// fields, except SchemaVersion (this document has its own). Unlike
// AlertSettingsSection's excluded mute state, Automatic Flight Recording
// has no comparable operational/time-bound field to exclude - every
// field here is durable configuration, and a restore never changes any
// existing recording's own recorded origin (manual/automatic), only the
// going-forward configuration.
type AutoRecordSettingsSection struct {
	Enabled                         bool    `json:"enabled"`
	StartGroundspeedKnots           float64 `json:"startGroundspeedKnots"`
	StartDwellSeconds               float64 `json:"startDwellSeconds"`
	StopGroundspeedKnots            float64 `json:"stopGroundspeedKnots"`
	StopDwellSeconds                float64 `json:"stopDwellSeconds"`
	GPSLossGraceSeconds             float64 `json:"gpsLossGraceSeconds"`
	RestartCooldownSeconds          float64 `json:"restartCooldownSeconds"`
	MinimumRecordingDurationSeconds float64 `json:"minimumRecordingDurationSeconds"`
}

// FISBCacheSettingsSection mirrors main.FISBCacheSettings' own persisted
// fields, except SchemaVersion (this document has its own). Like
// AutoRecordSettingsSection, every field here is durable configuration -
// there is no operational/time-bound field to exclude. A restore never
// touches the cache's own stored entries (this section is settings only,
// never cache contents), and ReplayEnabled is carried through unchanged
// even though this build's own settings API always rejects true for it -
// see main.FISBCacheSettings.Validate's doc comment - so a backup taken
// on a future build that DOES support replay is not silently altered by
// an older build's restore path; Validate below still rejects
// ReplayEnabled:true exactly as the live settings API does.
type FISBCacheSettingsSection struct {
	Enabled            bool  `json:"enabled"`
	PersistenceEnabled bool  `json:"persistenceEnabled"`
	ReplayEnabled      bool  `json:"replayEnabled"`
	MaxCacheBytes      int64 `json:"maxCacheBytes"`
	MaxEntries         int   `json:"maxEntries"`
}

// Document is the complete, portable configuration backup.
type Document struct {
	SchemaVersion int `json:"schemaVersion"`

	// CreatedAtUTC is nil - honestly absent, never a fabricated or
	// wall-clock-only guess - when trusted (GNSS-corrected) time was not
	// available at export time. See docs/readiness-and-time-trust.md.
	CreatedAtUTC *time.Time `json:"createdAtUTC,omitempty"`

	SourceVersion            string `json:"sourceVersion"`
	SourceCommit             string `json:"sourceCommit"`
	MinimumCompatibleVersion int    `json:"minimumCompatibleVersion"`

	Configuration              ConfigurationSection      `json:"configuration"`
	CalibrationProfiles        []calprofile.Profile      `json:"calibrationProfiles"`
	ActiveCalibrationProfileID string                    `json:"activeCalibrationProfileId,omitempty"`
	AlertSettings              AlertSettingsSection      `json:"alertSettings"`
	AutoRecordSettings         AutoRecordSettingsSection `json:"autoRecordSettings"`
	FISBCacheSettings          FISBCacheSettingsSection  `json:"fisbCacheSettings"`

	// SectionChecksums/ContentChecksum detect accidental corruption and
	// incomplete modification (a truncated download, a flipped byte, a
	// half-written file) - they are NOT authentication and provide NO
	// protection against deliberate modification: anyone editing this
	// document can recompute a matching checksum for their edit, exactly
	// as this package does. Restore only a backup you trust. See the
	// package doc comment and Validate's doc comment.
	SectionChecksums map[string]string `json:"sectionChecksums"`
	ContentChecksum  string            `json:"contentChecksum"`
}

// BuildInputs is everything the caller (main/) must gather to build a
// Document. Every field is an already-loaded value - this function
// performs no I/O.
type BuildInputs struct {
	SourceVersion string
	SourceCommit  string
	// CreatedAtUTC should be nil when trusted time is unavailable - see
	// Document.CreatedAtUTC.
	CreatedAtUTC *time.Time

	Configuration              ConfigurationSection
	CalibrationProfiles        []calprofile.Profile
	ActiveCalibrationProfileID string
	AlertSettings              AlertSettingsSection
	AutoRecordSettings         AutoRecordSettingsSection
	FISBCacheSettings          FISBCacheSettingsSection
}

// sectionChecksum returns the hex SHA-256 of v's canonical JSON encoding.
// encoding/json's struct-field order is the declaration order (stable
// across calls in the same build) and its map-key order is always
// sorted, so this is deterministic for every type this package checksums.
func sectionChecksum(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// profilesSectionPayload is the exact shape checksummed for the combined
// "calibration profiles + active pointer" section - kept as one section
// since an active pointer is meaningless without the profile list it
// resolves against.
type profilesSectionPayload struct {
	Profiles []calprofile.Profile `json:"profiles"`
	ActiveID string               `json:"activeId"`
}

// BuildDocument assembles and checksums a Document from already-gathered
// inputs. Profiles are sorted by ID for deterministic output (two exports
// of the same underlying state, taken via calprofile.Store.List in
// whatever order the filesystem returned, must produce byte-identical
// documents and checksums).
func BuildDocument(in BuildInputs) (Document, error) {
	profiles := make([]calprofile.Profile, len(in.CalibrationProfiles))
	copy(profiles, in.CalibrationProfiles)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })

	doc := Document{
		SchemaVersion:              SchemaVersion,
		CreatedAtUTC:               in.CreatedAtUTC,
		SourceVersion:              in.SourceVersion,
		SourceCommit:               in.SourceCommit,
		MinimumCompatibleVersion:   MinimumCompatibleSchemaVersion,
		Configuration:              in.Configuration,
		CalibrationProfiles:        profiles,
		ActiveCalibrationProfileID: in.ActiveCalibrationProfileID,
		AlertSettings:              in.AlertSettings,
		AutoRecordSettings:         in.AutoRecordSettings,
		FISBCacheSettings:          in.FISBCacheSettings,
	}

	cfgSum, err := sectionChecksum(doc.Configuration)
	if err != nil {
		return Document{}, fmt.Errorf("configbackup: checksumming configuration: %w", err)
	}
	profSum, err := sectionChecksum(profilesSectionPayload{Profiles: profiles, ActiveID: doc.ActiveCalibrationProfileID})
	if err != nil {
		return Document{}, fmt.Errorf("configbackup: checksumming calibration profiles: %w", err)
	}
	alertSum, err := sectionChecksum(doc.AlertSettings)
	if err != nil {
		return Document{}, fmt.Errorf("configbackup: checksumming alert settings: %w", err)
	}
	autoRecordSum, err := sectionChecksum(doc.AutoRecordSettings)
	if err != nil {
		return Document{}, fmt.Errorf("configbackup: checksumming automatic-recording settings: %w", err)
	}
	fisbCacheSum, err := sectionChecksum(doc.FISBCacheSettings)
	if err != nil {
		return Document{}, fmt.Errorf("configbackup: checksumming fisb weather cache settings: %w", err)
	}
	doc.SectionChecksums = map[string]string{
		"configuration":       cfgSum,
		"calibrationProfiles": profSum,
		"alertSettings":       alertSum,
		"autoRecordSettings":  autoRecordSum,
		"fisbCacheSettings":   fisbCacheSum,
	}

	contentSum, err := contentChecksum(doc)
	if err != nil {
		return Document{}, fmt.Errorf("configbackup: checksumming document: %w", err)
	}
	doc.ContentChecksum = contentSum
	return doc, nil
}

// contentChecksum hashes doc's canonical JSON with ContentChecksum itself
// cleared first, so the checksum never references itself.
func contentChecksum(doc Document) (string, error) {
	doc.ContentChecksum = ""
	return sectionChecksum(doc)
}

// SanitizedFilename returns the fixed, safe filename this document should
// always be downloaded/uploaded as - never derived from any user- or
// backup-supplied string. includesPrivacySensitive should match exactly
// the document's own PrivacySensitiveIncluded, so a privacy-inclusive
// export is distinguishable at a glance (and therefore handled with more
// care) without the filename itself containing any identifying value.
func SanitizedFilename(includesPrivacySensitive bool) string {
	if includesPrivacySensitive {
		return "stratux-configuration-backup-with-identifiers.json"
	}
	return "stratux-configuration-backup.json"
}
