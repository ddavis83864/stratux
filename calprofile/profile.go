// Package calprofile implements persistent named aircraft calibration
// profiles for the Stratux AHRS: a level-reference quaternion (Set Level)
// and a gyro zero-drift bias (Zero Drift), tagged with an owner-facing
// name, optional aircraft registration/type/mounting note, and validity/
// timestamp bookkeeping, so the same receiver can move between airframes
// and restore the right mounting calibration instead of overwriting a
// single global one.
//
// Like readiness/recording/ota, this package is pure and hardware/cgo-free:
// every function here takes already-gathered values (the calibration
// vectors main/sensors.go's existing algorithm already computes) and
// returns a derived profile or judgment. Nothing in this package touches
// I2C, spawns a process, or depends on CGO; the thin glue that reads/
// writes globalSettings and wires HTTP handlers lives in main/.
//
// This is supplemental, non-certified equipment. A profile - however named
// or however "VALID" its calibration state - never makes the AHRS
// certified, and is never used for flight-control commands.
package calprofile

import (
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"
)

// ErrInvalidField is wrapped by ValidateProfile's returned errors so
// callers (main/'s HTTP handlers) can distinguish a validation failure
// from a storage/IO error with errors.Is, without string-matching.
var ErrInvalidField = errors.New("calprofile: invalid profile field")

// SchemaVersion is the current on-disk Profile schema version. Bump this
// and add a migration path in Store.Load (see store.go) whenever the
// Profile shape changes in a way older stored JSON cannot be read as-is.
const SchemaVersion = 1

// Profile kinds describe how a profile came to exist, for honest display
// ("this was auto-created from your existing calibration" vs. "you made
// this") - never a claim about certification.
const (
	// KindMigrated is the one profile EnsureMigrated creates the first
	// time a profile-aware build starts on a system with an existing
	// (pre-profile) legacy calibration - see migrate.go.
	KindMigrated = "migrated"
	// KindUser is any profile the owner explicitly created.
	KindUser = "user"
	// KindUncalibrated marks a profile - migrated or user-created - whose
	// level reference and/or gyro bias have never been successfully set.
	// Never silently treated the same as a calibrated profile.
	KindUncalibrated = "uncalibrated"
)

// Bounds enforced by ValidateProfile - see that function's doc comment for
// why each exists.
const (
	MaxProfiles          = 20
	MaxNameRunes         = 64
	MaxRegistrationRunes = 20
	MaxAircraftTypeRunes = 64
	MaxMountingNoteRunes = 200
)

// Profile is one named aircraft calibration profile.
//
// IMUMapping/SensorQuaternion/C/D are exactly globalSettings' fields of the
// same name (main/gen_gdl90.go) - this package does not reinterpret or
// rescale them, only stores/restores them under a name. LastCalibratedAt
// is a nullable pointer, never a Go zero-value time.Time, so "never
// calibrated" is never confused with a real (if old) calibration date -
// see docs/readiness-and-time-trust.md's OptionalTime pattern, which this
// mirrors with a plain *time.Time since Profile has no legacy field that
// needs OptionalTime's exact wire-compatibility behavior.
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Registration/AircraftType/MountingNote are all optional, free-text,
	// owner-facing identification - never used in any calibration or
	// readiness decision, purely descriptive.
	Registration string `json:"registration,omitempty"`
	AircraftType string `json:"aircraftType,omitempty"`
	MountingNote string `json:"mountingNote,omitempty"`

	IMUMapping       [2]int     `json:"imuMapping"`
	SensorQuaternion [4]float64 `json:"sensorQuaternion"`
	C                [3]float64 `json:"c"`
	D                [3]float64 `json:"d"`

	// LevelCalibrated/GyroCalibrated mirror the same nonzero-magnitude
	// check main/sensors.go's sensorAttitudeSender already uses to decide
	// whether a calibration exists (SensorQuaternion/D magnitude > 0) -
	// see RecomputeValidity, which derives these from the vectors above
	// rather than trusting a caller-supplied bool.
	LevelCalibrated bool `json:"levelCalibrated"`
	GyroCalibrated  bool `json:"gyroCalibrated"`

	// Kind is one of KindMigrated/KindUser/KindUncalibrated - see those
	// constants' doc comments. Never implies certification.
	Kind string `json:"kind"`

	SchemaVersion int `json:"schemaVersion"`

	CreatedAt  time.Time `json:"createdAt"`
	ModifiedAt time.Time `json:"modifiedAt"`
	// LastCalibratedAt is nil until Set Level or Zero Drift has actually
	// succeeded for this profile at least once - see ApplyCalibration.
	LastCalibratedAt *time.Time `json:"lastCalibratedAt"`
}

