package configbackup

import (
	"sort"
	"time"

	"github.com/stratux/stratux/calprofile"
)

/*
legacy.go: narrow, finite backward-compatibility for restoring a
Configuration Backup document created by an earlier, exact, known
historical version of this package's own checksum schema - never a
general "guess the shape" or "skip missing checksums" mechanism.

Today this covers exactly two historical layouts, both schema 2, the
only Configuration Backup shapes that have ever existed on this
project's master branch:

 1. preAutoRecord: from PR #9 (this package's original merge) through
    commit 5b8509fc (PR #13's merge, immediately before Automatic
    Flight Recording added the autoRecordSettings section). Missing
    both autoRecordSettings and fisbCacheSettings. (alertSettings was
    present from this package's very first commit - there is no
    even-older "before alertSettings" schema-2 shape to support.)
 2. preFISBCache: from commit 5b8509fc through commit 83a20a8c (PR #14's
    merge, immediately before the Rolling FIS-B Weather Cache added the
    fisbCacheSettings section). Has autoRecordSettings, missing
    fisbCacheSettings.

A schema-1 document is not covered: schema 1 was superseded by a
deliberate, documented, non-additive break before this feature ever
shipped a real document (see SchemaVersion's own doc comment), and
MinimumCompatibleSchemaVersion already rejects it before any of this
file's logic runs.

Design (see docs/configuration-backup-restore.md's "Legacy backup
compatibility" section for the full rationale):

 1. Validate first checks the ordinary structural/schema/size gates.
 2. It then asks whether doc's checksums are EXACTLY consistent with
    having been produced by one of these known historical shapes - not
    "close enough," not "missing some checksums so let's skip those":
    checked newest-to-oldest (preFISBCache, then preAutoRecord) so a
    genuine preAutoRecord document (whose checksum key set is a strict
    subset of preFISBCache's) is never mistaken for the newer shape.
    For whichever shape matches, its own section-checksum key set must
    equal doc's key set exactly, every section absent from that
    historical shape must be exactly its zero value, and every checksum
    (each section's, and the whole historical-shape document's own)
    must be recomputed from doc's OTHER fields using that historical
    shape's exact serialization and must match exactly. Any mismatch
    anywhere means doc is not that historical shape - either the other
    historical shape, a genuine current-format document (handled by the
    existing, unchanged current-shape verification), or corrupt/
    tampered, and must be rejected exactly as before.
 3. Only once verification against one shape - historical or current -
    has actually succeeded does normalizeLegacyDocument fill each
    now-known-absent section with the same safe, disabled default a
    live device with that feature never configured would already
    report - never the bare Go zero value, which (all thresholds at 0,
    or all cache limits at 0) would itself fail this package's own
    semantic hysteresis/bounds checks, and every ordinary legacy backup
    would have hit this same trap.

This never weakens verification of a genuinely current-format document:
that document's own section-checksum key set includes
"fisbCacheSettings", which does not equal either historical set, so it
is never even considered for either historical path - it always goes
through the existing, unchanged rebuild-and-compare in Validate.
*/

// legacyPreAutoRecordSectionKeys is the exact, frozen section-checksum
// key set the pre-autoRecordSettings schema-2 format always produced -
// used to recognize a candidate historical document by exact key-set
// equality (never a subset/superset match) before attempting the more
// expensive checksum re-derivation.
var legacyPreAutoRecordSectionKeys = map[string]bool{
	"configuration":       true,
	"calibrationProfiles": true,
	"alertSettings":       true,
}

// legacyDocumentV2PreAutoRecord mirrors this package's own Document
// exactly as it existed at commit 5b8509fc - frozen permanently once
// written, never updated to track later changes to Document itself
// (that would defeat its entire purpose: reproducing what THAT commit's
// code actually serialized). ConfigurationSection, AlertSettingsSection,
// and calprofile.Profile are reused directly (unchanged since that
// commit); only Document's own top-level shape (the presence/absence of
// AutoRecordSettings) differs.
type legacyDocumentV2PreAutoRecord struct {
	SchemaVersion int `json:"schemaVersion"`

	CreatedAtUTC *time.Time `json:"createdAtUTC,omitempty"`

	SourceVersion            string `json:"sourceVersion"`
	SourceCommit             string `json:"sourceCommit"`
	MinimumCompatibleVersion int    `json:"minimumCompatibleVersion"`

	Configuration              ConfigurationSection `json:"configuration"`
	CalibrationProfiles        []calprofile.Profile `json:"calibrationProfiles"`
	ActiveCalibrationProfileID string               `json:"activeCalibrationProfileId,omitempty"`
	AlertSettings              AlertSettingsSection `json:"alertSettings"`

	SectionChecksums map[string]string `json:"sectionChecksums"`
	ContentChecksum  string            `json:"contentChecksum"`
}

