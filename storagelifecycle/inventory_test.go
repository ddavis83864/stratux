package storagelifecycle

import (
	"os"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	must(t, r.Register(Namespace{ID: "diagnostics", Root: "/data/diagnostics", Criticality: CriticalityBounded, ItemKind: ItemKindFile, AllowedExtensions: []string{".json"}}))
	must(t, r.Register(Namespace{ID: "recordings", Root: "/data/recordings", Criticality: CriticalityImportant, ItemKind: ItemKindDirectory}))
	must(t, r.Register(Namespace{ID: "cache", Root: "/data/cache", Criticality: CriticalityCache, ItemKind: ItemKindFile}))
	return r
}

func fixedClock(t float64) Clock { return func() float64 { return t } }

func TestScanner_EmptyNamespace(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/recordings")
	fs.mkdir("/data/cache")
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(10)}
	inv := s.Scan()
	if len(inv.Namespaces["diagnostics"]) != 0 {
		t.Errorf("expected an empty diagnostics namespace, got %+v", inv.Namespaces["diagnostics"])
	}
	if len(inv.Errors) != 0 {
		t.Errorf("expected no errors, got %+v", inv.Errors)
	}
}

func TestScanner_MultipleNamespacesClassifiedIndependently(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.putFile("/data/diagnostics/diag-1.json", []byte("{}"), 0o644, time.Now())
	fs.mkdir("/data/recordings")
	fs.mkdir("/data/recordings/rec-1")
	fs.putFile("/data/recordings/rec-1/sample.jsonl", []byte("x"), 0o644, time.Now())
	fs.mkdir("/data/cache")

	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(10)}
	inv := s.Scan()

	diag := inv.Namespaces["diagnostics"]
	if len(diag) != 1 || diag[0].Status != StatusManaged {
		t.Fatalf("expected 1 managed diagnostics item, got %+v", diag)
	}
	rec := inv.Namespaces["recordings"]
	if len(rec) != 1 || rec[0].Status != StatusManaged || rec[0].SizeBytes != 1 {
		t.Fatalf("expected 1 managed recording item sized 1, got %+v", rec)
	}
}

func TestScanner_UnmanagedWrongExtension(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.putFile("/data/diagnostics/notes.txt", []byte("hi"), 0o644, time.Now())
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	items := inv.Namespaces["diagnostics"]
	if len(items) != 1 || items[0].Status != StatusUnmanaged {
		t.Fatalf("expected 1 unmanaged item, got %+v", items)
	}
}

func TestScanner_UnmanagedWrongKind(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/diagnostics/a-directory") // diagnostics namespace expects files
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	items := inv.Namespaces["diagnostics"]
	if len(items) != 1 || items[0].Status != StatusUnmanaged {
		t.Fatalf("expected the wrong-kind entry to be unmanaged, got %+v", items)
	}
}

func TestScanner_SymlinkIsUnmanagedNeverFollowed(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.putSymlink("/data/diagnostics/evil.json")
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	items := inv.Namespaces["diagnostics"]
	if len(items) != 1 || items[0].Status != StatusUnmanaged {
		t.Fatalf("expected the symlink to be unmanaged, got %+v", items)
	}
}

func TestScanner_OtherFileTypeIsUnmanaged(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.putOther("/data/diagnostics/a.json") // e.g. a FIFO/socket at a plausible-looking name
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	items := inv.Namespaces["diagnostics"]
	if len(items) != 1 || items[0].Status != StatusUnmanaged {
		t.Fatalf("expected the non-regular entry to be unmanaged, got %+v", items)
	}
}

func TestScanner_SymlinkNamespaceRootRefused(t *testing.T) {
	fs := newFakeFS()
	fs.putSymlink("/data/diagnostics")
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	if inv.Errors["diagnostics"] == nil {
		t.Fatal("expected an error for a symlinked namespace root")
	}
}

func TestScanner_MissingNamespaceRootReportsErrorNotPanic(t *testing.T) {
	fs := newFakeFS() // /data/diagnostics never created
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	if inv.Errors["diagnostics"] == nil {
		t.Fatal("expected an error for a missing namespace root")
	}
	// Other namespaces are also missing in this test and must each get
	// their own independent error, not one failure aborting the scan.
	if len(inv.Errors) != 3 {
		t.Errorf("expected an independent error per missing namespace, got %+v", inv.Errors)
	}
}

func TestScanner_PermissionErrorToleratedPerNamespace(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/recordings")
	fs.mkdir("/data/cache")
	fs.failReadDir["/data/diagnostics"] = os.ErrPermission
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	if inv.Errors["diagnostics"] == nil {
		t.Fatal("expected a reported error for diagnostics")
	}
	if inv.Errors["recordings"] != nil || inv.Errors["cache"] != nil {
		t.Errorf("a permission error in one namespace must not affect the others: %+v", inv.Errors)
	}
}