// CalibrationComplete reports whether both the level reference and the
// gyro zero-drift bias have been set - the only condition under which a
// profile's calibration should ever be presented as fully valid.
func (p Profile) CalibrationComplete() bool {
	return p.LevelCalibrated && p.GyroCalibrated
}

// vectorMagnitudeSquaredPositive reports whether v has nonzero magnitude,
// the same test main/sensors.go's sensorAttitudeSender already applies to
// decide whether globalSettings.D/SensorQuaternion hold a real calibration
// (see that file's outer loop: "d[0]*d[0]+d[1]*d[1]+d[2]*d[2] > 0").
func vectorMagnitudeSquaredPositive(v []float64) bool {
	sum := 0.0
	for _, x := range v {
		sum += x * x
	}
	return sum > 0
}

// RecomputeValidity sets LevelCalibrated/GyroCalibrated from the current
// SensorQuaternion/D vectors, and Kind to KindUncalibrated if neither
// component is calibrated and Kind was not already KindUser (a user
// profile keeps its identity as "user-created" even before its first
// calibration - only migration's own honesty-marking uses
// KindUncalibrated automatically). Call this after loading a profile from
// disk or before validating/saving one, rather than trusting a
// caller-supplied bool - the vectors are the source of truth.
func (p *Profile) RecomputeValidity() {
	p.LevelCalibrated = vectorMagnitudeSquaredPositive(p.SensorQuaternion[:])
	p.GyroCalibrated = vectorMagnitudeSquaredPositive(p.D[:])
}

// runeLen is utf8.RuneCountInString, named locally so every length check
// in this package visibly uses the unicode-safe (rune, not byte) count a
// display name in any script requires.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// ValidateProfile checks the bounds and required fields a Profile must
// satisfy before Store.Save will persist it. It does not check name
// uniqueness or the store's profile-count limit - those depend on the
// rest of the store's contents, so Store.Save checks them itself under
// its own lock, immediately after calling this function.
func ValidateProfile(p Profile) error {
	if !ValidID(p.ID) {
		return ErrInvalidID
	}
	if runeLen(p.Name) == 0 {
		return fmt.Errorf("%w: name is required", ErrInvalidField)
	}
	if runeLen(p.Name) > MaxNameRunes {
		return fmt.Errorf("%w: name exceeds %d characters", ErrInvalidField, MaxNameRunes)
	}
	if runeLen(p.Registration) > MaxRegistrationRunes {
		return fmt.Errorf("%w: registration exceeds %d characters", ErrInvalidField, MaxRegistrationRunes)
	}
	if runeLen(p.AircraftType) > MaxAircraftTypeRunes {
		return fmt.Errorf("%w: aircraft type exceeds %d characters", ErrInvalidField, MaxAircraftTypeRunes)
	}
	if runeLen(p.MountingNote) > MaxMountingNoteRunes {
		return fmt.Errorf("%w: mounting note exceeds %d characters", ErrInvalidField, MaxMountingNoteRunes)
	}
	if p.Kind != KindMigrated && p.Kind != KindUser && p.Kind != KindUncalibrated {
		return fmt.Errorf("%w: unrecognized kind %q", ErrInvalidField, p.Kind)
	}
	for _, v := range p.SensorQuaternion {
		if !isFinite(v) {
			return fmt.Errorf("%w: sensor quaternion contains a non-finite value", ErrInvalidField)
		}
	}
	for _, v := range p.C {
		if !isFinite(v) {
			return fmt.Errorf("%w: accelerometer calibration contains a non-finite value", ErrInvalidField)
		}
	}
	for _, v := range p.D {
		if !isFinite(v) {
			return fmt.Errorf("%w: gyro calibration contains a non-finite value", ErrInvalidField)
		}
	}
	return nil
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
