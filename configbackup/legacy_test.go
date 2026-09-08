package configbackup

import (
	"encoding/json"
	"os"
	"testing"
)

// loadLegacyFixture reads the authentic pre-autoRecordSettings backup -
// see testdata/README.md for exactly how it was generated (literally
// run from commit 5b8509fc's own configbackup.BuildDocument, never
// hand-simulated).
func loadLegacyFixture(t *testing.T) (doc Document, raw []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/legacy-pre-autorecord-backup.json")
	if err != nil {
		t.Fatalf("reading legacy fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshaling legacy fixture: %v", err)
	}
	return doc, raw
}

// TestLegacyFixture_HasNoAutoRecordChecksum sanity-checks the fixture
// itself is genuinely the historical shape (no autoRecordSettings
// checksum entry) before any test below relies on that.
func TestLegacyFixture_HasNoAutoRecordChecksum(t *testing.T) {
	doc, _ := loadLegacyFixture(t)
	if _, present := doc.SectionChecksums["autoRecordSettings"]; present {
		t.Fatal("fixture unexpectedly carries an autoRecordSettings checksum - it is not the historical shape this test file assumes")
	}
	if doc.SchemaVersion != 2 {
		t.Fatalf("fixture SchemaVersion = %d, want 2", doc.SchemaVersion)
	}
}

// TestLegacyBackup_PassesOriginalChecksumVerification is the core
// compatibility requirement: an authentic, unmodified pre-
// autoRecordSettings backup must validate successfully against the
// CURRENT code, verified using its own original checksums (see
// legacy.go) - not rejected outright as the previous mission's
// rejection-only behavior did.
func TestLegacyBackup_PassesOriginalChecksumVerification(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	res := Validate(doc, len(raw))
	if !res.OK() {
		t.Fatalf("expected an authentic legacy backup to validate, got errors: %v", res.Errors)
	}
}

// TestLegacyBackup_NormalizesToDisabledAutoRecordSettings proves
// defaulting happens ONLY after successful verification, and produces
// the documented safe default (disabled, standard thresholds) - never
// the bare zero value, and never a value that itself violates this
// package's own semantic validation.
func TestLegacyBackup_NormalizesToDisabledAutoRecordSettings(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	if res := Validate(doc, len(raw)); !res.OK() {
		t.Fatalf("Validate: %v", res.Errors)
	}
	normalized := NormalizeDocument(doc)
	if normalized.AutoRecordSettings.Enabled {
		t.Fatal("a legacy backup must never normalize to Automatic Flight Recording enabled")
	}
	if normalized.AutoRecordSettings != legacyDefaultAutoRecordSettings {
		t.Fatalf("normalized AutoRecordSettings = %+v, want the documented legacy default %+v", normalized.AutoRecordSettings, legacyDefaultAutoRecordSettings)
	}
	// The normalized value must itself be semantically valid - a
	// legacy-default that failed the package's own bounds/hysteresis
	// check would defeat the entire point of defaulting instead of
	// zeroing.
	var res ValidationResult
	validateAutoRecordSettings(normalized.AutoRecordSettings, &res)
	if !res.OK() {
		t.Fatalf("legacyDefaultAutoRecordSettings itself fails validateAutoRecordSettings: %v", res.Errors)
	}
	// Every other field must be untouched by normalization.
	if normalized.SchemaVersion != doc.SchemaVersion ||
		normalized.SourceVersion != doc.SourceVersion ||
		normalized.SourceCommit != doc.SourceCommit ||
		normalized.Configuration != doc.Configuration ||
		normalized.ActiveCalibrationProfileID != doc.ActiveCalibrationProfileID ||
		normalized.AlertSettings != doc.AlertSettings ||
		normalized.ContentChecksum != doc.ContentChecksum ||
		len(normalized.CalibrationProfiles) != len(doc.CalibrationProfiles) {
		t.Fatalf("normalization touched a field other than AutoRecordSettings:\nbefore=%+v\nafter=%+v", doc, normalized)
	}
}

// TestLegacyBackup_NotNormalizedBeforeVerification proves ordering:
// NormalizeDocument on a document that has NOT been (or cannot be)
// verified must not silently "fix" it - it simply returns the document
// unchanged (never OK on its own; callers must still call Validate
// first and check res.OK()).
func TestLegacyBackup_NotNormalizedBeforeVerification(t *testing.T) {
	doc, _ := loadLegacyFixture(t)
	corrupted := doc
	corrupted.Configuration.RegionSelected++ // accidental corruption, checksums stale
	normalized := NormalizeDocument(corrupted)
	if normalized.AutoRecordSettings != (AutoRecordSettingsSection{}) {
		t.Fatal("NormalizeDocument must not default a document whose checksums were never actually verified as the historical shape")
	}
}

// --- Corruption must still be rejected for the legacy shape ---

