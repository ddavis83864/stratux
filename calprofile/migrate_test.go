package calprofile

import (
	"math"
	"testing"
	"time"
)

func TestBuildMigratedProfile_PreservesCalibratedLegacyValues(t *testing.T) {
	legacy := LegacyCalibration{
		IMUMapping:       [2]int{-1, 0},
		SensorQuaternion: [4]float64{0.1, 0.2, 0.3, 0.4},
		C:                [3]float64{1.0, 0.0, 0.0},
		D:                [3]float64{0.01, 0.02, 0.03},
	}
	now := time.Now().UTC()
	p := BuildMigratedProfile(legacy, now)

	if p.Name != "Current Installation" {
		t.Errorf("Name = %q, want %q", p.Name, "Current Installation")
	}
	if p.IMUMapping != legacy.IMUMapping || p.SensorQuaternion != legacy.SensorQuaternion || p.C != legacy.C || p.D != legacy.D {
		t.Error("migrated profile must preserve the legacy calibration values verbatim")
	}
	if p.Kind != KindMigrated {
		t.Errorf("Kind = %q, want %q for a fully-calibrated legacy state", p.Kind, KindMigrated)
	}
	if !p.CalibrationComplete() {
		t.Error("a migrated profile from a fully-calibrated legacy state should be calibration-complete")
	}
	if p.LastCalibratedAt != nil {
		t.Error("LastCalibratedAt must be nil - migration has no real timestamp for when the legacy calibration was actually set")
	}
	if !ValidID(p.ID) {
		t.Errorf("migrated profile must have a valid, freshly-generated ID, got %q", p.ID)
	}
}

func TestBuildMigratedProfile_NeverCalibratedLegacyIsHonestlyUncalibrated(t *testing.T) {
	// The normal factory-fresh state: all-zero vectors.
	p := BuildMigratedProfile(LegacyCalibration{}, time.Now().UTC())
	if p.Kind != KindUncalibrated {
		t.Errorf("Kind = %q, want %q for an all-zero legacy calibration", p.Kind, KindUncalibrated)
	}
	if p.CalibrationComplete() {
		t.Error("an all-zero legacy calibration must not report CalibrationComplete")
	}
}

func TestBuildMigratedProfile_NonFiniteLegacyValueIsSanitizedNotDestroyed(t *testing.T) {
	legacy := LegacyCalibration{
		SensorQuaternion: [4]float64{math.NaN(), 0.2, 0.3, 0.4},
		D:                [3]float64{0.01, 0.02, 0.03},
	}
	p := BuildMigratedProfile(legacy, time.Now().UTC())
	for _, v := range p.SensorQuaternion {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("a stored profile must never contain a non-finite value, got %v", p.SensorQuaternion)
		}
	}
	if p.Kind != KindUncalibrated {
		t.Errorf("a legacy calibration containing NaN must be honestly marked uncalibrated, got Kind=%q", p.Kind)
	}
	// The finite components of D (unaffected by the NaN in
	// SensorQuaternion) must still be preserved, not zeroed wholesale.
	if p.D != legacy.D {
		t.Errorf("D should be unaffected by a NaN elsewhere, got %v want %v", p.D, legacy.D)
	}
}

func TestEnsureMigrated_FirstStartupCreatesProfile(t *testing.T) {
	s := NewStore(t.TempDir())
	legacy := LegacyCalibration{
		SensorQuaternion: [4]float64{1, 0, 0, 0},
		D:                [3]float64{0.1, 0.1, 0.1},
	}
	p, err := EnsureMigrated(s, legacy, time.Now().UTC())
	if err != nil {
		t.Fatalf("EnsureMigrated: %v", err)
	}
	if p.Name != "Current Installation" {
		t.Errorf("Name = %q", p.Name)
	}
	active, err := s.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active.ID != p.ID {
		t.Error("migrated profile must be marked active")
	}
}

func TestEnsureMigrated_IdempotentOnRepeatedStartup(t *testing.T) {
	s := NewStore(t.TempDir())
	legacy := LegacyCalibration{SensorQuaternion: [4]float64{1, 0, 0, 0}, D: [3]float64{0.1, 0.1, 0.1}}
	now := time.Now().UTC()
	first, err := EnsureMigrated(s, legacy, now)
	if err != nil {
		t.Fatal(err)
	}
	// Second call, simulating a second startup - even with DIFFERENT
	// legacy values (as if globalSettings had since changed), it must be
	// a pure no-op: never overwrite an existing profile store.
	differentLegacy := LegacyCalibration{SensorQuaternion: [4]float64{9, 9, 9, 9}, D: [3]float64{9, 9, 9}}
	second, err := EnsureMigrated(s, differentLegacy, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Error("EnsureMigrated must be idempotent - a second call must not create a new profile")
	}
	if second.SensorQuaternion != first.SensorQuaternion {
		t.Error("EnsureMigrated must never overwrite an existing profile store's calibration on a later startup")
	}
	n, _ := s.Count()
	if n != 1 {
		t.Errorf("expected exactly 1 profile after two EnsureMigrated calls, got %d", n)
	}
}

