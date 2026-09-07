package configbackup

import (
	"strings"
	"testing"

	"github.com/stratux/stratux/calprofile"
)

func validDoc(t *testing.T) Document {
	t.Helper()
	doc, err := BuildDocument(testBuildInputs())
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	return doc
}

func TestValidate_ValidCurrentVersionBackup(t *testing.T) {
	doc := validDoc(t)
	res := Validate(doc, mustMarshalLen(t, doc))
	if !res.OK() {
		t.Fatalf("expected valid document to pass, got errors: %v", res.Errors)
	}
}

func TestValidate_UnsupportedFutureSchema(t *testing.T) {
	doc := validDoc(t)
	doc.SchemaVersion = SchemaVersion + 1
	res := Validate(doc, mustMarshalLen(t, doc))
	if res.OK() {
		t.Fatal("expected a future schema version to be rejected")
	}
}

func TestValidate_UnsupportedOlderSchema(t *testing.T) {
	doc := validDoc(t)
	doc.SchemaVersion = MinimumCompatibleSchemaVersion - 1
	res := Validate(doc, mustMarshalLen(t, doc))
	if res.OK() {
		t.Fatal("expected a too-old schema version to be rejected")
	}
}

func TestValidate_OversizedRejected(t *testing.T) {
	doc := validDoc(t)
	res := Validate(doc, MaxDocumentBytes+1)
	if res.OK() {
		t.Fatal("expected an oversized document to be rejected")
	}
	if !strings.Contains(res.Errors[0], "exceeds") {
		t.Errorf("expected a size-related error, got %v", res.Errors)
	}
}

func TestValidate_ChecksumMismatchDetected(t *testing.T) {
	doc := validDoc(t)
	doc.Configuration.RegionSelected = 2 // mutate content after checksums were stamped
	res := Validate(doc, mustMarshalLen(t, doc))
	if res.OK() {
		t.Fatal("expected a tampered document to fail checksum validation")
	}
}

func TestValidate_MissingContentChecksumRejected(t *testing.T) {
	doc := validDoc(t)
	doc.ContentChecksum = ""
	res := Validate(doc, mustMarshalLen(t, doc))
	if res.OK() {
		t.Fatal("expected a missing content checksum to be rejected")
	}
}

func TestValidate_MissingSectionChecksumRejected(t *testing.T) {
	doc := validDoc(t)
	delete(doc.SectionChecksums, "alertSettings")
	res := Validate(doc, mustMarshalLen(t, doc))
	if res.OK() {
		t.Fatal("expected a missing section checksum to be rejected")
	}
}

// TestValidate_NoWrites is not a filesystem assertion (this package never
// touches a filesystem) but documents the guarantee: calling Validate
// repeatedly on the same input must never change its result.
func TestValidate_NoWrites(t *testing.T) {
	doc := validDoc(t)
	size := mustMarshalLen(t, doc)
	first := Validate(doc, size)
	second := Validate(doc, size)
	if first.OK() != second.OK() || len(first.Errors) != len(second.Errors) {
		t.Fatalf("Validate must be idempotent: %v vs %v", first, second)
	}
}

func rebuildWithProfiles(t *testing.T, profiles []calprofile.Profile, activeID string) Document {
	t.Helper()
	in := testBuildInputs()
	in.CalibrationProfiles = profiles
	in.ActiveCalibrationProfileID = activeID
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	return doc
}

func TestValidate_ExcessiveProfilesRejected(t *testing.T) {
	profiles := make([]calprofile.Profile, MaxProfiles+1)
	for i := range profiles {
		profiles[i] = testProfile(calprofile.NewID(), "P")
	}
	doc := rebuildWithProfiles(t, profiles, "")
	res := Validate(doc, mustMarshalLen(t, doc))
	if res.OK() {
		t.Fatal("expected too many profiles to be rejected")
	}
	if !errorsContain(res.Errors, ErrTooManyProfiles) {
		t.Errorf("expected ErrTooManyProfiles, got %v", res.Errors)
	}
}

func TestValidate_UnsafeProfileIDRejected(t *testing.T) {
	p := testProfile("profile-1316db66ab7a7767", "X")
	p.ID = "../../etc/passwd"
	doc := rebuildWithProfiles(t, []calprofile.Profile{p}, "")
	res := Validate(doc, mustMarshalLen(t, doc))
	if res.OK() {
		t.Fatal("expected an unsafe profile id to be rejected")
	}
}