func TestLegacyBackup_CorruptedConfigurationRejected(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	doc.Configuration.RegionSelected++ // content changed, section checksum left stale
	if res := Validate(doc, len(raw)); res.OK() {
		t.Fatal("expected corrupted legacy configuration section to be rejected")
	}
}

func TestLegacyBackup_CorruptedSectionChecksumRejected(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	doc.SectionChecksums["alertSettings"] = "0000000000000000000000000000000000000000000000000000000000000000"
	if res := Validate(doc, len(raw)); res.OK() {
		t.Fatal("expected a tampered legacy section checksum to be rejected")
	}
}

func TestLegacyBackup_CorruptedContentChecksumRejected(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	doc.ContentChecksum = "0000000000000000000000000000000000000000000000000000000000000000"
	if res := Validate(doc, len(raw)); res.OK() {
		t.Fatal("expected a tampered legacy whole-document checksum to be rejected")
	}
}

func TestLegacyBackup_MissingContentChecksumRejected(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	doc.ContentChecksum = ""
	if res := Validate(doc, len(raw)); res.OK() {
		t.Fatal("expected a legacy document with no contentChecksum at all to be rejected")
	}
}

// TestLegacyBackup_TamperedWithHiddenAutoRecordDataRejected: a legacy-
// shaped document (exactly the historical section-checksum key set)
// that ALSO carries a non-zero autoRecordSettings section is not
// honestly historical - the historical code could never have produced
// non-zero AutoRecordSettings, since it did not know the field existed.
// This must be rejected outright, never silently accepted with the
// smuggled section discarded.
func TestLegacyBackup_TamperedWithHiddenAutoRecordDataRejected(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	doc.AutoRecordSettings.Enabled = true
	doc.AutoRecordSettings.StartGroundspeedKnots = 8
	doc.AutoRecordSettings.StopGroundspeedKnots = 4
	if res := Validate(doc, len(raw)); res.OK() {
		t.Fatal("expected a legacy-shaped document smuggling a non-zero autoRecordSettings section (without its own checksum) to be rejected")
	}
}

