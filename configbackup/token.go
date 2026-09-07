package configbackup

import (
	"errors"
	"sort"

	"github.com/stratux/stratux/calprofile"
)

// ConfirmationToken is the server-held record behind a one-time restore
// confirmation. The random opaque token string a client presents back is
// generated and stored by main/ (inherently stateful - this package stays
// pure); this type only captures the record's shape and the pure
// comparison logic used to verify one. See docs/configuration-backup-
// restore.md's "Confirmation token" section for the full contract.
type ConfirmationToken struct {
	// ContentChecksum binds this token to the exact uploaded document -
	// see Document.ContentChecksum.
	ContentChecksum string
	// CurrentStateFingerprint binds this token to the exact live
	// configuration state Preview was computed against - see
	// Fingerprint. Recomputed and compared at apply time; any change
	// (through this API, the existing settings APIs, or a calibration
	// profile API) invalidates the token.
	CurrentStateFingerprint string
	// BootSessionID binds this token to the daemon process that issued
	// it - a restart always invalidates every outstanding token, even an
	// otherwise unexpired one.
	BootSessionID string
	// IssuedAtMonotonic/ExpiresAtMonotonic are seconds from an arbitrary
	// monotonic epoch (main/'s stratuxClock), never wall-clock - a
	// restart-time clock jump must never extend or shorten a token's
	// effective lifetime unpredictably.
	IssuedAtMonotonic  float64
	ExpiresAtMonotonic float64
	// Used is set by the caller (main/) after a successful apply -
	// VerifyToken treats an already-used record as invalid, but marking
	// it used is the caller's responsibility: a pure function cannot
	// safely perform "check-then-mark" atomically across concurrent
	// callers without the caller's own mutex.
	Used bool
}

var (
	ErrTokenAlreadyUsed        = errors.New("configbackup: confirmation token already used")
	ErrTokenExpired            = errors.New("configbackup: confirmation token expired")
	ErrTokenBootSessionChanged = errors.New("configbackup: daemon restarted since preview")
	ErrTokenContentChanged     = errors.New("configbackup: uploaded backup content changed since preview")
	ErrTokenStateChanged       = errors.New("configbackup: current device configuration changed since preview")
	ErrTokenNotFound           = errors.New("configbackup: confirmation token not found")
)

// VerifyToken checks record against everything an apply request must
// still match to be honored. It mutates nothing.
func VerifyToken(record ConfirmationToken, presentedContentChecksum, currentStateFingerprint, bootSessionID string, nowMonotonic float64) error {
	if record.Used {
		return ErrTokenAlreadyUsed
	}
	if nowMonotonic > record.ExpiresAtMonotonic {
		return ErrTokenExpired
	}
	if record.BootSessionID != bootSessionID {
		return ErrTokenBootSessionChanged
	}
	if record.ContentChecksum != presentedContentChecksum {
		return ErrTokenContentChanged
	}
	if record.CurrentStateFingerprint != currentStateFingerprint {
		return ErrTokenStateChanged
	}
	return nil
}

// Fingerprint returns a deterministic, checksummed summary of current -
// this package's substitute for an explicit, globally-incremented
// "configuration generation" counter (which would require instrumenting
// every existing settings/profile-mutating handler in main/). Any change
// to current's fields, alert settings, profiles, or active profile
// produces a different fingerprint, which is exactly what token
// invalidation needs: detecting *that* something changed, not enumerating
// what.
func Fingerprint(current CurrentState) (string, error) {
	profiles := make([]calprofile.Profile, len(current.CalibrationProfiles))
	copy(profiles, current.CalibrationProfiles)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })

	return sectionChecksum(struct {
		Configuration       ConfigurationSection   `json:"configuration"`
		CalibrationProfiles profilesSectionPayload `json:"calibrationProfiles"`
		AlertSettings       AlertSettingsSection   `json:"alertSettings"`
	}{
		Configuration:       current.Configuration,
		CalibrationProfiles: profilesSectionPayload{Profiles: profiles, ActiveID: current.ActiveProfileID},
		AlertSettings:       current.AlertSettings,
	})
}