func TestValidate_DuplicateProfileIDRejected(t *testing.T) {
	doc := validDoc(t)
	dup := doc.CalibrationProfiles[0]
	doc.CalibrationProfiles = append(doc.CalibrationProfiles, dup)
	// Recompute checksums so this test exercises the duplicate-ID check
	// itself, not an incidental checksum failure.
	rebuilt, err := BuildDocument(BuildInputs{
		SourceVersion: doc.SourceVersion, SourceCommit: doc.SourceCommit, CreatedAtUTC: doc.CreatedAtUTC,
		Configuration: doc.Configuration, CalibrationProfiles: doc.CalibrationProfiles,
		ActiveCalibrationProfileID: doc.ActiveCalibrationProfileID, AlertSettings: doc.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := Validate(rebuilt, mustMarshalLen(t, rebuilt))
	if res.OK() {
		t.Fatal("expected a duplicate profile id to be rejected")
	}
}

func TestValidate_MissingActiveProfileTargetRejected(t *testing.T) {
	doc := rebuildWithProfiles(t, []calprofile.Profile{testProfile("profile-1316db66ab7a7767", "X")}, "profile-ffffffffffffffff")
	res := Validate(doc, mustMarshalLen(t, doc))
	if res.OK() {
		t.Fatal("expected a dangling active-profile reference to be rejected")
	}
	if !errorsContain(res.Errors, ErrDanglingActiveProfile) {
		t.Errorf("expected ErrDanglingActiveProfile, got %v", res.Errors)
	}
}

func TestValidate_InvalidCalibrationDataRejected(t *testing.T) {
	p := testProfile("profile-1316db66ab7a7767", strings.Repeat("x", 1000)) // exceeds calprofile.MaxNameRunes
	doc := rebuildWithProfiles(t, []calprofile.Profile{p}, "")
	res := Validate(doc, mustMarshalLen(t, doc))
	if res.OK() {
		t.Fatal("expected an over-length profile name to be rejected")
	}
}

func TestValidate_NegativeAlertThresholdRejected(t *testing.T) {
	doc := validDoc(t)
	doc.AlertSettings.MonitoringHorizontalNM = -1
	rebuilt, err := BuildDocument(BuildInputs{
		SourceVersion: doc.SourceVersion, SourceCommit: doc.SourceCommit, CreatedAtUTC: doc.CreatedAtUTC,
		Configuration: doc.Configuration, CalibrationProfiles: doc.CalibrationProfiles,
		ActiveCalibrationProfileID: doc.ActiveCalibrationProfileID, AlertSettings: doc.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := Validate(rebuilt, mustMarshalLen(t, rebuilt))
	if res.OK() {
		t.Fatal("expected a negative alert threshold to be rejected")
	}
}

func TestValidate_OutOfRangeAudioVolumeRejected(t *testing.T) {
	doc := validDoc(t)
	doc.AlertSettings.AudioVolume = 1.5
	rebuilt, err := BuildDocument(BuildInputs{
		SourceVersion: doc.SourceVersion, SourceCommit: doc.SourceCommit, CreatedAtUTC: doc.CreatedAtUTC,
		Configuration: doc.Configuration, CalibrationProfiles: doc.CalibrationProfiles,
		ActiveCalibrationProfileID: doc.ActiveCalibrationProfileID, AlertSettings: doc.AlertSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := Validate(rebuilt, mustMarshalLen(t, rebuilt))
	if res.OK() {
		t.Fatal("expected an out-of-range audio volume to be rejected")
	}
}

func TestValidate_PrivacyIncludedWarnsNotBlocks(t *testing.T) {
	in := testBuildInputs()
	in.Configuration.PrivacySensitiveIncluded = true
	in.Configuration.PrivacySensitive = PrivacySensitiveSection{OwnshipModeS: "A1B2C3"}
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatal(err)
	}
	res := Validate(doc, mustMarshalLen(t, doc))
	if !res.OK() {
		t.Fatalf("a deliberately-included privacy section must not block validation, got errors: %v", res.Errors)
	}
	if len(res.Warnings) == 0 {
		t.Error("expected a privacy-inclusion warning")
	}
}

func TestValidate_PrivacyOmittedNoWarning(t *testing.T) {
	doc := validDoc(t) // PrivacySensitiveIncluded false, PrivacySensitive empty, by construction
	res := Validate(doc, mustMarshalLen(t, doc))
	if !res.OK() {
		t.Fatalf("expected a document with no privacy section to validate cleanly, got errors: %v", res.Errors)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("expected no warning when the privacy section is genuinely omitted, got %v", res.Warnings)
	}
}

// TestValidate_PrivacySectionMismatchRejected is the exact defect this
// check exists to catch: a document that claims "no privacy-sensitive
// data" (PrivacySensitiveIncluded: false) while actually carrying it.
// This must be rejected outright, not merely warned about - see
// ErrPrivacySectionMismatch's doc comment.
func TestValidate_PrivacySectionMismatchRejected(t *testing.T) {
	in := testBuildInputs()
	in.Configuration.PrivacySensitiveIncluded = false
	in.Configuration.PrivacySensitive = PrivacySensitiveSection{OGNPilot: "Jane Doe"}
	doc, err := BuildDocument(in)
	if err != nil {
		t.Fatal(err)
	}
	res := Validate(doc, mustMarshalLen(t, doc))
	if res.OK() {
		t.Fatal("expected a privacy-section mismatch (data present, included=false) to be rejected")
	}
	if !errorsContain(res.Errors, ErrPrivacySectionMismatch) {
		t.Errorf("expected ErrPrivacySectionMismatch, got %v", res.Errors)
	}
}

func errorsContain(msgs []string, target error) bool {
	for _, m := range msgs {
		if strings.Contains(m, target.Error()) {
			return true
		}
	}
	return false
}