// legacyProfilesSectionPayload mirrors profilesSectionPayload exactly -
// a distinct, frozen type (rather than reusing profilesSectionPayload
// directly) so this file's historical-shape reproduction never silently
// starts sharing a definition with, and being affected by, any future
// change to the current-shape payload type.
type legacyProfilesSectionPayload struct {
	Profiles []calprofile.Profile `json:"profiles"`
	ActiveID string               `json:"activeId"`
}

// legacyDefaultAutoRecordSettings is the value normalizeLegacyDocument
// fills in for a verified-historical document's missing
// autoRecordSettings section - deliberately the same disabled/default
// values as autorecord.DefaultSettings() (this package cannot import
// autorecord - see the package doc comment's leaf-dependency note - so
// these are independently restated; main/'s glue cross-checks them
// against the real package in a test so the two can never silently
// drift apart - see main/configbackupapi_test.go). Never the bare Go
// zero value: an all-zero AutoRecordSettingsSection has
// StartGroundspeedKnots == StopGroundspeedKnots == 0, which itself
// fails validateAutoRecordSettings' strict hysteresis check - exactly
// the trap a genuinely absent section must not fall into.
var legacyDefaultAutoRecordSettings = AutoRecordSettingsSection{
	Enabled:                         false,
	StartGroundspeedKnots:           8,
	StartDwellSeconds:               30,
	StopGroundspeedKnots:            4,
	StopDwellSeconds:                120,
	GPSLossGraceSeconds:             30,
	RestartCooldownSeconds:          300,
	MinimumRecordingDurationSeconds: 0,
}

// legacyPreFISBCacheSectionKeys is the exact, frozen section-checksum key
// set the pre-fisbCacheSettings schema-2 format always produced (commit
// 5b8509fc through commit 83a20a8c) - used to recognize a candidate
// historical document by exact key-set equality (never a subset/superset
// match) before attempting the more expensive checksum re-derivation.
var legacyPreFISBCacheSectionKeys = map[string]bool{
	"configuration":       true,
	"calibrationProfiles": true,
	"alertSettings":       true,
	"autoRecordSettings":  true,
}

// legacyDocumentV2PreFISBCache mirrors this package's own Document
// exactly as it existed at commit 83a20a8c - frozen permanently once
// written, never updated to track later changes to Document itself, for
// the same reason legacyDocumentV2PreAutoRecord is frozen. Only
// Document's own top-level shape (the presence/absence of
// FISBCacheSettings) differs from the current type.
type legacyDocumentV2PreFISBCache struct {
	SchemaVersion int `json:"schemaVersion"`

	CreatedAtUTC *time.Time `json:"createdAtUTC,omitempty"`

	SourceVersion            string `json:"sourceVersion"`
	SourceCommit             string `json:"sourceCommit"`
	MinimumCompatibleVersion int    `json:"minimumCompatibleVersion"`

	Configuration              ConfigurationSection      `json:"configuration"`
	CalibrationProfiles        []calprofile.Profile      `json:"calibrationProfiles"`
	ActiveCalibrationProfileID string                    `json:"activeCalibrationProfileId,omitempty"`
	AlertSettings              AlertSettingsSection      `json:"alertSettings"`
	AutoRecordSettings         AutoRecordSettingsSection `json:"autoRecordSettings"`

	SectionChecksums map[string]string `json:"sectionChecksums"`
	ContentChecksum  string            `json:"contentChecksum"`
}

// legacyDefaultFISBCacheSettings is the value normalizeLegacyDocument
// fills in for a verified-historical document's missing
// fisbCacheSettings section - deliberately the same disabled/default
// values as main.DefaultFISBCacheSettings() (this package cannot import
// main - see the package doc comment's leaf-dependency note - so these
// are independently restated; main/'s glue cross-checks them against the
// real settings default in a test, mirroring
// TestLegacyDefaultAutoRecordSettingsMatchesAutoRecordPackageDefault).
// Never the bare Go zero value: an all-zero FISBCacheSettingsSection has
// MaxCacheBytes == MaxEntries == 0, which itself fails
// validateFISBCacheSettings' bounds check - exactly the trap a genuinely
// absent section must not fall into.
var legacyDefaultFISBCacheSettings = FISBCacheSettingsSection{
	Enabled:            false,
	PersistenceEnabled: false,
	ReplayEnabled:      false,
	MaxCacheBytes:      16 * 1024 * 1024,
	MaxEntries:         2000,
}

