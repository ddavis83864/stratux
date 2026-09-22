package configbackup

import (
	"encoding/json"
	"os"
	"testing"
)

// loadLegacyPreEpaperFixture reads the authentic pre-epaperSettings
// backup - see testdata/README.md for exactly how it was generated
// (literally run from commit 6ca35c8f's own configbackup.BuildDocument,
// never hand-simulated).
func loadLegacyPreEpaperFixture(t *testing.T) (doc Document, raw []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/legacy-pre-epaper-backup.json")
	if err != nil {
		t.Fatalf("reading legacy fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshaling legacy fixture: %v", err)
	}
	return doc, raw
}

func TestLegacyPreEpaperFixture_HasNoEpaperChecksum(t *testing.T) {
	doc, _ := loadLegacyPreEpaperFixture(t)
	if _, present := doc.SectionChecksums["epaperSettings"]; present {
		t.Fatal("fixture unexpectedly carries an epaperSettings checksum - it is not the historical shape this test file assumes")
	}
	if _, present := doc.SectionChecksums["trafficCpaSettings"]; !present {
		t.Fatal("fixture must carry a trafficCpaSettings checksum - it postdates that feature")
	}
}

func TestLegacyPreEpaperBackup_PassesOriginalChecksumVerification(t *testing.T) {
	doc, raw := loadLegacyPreEpaperFixture(t)
	res := Validate(doc, len(raw))
	if !res.OK() {
		t.Fatalf("expected an authentic legacy backup to validate, got errors: %v", res.Errors)
	}
}

func TestLegacyPreEpaperBackup_NormalizesToDisabledDefault(t *testing.T) {
	doc, raw := loadLegacyPreEpaperFixture(t)
	if res := Validate(doc, len(raw)); !res.OK() {
		t.Fatalf("Validate: %v", res.Errors)
	}
	normalized := NormalizeDocument(doc)
	if normalized.EpaperSettings.Enabled {
		t.Fatal("a legacy backup must never normalize to the e-paper display enabled")
	}
	if normalized.EpaperSettings != legacyDefaultEpaperSettings {
		t.Fatalf("normalized EpaperSettings = %+v, want the documented legacy default %+v", normalized.EpaperSettings, legacyDefaultEpaperSettings)
	}
	var res ValidationResult
	validateEpaperSettings(normalized.EpaperSettings, &res)
	if !res.OK() {
		t.Fatalf("legacyDefaultEpaperSettings itself fails validateEpaperSettings: %v", res.Errors)
	}
	// TrafficCPASettings/AutoRecordSettings (already present in this
	// shape) must be left completely untouched - only the
	// genuinely-absent section is defaulted.
	if normalized.TrafficCPASettings != doc.TrafficCPASettings {
		t.Fatalf("normalization touched TrafficCPASettings, which this shape already had: before=%+v after=%+v", doc.TrafficCPASettings, normalized.TrafficCPASettings)
	}
	if normalized.AutoRecordSettings != doc.AutoRecordSettings {
		t.Fatalf("normalization touched AutoRecordSettings, which this shape already had: before=%+v after=%+v", doc.AutoRecordSettings, normalized.AutoRecordSettings)
	}
	if normalized.SchemaVersion != doc.SchemaVersion ||
		normalized.Configuration != doc.Configuration ||
		normalized.AlertSettings != doc.AlertSettings ||
		normalized.ContentChecksum != doc.ContentChecksum {
		t.Fatalf("normalization touched a field other than EpaperSettings:\nbefore=%+v\nafter=%+v", doc, normalized)
	}
}

func TestLegacyPreEpaperBackup_TamperedWithHiddenDataRejected(t *testing.T) {
	doc, raw := loadLegacyPreEpaperFixture(t)
	doc.EpaperSettings.Enabled = true
	doc.EpaperSettings.Panel = "waveshare-3.7in"
	doc.EpaperSettings.Rotation = 90
	doc.EpaperSettings.RefreshIntervalSeconds = 15
	doc.EpaperSettings.FullRefreshEvery = 20
	doc.EpaperSettings.Page = "overview"
	if res := Validate(doc, len(raw)); res.OK() {
		t.Fatal("expected a legacy-shaped document smuggling a non-zero epaperSettings section (without its own checksum) to be rejected")
	}
}

func TestLegacyPreEpaperBackup_CorruptedSectionChecksumRejected(t *testing.T) {
	doc, raw := loadLegacyPreEpaperFixture(t)
	doc.SectionChecksums["trafficCpaSettings"] = "0000000000000000000000000000000000000000000000000000000000000000"
	if res := Validate(doc, len(raw)); res.OK() {
		t.Fatal("expected a tampered legacy section checksum to be rejected")
	}
}

