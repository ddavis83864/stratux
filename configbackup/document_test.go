package configbackup

import (
	"testing"
	"time"

	"github.com/stratux/stratux/calprofile"
)

func testProfile(id, name string) calprofile.Profile {
	p := calprofile.Profile{
		ID:               id,
		Name:             name,
		IMUMapping:       [2]int{-1, 0},
		SensorQuaternion: [4]float64{0.1, 0.2, 0.3, 0.9},
		C:                [3]float64{0, 0, 0},
		D:                [3]float64{1, 2, 3},
		Kind:             calprofile.KindUser,
		SchemaVersion:    calprofile.SchemaVersion,
		CreatedAt:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ModifiedAt:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	p.RecomputeValidity()
	return p
}

func testBuildInputs() BuildInputs {
	created := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	return BuildInputs{
		SourceVersion: "2.0-pre5",
		SourceCommit:  "deadbeef",
		CreatedAtUTC:  &created,
		Configuration: ConfigurationSection{
			UATEnabled:     true,
			ESEnabled:      true,
			RegionSelected: 1,
		},
		CalibrationProfiles:        []calprofile.Profile{testProfile("profile-1316db66ab7a7767", "Current Installation")},
		ActiveCalibrationProfileID: "profile-1316db66ab7a7767",
		AlertSettings: AlertSettingsSection{
			MasterEnabled: true,
			AudioVolume:   0.5,
		},
		AutoRecordSettings: AutoRecordSettingsSection{
			Enabled:                false,
			StartGroundspeedKnots:  8,
			StartDwellSeconds:      30,
			StopGroundspeedKnots:   4,
			StopDwellSeconds:       120,
			GPSLossGraceSeconds:    30,
			RestartCooldownSeconds: 300,
		},
	}
}

func TestBuildDocument_Deterministic(t *testing.T) {
	a, err := BuildDocument(testBuildInputs())
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	b, err := BuildDocument(testBuildInputs())
	if err != nil {
		t.Fatalf("BuildDocument (2nd): %v", err)
	}
	if a.ContentChecksum != b.ContentChecksum {
		t.Fatalf("expected identical content checksums, got %q vs %q", a.ContentChecksum, b.ContentChecksum)
	}
	for k, v := range a.SectionChecksums {
		if b.SectionChecksums[k] != v {
			t.Fatalf("section %q checksum differs: %q vs %q", k, v, b.SectionChecksums[k])
		}
	}
}

func TestBuildDocument_ProfileOrderDoesNotAffectChecksum(t *testing.T) {
	in1 := testBuildInputs()
	in1.CalibrationProfiles = []calprofile.Profile{
		testProfile("profile-aaaaaaaaaaaaaaaa", "A"),
		testProfile("profile-bbbbbbbbbbbbbbbb", "B"),
	}
	in2 := testBuildInputs()
	in2.CalibrationProfiles = []calprofile.Profile{
		testProfile("profile-bbbbbbbbbbbbbbbb", "B"),
		testProfile("profile-aaaaaaaaaaaaaaaa", "A"),
	}
	a, err := BuildDocument(in1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildDocument(in2)
	if err != nil {
		t.Fatal(err)
	}
	if a.ContentChecksum != b.ContentChecksum {
		t.Fatalf("expected order-independent checksum, got %q vs %q", a.ContentChecksum, b.ContentChecksum)
	}
}

func TestBuildDocument_SourceIdentityAndSchemaCarried(t *testing.T) {
	doc, err := BuildDocument(testBuildInputs())
	if err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", doc.SchemaVersion, SchemaVersion)
	}
	if doc.SourceVersion != "2.0-pre5" || doc.SourceCommit != "deadbeef" {
		t.Errorf("source identity not carried through: %+v", doc)
	}
	if doc.MinimumCompatibleVersion != MinimumCompatibleSchemaVersion {
		t.Errorf("MinimumCompatibleVersion = %d, want %d", doc.MinimumCompatibleVersion, MinimumCompatibleSchemaVersion)
	}
}

func TestBuildDocument_TrustedTimeUnavailableIsHonestlyAbsent(t *testing.T) {
	in := testBuildInputs()
	in.CreatedAtUTC = nil
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatal(err)
	}
	if doc.CreatedAtUTC != nil {
		t.Errorf("expected nil CreatedAtUTC when trusted time unavailable, got %v", doc.CreatedAtUTC)
	}
}

func TestBuildDocument_NoProfiles(t *testing.T) {
	in := testBuildInputs()
	in.CalibrationProfiles = nil
	in.ActiveCalibrationProfileID = ""
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.CalibrationProfiles) != 0 {
		t.Errorf("expected zero profiles, got %d", len(doc.CalibrationProfiles))
	}
	if !Validate(doc, mustMarshalLen(t, doc)).OK() {
		t.Errorf("a document with zero profiles must still validate")
	}
}
