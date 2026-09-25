package configbackup

import (
	"encoding/json"
	"os"
	"testing"
)

// loadLegacyPreTrafficCPAFixture reads the authentic pre-trafficCpaSettings
// backup - see testdata/README.md for exactly how it was generated
// (literally run from commit 936c38e4's own configbackup.BuildDocument,
// never hand-simulated).
func loadLegacyPreTrafficCPAFixture(t *testing.T) (doc Document, raw []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/legacy-pre-trafficcpa-backup.json")
	if err != nil {
		t.Fatalf("reading legacy fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshaling legacy fixture: %v", err)
	}
	return doc, raw
}

func TestLegacyPreTrafficCPAFixture_HasNoTrafficCPAChecksum(t *testing.T) {
	doc, _ := loadLegacyPreTrafficCPAFixture(t)
	if _, present := doc.SectionChecksums["trafficCpaSettings"]; present {
		t.Fatal("fixture unexpectedly carries a trafficCpaSettings checksum - it is not the historical shape this test file assumes")
	}
	if _, present := doc.SectionChecksums["autoRecordSettings"]; !present {
		t.Fatal("fixture must carry an autoRecordSettings checksum - it postdates that feature")
	}
}

func TestLegacyPreTrafficCPABackup_PassesOriginalChecksumVerification(t *testing.T) {
	doc, raw := loadLegacyPreTrafficCPAFixture(t)
	res := Validate(doc, len(raw))
	if !res.OK() {
		t.Fatalf("expected an authentic legacy backup to validate, got errors: %v", res.Errors)
	}
}

func TestLegacyPreTrafficCPABackup_NormalizesToDisabledDefault(t *testing.T) {
	doc, raw := loadLegacyPreTrafficCPAFixture(t)
	if res := Validate(doc, len(raw)); !res.OK() {
		t.Fatalf("Validate: %v", res.Errors)
	}
	normalized := NormalizeDocument(doc)
	if normalized.TrafficCPASettings.EscalationEnabled {
		t.Fatal("a legacy backup must never normalize to CPA escalation enabled")
	}
	if normalized.TrafficCPASettings != legacyDefaultTrafficCPASettings {
		t.Fatalf("normalized TrafficCPASettings = %+v, want the documented legacy default %+v", normalized.TrafficCPASettings, legacyDefaultTrafficCPASettings)
	}
	var res ValidationResult
	validateTrafficCPASettings(normalized.TrafficCPASettings, &res)
	if !res.OK() {
		t.Fatalf("legacyDefaultTrafficCPASettings itself fails validateTrafficCPASettings: %v", res.Errors)
	}
	// AutoRecordSettings (already present in this shape) must be left
	// completely untouched - only the genuinely-absent section is
	// defaulted.
	if normalized.AutoRecordSettings != doc.AutoRecordSettings {
		t.Fatalf("normalization touched AutoRecordSettings, which this shape already had: before=%+v after=%+v", doc.AutoRecordSettings, normalized.AutoRecordSettings)
	}
	if normalized.SchemaVersion != doc.SchemaVersion ||
		normalized.Configuration != doc.Configuration ||
		normalized.AlertSettings != doc.AlertSettings ||
		normalized.ContentChecksum != doc.ContentChecksum {
		t.Fatalf("normalization touched a field other than TrafficCPASettings:\nbefore=%+v\nafter=%+v", doc, normalized)
	}
}

func TestLegacyPreTrafficCPABackup_TamperedWithHiddenDataRejected(t *testing.T) {
	doc, raw := loadLegacyPreTrafficCPAFixture(t)
	doc.TrafficCPASettings.EscalationEnabled = true
	doc.TrafficCPASettings.HorizonSeconds = 180
	doc.TrafficCPASettings.MinRelativeSpeedKnots = 20
	doc.TrafficCPASettings.MinClosureRateKnots = 30
	if res := Validate(doc, len(raw)); res.OK() {
		t.Fatal("expected a legacy-shaped document smuggling a non-zero trafficCpaSettings section (without its own checksum) to be rejected")
	}
}

func TestLegacyPreTrafficCPABackup_CorruptedSectionChecksumRejected(t *testing.T) {
	doc, raw := loadLegacyPreTrafficCPAFixture(t)
	doc.SectionChecksums["autoRecordSettings"] = "0000000000000000000000000000000000000000000000000000000000000000"
	if res := Validate(doc, len(raw)); res.OK() {
		t.Fatal("expected a tampered legacy section checksum to be rejected")
	}
}