// TestLegacyEpaperShapeIsMutuallyExclusiveWithOlderShapes proves the
// pre-epaperSettings fixture is never mistaken for either older
// historical shape (or vice versa) - their section-checksum key sets are
// disjoint by construction (3 vs. 4 vs. 5 keys), so
// normalizeLegacyDocument's ordering never matters in practice, but this
// proves it explicitly rather than leaving it merely implied.
func TestLegacyEpaperShapeIsMutuallyExclusiveWithOlderShapes(t *testing.T) {
	preEpaper, _ := loadLegacyPreEpaperFixture(t)
	if verifyLegacyPreAutoRecordChecksum(preEpaper) {
		t.Fatal("the pre-epaperSettings fixture must not also match the pre-autoRecordSettings shape")
	}
	if verifyLegacyPreTrafficCPAChecksum(preEpaper) {
		t.Fatal("the pre-epaperSettings fixture must not also match the pre-trafficCpaSettings shape")
	}

	preAutoRecord, _ := loadLegacyFixture(t)
	if verifyLegacyPreEpaperChecksum(preAutoRecord) {
		t.Fatal("the pre-autoRecordSettings fixture must not also match the pre-epaperSettings shape")
	}
	preTrafficCPA, _ := loadLegacyPreTrafficCPAFixture(t)
	if verifyLegacyPreEpaperChecksum(preTrafficCPA) {
		t.Fatal("the pre-trafficCpaSettings fixture must not also match the pre-epaperSettings shape")
	}
}

// TestVeryOldBackup_NormalizesAllThreeMissingSections proves a document
// from BEFORE autoRecordSettings existed (missing all three later
// sections) gets all three defaulted correctly when normalized against
// current code.
func TestVeryOldBackup_NormalizesAllThreeMissingSections(t *testing.T) {
	doc, raw := loadLegacyFixture(t) // the oldest, pre-autoRecordSettings fixture
	if res := Validate(doc, len(raw)); !res.OK() {
		t.Fatalf("Validate: %v", res.Errors)
	}
	normalized := NormalizeDocument(doc)
	if normalized.AutoRecordSettings != legacyDefaultAutoRecordSettings {
		t.Errorf("expected AutoRecordSettings defaulted, got %+v", normalized.AutoRecordSettings)
	}
	if normalized.TrafficCPASettings != legacyDefaultTrafficCPASettings {
		t.Errorf("expected TrafficCPASettings defaulted, got %+v", normalized.TrafficCPASettings)
	}
	if normalized.EpaperSettings != legacyDefaultEpaperSettings {
		t.Errorf("expected EpaperSettings ALSO defaulted for a document that predates all three sections, got %+v", normalized.EpaperSettings)
	}
}

// TestMiddleAgeBackup_NormalizesOnlyMissingEpaperSection proves a
// pre-trafficCpaSettings document (missing BOTH trafficCpaSettings and
// epaperSettings) gets both defaulted, never touching the AutoRecordSettings
// it already had.
func TestMiddleAgeBackup_NormalizesOnlyMissingEpaperSection(t *testing.T) {
	doc, raw := loadLegacyPreTrafficCPAFixture(t)
	if res := Validate(doc, len(raw)); !res.OK() {
		t.Fatalf("Validate: %v", res.Errors)
	}
	normalized := NormalizeDocument(doc)
	if normalized.TrafficCPASettings != legacyDefaultTrafficCPASettings {
		t.Errorf("expected TrafficCPASettings defaulted, got %+v", normalized.TrafficCPASettings)
	}
	if normalized.EpaperSettings != legacyDefaultEpaperSettings {
		t.Errorf("expected EpaperSettings ALSO defaulted for a document predating it, got %+v", normalized.EpaperSettings)
	}
	if normalized.AutoRecordSettings != doc.AutoRecordSettings {
		t.Fatalf("normalization touched AutoRecordSettings, which this shape already had: before=%+v after=%+v", doc.AutoRecordSettings, normalized.AutoRecordSettings)
	}
}

// --- an unpopulated (zero-value) EpaperSettings must still validate ---

func TestUnpopulatedEpaperSettingsIsAccepted(t *testing.T) {
	in := testBuildInputs()
	in.EpaperSettings = EpaperSettingsSection{} // deliberately unset
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if res := Validate(doc, mustMarshalLen(t, doc)); !res.OK() {
		t.Fatalf("a document with an unpopulated (zero-value) epaperSettings section must still validate: %v", res.Errors)
	}
}

