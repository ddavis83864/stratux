package fisbcache

import (
	"sort"
	"testing"
)

func planKey(id string) Key { return TextKey(TextProductMETAR, id) }

func TestPlanEviction_EmptySnapshotIsEmptyPlan(t *testing.T) {
	if got := PlanEviction(map[Key]Entry{}, 1024, 10, 0); len(got) != 0 {
		t.Fatalf("expected an empty plan for an empty snapshot, got %v", got)
	}
}

func TestPlanEviction_AllExpiredEvictedRegardlessOfBudget(t *testing.T) {
	// METAR ExpireLimit is 3h (policy.go) - well past it, and budgets are
	// deliberately huge so only expiry, never budget, could explain the
	// result.
	snap := map[Key]Entry{
		planKey("A"): {Key: planKey("A"), ReceivedAtMonotonic: -100000, SizeBytes: 1},
		planKey("B"): {Key: planKey("B"), ReceivedAtMonotonic: -100000, SizeBytes: 1},
	}
	got := PlanEviction(snap, 1<<30, 1<<20, 0)
	if len(got) != 2 {
		t.Fatalf("expected both expired entries evicted regardless of budget, got %v", got)
	}
}

func TestPlanEviction_NothingExpiredUnderBudgetIsEmptyPlan(t *testing.T) {
	snap := map[Key]Entry{
		planKey("A"): {Key: planKey("A"), ReceivedAtMonotonic: 0, SizeBytes: 10},
		planKey("B"): {Key: planKey("B"), ReceivedAtMonotonic: 1, SizeBytes: 10},
	}
	got := PlanEviction(snap, 1<<30, 1<<20, 100 /* nowMonotonic close to receipt, nothing stale */)
	if len(got) != 0 {
		t.Fatalf("expected nothing evicted when fresh and well under budget, got %v", got)
	}
}

func TestPlanEviction_OverByteBudgetEvictsOldestReceivedFirst(t *testing.T) {
	// Three fresh (not expired) entries received in order A, B, C -
	// each 10 bytes, budget only 15 bytes -> must evict the oldest
	// (A) first, just enough to fit, leaving the newest behind.
	snap := map[Key]Entry{
		planKey("A"): {Key: planKey("A"), ReceivedAtMonotonic: 0, SizeBytes: 10},
		planKey("B"): {Key: planKey("B"), ReceivedAtMonotonic: 1, SizeBytes: 10},
		planKey("C"): {Key: planKey("C"), ReceivedAtMonotonic: 2, SizeBytes: 10},
	}
	got := PlanEviction(snap, 15, 0, 2 /* nowMonotonic: all still fresh */)
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 entries evicted to reach <=15 bytes from 30, got %v", got)
	}
	evicted := map[Key]bool{got[0]: true, got[1]: true}
	if !evicted[planKey("A")] || !evicted[planKey("B")] {
		t.Errorf("expected the two OLDEST entries (A, B) evicted, got %v", got)
	}
	if evicted[planKey("C")] {
		t.Error("expected the newest entry (C) to survive")
	}
}

func TestPlanEviction_OverEntryCountBudgetEvictsOldestReceivedFirst(t *testing.T) {
	snap := map[Key]Entry{
		planKey("A"): {Key: planKey("A"), ReceivedAtMonotonic: 0},
		planKey("B"): {Key: planKey("B"), ReceivedAtMonotonic: 1},
		planKey("C"): {Key: planKey("C"), ReceivedAtMonotonic: 2},
	}
	got := PlanEviction(snap, 0, 2, 2)
	if len(got) != 1 || got[0] != planKey("A") {
		t.Fatalf("expected exactly the single oldest entry (A) evicted to reach the 2-entry cap, got %v", got)
	}
}

func TestPlanEviction_ExpiredFirstThenOldestOfRemainingIfStillOverBudget(t *testing.T) {
	// D is expired (always evicted). After removing it, A/B/C (fresh)
	// still exceed a 2-entry budget, so the oldest of THOSE (A) must
	// also go - proving expired-first, then oldest-remaining, not just
	// "oldest of everything including already-expired."
	snap := map[Key]Entry{
		planKey("D"): {Key: planKey("D"), ReceivedAtMonotonic: -100000}, // expired
		planKey("A"): {Key: planKey("A"), ReceivedAtMonotonic: 0},
		planKey("B"): {Key: planKey("B"), ReceivedAtMonotonic: 1},
		planKey("C"): {Key: planKey("C"), ReceivedAtMonotonic: 2},
	}
	got := PlanEviction(snap, 0, 2, 2)
	evicted := map[Key]bool{}
	for _, k := range got {
		evicted[k] = true
	}
	if len(got) != 2 {
		t.Fatalf("expected D (expired) + A (oldest remaining) evicted = 2 total, got %v", got)
	}
	if !evicted[planKey("D")] {
		t.Error("expected the expired entry (D) evicted")
	}
	if !evicted[planKey("A")] {
		t.Error("expected the oldest of the remaining fresh entries (A) evicted")
	}
	if evicted[planKey("B")] || evicted[planKey("C")] {
		t.Errorf("expected B and C to survive, got %v", got)
	}
}