// verifyLegacyPreFISBCacheChecksum reports whether doc's checksums are
// exactly consistent with having been produced by the pre-
// fisbCacheSettings schema-2 code - see this file's doc comment. Never a
// partial or best-effort match: any discrepancy anywhere returns false.
func verifyLegacyPreFISBCacheChecksum(doc Document) bool {
	if len(doc.SectionChecksums) != len(legacyPreFISBCacheSectionKeys) {
		return false
	}
	for k := range doc.SectionChecksums {
		if !legacyPreFISBCacheSectionKeys[k] {
			return false
		}
	}
	// A document that explicitly carries a non-zero fisbCacheSettings
	// section while also lacking its checksum is not honestly
	// historical - it is corrupt or tampered, and must be rejected, not
	// silently accepted with the extra data discarded.
	if doc.FISBCacheSettings != (FISBCacheSettingsSection{}) {
		return false
	}

	profiles := make([]calprofile.Profile, len(doc.CalibrationProfiles))
	copy(profiles, doc.CalibrationProfiles)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })

	legacy := legacyDocumentV2PreFISBCache{
		SchemaVersion:              doc.SchemaVersion,
		CreatedAtUTC:               doc.CreatedAtUTC,
		SourceVersion:              doc.SourceVersion,
		SourceCommit:               doc.SourceCommit,
		MinimumCompatibleVersion:   doc.MinimumCompatibleVersion,
		Configuration:              doc.Configuration,
		CalibrationProfiles:        profiles,
		ActiveCalibrationProfileID: doc.ActiveCalibrationProfileID,
		AlertSettings:              doc.AlertSettings,
		AutoRecordSettings:         doc.AutoRecordSettings,
	}

	cfgSum, err := sectionChecksum(legacy.Configuration)
	if err != nil {
		return false
	}
	profSum, err := sectionChecksum(legacyProfilesSectionPayload{Profiles: profiles, ActiveID: legacy.ActiveCalibrationProfileID})
	if err != nil {
		return false
	}
	alertSum, err := sectionChecksum(legacy.AlertSettings)
	if err != nil {
		return false
	}
	autoRecordSum, err := sectionChecksum(legacy.AutoRecordSettings)
	if err != nil {
		return false
	}
	want := map[string]string{
		"configuration":       cfgSum,
		"calibrationProfiles": profSum,
		"alertSettings":       alertSum,
		"autoRecordSettings":  autoRecordSum,
	}
	for k, w := range want {
		if doc.SectionChecksums[k] != w {
			return false
		}
	}

	legacy.SectionChecksums = want
	legacy.ContentChecksum = ""
	contentSum, err := sectionChecksum(legacy)
	if err != nil {
		return false
	}
	return contentSum == doc.ContentChecksum
}