// TestDisabledEpaperWithInvalidFieldsStillAccepted proves the
// always-valid-when-disabled rule (mirroring epaper.Normalize) is not
// merely the zero-value case above: even a deliberately out-of-range
// Rotation is accepted so long as Enabled is false.
func TestDisabledEpaperWithInvalidFieldsStillAccepted(t *testing.T) {
	in := testBuildInputs()
	in.EpaperSettings = EpaperSettingsSection{Enabled: false, Rotation: 999, Panel: "not-a-real-panel"}
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if res := Validate(doc, mustMarshalLen(t, doc)); !res.OK() {
		t.Fatalf("a disabled epaperSettings section must validate regardless of its other fields: %v", res.Errors)
	}
}

func TestEnabledEpaperWithInvalidRotationRejected(t *testing.T) {
	in := testBuildInputs()
	in.EpaperSettings = EpaperSettingsSection{
		Enabled: true, Panel: "waveshare-3.7in", Page: "overview",
		Rotation: 45, RefreshIntervalSeconds: 15, FullRefreshEvery: 20,
	}
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if res := Validate(doc, mustMarshalLen(t, doc)); res.OK() {
		t.Fatal("expected an enabled epaperSettings section with an invalid rotation (45) to be rejected")
	}
}

func TestEnabledEpaperWithTooShortRefreshIntervalRejected(t *testing.T) {
	in := testBuildInputs()
	in.EpaperSettings = EpaperSettingsSection{
		Enabled: true, Panel: "waveshare-3.7in", Page: "overview",
		Rotation: 0, RefreshIntervalSeconds: 1, FullRefreshEvery: 20,
	}
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if res := Validate(doc, mustMarshalLen(t, doc)); res.OK() {
		t.Fatal("expected an enabled epaperSettings section with too short a refresh interval to be rejected")
	}
}

func TestEnabledEpaperWithUnsupportedPanelRejected(t *testing.T) {
	in := testBuildInputs()
	in.EpaperSettings = EpaperSettingsSection{
		Enabled: true, Panel: "some-future-panel", Page: "overview",
		Rotation: 0, RefreshIntervalSeconds: 15, FullRefreshEvery: 20,
	}
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if res := Validate(doc, mustMarshalLen(t, doc)); res.OK() {
		t.Fatal("expected an enabled epaperSettings section with an unsupported panel to be rejected")
	}
}

// TestEnabledEpaperWithWaveshare42V2PanelAccepted confirms the second
// supported panel identifier, added alongside the Waveshare 4.2in V2
// driver, is accepted exactly like waveshare-3.7in - configbackup
// cannot import the epaper package (leaf-dependency direction), so its
// own validEpaperPanels map is an independently-maintained copy that
// must be kept in sync by hand; this is a direct regression test for
// that sync.
func TestEnabledEpaperWithWaveshare42V2PanelAccepted(t *testing.T) {
	in := testBuildInputs()
	in.EpaperSettings = EpaperSettingsSection{
		Enabled: true, Panel: "waveshare-4.2in-v2", Page: "overview",
		Rotation: 0, RefreshIntervalSeconds: 15, FullRefreshEvery: 20,
	}
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if res := Validate(doc, mustMarshalLen(t, doc)); !res.OK() {
		t.Fatalf("expected an enabled epaperSettings section with panel waveshare-4.2in-v2 to be accepted, got: %v", res.Errors)
	}
}

func TestEpaperSettingsPreview_ShowsChange(t *testing.T) {
	doc := validDoc(t)
	doc.EpaperSettings = EpaperSettingsSection{
		Enabled: true, Panel: "waveshare-3.7in", Page: "health",
		Rotation: 180, RefreshIntervalSeconds: 30, FullRefreshEvery: 10,
	}
	current := CurrentState{
		Configuration:      doc.Configuration,
		AlertSettings:      doc.AlertSettings,
		AutoRecordSettings: doc.AutoRecordSettings,
		TrafficCPASettings: doc.TrafficCPASettings,
		EpaperSettings:     EpaperSettingsSection{},
	}
	preview := ComputePreview(doc, current)
	if len(preview.EpaperSettingsChanges) == 0 {
		t.Fatal("expected the preview to show epaperSettings changing")
	}
	if !preview.HasChanges {
		t.Fatal("expected HasChanges to be true given an epaperSettings diff")
	}
}