func TestEnsureMigrated_NeverResetsWorkingCalibrationToZero(t *testing.T) {
	s := NewStore(t.TempDir())
	legacy := LegacyCalibration{
		SensorQuaternion: [4]float64{0.5, 0.5, 0.5, 0.5},
		C:                [3]float64{1, 0, 0},
		D:                [3]float64{0.02, 0.02, 0.02},
	}
	p, err := EnsureMigrated(s, legacy, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if p.SensorQuaternion == [4]float64{} || p.D == [3]float64{} {
		t.Error("a working legacy calibration must never be reset to zero by migration")
	}
}

func TestEnsureMigrated_ExistingProfilesWithValidActive_ReturnsActive(t *testing.T) {
	s := NewStore(t.TempDir())
	p := newTestProfile("Already Set Up")
	s.Save(p)
	s.SetActiveID(p.ID, time.Now().UTC())

	got, err := EnsureMigrated(s, LegacyCalibration{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != p.ID {
		t.Error("EnsureMigrated with existing profiles must return the already-active one, not migrate again")
	}
	n, _ := s.Count()
	if n != 1 {
		t.Errorf("expected no new profile to be created, got %d profiles", n)
	}
}

func TestEnsureMigrated_ProfilesExistButNoActivePointer_ReturnsDistinguishedError(t *testing.T) {
	s := NewStore(t.TempDir())
	p := newTestProfile("Orphaned")
	s.Save(p) // saved, but never activated

	_, err := EnsureMigrated(s, LegacyCalibration{}, time.Now().UTC())
	if err != ErrNoActiveProfile {
		t.Errorf("expected ErrNoActiveProfile for a store with profiles but no active pointer, got %v", err)
	}
}

func TestApplyCalibration_SetLevel(t *testing.T) {
	p := newTestProfile("Test")
	now := time.Now().UTC()
	updated := ApplyCalibration(p, "level", [2]int{-1, 0}, [4]float64{1, 0, 0, 0}, [3]float64{1, 0, 0}, [3]float64{}, now)
	if updated.SensorQuaternion != [4]float64{1, 0, 0, 0} {
		t.Error("Set Level should update SensorQuaternion")
	}
	if !updated.LevelCalibrated {
		t.Error("LevelCalibrated should be true after Set Level")
	}
	if updated.GyroCalibrated {
		t.Error("Set Level alone must not mark GyroCalibrated true")
	}
	if updated.LastCalibratedAt != nil {
		t.Error("LastCalibratedAt must stay nil until BOTH level and gyro calibration are complete")
	}
}

func TestApplyCalibration_ZeroDrift(t *testing.T) {
	p := newTestProfile("Test")
	now := time.Now().UTC()
	updated := ApplyCalibration(p, "cal", [2]int{}, [4]float64{}, [3]float64{}, [3]float64{0.1, 0.1, 0.1}, now)
	if updated.D != [3]float64{0.1, 0.1, 0.1} {
		t.Error("Zero Drift should update D")
	}
	if !updated.GyroCalibrated {
		t.Error("GyroCalibrated should be true after Zero Drift")
	}
}

func TestApplyCalibration_BothStepsMarkLastCalibrated(t *testing.T) {
	p := newTestProfile("Test")
	now := time.Now().UTC()
	p = ApplyCalibration(p, "level", [2]int{-1, 0}, [4]float64{1, 0, 0, 0}, [3]float64{1, 0, 0}, [3]float64{}, now)
	p = ApplyCalibration(p, "cal", [2]int{}, [4]float64{}, [3]float64{}, [3]float64{0.1, 0.1, 0.1}, now)
	if !p.CalibrationComplete() {
		t.Fatal("both steps applied should yield CalibrationComplete")
	}
	if p.LastCalibratedAt == nil {
		t.Fatal("LastCalibratedAt should be set once calibration is complete")
	}
	if !p.LastCalibratedAt.Equal(now) {
		t.Errorf("LastCalibratedAt = %v, want %v", p.LastCalibratedAt, now)
	}
}

func TestApplyCalibration_UncalibratedGraduatesToUser(t *testing.T) {
	p := newTestProfile("Migrated")
	p.Kind = KindUncalibrated
	now := time.Now().UTC()
	p = ApplyCalibration(p, "level", [2]int{-1, 0}, [4]float64{1, 0, 0, 0}, [3]float64{1, 0, 0}, [3]float64{}, now)
	p = ApplyCalibration(p, "cal", [2]int{}, [4]float64{}, [3]float64{}, [3]float64{0.1, 0.1, 0.1}, now)
	if p.Kind != KindUser {
		t.Errorf("Kind = %q, want %q once an uncalibrated profile receives a real calibration", p.Kind, KindUser)
	}
}

func TestApplyCalibration_DoesNotModifyInactiveProfileState(t *testing.T) {
	// ApplyCalibration is pure - applying it to a copy must never affect
	// the original value's fields (guards against an accidental shared
	// backing array on the fixed-size arrays).
	original := newTestProfile("Original")
	original.SensorQuaternion = [4]float64{9, 9, 9, 9}
	updated := ApplyCalibration(original, "level", [2]int{-1, 0}, [4]float64{1, 0, 0, 0}, [3]float64{1, 0, 0}, [3]float64{}, time.Now().UTC())
	if original.SensorQuaternion != [4]float64{9, 9, 9, 9} {
		t.Error("ApplyCalibration must not mutate its input profile")
	}
	if updated.SensorQuaternion == original.SensorQuaternion {
		t.Error("updated profile should differ from the original after calibration")
	}
}
