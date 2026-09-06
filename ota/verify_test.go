package ota

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHashFile_KnownContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	got, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile error: %v", err)
	}
	// sha256("hello world\n")
	want := "a948904f2f0f479b8f8197694b30184b0d2ed1c1cd2a1ec0fb85d299a192a447"
	if got != want {
		t.Errorf("HashFile = %s, want %s", got, want)
	}
}

func TestVerifyPackageFile_MissingFile(t *testing.T) {
	err := VerifyPackageFile("/nonexistent/ota/verify/test.deb", "anything")
	if err == nil {
		t.Error("expected an error for a missing package file")
	}
}

func TestVerifyPackageFile_HashMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pkg.deb")
	if err := os.WriteFile(path, []byte("package contents"), 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if err := VerifyPackageFile(path, "0000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Error("expected an error for a hash mismatch")
	}
}

func TestVerifyPackageFile_MatchSucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pkg.deb")
	content := []byte("package contents")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	hash, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile error: %v", err)
	}
	if err := VerifyPackageFile(path, hash); err != nil {
		t.Errorf("expected success verifying a correct hash, got %v", err)
	}
	// Case-insensitivity.
	upper := ""
	for _, c := range hash {
		if c >= 'a' && c <= 'f' {
			upper += string(c - 32)
		} else {
			upper += string(c)
		}
	}
	if err := VerifyPackageFile(path, upper); err != nil {
		t.Errorf("expected success verifying an uppercase-hex hash, got %v", err)
	}
}
