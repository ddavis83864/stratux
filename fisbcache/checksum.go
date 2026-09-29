package fisbcache

import (
	"crypto/sha256"
	"encoding/hex"
)

// checksum detects accidental corruption of s only - see
// PersistedEntry.PayloadChecksum's own doc comment for why this is
// explicitly not an authentication mechanism, mirroring
// configbackup.sectionChecksum's identical, already-documented design.
func checksum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
