package calprofile

import "time"

// LegacyCalibration is the pre-profile calibration state - exactly
// globalSettings.IMUMapping/SensorQuaternion/C/D (main/gen_gdl90.go)
// before any profile has ever existed. main/'s wiring reads these fields
// (unsynchronized, matching every other globalSettings access in the
// existing codebase - see calprofile's package doc comment) and passes
// them here; this package never reads globalSettings itself.
type LegacyCalibration struct {
	IMUMapping       [2]int
	SensorQuaternion [4]float64
	C, D             [3]float64
}

// sanitizeVector replaces any non-finite (NaN/Inf) component with 0,
// reporting whether it had to change anything. json.Marshal cannot
// represent NaN/Inf at all, so a non-finite value can never be "preserved
// as-is" in a stored profile - and a non-finite component is never a
// value main/sensors.go's calibration algorithm actually produces from a
// real sensor reading, so there is nothing legitimate being discarded:
// zeroing it and marking the affected axis uncalibrated is honest, not
// destructive, exactly because a real calibration never reaches this
// state in the first place.
func sanitizeVector(v []float64) (out []float64, changed bool) {
	out = make([]float64, len(v))
	for i, x := range v {
		if isFinite(x) {
			out[i] = x
		} else {
			out[i] = 0
			changed = true
		}
	}
	return out, changed
}

// BuildMigratedProfile derives the one-time "Current Installation" profile
// EnsureMigrated creates from a system's pre-profile legacy calibration.
// Pure and side-effect-free so it can be tested directly without a Store.
//
// The legacy calibration's values are copied verbatim (after sanitizing
// any non-finite component per sanitizeVector's doc comment) - never
// reset to zero, never silently discarded, matching the mission
// requirement that a working AHRS calibration is never destroyed by
// migration. LastCalibratedAt is deliberately left nil: the legacy
// calibration has no recorded timestamp of when it was actually set (it
// could be from any point up to the current boot), and fabricating "now"
// would overclaim freshness this package has no evidence for.
func BuildMigratedProfile(legacy LegacyCalibration, now time.Time) Profile {
	quaternion, quaternionSanitized := sanitizeVector(legacy.SensorQuaternion[:])
	c, cSanitized := sanitizeVector(legacy.C[:])
	d, dSanitized := sanitizeVector(legacy.D[:])
	anySanitized := quaternionSanitized || cSanitized || dSanitized

	p := Profile{
		ID:            NewID(),
		Name:          "Current Installation",
		IMUMapping:    legacy.IMUMapping,
		SchemaVersion: SchemaVersion,
		CreatedAt:     now,
		ModifiedAt:    now,
	}
	copy(p.SensorQuaternion[:], quaternion)
	copy(p.C[:], c)
	copy(p.D[:], d)
	p.RecomputeValidity()

	switch {
	case anySanitized:
		// A non-finite legacy value is never something the real
		// calibration algorithm produces - the input was already
		// unsafe to interpret, so the resulting profile is never
		// presented as trusted "migrated" data, regardless of whether
		// the sanitized vector's magnitude happens to look calibrated.
		p.Kind = KindUncalibrated
	case p.CalibrationComplete():
		p.Kind = KindMigrated
	default:
		// Genuinely never calibrated - the normal state for a
		// factory-fresh install (all-zero legacy vectors).
		p.Kind = KindUncalibrated
	}
	return p
}

// EnsureMigrated initializes store the first time a profile-aware build
// runs on a system that has never had a profile store: if store already
// has one or more profiles, this is a no-op that just returns the active
// one (idempotent - safe to call on every startup, not just the first).
// If the store is empty, it creates the migrated profile from legacy,
// marks it active, and returns it.
//
// If profiles already exist but the active pointer is missing or does not
// resolve (a corrupt/inconsistent store - see Store.Active's distinguished
// errors), this function does NOT guess or auto-repair by picking an
// arbitrary profile as active: it returns the underlying error so the
// caller (main/'s wiring) can report an honest DEGRADED readiness state
// rather than silently activating something that was never chosen.
func EnsureMigrated(store *Store, legacy LegacyCalibration, now time.Time) (Profile, error) {
	n, err := store.Count()
	if err != nil {
		return Profile{}, err
	}
	if n > 0 {
		return store.Active()
	}
	p := BuildMigratedProfile(legacy, now)
	if err := store.Save(p); err != nil {
		return Profile{}, err
	}
	if err := store.SetActiveID(p.ID, now); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// ApplyCalibration returns a copy of p with its calibration vectors
// replaced by the given values and LastCalibratedAt/ModifiedAt set to now
// - the pure transformation main/'s calibration-capture hook applies
// before calling Store.Save. which is either "level" (Set Level: C and
// SensorQuaternion) or "cal" (Zero Drift: D) mirrors the exact two action
// strings main/sensors.go's cal channel already uses
// (strings.Contains(action, "cal")/"level"), so the caller can pass the
// same action value through unchanged.
func ApplyCalibration(p Profile, action string, imuMapping [2]int, quaternion [4]float64, c, d [3]float64, now time.Time) Profile {
	updated := p
	switch action {
	case "level":
		updated.IMUMapping = imuMapping
		updated.SensorQuaternion = quaternion
		updated.C = c
	case "cal":
		updated.D = d
	}
	updated.ModifiedAt = now
	updated.RecomputeValidity()
	if updated.CalibrationComplete() {
		updated.LastCalibratedAt = &now
		if updated.Kind == KindUncalibrated {
			// A profile that was honestly marked uncalibrated (e.g. the
			// migrated-but-never-calibrated case) graduates to KindUser
			// once it actually receives a real calibration - it is no
			// longer just "whatever was inherited," it now reflects an
			// action the owner deliberately took.
			updated.Kind = KindUser
		}
	}
	return updated
}
