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

Today this covers exactly four historical layouts, all schema 2, the
only Configuration Backup shapes that have ever existed on this
project's master branch (each added the same way, at the point a new
section was):

 1. preAutoRecord: from PR #9 (this package's original merge) through
    commit 5b8509fc (PR #13's merge, immediately before Automatic
    Flight Recording added the autoRecordSettings section). (alertSettings
    was present from this package's very first commit - there is no
    even-older "before alertSettings" schema-2 shape to support.)
 2. preTrafficCPA: through the closure-rate/CPA traffic-alerting
    enhancement adding trafficCpaSettings.
 3. preEpaper: through the optional Waveshare e-paper display feature
    adding epaperSettings.
 4. preFISBCache: through the Rolling FIS-B Weather Cache adding
    fisbCacheSettings - i.e. exactly the shape master produced up to
    that point (key set: configuration, calibrationProfiles,
    alertSettings, autoRecordSettings, trafficCpaSettings,
    epaperSettings).

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
    checked oldest-to-newest, each shape's key set being disjoint from
    the others' by construction (they differ in size), so a genuine older
    document is never mistaken for a newer shape.
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

// legacyPreTrafficCPASectionKeys is the exact, frozen section-checksum
// key set the schema-2, post-autoRecordSettings, pre-trafficCpaSettings
// format always produced - the shape this package's own Document had
// immediately before the closure-rate/CPA traffic-alerting enhancement
// added TrafficCPASettings. Used to recognize a candidate historical
// document by exact key-set equality, exactly like
// legacyPreAutoRecordSectionKeys above.
var legacyPreTrafficCPASectionKeys = map[string]bool{
	"configuration":       true,
	"calibrationProfiles": true,
	"alertSettings":       true,
	"autoRecordSettings":  true,
}