// TestLegacyShapesAreMutuallyExclusive proves the pre-autoRecordSettings
// fixture is never mistaken for the pre-trafficCpaSettings shape (or vice
// versa) - their section-checksum key sets are disjoint by construction
// (3 keys vs. 4), so normalizeLegacyDocument's ordering (try
// pre-autoRecordSettings first) never matters in practice, but this
// proves it explicitly rather than leaving it merely implied.
func TestLegacyShapesAreMutuallyExclusive(t *testing.T) {
	preAutoRecord, _ := loadLegacyFixture(t)
	if verifyLegacyPreTrafficCPAChecksum(preAutoRecord) {
		t.Fatal("the pre-autoRecordSettings fixture must not also match the pre-trafficCpaSettings shape")
	}
	preTrafficCPA, _ := loadLegacyPreTrafficCPAFixture(t)
	if verifyLegacyPreAutoRecordChecksum(preTrafficCPA) {
		t.Fatal("the pre-trafficCpaSettings fixture must not also match the pre-autoRecordSettings shape")
	}
}

// TestVeryOldBackup_NormalizesBothMissingSections proves a document from
// BEFORE autoRecordSettings existed (missing BOTH later sections) gets
// BOTH defaulted correctly when normalized against current code.
func TestVeryOldBackup_NormalizesBothMissingSections(t *testing.T) {
	doc, raw := loadLegacyFixture(t) // the older, pre-autoRecordSettings fixture
	if res := Validate(doc, len(raw)); !res.OK() {
		t.Fatalf("Validate: %v", res.Errors)
	}
	normalized := NormalizeDocument(doc)
	if normalized.AutoRecordSettings != legacyDefaultAutoRecordSettings {
		t.Errorf("expected AutoRecordSettings defaulted, got %+v", normalized.AutoRecordSettings)
	}
	if normalized.TrafficCPASettings != legacyDefaultTrafficCPASettings {
		t.Errorf("expected TrafficCPASettings ALSO defaulted for a document that predates both sections, got %+v", normalized.TrafficCPASettings)
	}
}

// --- an unpopulated (zero-value) TrafficCPASettings must still validate ---

func TestUnpopulatedTrafficCPASettingsIsAccepted(t *testing.T) {
	in := testBuildInputs()
	in.TrafficCPASettings = TrafficCPASettingsSection{} // deliberately unset
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if res := Validate(doc, mustMarshalLen(t, doc)); !res.OK() {
		t.Fatalf("a document with an unpopulated (zero-value) trafficCpaSettings section must still validate: %v", res.Errors)
	}
}

// TestEnabledTrafficCPAWithZeroThresholdsStillRejected proves the
// zero-value tolerance above is narrow: EscalationEnabled:true together
// with all-zero thresholds is NOT the zero value and must still be
// rejected.
func TestEnabledTrafficCPAWithZeroThresholdsStillRejected(t *testing.T) {
	in := testBuildInputs()
	in.TrafficCPASettings = TrafficCPASettingsSection{EscalationEnabled: true}
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if res := Validate(doc, mustMarshalLen(t, doc)); res.OK() {
		t.Fatal("expected EscalationEnabled:true with all-zero thresholds to still be rejected")
	}
}

func TestTrafficCPASettings_InvertedThresholdRejected(t *testing.T) {
	in := testBuildInputs()
	in.TrafficCPASettings = TrafficCPASettingsSection{
		EscalationEnabled:     true,
		HorizonSeconds:        180,
		MinRelativeSpeedKnots: 30,
		MinClosureRateKnots:   20, // lower than MinRelativeSpeedKnots - inverted
	}
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if res := Validate(doc, mustMarshalLen(t, doc)); res.OK() {
		t.Fatal("expected minClosureRateKnots below minRelativeSpeedKnots to be rejected as inverted")
	}
}

func TestTrafficCPASettingsPreview_ShowsChange(t *testing.T) {
	doc := validDoc(t)
	doc.TrafficCPASettings = TrafficCPASettingsSection{
		EscalationEnabled: true, HorizonSeconds: 120, MinRelativeSpeedKnots: 25, MinClosureRateKnots: 35,
	}
	current := CurrentState{
		Configuration:      doc.Configuration,
		AlertSettings:      doc.AlertSettings,
		AutoRecordSettings: doc.AutoRecordSettings,
		TrafficCPASettings: TrafficCPASettingsSection{EscalationEnabled: false, HorizonSeconds: 180, MinRelativeSpeedKnots: 20, MinClosureRateKnots: 30},
	}
	preview := ComputePreview(doc, current)
	if len(preview.TrafficCPASettingsChanges) == 0 {
		t.Fatal("expected the preview to show trafficCpaSettings changing")
	}
	if !preview.HasChanges {
		t.Fatal("expected HasChanges to be true given a trafficCpaSettings diff")
	}
}
