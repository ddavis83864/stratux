package configbackup

import (
	"testing"

	"github.com/stratux/stratux/calprofile"
)

func testCurrentState() CurrentState {
	return CurrentState{
		Version: "2.0-pre5",
		Commit:  "current-commit",
		Configuration: ConfigurationSection{
			UATEnabled:     true,
			ESEnabled:      true,
			RegionSelected: 1,
		},
		CalibrationProfiles: []calprofile.Profile{testProfile("profile-1316db66ab7a7767", "Current Installation")},
		ActiveProfileID:     "profile-1316db66ab7a7767",
		AlertSettings: AlertSettingsSection{
			MasterEnabled: true,
			AudioVolume:   0.5,
		},
	}
}

func TestComputePreview_NoChanges(t *testing.T) {
	current := testCurrentState()
	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: current.Configuration, CalibrationProfiles: current.CalibrationProfiles,
		ActiveCalibrationProfileID: current.ActiveProfileID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := ComputePreview(doc, current)
	if p.HasChanges {
		t.Fatalf("expected no changes for a no-op restore, got %+v", p)
	}
	if len(p.UnchangedProfiles) != 1 || len(p.AddedProfiles) != 0 || len(p.UpdatedProfiles) != 0 {
		t.Errorf("expected exactly one unchanged profile, got %+v", p)
	}
	if p.ActiveProfileChange != nil {
		t.Errorf("expected no active-profile change, got %+v", p.ActiveProfileChange)
	}
}

func TestComputePreview_SingleBenignSettingChange(t *testing.T) {
	current := testCurrentState()
	backupCfg := current.Configuration
	backupCfg.RegionSelected = 2
	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: backupCfg, CalibrationProfiles: current.CalibrationProfiles,
		ActiveCalibrationProfileID: current.ActiveProfileID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := ComputePreview(doc, current)
	if !p.HasChanges {
		t.Fatal("expected a change to be detected")
	}
	if len(p.ConfigurationChanges) != 1 {
		t.Fatalf("expected exactly one configuration change, got %+v", p.ConfigurationChanges)
	}
	if p.ConfigurationChanges[0].Field != "regionSelected" {
		t.Errorf("expected regionSelected to change, got %q", p.ConfigurationChanges[0].Field)
	}
	if len(p.AlertSettingsChanges) != 0 {
		t.Errorf("expected no unrelated alert-settings changes, got %+v", p.AlertSettingsChanges)
	}
}

func TestComputePreview_AddedProfile(t *testing.T) {
	current := testCurrentState()
	backupProfiles := append([]calprofile.Profile{}, current.CalibrationProfiles...)
	backupProfiles = append(backupProfiles, testProfile("profile-2222222222222222", "New Aircraft"))
	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: current.Configuration, CalibrationProfiles: backupProfiles,
		ActiveCalibrationProfileID: current.ActiveProfileID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := ComputePreview(doc, current)
	if len(p.AddedProfiles) != 1 || p.AddedProfiles[0].ID != "profile-2222222222222222" {
		t.Fatalf("expected exactly one added profile, got %+v", p.AddedProfiles)
	}
	if len(p.UnchangedProfiles) != 1 {
		t.Errorf("expected the existing profile to remain unchanged, got %+v", p.UnchangedProfiles)
	}
}

func TestComputePreview_UpdatedProfile(t *testing.T) {
	current := testCurrentState()
	updated := testProfile("profile-1316db66ab7a7767", "Renamed")
	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: current.Configuration, CalibrationProfiles: []calprofile.Profile{updated},
		ActiveCalibrationProfileID: current.ActiveProfileID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := ComputePreview(doc, current)
	if len(p.UpdatedProfiles) != 1 || p.UpdatedProfiles[0].Name != "Renamed" {
		t.Fatalf("expected exactly one updated profile, got %+v", p.UpdatedProfiles)
	}
}

func TestComputePreview_ActiveProfileChange(t *testing.T) {
	current := testCurrentState()
	other := testProfile("profile-3333333333333333", "Other Aircraft")
	profiles := append(current.CalibrationProfiles, other)
	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: current.Configuration, CalibrationProfiles: profiles,
		ActiveCalibrationProfileID: other.ID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := ComputePreview(doc, current)
	if p.ActiveProfileChange == nil {
		t.Fatal("expected an active-profile change to be reported")
	}
	if p.ActiveProfileChange.Proposed != other.ID {
		t.Errorf("expected proposed active id %q, got %v", other.ID, p.ActiveProfileChange.Proposed)
	}
}

