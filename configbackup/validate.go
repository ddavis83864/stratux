package configbackup

import (
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/stratux/stratux/calprofile"
)

// ValidationResult is Validate's pure output. It never mutates anything
// and never performs I/O - see the package doc comment.
//
// A checksum failure documented here detects corruption, truncation, or
// accidental/incidental modification in transit - it is NOT
// authentication and proves nothing about who produced the file. This
// package has no notion of a trusted signer; do not treat a passing
// checksum as proof of origin.
type ValidationResult struct {
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// OK reports whether doc has no blocking errors.
func (r ValidationResult) OK() bool { return len(r.Errors) == 0 }

func (r *ValidationResult) addErrorf(format string, args ...interface{}) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

func (r *ValidationResult) addWarning(w string) {
	r.Warnings = append(r.Warnings, w)
}

// runeLen is utf8.RuneCountInString, named to make every length check
// below visibly unicode-safe.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// Validate checks rawSize (the exact byte length of the uploaded/
// downloaded JSON) and doc for every structural, size, checksum, and
// content-bound requirement this package enforces. It performs no writes
// and calls no other package's mutating APIs - safe to call as many times
// as needed (initial validate, re-validation immediately before apply).
//
// rawSize is passed separately (not measured from a re-marshaled doc)
// because the caller already has the exact uploaded byte count and
// re-marshaling could mask an oversized-but-otherwise-valid document.
func Validate(doc Document, rawSize int) ValidationResult {
	var res ValidationResult

	if rawSize > MaxDocumentBytes {
		res.addErrorf("%s: %d bytes exceeds the %d byte maximum", ErrTooLarge, rawSize, MaxDocumentBytes)
		return res
	}
	if doc.SchemaVersion < MinimumCompatibleSchemaVersion || doc.SchemaVersion > SchemaVersion {
		res.addErrorf("%s: schema %d, this build supports %d-%d", ErrUnsupportedSchema, doc.SchemaVersion, MinimumCompatibleSchemaVersion, SchemaVersion)
		return res
	}

	// Before the general current-shape rebuild-and-compare below, ask
	// whether doc's checksums are EXACTLY consistent with the one known
	// historical pre-autoRecordSettings shape (see legacy.go) - never a
	// partial/best-effort match. If they are, that IS this call's
	// checksum verification (doc was never touched by
	// autoRecordSettings-aware code, so re-deriving checksums against
	// the CURRENT shape below would spuriously fail); skip straight to
	// semantic validation, using a normalized copy with
	// AutoRecordSettings defaulted (disabled) rather than the
	// hysteresis-violating Go zero value. If they are not, doc is
	// either a genuine current-format document or corrupt/tampered -
	// either way, the existing rebuild-and-compare below decides,
	// completely unchanged.
	if normalized, ok := normalizeLegacyDocument(doc); ok {
		doc = normalized
	} else {
		// Recompute every checksum from doc's own content and compare -
		// before trusting anything else in the document. A mismatch here
		// means the document was corrupted, truncated, or edited outside
		// this package's own export path; stop rather than partially
		// validate content that may not even be self-consistent.
		rebuilt, err := BuildDocument(BuildInputs{
			SourceVersion:              doc.SourceVersion,
			SourceCommit:               doc.SourceCommit,
			CreatedAtUTC:               doc.CreatedAtUTC,
			Configuration:              doc.Configuration,
			CalibrationProfiles:        doc.CalibrationProfiles,
			ActiveCalibrationProfileID: doc.ActiveCalibrationProfileID,
			AlertSettings:              doc.AlertSettings,
			AutoRecordSettings:         doc.AutoRecordSettings,
		})
		if err != nil {
			res.addErrorf("configbackup: could not verify checksums: %s", err)
			return res
		}
		if doc.ContentChecksum == "" {
			res.addErrorf("%s: missing contentChecksum", ErrChecksumMismatch)
		} else if rebuilt.ContentChecksum != doc.ContentChecksum {
			res.addErrorf("%s: whole-document checksum", ErrChecksumMismatch)
		}
		for section, want := range rebuilt.SectionChecksums {
			got, present := doc.SectionChecksums[section]
			if !present {
				res.addErrorf("%s: missing checksum for section %q", ErrChecksumMismatch, section)
				continue
			}
			if got != want {
				res.addErrorf("%s: section %q", ErrChecksumMismatch, section)
			}
		}
		if !res.OK() {
			// Checksums didn't match at all - every further "content"
			// check below would be validating data we already know is
			// inconsistent with its own declared checksum. Stop here.
			return res
		}
	}

	validateConfiguration(doc.Configuration, &res)
	validateAlertSettings(doc.AlertSettings, &res)
	validateAutoRecordSettings(doc.AutoRecordSettings, &res)
	validateProfiles(doc.CalibrationProfiles, doc.ActiveCalibrationProfileID, &res)

	// A document cannot honestly disclaim "no privacy-sensitive data" while
	// actually carrying it - reject outright rather than warn, since a
	// producer that gets this wrong (or a document edited to lie about it)
	// could cause those values to leak through a path that trusts the flag.
	if !doc.Configuration.PrivacySensitiveIncluded && !doc.Configuration.PrivacySensitive.Empty() {
		res.addErrorf("%s", ErrPrivacySectionMismatch)
	} else if doc.Configuration.PrivacySensitiveIncluded {
		res.addWarning("backup includes privacy-sensitive ownship-identifying fields (ownship Mode S address, and/or OGN/FLARM address, registration, or pilot name) - restore only a backup you trust")
	}

	return res
}

func validateConfiguration(c ConfigurationSection, res *ValidationResult) {
	if runeLen(c.GLimits) > MaxShortStringRunes {
		res.addErrorf("%s: gLimits exceeds %d characters", ErrInvalidField, MaxShortStringRunes)
	}
	if runeLen(c.GpsManualDevice) > MaxShortStringRunes {
		res.addErrorf("%s: gpsManualDevice exceeds %d characters", ErrInvalidField, MaxShortStringRunes)
	}
	if runeLen(c.GpsManualChip) > MaxShortStringRunes {
		res.addErrorf("%s: gpsManualChip exceeds %d characters", ErrInvalidField, MaxShortStringRunes)
	}
	if !finite(c.Dump1090Gain) {
		res.addErrorf("%s: dump1090Gain must be finite", ErrInvalidField)
	}
	if c.GpsManualTargetBaud < 0 || c.GpsManualTargetBaud > 4000000 {
		res.addErrorf("%s: gpsManualTargetBaud out of range", ErrInvalidField)
	}
	if c.RegionSelected < 0 || c.RegionSelected > 2 {
		res.addErrorf("%s: regionSelected must be 0 (none), 1 (US), or 2 (EU)", ErrInvalidField)
	}
	if c.PWMDutyMin < 0 || c.PWMDutyMin > 100 {
		res.addErrorf("%s: pwmDutyMin must be 0-100", ErrInvalidField)
	}
	if c.OGNAddrType < 0 || c.OGNAddrType > 3 {
		res.addErrorf("%s: ognAddrType out of range", ErrInvalidField)
	}
	if c.OGNTxPower < 0 || c.OGNTxPower > 100 {
		res.addErrorf("%s: ognTxPower out of range", ErrInvalidField)
	}

	p := c.PrivacySensitive
	for name, v := range map[string]string{
		"ownshipModeS": p.OwnshipModeS,
		"ognAddr":      p.OGNAddr,
		"ognReg":       p.OGNReg,
		"ognPilot":     p.OGNPilot,
	} {
		if runeLen(v) > MaxLongStringRunes {
			res.addErrorf("%s: privacySensitive.%s exceeds %d characters", ErrInvalidField, name, MaxLongStringRunes)
		}
	}
}

func validateAlertSettings(a AlertSettingsSection, res *ValidationResult) {
	if !finite(a.AudioVolume) || a.AudioVolume < 0 || a.AudioVolume > 1 {
		res.addErrorf("%s: alertSettings.audioVolume must be 0.0-1.0", ErrInvalidField)
	}
	numeric := map[string]float64{
		"monitoringHorizontalNm":       a.MonitoringHorizontalNM,
		"monitoringVerticalFeet":       a.MonitoringVerticalFeet,
		"noticeHorizontalNm":           a.NoticeHorizontalNM,
		"noticeVerticalFeet":           a.NoticeVerticalFeet,
		"cautionHorizontalNm":          a.CautionHorizontalNM,
		"cautionVerticalFeet":          a.CautionVerticalFeet,
		"highCautionHorizontalNm":      a.HighCautionHorizontalNM,
		"highCautionVerticalFeet":      a.HighCautionVerticalFeet,
		"audioCooldownNoticeSeconds":   a.AudioCooldownNoticeSeconds,
		"audioCooldownCautionSeconds":  a.AudioCooldownCautionSeconds,
		"globalMinAudioSpacingSeconds": a.GlobalMinAudioSpacingSeconds,
	}
	for name, v := range numeric {
		if !finite(v) {
			res.addErrorf("%s: alertSettings.%s must be finite", ErrInvalidField, name)
		} else if v < 0 {
			res.addErrorf("%s: alertSettings.%s must not be negative", ErrInvalidField, name)
		}
	}
}

// validateAutoRecordSettings mirrors autorecord.Settings.Validate's own
// bound/hysteresis rules - this package cannot import autorecord (leaf-
// dependency direction), so the rules are independently re-checked here
// against the same documented bounds, exactly as validateAlertSettings
// re-checks AlertSettings.Validate's rules.
func validateAutoRecordSettings(a AutoRecordSettingsSection, res *ValidationResult) {
	if a == (AutoRecordSettingsSection{}) {
		// An entirely zero-valued section - never explicitly configured
		// (e.g. a caller/test that built a Document without populating
		// AutoRecordSettings at all, relying on Go's zero value; a
		// document verified as the pre-autoRecordSettings historical
		// shape is normalized to legacyDefaultAutoRecordSettings before
		// this function ever runs, so it never reaches this branch).
		// Enabled is necessarily false here (it's part of the zero
		// value itself), so there is no live threshold configuration to
		// validate - skip the bounds/hysteresis checks below, which an
		// all-zero value can never satisfy (0 is not strictly less than
		// 0) despite posing no actual risk: a disabled feature never
		// reads these thresholds. Any OTHER value - including
		// Enabled:true with all-zero thresholds - is not equal to the
		// zero value and still goes through full validation below.
		return
	}
	speeds := map[string]float64{
		"autoRecordSettings.startGroundspeedKnots": a.StartGroundspeedKnots,
		"autoRecordSettings.stopGroundspeedKnots":  a.StopGroundspeedKnots,
	}
	for name, v := range speeds {
		if !finite(v) || v < 0 || v > 500 {
			res.addErrorf("%s: %s must be between 0 and 500 knots", ErrInvalidField, name)
		}
	}
	durations := map[string]float64{
		"autoRecordSettings.startDwellSeconds":               a.StartDwellSeconds,
		"autoRecordSettings.stopDwellSeconds":                a.StopDwellSeconds,
		"autoRecordSettings.gpsLossGraceSeconds":             a.GPSLossGraceSeconds,
		"autoRecordSettings.restartCooldownSeconds":          a.RestartCooldownSeconds,
		"autoRecordSettings.minimumRecordingDurationSeconds": a.MinimumRecordingDurationSeconds,
	}
	for name, v := range durations {
		if !finite(v) || v < 0 || v > 2*60*60 {
			res.addErrorf("%s: %s must be between 0 and 7200 seconds", ErrInvalidField, name)
		}
	}
	if a.StopGroundspeedKnots >= a.StartGroundspeedKnots {
		res.addErrorf("%s: autoRecordSettings.stopGroundspeedKnots must be strictly lower than startGroundspeedKnots", ErrInvalidField)
	}
}

func validateProfiles(profiles []calprofile.Profile, activeID string, res *ValidationResult) {
	if len(profiles) > MaxProfiles {
		res.addErrorf("%s: %d profiles exceeds the %d maximum", ErrTooManyProfiles, len(profiles), MaxProfiles)
		return
	}
	seen := make(map[string]bool, len(profiles))
	for _, p := range profiles {
		if !calprofile.ValidID(p.ID) {
			res.addErrorf("%s: invalid calibration profile id %q", ErrInvalidField, p.ID)
			continue
		}
		if seen[p.ID] {
			res.addErrorf("%s: %q", ErrDuplicateProfileID, p.ID)
			continue
		}
		seen[p.ID] = true
		if err := calprofile.ValidateProfile(p); err != nil {
			res.addErrorf("calibration profile %s: %s", p.ID, err)
		}
	}
	if activeID != "" && !seen[activeID] {
		res.addErrorf("%s: %q", ErrDanglingActiveProfile, activeID)
	}
}