// verifyLegacyPreAutoRecordChecksum reports whether doc's checksums are
// exactly consistent with having been produced by the pre-
// autoRecordSettings schema-2 code - see this file's doc comment. Never
// a partial or best-effort match: any discrepancy anywhere returns
// false, leaving doc's rejection (or its evaluation against the current
// shape) entirely to Validate's existing, unchanged logic.
func verifyLegacyPreAutoRecordChecksum(doc Document) bool {
	if len(doc.SectionChecksums) != len(legacyPreAutoRecordSectionKeys) {
		return false
	}
	for k := range doc.SectionChecksums {
		if !legacyPreAutoRecordSectionKeys[k] {
			return false
		}
	}
	// A document that explicitly carries a non-zero autoRecordSettings
	// and/or fisbCacheSettings section while also lacking its checksum
	// is not honestly historical - it is corrupt or tampered, and must
	// be rejected, not silently accepted with the extra data discarded.
	if doc.AutoRecordSettings != (AutoRecordSettingsSection{}) || doc.FISBCacheSettings != (FISBCacheSettingsSection{}) {
		return false
	}

	profiles := make([]calprofile.Profile, len(doc.CalibrationProfiles))
	copy(profiles, doc.CalibrationProfiles)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })

	legacy := legacyDocumentV2PreAutoRecord{
		SchemaVersion:              doc.SchemaVersion,
		CreatedAtUTC:               doc.CreatedAtUTC,
		SourceVersion:              doc.SourceVersion,
		SourceCommit:               doc.SourceCommit,
		MinimumCompatibleVersion:   doc.MinimumCompatibleVersion,
		Configuration:              doc.Configuration,
		CalibrationProfiles:        profiles,
		ActiveCalibrationProfileID: doc.ActiveCalibrationProfileID,
		AlertSettings:              doc.AlertSettings,
	}

	cfgSum, err := sectionChecksum(legacy.Configuration)
	if err != nil {
		return false
	}
	profSum, err := sectionChecksum(legacyProfilesSectionPayload{Profiles: profiles, ActiveID: legacy.ActiveCalibrationProfileID})
	if err != nil {
		return false
	}
	alertSum, err := sectionChecksum(legacy.AlertSettings)
	if err != nil {
		return false
	}
	want := map[string]string{
		"configuration":       cfgSum,
		"calibrationProfiles": profSum,
		"alertSettings":       alertSum,
	}
	for k, w := range want {
		if doc.SectionChecksums[k] != w {
			return false
		}
	}

	legacy.SectionChecksums = want
	legacy.ContentChecksum = ""
	contentSum, err := sectionChecksum(legacy)
	if err != nil {
		return false
	}
	return contentSum == doc.ContentChecksum
}

// normalizeLegacyDocument returns doc unchanged if it does not match
// either known historical shape, or doc with the section(s) that shape
// lacks filled in from the corresponding legacy default(s) if it does.
// Checked newest-to-oldest (preFISBCache, then preAutoRecord) since a
// genuine preAutoRecord document's checksum key set is a strict subset
// of preFISBCache's and must never be mistaken for it. Callers must call
// this (or NormalizeDocument) only after Validate has already reported
// doc OK - never before, and never as a substitute for verification.
func normalizeLegacyDocument(doc Document) (normalized Document, wasLegacy bool) {
	if verifyLegacyPreFISBCacheChecksum(doc) {
		doc.FISBCacheSettings = legacyDefaultFISBCacheSettings
		return doc, true
	}
	if verifyLegacyPreAutoRecordChecksum(doc) {
		doc.AutoRecordSettings = legacyDefaultAutoRecordSettings
		doc.FISBCacheSettings = legacyDefaultFISBCacheSettings
		return doc, true
	}
	return doc, false
}

// LegacyDefaultAutoRecordSettings exposes legacyDefaultAutoRecordSettings
// for cross-checking against autorecord.DefaultSettings() (this package
// cannot import autorecord itself - see the package doc comment's leaf-
// dependency note) - see main/configbackupapi_test.go's
// TestLegacyDefaultAutoRecordSettingsMatchesAutoRecordPackageDefault.
// Not used by this package's own normal restore path (which uses the
// unexported value directly); exported solely for that one test.
func LegacyDefaultAutoRecordSettings() AutoRecordSettingsSection {
	return legacyDefaultAutoRecordSettings
}

// LegacyDefaultFISBCacheSettings exposes legacyDefaultFISBCacheSettings
// for cross-checking against main.DefaultFISBCacheSettings() (this
// package cannot import main itself - see the package doc comment's
// leaf-dependency note), mirroring LegacyDefaultAutoRecordSettings's
// identical purpose. Not used by this package's own normal restore path
// (which uses the unexported value directly); exported solely for that
// one cross-check test.
func LegacyDefaultFISBCacheSettings() FISBCacheSettingsSection {
	return legacyDefaultFISBCacheSettings
}

// NormalizeDocument returns doc with any historical-shape-only
// defaulting applied (currently: a verified pre-autoRecordSettings
// and/or pre-fisbCacheSettings document's missing section(s) filled in
// as disabled with standard default thresholds/limits). Callers (main/'s
// glue) must call this exactly once, immediately after Validate(doc,
// ...) has reported doc OK, before using doc for ComputePreview or
// applying it - never before Validate has succeeded. A no-op (returns
// doc unchanged) for every current-format document.
func NormalizeDocument(doc Document) Document {
	normalized, _ := normalizeLegacyDocument(doc)
	return normalized
}
