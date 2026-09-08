package storagelifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestAtomicWrite_RealFilesystemRoundTrip is this package's one
// real-filesystem test (every other test uses the fake FS - see
// fakefs_test.go): it exercises NewOSFS/osFS directly against a real
// t.TempDir() ext4-or-whatever-the-CI-host-uses directory, proving the
// actual os.OpenFile/os.Rename/os.Open-for-fsync sequence genuinely
// works end to end, not just this package's own model of it.
func TestAtomicWrite_RealFilesystemRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ns := Namespace{ID: "cache", Root: dir, Criticality: CriticalityCache, ItemKind: ItemKindFile}
	w := NewAtomicWriter(NewOSFS())

	path, err := w.Write(context.Background(), AtomicWriteOptions{
		Namespace: ns,
		Name:      "product.json",
		Mode:      0o644,
		Write:     writeBytes([]byte(`{"real":true}`)),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read the written file: %v", err)
	}
	if string(got) != `{"real":true}` {
		t.Errorf("unexpected content: %s", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file left in the directory (no leftover temp file), got %v", entries)
	}

	// A second write to the same name must cleanly replace it.
	path2, err := w.Write(context.Background(), AtomicWriteOptions{
		Namespace: ns, Name: "product.json", Mode: 0o644, Write: writeBytes([]byte(`{"real":"again"}`)),
	})
	if err != nil {
		t.Fatalf("unexpected error on replacement: %v", err)
	}
	got2, _ := os.ReadFile(path2)
	if string(got2) != `{"real":"again"}` {
		t.Errorf("unexpected content after replacement: %s", got2)
	}
}

// TestScanner_RealFilesystem exercises the Scanner against a real
// directory too, so inventory.go's Lstat/ReadDir usage is proven against
// actual filesystem semantics, not only the fake's approximation of them.
func TestScanner_RealFilesystem(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "a.json"), filepath.Join(dir, "link.json")); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}

	r := NewRegistry()
	must(t, r.Register(Namespace{ID: "cache", Root: dir, Criticality: CriticalityCache, ItemKind: ItemKindFile, AllowedExtensions: []string{".json"}}))
	s := &Scanner{Registry: r, FS: NewOSFS(), Clock: fixedClock(0)}
	inv := s.Scan()

	items := inv.Namespaces["cache"]
	if len(items) != 2 {
		t.Fatalf("expected 2 entries, got %+v", items)
	}
	byName := map[string]Item{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if byName["a.json"].Status != StatusManaged {
		t.Errorf("expected a.json managed, got %+v", byName["a.json"])
	}
	if byName["link.json"].Status != StatusUnmanaged {
		t.Errorf("expected the real symlink to be classified unmanaged, got %+v", byName["link.json"])
	}
}