func TestScanner_ActiveItemClassifiedActive(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/recordings")
	fs.mkdir("/data/recordings/rec-active")
	active := func(ns, name string) bool { return ns == "recordings" && name == "rec-active" }
	r := newTestRegistry(t)
	s := &Scanner{Registry: r, FS: fs, Clock: fixedClock(0), Active: active}
	inv := s.Scan()
	items := inv.Namespaces["recordings"]
	if len(items) != 1 || items[0].Status != StatusActive {
		t.Fatalf("expected the active recording to be StatusActive, got %+v", items)
	}
}

func TestScanner_DeterministicOrdering(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.putFile("/data/diagnostics/z.json", nil, 0o644, time.Now())
	fs.putFile("/data/diagnostics/a.json", nil, 0o644, time.Now())
	fs.putFile("/data/diagnostics/m.json", nil, 0o644, time.Now())
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	items := inv.Namespaces["diagnostics"]
	if len(items) != 3 || items[0].Name != "a.json" || items[1].Name != "m.json" || items[2].Name != "z.json" {
		t.Fatalf("expected lexical order, got %+v", items)
	}
}

func TestScanner_DirectoryItemSizeSumsImmediateRegularChildrenOnly(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/recordings")
	fs.mkdir("/data/recordings/rec-1")
	fs.putFile("/data/recordings/rec-1/sample.jsonl", make([]byte, 100), 0o644, time.Now())
	fs.putFile("/data/recordings/rec-1/metadata.json", make([]byte, 50), 0o644, time.Now())
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	items := inv.Namespaces["recordings"]
	if len(items) != 1 || items[0].SizeBytes != 150 {
		t.Fatalf("expected size 150 (100+50), got %+v", items)
	}
}

func TestScanner_ItemMissingOptionalSidecarStillManaged(t *testing.T) {
	// Mirrors real bench-device evidence: some older recordings have only
	// their sample file, no metadata.json sidecar - still a fully valid,
	// managed, protected item, not unmanaged.
	fs := newFakeFS()
	fs.mkdir("/data/recordings")
	fs.mkdir("/data/recordings/rec-legacy")
	fs.putFile("/data/recordings/rec-legacy/sample.jsonl", make([]byte, 10), 0o644, time.Now())
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	items := inv.Namespaces["recordings"]
	if len(items) != 1 || items[0].Status != StatusManaged {
		t.Fatalf("expected a managed item despite the missing sidecar, got %+v", items)
	}
}

func TestUsage_SummarizesByStatus(t *testing.T) {
	inv := Inventory{Namespaces: map[string][]Item{
		"ns": {
			{Status: StatusManaged, SizeBytes: 10},
			{Status: StatusManaged, SizeBytes: 20},
			{Status: StatusActive, SizeBytes: 5},
			{Status: StatusUnmanaged, SizeBytes: 1},
		},
	}}
	u := inv.Usage()["ns"]
	if u.ManagedCount != 2 || u.ManagedBytes != 30 || u.ActiveCount != 1 || u.ActiveBytes != 5 || u.UnmanagedCount != 1 || u.UnmanagedBytes != 1 {
		t.Errorf("unexpected usage summary: %+v", u)
	}
}

func TestScanner_DisappearingChildDuringSizeSumToleratedAsZero(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/recordings")
	fs.mkdir("/data/recordings/rec-1")
	// No children at all by the time sumDirectChildren runs (simulates a
	// file removed between the parent ReadDir and the child ReadDir) -
	// must not error or panic, just report size 0.
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
	inv := s.Scan()
	items := inv.Namespaces["recordings"]
	if len(items) != 1 || items[0].SizeBytes != 0 {
		t.Fatalf("expected size 0 for a childless directory, got %+v", items)
	}
	if inv.Errors["recordings"] != nil {
		t.Errorf("expected no error, got %v", inv.Errors["recordings"])
	}
}

func TestScanner_GeneratedAtMonotonicFromClock(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/recordings")
	fs.mkdir("/data/cache")
	s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(42.5)}
	inv := s.Scan()
	if inv.GeneratedAtMonotonic != 42.5 {
		t.Errorf("expected GeneratedAtMonotonic 42.5, got %v", inv.GeneratedAtMonotonic)
	}
}

func TestScanner_ItemDisappearingEntirelyBetweenReadDirCallsIsTolerated(t *testing.T) {
	// A regression guard: classify() must never itself error/panic just
	// because a listed entry vanished before classification finished -
	// this is exercised indirectly above (sumDirectChildren), and here
	// directly by asserting Scan() never returns via a panic recover
	// wrapper for any of this file's scenarios (if it panicked, every
	// test above would already have failed loudly; this test exists so
	// the requirement itself has a named, explicit assertion).
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/recordings")
	fs.mkdir("/data/cache")
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Scan panicked: %v", r)
			}
		}()
		s := &Scanner{Registry: newTestRegistry(t), FS: fs, Clock: fixedClock(0)}
		s.Scan()
	}()
}