func TestComputePreview_UnsupportedSectionIgnoredHonestly(t *testing.T) {
	// A backup from a hypothetically newer, still-compatible schema might
	// carry fields this build doesn't know about at the JSON level; since
	// Document is strongly typed (no interface{} passthrough - see the
	// package doc comment), any field this build doesn't declare is
	// simply absent from Document after unmarshal, not silently applied.
	// This test documents that expectation rather than exercising new
	// wire bytes (there is no newer schema yet to construct).
	current := testCurrentState()
	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: current.Configuration, CalibrationProfiles: current.CalibrationProfiles,
		ActiveCalibrationProfileID: current.ActiveProfileID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != SchemaVersion {
		t.Fatalf("expected current schema version, got %d", doc.SchemaVersion)
	}
}

func TestComputePreview_PrivacyIncluded(t *testing.T) {
	current := testCurrentState()
	backupCfg := current.Configuration
	backupCfg.PrivacySensitiveIncluded = true
	backupCfg.PrivacySensitive = PrivacySensitiveSection{OGNReg: "N12345"}
	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: backupCfg, CalibrationProfiles: current.CalibrationProfiles,
		ActiveCalibrationProfileID: current.ActiveProfileID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := ComputePreview(doc, current)
	if !p.PrivacySectionIncluded {
		t.Fatal("expected PrivacySectionIncluded to be true")
	}
	found := false
	for _, n := range p.PrivacySensitiveFieldNames {
		if n == "ognReg" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ognReg to be named, got %v", p.PrivacySensitiveFieldNames)
	}
	// A privacy-sensitive field must never leak into the generic
	// undifferentiated configuration diff.
	for _, c := range p.ConfigurationChanges {
		if c.Field == "privacySensitive" || c.Field == "privacySensitiveIncluded" {
			t.Errorf("privacy fields must not appear as a generic configuration change, got %q", c.Field)
		}
	}
}

// TestComputePreview_PrivacyOmittedIsPreserved is the default,
// sanitized-export case: a backup that never captured privacy-sensitive
// fields must report them as omitted (not "will be cleared"), and the
// current device's real values (whatever they are) are never touched by
// applying it.
func TestComputePreview_PrivacyOmittedIsPreserved(t *testing.T) {
	current := testCurrentState()
	current.Configuration.PrivacySensitiveIncluded = true // real device state
	current.Configuration.PrivacySensitive = PrivacySensitiveSection{OwnshipModeS: "ABC123"}

	backupCfg := current.Configuration
	backupCfg.PrivacySensitiveIncluded = false
	backupCfg.PrivacySensitive = PrivacySensitiveSection{} // sanitized export

	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: backupCfg, CalibrationProfiles: current.CalibrationProfiles,
		ActiveCalibrationProfileID: current.ActiveProfileID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := ComputePreview(doc, current)
	if p.PrivacySectionIncluded {
		t.Fatal("expected PrivacySectionIncluded to be false for a sanitized export")
	}
	if len(p.PrivacySensitiveFieldNames) != 0 {
		t.Errorf("expected no privacy field names when the section is omitted, got %v", p.PrivacySensitiveFieldNames)
	}
	if p.PrivacyPreserved == "" {
		t.Error("expected a non-empty preservation note when the privacy section is omitted")
	}
	for _, c := range p.ConfigurationChanges {
		if c.Field == "privacySensitive" || c.Field == "privacySensitiveIncluded" {
			t.Errorf("omitted privacy section must never surface as a proposed change, got %q", c.Field)
		}
	}
}

func TestComputePreview_RestartRequiredHeuristic(t *testing.T) {
	current := testCurrentState()
	backupCfg := current.Configuration
	backupCfg.UATEnabled = !backupCfg.UATEnabled
	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: backupCfg, CalibrationProfiles: current.CalibrationProfiles,
		ActiveCalibrationProfileID: current.ActiveProfileID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := ComputePreview(doc, current)
	if !p.RestartRequired {
		t.Error("expected a radio-enablement change to require a restart")
	}
}

func TestComputePreview_CosmeticChangeNeverRequiresRestart(t *testing.T) {
	current := testCurrentState()
	backupCfg := current.Configuration
	backupCfg.DarkMode = !backupCfg.DarkMode
	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: backupCfg, CalibrationProfiles: current.CalibrationProfiles,
		ActiveCalibrationProfileID: current.ActiveProfileID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := ComputePreview(doc, current)
	if p.RestartRequired {
		t.Error("expected a purely cosmetic change to never require a restart")
	}
}

func TestComputePreview_Deterministic(t *testing.T) {
	current := testCurrentState()
	doc, err := BuildDocument(BuildInputs{
		SourceVersion: current.Version, SourceCommit: current.Commit,
		Configuration: ConfigurationSection{RegionSelected: 2}, CalibrationProfiles: current.CalibrationProfiles,
		ActiveCalibrationProfileID: current.ActiveProfileID, AlertSettings: current.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	a := ComputePreview(doc, current)
	b := ComputePreview(doc, current)
	if len(a.ConfigurationChanges) != len(b.ConfigurationChanges) {
		t.Fatalf("expected deterministic preview, got %+v vs %+v", a, b)
	}
}