func TestPlanEviction_ZeroOrNegativeByteBudgetDisablesByteLimit(t *testing.T) {
	snap := map[Key]Entry{
		planKey("A"): {Key: planKey("A"), ReceivedAtMonotonic: 0, SizeBytes: 1 << 30}, // 1 GiB, would blow any real budget
	}
	for _, maxBytes := range []int64{0, -1} {
		got := PlanEviction(snap, maxBytes, 0, 0)
		if len(got) != 0 {
			t.Errorf("maxBytes=%d: expected the byte budget to be treated as disabled (no limit), got eviction %v", maxBytes, got)
		}
	}
}

func TestPlanEviction_ZeroOrNegativeEntryBudgetDisablesEntryLimit(t *testing.T) {
	snap := make(map[Key]Entry, 500)
	for i := 0; i < 500; i++ {
		k := planKey(string(rune('A' + i%26)))
		snap[Key{Class: ClassText, Identity: k.Identity + string(rune(i))}] = Entry{ReceivedAtMonotonic: float64(i)}
	}
	for _, maxEntries := range []int{0, -1} {
		got := PlanEviction(snap, 0, maxEntries, 0)
		if len(got) != 0 {
			t.Errorf("maxEntries=%d: expected the entry-count budget to be treated as disabled (no limit), got %d evicted", maxEntries, len(got))
		}
	}
}

func TestPlanEviction_BothBudgetsDisabledOnlyExpiredEvicted(t *testing.T) {
	snap := map[Key]Entry{
		planKey("fresh"):   {Key: planKey("fresh"), ReceivedAtMonotonic: 0, SizeBytes: 1 << 30},
		planKey("expired"): {Key: planKey("expired"), ReceivedAtMonotonic: -100000, SizeBytes: 1},
	}
	got := PlanEviction(snap, 0, 0, 0)
	if len(got) != 1 || got[0] != planKey("expired") {
		t.Fatalf("expected only the expired entry evicted with both budgets disabled, got %v", got)
	}
}

// TestPlanEviction_DeterministicTieBreakByIdentity proves two entries
// with the identical ReceivedAtMonotonic (a real possibility - multiple
// products can arrive in the same capture-worker tick) are ordered
// deterministically by Key.Identity, not by map-iteration order (which
// Go deliberately randomizes) - repeated runs against the same input
// must always produce the identical plan.
func TestPlanEviction_DeterministicTieBreakByIdentity(t *testing.T) {
	snap := map[Key]Entry{
		planKey("ZULU"):   {Key: planKey("ZULU"), ReceivedAtMonotonic: 5},
		planKey("ALPHA"):  {Key: planKey("ALPHA"), ReceivedAtMonotonic: 5},
		planKey("MIKE"):   {Key: planKey("MIKE"), ReceivedAtMonotonic: 5},
		planKey("BRAVO"):  {Key: planKey("BRAVO"), ReceivedAtMonotonic: 5},
		planKey("KILO"):   {Key: planKey("KILO"), ReceivedAtMonotonic: 5},
		planKey("YANKEE"): {Key: planKey("YANKEE"), ReceivedAtMonotonic: 5},
		planKey("ECHO"):   {Key: planKey("ECHO"), ReceivedAtMonotonic: 5},
		planKey("VICTOR"): {Key: planKey("VICTOR"), ReceivedAtMonotonic: 5},
		planKey("OSCAR"):  {Key: planKey("OSCAR"), ReceivedAtMonotonic: 5},
		planKey("QUEBEC"): {Key: planKey("QUEBEC"), ReceivedAtMonotonic: 5},
	}
	// Every entry shares the same ReceivedAtMonotonic, so a maxEntries
	// budget of 3 must evict exactly the 7 lexicographically-first
	// identities, every time, regardless of Go's randomized map order.
	var first []Key
	for i := 0; i < 10; i++ {
		got := PlanEviction(snap, 0, 3, 5)
		sort.Slice(got, func(a, b int) bool { return got[a].Identity < got[b].Identity })
		if i == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("run %d: plan length changed run to run: %v vs %v", i, got, first)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d: plan is not deterministic: %v vs %v", i, got, first)
			}
		}
	}
	if len(first) != 7 {
		t.Fatalf("expected exactly 7 entries evicted to reach the 3-entry cap from 10, got %d: %v", len(first), first)
	}
}

func TestPlanEviction_UnsupportedOrUnknownPolicyEntryNeverCrashes(t *testing.T) {
	// A Key with no known ProductPolicy (PolicyFor returns the zero
	// value) must be treated as FreshnessUnsupported by Freshness, which
	// is never FreshnessExpired - such an entry is therefore only ever
	// budget-evicted, never expiry-evicted, and PlanEviction must not
	// panic or misbehave when this occurs.
	snap := map[Key]Entry{
		{Class: "unsupported-class", Identity: "x"}: {ReceivedAtMonotonic: 0, SizeBytes: 1},
	}
	got := PlanEviction(snap, 0, 0, 1_000_000)
	if len(got) != 0 {
		t.Fatalf("an entry with no known policy must never be expiry-evicted, got %v", got)
	}
}