// legacyDocumentV2PreTrafficCPA mirrors this package's own Document
// exactly as it existed immediately before TrafficCPASettings was added -
// frozen permanently, never updated to track later changes to Document
// itself, exactly like legacyDocumentV2PreAutoRecord above.
type legacyDocumentV2PreTrafficCPA struct {
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

// legacyDefaultTrafficCPASettings is the value normalizeLegacyDocument
// fills in for a verified-historical document's missing
// trafficCpaSettings section - deliberately the same disabled/default
// values as main.DefaultTrafficCPASettings() (this package cannot import
// main - see the package doc comment's leaf-dependency note - so these
// are independently restated; main/'s glue cross-checks them against the
// real package in a test - see main/configbackupapi_test.go). Never the
// bare Go zero value: an all-zero TrafficCPASettingsSection has
// MinRelativeSpeedKnots == MinClosureRateKnots == HorizonSeconds == 0,
// which itself fails validateTrafficCPASettings' strict bounds check -
// exactly the trap a genuinely absent section must not fall into.
// EscalationEnabled stays false regardless - restoring an old backup
// must never silently enable CPA-based escalation on a device that never
// had the setting to begin with.
var legacyDefaultTrafficCPASettings = TrafficCPASettingsSection{
	EscalationEnabled:     false,
	HorizonSeconds:        180,
	MinRelativeSpeedKnots: 20,
	MinClosureRateKnots:   30,
}

// legacyPreEpaperSectionKeys is the exact, frozen section-checksum key
// set the schema-2, post-trafficCpaSettings, pre-epaperSettings format
// always produced - the shape this package's own Document had
// immediately before the optional Waveshare e-paper display feature
// added EpaperSettings. Used to recognize a candidate historical
// document by exact key-set equality, exactly like
// legacyPreAutoRecordSectionKeys/legacyPreTrafficCPASectionKeys above.
var legacyPreEpaperSectionKeys = map[string]bool{
	"configuration":       true,
	"calibrationProfiles": true,
	"alertSettings":       true,
	"autoRecordSettings":  true,
	"trafficCpaSettings":  true,
}

// legacyDocumentV2PreEpaper mirrors this package's own Document exactly
// as it existed immediately before EpaperSettings was added - frozen
// permanently, never updated to track later changes to Document itself,
// exactly like legacyDocumentV2PreAutoRecord/legacyDocumentV2PreTrafficCPA
// above.
type legacyDocumentV2PreEpaper struct {
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
	TrafficCPASettings         TrafficCPASettingsSection `json:"trafficCpaSettings"`

	SectionChecksums map[string]string `json:"sectionChecksums"`
	ContentChecksum  string            `json:"contentChecksum"`
}

// legacyDefaultEpaperSettings is the value normalizeLegacyDocument fills
// in for a verified-historical document's missing epaperSettings
// section. Unlike legacyDefaultAutoRecordSettings/
// legacyDefaultTrafficCPASettings, the bare Go zero value is safe to use
// directly here: validateEpaperSettings (mirroring epaper.Normalize's own
// rule) treats a disabled section - Enabled: false, exactly the zero
// value - as always valid regardless of its other fields, so there is no
// hysteresis-style trap to avoid. This also exactly matches what a real
// device that has never touched e-paper settings actually has: only
// EpaperEnabled is explicitly defaulted (to false) by main's own
// defaultSettings(); the other five fields are left at Go's zero value
// until epaper.Normalize fills them in at the point of actual use.
var legacyDefaultEpaperSettings = EpaperSettingsSection{}

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

// verifyLegacyPreTrafficCPAChecksum reports whether doc's checksums are
// exactly consistent with having been produced by the pre-
// trafficCpaSettings schema-2 code - see this file's doc comment. Never
// a partial or best-effort match, exactly like
// verifyLegacyPreAutoRecordChecksum above.
func verifyLegacyPreTrafficCPAChecksum(doc Document) bool {
	if len(doc.SectionChecksums) != len(legacyPreTrafficCPASectionKeys) {
		return false
	}
	for k := range doc.SectionChecksums {
		if !legacyPreTrafficCPASectionKeys[k] {
			return false
		}
	}
	// A document that explicitly carries a non-zero trafficCpaSettings
	// section while also lacking its checksum is not honestly
	// historical - it is corrupt or tampered, and must be rejected, not
	// silently accepted with the extra data discarded.
	if doc.TrafficCPASettings != (TrafficCPASettingsSection{}) || doc.FISBCacheSettings != (FISBCacheSettingsSection{}) {
		return false
	}

	profiles := make([]calprofile.Profile, len(doc.CalibrationProfiles))
	copy(profiles, doc.CalibrationProfiles)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })

	legacy := legacyDocumentV2PreTrafficCPA{
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

// verifyLegacyPreEpaperChecksum reports whether doc's checksums are
// exactly consistent with having been produced by the pre-
// epaperSettings schema-2 code - see this file's doc comment. Never a
// partial or best-effort match, exactly like
// verifyLegacyPreAutoRecordChecksum/verifyLegacyPreTrafficCPAChecksum
// above.
func verifyLegacyPreEpaperChecksum(doc Document) bool {
	if len(doc.SectionChecksums) != len(legacyPreEpaperSectionKeys) {
		return false
	}
	for k := range doc.SectionChecksums {
		if !legacyPreEpaperSectionKeys[k] {
			return false
		}
	}
	// A document that explicitly carries a non-zero epaperSettings
	// section while also lacking its checksum is not honestly
	// historical - it is corrupt or tampered, and must be rejected, not
	// silently accepted with the extra data discarded.
	if doc.EpaperSettings != (EpaperSettingsSection{}) || doc.FISBCacheSettings != (FISBCacheSettingsSection{}) {
		return false
	}

	profiles := make([]calprofile.Profile, len(doc.CalibrationProfiles))
	copy(profiles, doc.CalibrationProfiles)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })

	legacy := legacyDocumentV2PreEpaper{
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
		TrafficCPASettings:         doc.TrafficCPASettings,
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
	trafficCPASum, err := sectionChecksum(legacy.TrafficCPASettings)
	if err != nil {
		return false
	}
	want := map[string]string{
		"configuration":       cfgSum,
		"calibrationProfiles": profSum,
		"alertSettings":       alertSum,
		"autoRecordSettings":  autoRecordSum,
		"trafficCpaSettings":  trafficCPASum,
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

// legacyPreFISBCacheSectionKeys is the exact, frozen section-checksum key
// set the schema-2, post-epaperSettings, pre-fisbCacheSettings format
// always produced - the shape this package's own Document had immediately
// before the Rolling FIS-B Weather Cache added FISBCacheSettings. Used to
// recognize a candidate historical document by exact key-set equality,
// exactly like the shapes above.
var legacyPreFISBCacheSectionKeys = map[string]bool{
	"configuration":       true,
	"calibrationProfiles": true,
	"alertSettings":       true,
	"autoRecordSettings":  true,
	"trafficCpaSettings":  true,
	"epaperSettings":      true,
}

// legacyDocumentV2PreFISBCache mirrors this package's own Document
// exactly as it existed immediately before FISBCacheSettings was added -
// frozen permanently, never updated to track later changes to Document
// itself, exactly like the shapes above.
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
	TrafficCPASettings         TrafficCPASettingsSection `json:"trafficCpaSettings"`
	EpaperSettings             EpaperSettingsSection     `json:"epaperSettings"`

	SectionChecksums map[string]string `json:"sectionChecksums"`
	ContentChecksum  string            `json:"contentChecksum"`
}

// verifyLegacyPreFISBCacheChecksum reports whether doc's checksums are
// exactly consistent with having been produced by the pre-
// fisbCacheSettings schema-2 code - see this file's doc comment. Never a
// partial or best-effort match, exactly like the verifiers above.
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
		TrafficCPASettings:         doc.TrafficCPASettings,
		EpaperSettings:             doc.EpaperSettings,
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
	trafficCPASum, err := sectionChecksum(legacy.TrafficCPASettings)
	if err != nil {
		return false
	}
	epaperSum, err := sectionChecksum(legacy.EpaperSettings)
	if err != nil {
		return false
	}
	want := map[string]string{
		"configuration":       cfgSum,
		"calibrationProfiles": profSum,
		"alertSettings":       alertSum,
		"autoRecordSettings":  autoRecordSum,
		"trafficCpaSettings":  trafficCPASum,
		"epaperSettings":      epaperSum,
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

// normalizeLegacyDocument returns doc unchanged if it is not one of the
// verified historical shapes, or doc with the missing section(s) filled
// in from their safe defaults if it is - a pre-autoRecordSettings
// document is missing AutoRecordSettings, TrafficCPASettings,
// EpaperSettings AND FISBCacheSettings and gets all four defaulted; a pre-trafficCpaSettings
// document already has a real AutoRecordSettings and needs
// TrafficCPASettings, EpaperSettings and FISBCacheSettings defaulted; a pre-epaperSettings
// document already has real AutoRecordSettings/TrafficCPASettings and
// needs EpaperSettings and FISBCacheSettings defaulted; a
// pre-fisbCacheSettings document only needs FISBCacheSettings defaulted.
// These four recognized shapes have
// disjoint section-checksum key sets by construction, so at most one of
// these checks can ever match a given document. Callers must call this
// (or NormalizeDocument) only after Validate has already reported doc OK
// - never before, and never as a substitute for verification.
func normalizeLegacyDocument(doc Document) (normalized Document, wasLegacy bool) {
	if verifyLegacyPreAutoRecordChecksum(doc) {
		doc.AutoRecordSettings = legacyDefaultAutoRecordSettings
		doc.TrafficCPASettings = legacyDefaultTrafficCPASettings
		doc.EpaperSettings = legacyDefaultEpaperSettings
		doc.FISBCacheSettings = legacyDefaultFISBCacheSettings
		return doc, true
	}
	if verifyLegacyPreTrafficCPAChecksum(doc) {
		doc.TrafficCPASettings = legacyDefaultTrafficCPASettings
		doc.EpaperSettings = legacyDefaultEpaperSettings
		doc.FISBCacheSettings = legacyDefaultFISBCacheSettings
		return doc, true
	}
	if verifyLegacyPreEpaperChecksum(doc) {
		doc.EpaperSettings = legacyDefaultEpaperSettings
		doc.FISBCacheSettings = legacyDefaultFISBCacheSettings
		return doc, true
	}
	if verifyLegacyPreFISBCacheChecksum(doc) {
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

// LegacyDefaultTrafficCPASettings exposes legacyDefaultTrafficCPASettings
// for cross-checking against main.DefaultTrafficCPASettings() (this
// package cannot import main itself - see the package doc comment's
// leaf-dependency note) - see
// main/configbackupapi_test.go's
// TestLegacyDefaultTrafficCPASettingsMatchesPackageDefault. Not used by
// this package's own normal restore path (which uses the unexported
// value directly); exported solely for that one test.
func LegacyDefaultTrafficCPASettings() TrafficCPASettingsSection {
	return legacyDefaultTrafficCPASettings
}

// LegacyDefaultEpaperSettings exposes legacyDefaultEpaperSettings for
// cross-checking against main's own defaultSettings() e-paper defaults
// (this package cannot import main itself - see the package doc
// comment's leaf-dependency note) - see
// main/configbackupapi_test.go's
// TestLegacyDefaultEpaperSettingsMatchesPackageDefault. Not used by this
// package's own normal restore path (which uses the unexported value
// directly); exported solely for that one test.
func LegacyDefaultEpaperSettings() EpaperSettingsSection {
	return legacyDefaultEpaperSettings
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
