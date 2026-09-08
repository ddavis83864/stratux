package storagelifecycle

import (
	"crypto/rand"
	"encoding/hex"
)

// randomTempSuffix is AtomicWriter's default TempSuffixFunc - the same
// crypto/rand-backed pattern this project already uses for confirmation
// tokens (see configbackup's newConfigBackupToken, power's
// newShutdownToken), reused here because a temp-file suffix has exactly
// the same requirement: unique enough that two concurrent writers of the
// same final name never collide.
func randomTempSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("storagelifecycle: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