// TestLegacyBackup_ExtraSectionChecksumKeyIsNotTreatedAsLegacy: a
// document whose SectionChecksums key set is a SUPERSET of the
// historical set (e.g. the historical three keys plus some other,
// unrecognized key) must not match the historical shape either - the
// key-set comparison is exact equality, never subset/superset.
func TestLegacyBackup_ExtraSectionChecksumKeyIsNotTreatedAsLegacy(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	doc.SectionChecksums["somethingElse"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if res := Validate(doc, len(raw)); res.OK() {
		t.Fatal("expected an unrecognized extra section-checksum key to be rejected, not silently treated as the historical shape")
	}
}

// --- Current-format documents are unaffected ---

func TestCurrentBackup_StillValidatesNormally(t *testing.T) {
	doc := validDoc(t)
	if res := Validate(doc, mustMarshalLen(t, doc)); !res.OK() {
		t.Fatalf("a current-format document must still validate: %v", res.Errors)
	}
}

func TestCurrentBackup_NormalizeIsNoOp(t *testing.T) {
	doc := validDoc(t)
	doc.AutoRecordSettings.StartGroundspeedKnots = 12
	doc.AutoRecordSettings.StopGroundspeedKnots = 3
	normalized := NormalizeDocument(doc)
	if normalized.AutoRecordSettings != doc.AutoRecordSettings || normalized.ContentChecksum != doc.ContentChecksum {
		t.Fatalf("NormalizeDocument must be a no-op for a current-format document: got %+v, want unchanged %+v", normalized, doc)
	}
}

func TestCurrentBackup_MissingAutoRecordChecksumStillRejected(t *testing.T) {
	// A current-format document (has a non-zero-shaped autoRecordSettings
	// section reflecting real settings) that is missing ONLY the
	// autoRecordSettings checksum is neither a valid current document nor
	// the historical shape (key-set mismatch: 3 keys instead of 4, but
	// AutoRecordSettings is non-zero) - must still be rejected.
	doc := validDoc(t)
	doc.AutoRecordSettings.Enabled = true
	doc.AutoRecordSettings.StartGroundspeedKnots = 10
	doc.AutoRecordSettings.StopGroundspeedKnots = 2
	delete(doc.SectionChecksums, "autoRecordSettings")
	if res := Validate(doc, mustMarshalLen(t, doc)); res.OK() {
		t.Fatal("expected a current document missing only its autoRecordSettings checksum to be rejected")
	}
}

// --- Documented checksum limitation applies identically to the legacy shape ---

// TestLegacyBackup_DeliberateEditWithHonestRecomputeRemainsValid mirrors
// TestChecksum_DetectsAccidentalCorruptionButNotDeliberateModification
// for the legacy shape - the checksum's documented limitation (detects
// accidental corruption, not deliberate modification) applies exactly
// the same way here: this is expected, not a new hole opened by legacy
// support.
func TestLegacyBackup_DeliberateEditWithHonestRecomputeRemainsValid(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	if res := Validate(doc, len(raw)); !res.OK() {
		t.Fatalf("fixture must validate before this test's edit: %v", res.Errors)
	}

	edited := doc
	edited.Configuration.RegionSelected = 2 // deliberate change
	if verifyLegacyPreAutoRecordChecksum(edited) {
		t.Fatal("expected the edited document to still fail (stale checksums) before recomputation")
	}
	// Recompute exactly as the historical code would have.
	cfgSum, err := sectionChecksum(edited.Configuration)
	if err != nil {
		t.Fatal(err)
	}
	edited.SectionChecksums["configuration"] = cfgSum
	legacy := legacyDocumentV2PreAutoRecord{
		SchemaVersion:              edited.SchemaVersion,
		CreatedAtUTC:               edited.CreatedAtUTC,
		SourceVersion:              edited.SourceVersion,
		SourceCommit:               edited.SourceCommit,
		MinimumCompatibleVersion:   edited.MinimumCompatibleVersion,
		Configuration:              edited.Configuration,
		CalibrationProfiles:        edited.CalibrationProfiles,
		ActiveCalibrationProfileID: edited.ActiveCalibrationProfileID,
		AlertSettings:              edited.AlertSettings,
		SectionChecksums:           edited.SectionChecksums,
	}
	contentSum, err := sectionChecksum(legacy)
	if err != nil {
		t.Fatal(err)
	}
	edited.ContentChecksum = contentSum

	res := Validate(edited, mustMarshalLen(t, edited))
	if !res.OK() {
		t.Fatalf("a deliberately edited legacy document with an honestly recomputed checksum is documented to remain valid (checksums are not authentication): %v", res.Errors)
	}
}

// --- Re-export produces the current format ---

// TestLegacyBackup_ReExportProducesCurrentFormat proves that once a
// legacy backup is validated, normalized, and its settings gathered
// back into a fresh BuildInputs (mirroring what main/'s glue does after
// a successful restore - configuration/profiles/alert settings applied,
// then re-exported later), the resulting document is unambiguously
// CURRENT-format: it carries an autoRecordSettings section and checksum,
// and is schema-current.
func TestLegacyBackup_ReExportProducesCurrentFormat(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	if res := Validate(doc, len(raw)); !res.OK() {
		t.Fatalf("Validate: %v", res.Errors)
	}
	normalized := NormalizeDocument(doc)

	reExported, err := BuildDocument(BuildInputs{
		SourceVersion:              normalized.SourceVersion,
		SourceCommit:               "current-head-after-restore",
		Configuration:              normalized.Configuration,
		CalibrationProfiles:        normalized.CalibrationProfiles,
		ActiveCalibrationProfileID: normalized.ActiveCalibrationProfileID,
		AlertSettings:              normalized.AlertSettings,
		AutoRecordSettings:         normalized.AutoRecordSettings,
	})
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if reExported.SchemaVersion != SchemaVersion {
		t.Fatalf("re-exported SchemaVersion = %d, want current %d", reExported.SchemaVersion, SchemaVersion)
	}
	if _, present := reExported.SectionChecksums["autoRecordSettings"]; !present {
		t.Fatal("re-exported document must carry an autoRecordSettings checksum - it is current-format, not legacy")
	}
	if res := Validate(reExported, mustMarshalLen(t, reExported)); !res.OK() {
		t.Fatalf("re-exported document must itself validate: %v", res.Errors)
	}
}

// --- Preview of a legacy backup ---

func TestLegacyBackup_PreviewIsAccurate(t *testing.T) {
	doc, raw := loadLegacyFixture(t)
	if res := Validate(doc, len(raw)); !res.OK() {
		t.Fatalf("Validate: %v", res.Errors)
	}
	normalized := NormalizeDocument(doc)

	current := CurrentState{
		Version:             "2.0-pre5",
		Commit:              "livecommit",
		Configuration:       normalized.Configuration, // pretend live matches, isolate the AutoRecordSettings diff
		CalibrationProfiles: normalized.CalibrationProfiles,
		ActiveProfileID:     normalized.ActiveCalibrationProfileID,
		AlertSettings:       normalized.AlertSettings,
		AutoRecordSettings: AutoRecordSettingsSection{
			Enabled: true, StartGroundspeedKnots: 10, StartDwellSeconds: 45,
			StopGroundspeedKnots: 3, StopDwellSeconds: 90,
			GPSLossGraceSeconds: 20, RestartCooldownSeconds: 200,
		},
	}
	// ComputePreview only ever reads doc's field values (a pure diff
	// against current) - it never re-validates or re-checksums doc, so
	// the normalized legacy document can be passed directly, exactly as
	// main/'s glue does after Validate+NormalizeDocument succeed.
	preview := ComputePreview(normalized, current)
	if len(preview.AutoRecordSettingsChanges) == 0 {
		t.Fatal("expected the preview to show autoRecordSettings changing from the live device's enabled config down to the legacy backup's disabled default")
	}
}
