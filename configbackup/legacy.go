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

Today this covers exactly one historical layout: schema 2 as it existed
from PR #9 (this package's original merge) through commit 5b8509fc (PR
#13's merge, immediately before Automatic Flight Recording added the
autoRecordSettings section) - the only Configuration Backup shape that
has ever existed on this project's master branch. (alertSettings was
present from this package's very first commit - there is no even-older
"before alertSettings" schema-2 shape to support.) A schema-1 document
is not covered: schema 1 was superseded by a deliberate, documented,
non-additive break before this feature ever shipped a real document
(see SchemaVersion's own doc comment), and MinimumCompatibleSchemaVersion
already rejects it before any of this file's logic runs.

Design (see docs/configuration-backup-restore.md's "Legacy backup
compatibility" section for the full rationale):

 1. Validate first checks the ordinary structural/schema/size gates.
 2. It then asks whether doc's checksums are EXACTLY consistent with
    having been produced by this one known historical shape - not
    "close enough," not "missing some checksums so let's skip those":
    the historical shape's own section-checksum key set must match
    doc's key set exactly, AutoRecordSettings must be exactly its zero
    value, and every checksum (each section's, and the whole
    historical-shape document's own) must be recomputed from doc's
    OTHER fields using that historical shape's exact serialization and
    must match exactly. Any mismatch anywhere means doc is not this
    historical shape - it either is a current-format document (handled
    by the existing, unchanged current-shape verification) or it is
    corrupt/tampered, and must be rejected exactly as before.
 3. Only once verification against one shape - historical or current -
    has actually succeeded does normalizeLegacyDocument fill the
    now-known-absent autoRecordSettings section with the same safe,
    disabled default a live device with the feature never configured
    would already report - never the bare Go zero value, which (all
    thresholds at 0) would itself fail this package's own semantic
    hysteresis check, and every ordinary legacy backup would have hit
    this same trap.

This never weakens verification of a genuinely current-format document:
that document's own section-checksum key set includes
"autoRecordSettings", which does not equal the historical set, so it is
never even considered for the historical path - it always goes through
the existing, unchanged rebuild-and-compare in Validate.
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
	// section while also lacking its checksum is not honestly
	// historical - it is corrupt or tampered, and must be rejected, not
	// silently accepted with the extra data discarded.
	if doc.AutoRecordSettings != (AutoRecordSettingsSection{}) {
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

// normalizeLegacyDocument returns doc unchanged if it is not the
// verified historical shape, or doc with AutoRecordSettings filled in
// from legacyDefaultAutoRecordSettings if it is. Callers must call this
// (or NormalizeDocument) only after Validate has already reported doc
// OK - never before, and never as a substitute for verification.
func normalizeLegacyDocument(doc Document) (normalized Document, wasLegacy bool) {
	if !verifyLegacyPreAutoRecordChecksum(doc) {
		return doc, false
	}
	doc.AutoRecordSettings = legacyDefaultAutoRecordSettings
	return doc, true
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

// NormalizeDocument returns doc with any historical-shape-only
// defaulting applied (currently: a verified pre-autoRecordSettings
// document's AutoRecordSettings section filled in as disabled with
// standard default thresholds). Callers (main/'s glue) must call this
// exactly once, immediately after Validate(doc, ...) has reported doc
// OK, before using doc for ComputePreview or applying it - never
// before Validate has succeeded. A no-op (returns doc unchanged) for
// every current-format document.
func NormalizeDocument(doc Document) Document {
	normalized, _ := normalizeLegacyDocument(doc)
	return normalized
}
