package ota

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// HashFile computes the lowercase-hex SHA-256 of the file at path.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("could not open %s for hashing: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("could not read %s for hashing: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyPackageFile confirms path exists and its SHA-256 matches
// expectedSHA256 (case-insensitive). It is the direct guard against both
// "missing staged package" (the file is gone - a partial write, a wiped
// ephemeral mount, cross-boot data loss) and "hash mismatch" (the file is
// present but has changed - corruption, or a genuinely different package
// than the one that was originally verified and recorded).
func VerifyPackageFile(path, expectedSHA256 string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("staged package %s is missing", path)
		}
		return fmt.Errorf("could not stat staged package %s: %w", path, err)
	}
	got, err := HashFile(path)
	if err != nil {
		return err
	}
	if !equalFoldHex(got, expectedSHA256) {
		return fmt.Errorf("staged package %s hash mismatch: got %s, expected %s", path, got, expectedSHA256)
	}
	return nil
}

func equalFoldHex(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
