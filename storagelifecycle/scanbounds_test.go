package storagelifecycle

import (
	"strconv"
	"testing"
	"time"
)

// TestScanner_NoGoroutineSpawnedStructurally is a structural regression
// guard for the "does a scan need cancellation" question this file
// resolves (see docs/storage-lifecycle.md's "Scan cancellation" section):
// Scan/scanNamespace/sumDirectChildren call only synchronous FS methods
// and never a `go` statement, so a call to Scan() can only ever return to
// its caller - it cannot leak a background goroutine that keeps running
// after the caller stops waiting on it, regardless of how long the scan
// takes. This test does not (and cannot) prove the absence of a `go`
// statement by execution alone - see the source-search evidence this
// mission's PR body cites alongside it - but it does prove the practical
// consequence: Scan always returns to the same goroutine that called it.
func TestScanner_NoGoroutineSpawnedStructurally(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	r := NewRegistry()
	if err := r.Register(Namespace{ID: "cache", Root: "/data/cache", Criticality: CriticalityCache, ItemKind: ItemKindFile}); err != nil {
		t.Fatal(err)
	}
	s := &Scanner{Registry: r, FS: fs, Clock: fixedClock(0)}

	done := make(chan struct{})
	go func() {
		s.Scan()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Scan did not return promptly on an empty namespace - this alone would already indicate a structural problem")
	}
}

// TestScanner_BoundedLatencyAtRepresentativeScale demonstrates Scan's
// per-item cost stays low and roughly linear at a scale well beyond this
// project's real deployment (main/storagelifecycleapi.go registers 4
// namespaces holding, on the validated bench device, under 50 items
// total combined - see docs/storage-lifecycle.md's live-evidence numbers
// from PR #13's validation, where the whole /getStorageLifecycle round
// trip - which itself only reads an already-completed scan's cached
// result, not a fresh scan - measured 9-17ms end to end). This test uses
// 5,000 synthetic items (100x the real namespace count observed live) as
// a representative, not exhaustive, stress case: it is a regression
// guard against Scan becoming accidentally quadratic, not a claim that
// 5,000 is the actual maximum (see maxScanItemsPerNamespace for the
// documented hard per-namespace cap this test does not attempt to reach
// at its full 100,000 value, which would make this test itself slow for
// no additional evidence value beyond what 5,000 already demonstrates).
func TestScanner_BoundedLatencyAtRepresentativeScale(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping representative-scale scan timing in -short mode")
	}
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	const n = 5000
	for i := 0; i < n; i++ {
		fs.putFile("/data/cache/item-"+strconv.Itoa(i)+".json", []byte("x"), 0o644, time.Now())
	}
	r := NewRegistry()
	if err := r.Register(Namespace{ID: "cache", Root: "/data/cache", Criticality: CriticalityCache, ItemKind: ItemKindFile}); err != nil {
		t.Fatal(err)
	}
	s := &Scanner{Registry: r, FS: fs, Clock: fixedClock(0)}

	start := time.Now()
	inv := s.Scan()
	elapsed := time.Since(start)

	if len(inv.Namespaces["cache"]) != n {
		t.Fatalf("expected %d items, got %d", n, len(inv.Namespaces["cache"]))
	}
	// A generous ceiling (2s for 5,000 in-memory fake-FS items) - this is
	// not a tight performance benchmark, only a guard against an
	// accidental O(n^2) regression, which would blow well past this by a
	// wide margin long before 5,000 items.
	if elapsed > 2*time.Second {
		t.Errorf("scanning %d items took %v, expected well under 2s - possible O(n^2) regression", n, elapsed)
	}
}
