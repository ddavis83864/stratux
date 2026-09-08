package storagelifecycle

import "testing"

func TestNamespace_ValidateRejectsRelativeRoot(t *testing.T) {
	ns := Namespace{ID: "x", Root: "relative/path", Criticality: CriticalityCache}
	if err := ns.Validate(); err == nil {
		t.Fatal("expected an error for a relative Root")
	}
}

func TestNamespace_ValidateRejectsUncleanRoot(t *testing.T) {
	ns := Namespace{ID: "x", Root: "/data/../data/x", Criticality: CriticalityCache}
	if err := ns.Validate(); err == nil {
		t.Fatal("expected an error for a non-Clean Root")
	}
}

func TestNamespace_ValidateRejectsBadID(t *testing.T) {
	for _, id := range []string{"", "has/slash", "has.dot"} {
		ns := Namespace{ID: id, Root: "/data/x", Criticality: CriticalityCache}
		if err := ns.Validate(); err == nil {
			t.Errorf("expected an error for ID %q", id)
		}
	}
}

func TestNamespace_ValidateRejectsUnknownCriticality(t *testing.T) {
	ns := Namespace{ID: "x", Root: "/data/x", Criticality: "bogus"}
	if err := ns.Validate(); err == nil {
		t.Fatal("expected an error for an unknown criticality")
	}
}

func TestNamespace_ValidateRejectsNegativeMinRetention(t *testing.T) {
	ns := Namespace{ID: "x", Root: "/data/x", Criticality: CriticalityCache, MinRetention: -1}
	if err := ns.Validate(); err == nil {
		t.Fatal("expected an error for negative MinRetention")
	}
}

func TestRegistry_RejectsDuplicateID(t *testing.T) {
	r := NewRegistry()
	must(t, r.Register(Namespace{ID: "a", Root: "/data/a", Criticality: CriticalityCache}))
	if err := r.Register(Namespace{ID: "a", Root: "/data/b", Criticality: CriticalityCache}); err == nil {
		t.Fatal("expected an error for a duplicate namespace ID")
	}
}

func TestRegistry_RejectsOverlappingRoots(t *testing.T) {
	r := NewRegistry()
	must(t, r.Register(Namespace{ID: "a", Root: "/data/a", Criticality: CriticalityCache}))
	cases := []string{"/data/a", "/data/a/nested", "/data"}
	for _, root := range cases {
		if err := r.Register(Namespace{ID: "b", Root: root, Criticality: CriticalityCache}); err == nil {
			t.Errorf("expected an overlap error for root %q", root)
		}
	}
}

func TestRegistry_AllowsSiblingRootsWithSharedPrefix(t *testing.T) {
	// "/data/ab" and "/data/abc" share a string prefix but are NOT nested
	// - rootsOverlap must not be fooled by a naive strings.HasPrefix on
	// the raw strings without the trailing separator.
	r := NewRegistry()
	must(t, r.Register(Namespace{ID: "a", Root: "/data/ab", Criticality: CriticalityCache}))
	if err := r.Register(Namespace{ID: "b", Root: "/data/abc", Criticality: CriticalityCache}); err != nil {
		t.Errorf("sibling roots with a shared string prefix must not be rejected as overlapping: %v", err)
	}
}

func TestRegistry_NamespacesReturnsRegistrationOrder(t *testing.T) {
	r := NewRegistry()
	must(t, r.Register(Namespace{ID: "z", Root: "/data/z", Criticality: CriticalityCache}))
	must(t, r.Register(Namespace{ID: "a", Root: "/data/a", Criticality: CriticalityCache}))
	got := r.Namespaces()
	if len(got) != 2 || got[0].ID != "z" || got[1].ID != "a" {
		t.Errorf("expected registration order [z a], got %+v", got)
	}
}

func TestRegistry_LookupMissing(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("expected ok=false for an unregistered ID")
	}
}

func TestSafeJoin_ValidName(t *testing.T) {
	got, err := SafeJoin("/data/recordings", "rec-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/data/recordings/rec-1" {
		t.Errorf("got %q", got)
	}
}

func TestSafeJoin_RejectsTraversal(t *testing.T) {
	for _, name := range []string{"..", "../escape", "..\\escape", "a/../../etc/passwd"} {
		if _, err := SafeJoin("/data/recordings", name); err == nil {
			t.Errorf("expected traversal rejection for %q", name)
		}
	}
}

func TestSafeJoin_RejectsAbsolute(t *testing.T) {
	if _, err := SafeJoin("/data/recordings", "/etc/passwd"); err == nil {
		t.Fatal("expected rejection of an absolute name")
	}
}

func TestSafeJoin_RejectsSeparatorInName(t *testing.T) {
	for _, name := range []string{"a/b", "a\\b"} {
		if _, err := SafeJoin("/data/recordings", name); err == nil {
			t.Errorf("expected rejection of a name containing a separator: %q", name)
		}
	}
}

func TestSafeJoin_RejectsEmpty(t *testing.T) {
	if _, err := SafeJoin("/data/recordings", ""); err == nil {
		t.Fatal("expected rejection of an empty name")
	}
}

func TestSafeJoin_RejectsDotAndDotDot(t *testing.T) {
	for _, name := range []string{".", ".."} {
		if _, err := SafeJoin("/data/recordings", name); err == nil {
			t.Errorf("expected rejection of %q", name)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
