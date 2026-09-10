package wifiadmin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// ConfirmationToken is the server-held record behind one outstanding
// Wi-Fi apply transaction - the same shape and binding strategy as
// power/shutdown.go's and configbackup/token.go's own confirmation
// tokens (see docs/wifi-administration-hardening.md's "Lock ordering and
// token reuse" section for why this project's own established idiom is
// reused rather than inventing a new one). The random opaque token
// string a client presents back is generated and stored by main/ (this
// package stays pure); this type only captures the record's shape and
// the pure comparison logic used to verify one.
type ConfirmationToken struct {
	Token string
	// ProposedConfigChecksum binds this token to the EXACT proposed
	// configuration a preview was computed for - re-presenting a
	// modified body at apply time is rejected, matching
	// configbackup.ConfirmationToken.ContentChecksum.
	ProposedConfigChecksum string
	// CurrentConfigFingerprint binds this token to the exact
	// last-known-good configuration the preview's diff was computed
	// against - any change to that baseline (e.g. a concurrent Wi-Fi
	// transaction, or a direct /setSettings write) since the preview
	// invalidates the token, matching
	// configbackup.ConfirmationToken.CurrentStateFingerprint.
	CurrentConfigFingerprint string
	// BootSessionID binds this token to the daemon process that issued
	// it - a restart always invalidates every outstanding token, even
	// an otherwise unexpired one (and, for this feature specifically, a
	// restart is also exactly the moment startup recovery decides
	// whether to roll back an abandoned transaction - see Manager's own
	// doc comment).
	BootSessionID string
	// IssuedAtMonotonic/ExpiresAtMonotonic are seconds from an arbitrary
	// monotonic epoch, never wall-clock.
	IssuedAtMonotonic  float64
	ExpiresAtMonotonic float64
	// Used is set by the caller after a successful apply - VerifyToken
	// treats an already-used record as invalid, but marking it used is
	// the caller's responsibility (see configbackup.ConfirmationToken's
	// identical doc comment on why).
	Used bool
}

var (
	ErrTokenAlreadyUsed        = errors.New("wifiadmin: confirmation token already used")
	ErrTokenExpired            = errors.New("wifiadmin: confirmation token expired")
	ErrTokenBootSessionChanged = errors.New("wifiadmin: daemon restarted since preview")
	ErrTokenConfigChanged      = errors.New("wifiadmin: proposed configuration changed since preview")
	ErrTokenStateChanged       = errors.New("wifiadmin: current Wi-Fi configuration changed since preview")
	ErrTokenNotFound           = errors.New("wifiadmin: confirmation token not found")
)

// VerifyToken checks record against everything an apply request must
// still match to be honored. It mutates nothing.
func VerifyToken(record ConfirmationToken, presentedConfigChecksum, currentConfigFingerprint, bootSessionID string, nowMonotonic float64) error {
	if record.Used {
		return ErrTokenAlreadyUsed
	}
	if nowMonotonic > record.ExpiresAtMonotonic {
		return ErrTokenExpired
	}
	if record.BootSessionID != bootSessionID {
		return ErrTokenBootSessionChanged
	}
	if record.ProposedConfigChecksum != presentedConfigChecksum {
		return ErrTokenConfigChanged
	}
	if record.CurrentConfigFingerprint != currentConfigFingerprint {
		return ErrTokenStateChanged
	}
	return nil
}

// ChecksumConfig returns a deterministic SHA-256 hex checksum of c,
// covering every field including secrets (a checksum, never the secret
// itself, so this is safe to log/compare without disclosure) - used both
// as the "proposed configuration" binding and, applied to the current
// last-known-good configuration, as the "current state" fingerprint.
func ChecksumConfig(c Config) (string, error) {
	// json.Marshal on a fixed struct with fixed field order is
	// deterministic - no map types appear anywhere in Config or its
	// nested ClientNetwork slice (which preserves order), so no
	// separate canonicalization step is needed.
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
