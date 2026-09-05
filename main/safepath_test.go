package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var testNamePattern = regexp.MustCompile(`^[a-z]+-[0-9]+\.txt$`)
var testDirPattern = regexp.MustCompile(`^rec-[0-9]+$`)

func TestResolveNameInDir_ExactMatchSucceeds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bundle-1.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, ok := resolveNameInDir(dir, "bundle-1.txt", testNamePattern)
	if !ok {
		t.Fatal("expected exact match to succeed")
	}
	if path != filepath.Join(dir, "bundle-1.txt") {
		t.Errorf("path = %q", path)
	}
}

func TestResolveNameInDir_PathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	// A file that genuinely exists one level up - if traversal worked,
	// this attack would successfully reach it.
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err == nil {
		defer os.Remove(outside)
	}
	attempts := []string{
		"../secret.txt",
		"../../etc/passwd",
		"..%2Fsecret.txt",
		"foo/../../secret.txt",
		"/etc/passwd",
	}
	for _, a := range attempts {
		if _, ok := resolveNameInDir(dir, a, testNamePattern); ok {
			t.Errorf("traversal attempt %q was NOT rejected", a)
		}
	}
}

func TestResolveNameInDir_NonExistentNameRejected(t *testing.T) {
	dir := t.TempDir()
	if _, ok := resolveNameInDir(dir, "bundle-1.txt", testNamePattern); ok {
		t.Error("a name matching the pattern but not present in the directory must still be rejected")
	}
}

func TestResolveNameInDir_EmptyNameRejected(t *testing.T) {
	dir := t.TempDir()
	if _, ok := resolveNameInDir(dir, "", testNamePattern); ok {
		t.Error("empty name must be rejected")
	}
}

func TestResolveNameInDir_MalformedNameFastRejected(t *testing.T) {
	dir := t.TempDir()
	// Even if a file with this exact (pattern-violating) name somehow
	// existed, it must never be served.
	if err := os.WriteFile(filepath.Join(dir, "not-a-valid-name"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolveNameInDir(dir, "not-a-valid-name", testNamePattern); ok {
		t.Error("a name not matching the expected shape must be rejected even if the file exists")
	}
}

func TestResolveNameInDir_DirectoryEntryNotServedAsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "bundle-1.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolveNameInDir(dir, "bundle-1.txt", testNamePattern); ok {
		t.Error("a directory entry must not be resolved as a downloadable file")
	}
}

func TestResolveNameInDir_MissingBaseDirRejected(t *testing.T) {
	if _, ok := resolveNameInDir("/nonexistent/path/xyz", "bundle-1.txt", testNamePattern); ok {
		t.Error("a nonexistent base directory must reject, not error out unsafely")
	}
}

func TestResolveSubdirInDir_ExactMatchSucceeds(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "rec-123"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, ok := resolveSubdirInDir(dir, "rec-123", testDirPattern)
	if !ok || path != filepath.Join(dir, "rec-123") {
		t.Errorf("path=%q ok=%v", path, ok)
	}
}

func TestResolveSubdirInDir_FileEntryNotServedAsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rec-123"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolveSubdirInDir(dir, "rec-123", testDirPattern); ok {
		t.Error("a plain file must not be resolved as a session subdirectory")
	}
}

func TestResolveSubdirInDir_TraversalRejected(t *testing.T) {
	dir := t.TempDir()
	for _, a := range []string{"../", "../../", "rec-1/../../etc"} {
		if _, ok := resolveSubdirInDir(dir, a, testDirPattern); ok {
			t.Errorf("traversal attempt %q was NOT rejected", a)
		}
	}
}
