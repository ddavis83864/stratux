package calprofile

import (
	"strings"
	"testing"
)

func validProfile() Profile {
	p := Profile{
		ID:            NewID(),
		Name:          "Cherokee Six",
		Registration:  "N432NC",
		AircraftType:  "PA-32-300",
		MountingNote:  "Rear-left window",
		SchemaVersion: SchemaVersion,
	}
	p.SensorQuaternion = [4]float64{1, 0, 0, 0}
	p.D = [3]float64{0.1, 0.1, 0.1}
	p.RecomputeValidity()
	p.Kind = KindUser
	return p
}

func TestProfile_CalibrationComplete(t *testing.T) {
	p := validProfile()
	if !p.CalibrationComplete() {
		t.Error("a profile with nonzero SensorQuaternion and D should be calibration-complete")
	}
	p.D = [3]float64{}
	p.RecomputeValidity()
	if p.CalibrationComplete() {
		t.Error("zeroing D should make CalibrationComplete false")
	}
}

func TestProfile_RecomputeValidity_ZeroVectorsAreUncalibrated(t *testing.T) {
	var p Profile
	p.RecomputeValidity()
	if p.LevelCalibrated || p.GyroCalibrated {
		t.Error("an all-zero profile must report both calibrations as false")
	}
}

func TestValidateProfile_RequiresName(t *testing.T) {
	p := validProfile()
	p.Name = ""
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for an empty name")
	}
}

func TestValidateProfile_NameLengthBound(t *testing.T) {
	p := validProfile()
	p.Name = strings.Repeat("a", MaxNameRunes+1)
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for a too-long name")
	}
	p.Name = strings.Repeat("a", MaxNameRunes)
	if err := ValidateProfile(p); err != nil {
		t.Errorf("a name at exactly the limit should be valid, got %v", err)
	}
}

func TestValidateProfile_UnicodeNameIsRuneSafe(t *testing.T) {
	// Each of these runes is multiple bytes in UTF-8 but one rune each -
	// the length bound must count runes, not bytes.
	p := validProfile()
	p.Name = strings.Repeat("✈", MaxNameRunes)
	if err := ValidateProfile(p); err != nil {
		t.Errorf("a %d-rune unicode name at the limit should be valid, got %v", MaxNameRunes, err)
	}
	p.Name = strings.Repeat("✈", MaxNameRunes+1)
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for a unicode name exceeding the rune limit")
	}
}

func TestValidateProfile_FieldLengthBounds(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*Profile, string)
		limit int
	}{
		{"registration", func(p *Profile, s string) { p.Registration = s }, MaxRegistrationRunes},
		{"aircraftType", func(p *Profile, s string) { p.AircraftType = s }, MaxAircraftTypeRunes},
		{"mountingNote", func(p *Profile, s string) { p.MountingNote = s }, MaxMountingNoteRunes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validProfile()
			c.apply(&p, strings.Repeat("x", c.limit+1))
			if err := ValidateProfile(p); err == nil {
				t.Errorf("%s: expected an error exceeding the %d-rune limit", c.name, c.limit)
			}
		})
	}
}

func TestValidateProfile_InvalidID(t *testing.T) {
	p := validProfile()
	p.ID = "../../etc/passwd"
	if err := ValidateProfile(p); err != ErrInvalidID {
		t.Errorf("expected ErrInvalidID for a traversal-shaped id, got %v", err)
	}
}

func TestValidateProfile_UnrecognizedKind(t *testing.T) {
	p := validProfile()
	p.Kind = "bogus"
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for an unrecognized Kind")
	}
}

func TestValidateProfile_NonFiniteValuesRejected(t *testing.T) {
	for _, bad := range []float64{
		func() float64 { var x float64; return x / x }(),   // NaN
		1.0 / func() float64 { var x float64; return x }(), // +Inf
	} {
		p := validProfile()
		p.SensorQuaternion[0] = bad
		if err := ValidateProfile(p); err == nil {
			t.Errorf("expected an error for a non-finite SensorQuaternion component (%v)", bad)
		}
	}
}

func TestNewID_MatchesValidID(t *testing.T) {
	for i := 0; i < 20; i++ {
		id := NewID()
		if !ValidID(id) {
			t.Errorf("NewID produced an id that fails its own ValidID check: %q", id)
		}
	}
}

func TestNewID_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := NewID()
		if seen[id] {
			t.Fatalf("NewID produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}

func TestValidID_RejectsTraversalShapes(t *testing.T) {
	for _, bad := range []string{
		"../../etc/passwd",
		"profile-",
		"profile-tooshort",
		"profile-0123456789abcdef/../../etc",
		"",
		"PROFILE-0123456789ABCDEF",
	} {
		if ValidID(bad) {
			t.Errorf("ValidID incorrectly accepted malformed/traversal id: %q", bad)
		}
	}
}
