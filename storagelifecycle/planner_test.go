package storagelifecycle

import (
	"testing"
	"time"
)

func cacheNamespace() Namespace {
	return Namespace{ID: "cache", Root: "/data/cache", Criticality: CriticalityCache, ItemKind: ItemKindFile}
}

func TestPlan_NoQuotaConfiguredIsNoOp(t *testing.T) {
	items := []Item{{Status: StatusManaged, SizeBytes: 1000}}
	p := Plan(cacheNamespace(), items, Quota{}, PlanParams{})
	if !p.TargetMet || len(p.Candidates) != 0 {
		t.Fatalf("expected a no-op plan, got %+v", p)
	}
}

func TestPlan_AlreadyWithinQuotaIsNoOp(t *testing.T) {
	items := []Item{{Status: StatusManaged, SizeBytes: 100}}
	p := Plan(cacheNamespace(), items, Quota{MaxBytes: 1000}, PlanParams{})
	if !p.TargetMet || len(p.Candidates) != 0 {
		t.Fatalf("expected a no-op plan, got %+v", p)
	}
}

func TestPlan_ProtectedCriticalityNeverProposesEviction(t *testing.T) {
	ns := Namespace{ID: "recordings", Root: "/data/recordings", Criticality: CriticalityImportant, ItemKind: ItemKindDirectory}
	items := []Item{{Name: "rec-1", Status: StatusManaged, SizeBytes: 2000}}
	p := Plan(ns, items, Quota{MaxBytes: 1000}, PlanParams{})
	if p.TargetMet {
		t.Fatal("an over-quota protected namespace must report TargetMet=false, never silently succeed")
	}
	if len(p.Candidates) != 0 {
		t.Fatalf("expected zero candidates for a protected namespace, got %+v", p.Candidates)
	}
	found := false
	for _, s := range p.Skipped {
		if s.Name == "rec-1" && s.Reason == SkipProtectedCriticality {
			found = true
		}
	}
	if !found {
		t.Errorf("expected rec-1 to be recorded as skipped for its criticality, got %+v", p.Skipped)
	}
}

func TestPlan_BoundedCriticalityNeverProposesEviction(t *testing.T) {
	// Diagnostics: this foundation must never plan eviction against it -
	// see Policy.Evictable's doc comment.
	ns := Namespace{ID: "diagnostics", Root: "/data/diagnostics", Criticality: CriticalityBounded, ItemKind: ItemKindFile}
	items := []Item{{Name: "diag-1", Status: StatusManaged, SizeBytes: 2000}}
	p := Plan(ns, items, Quota{MaxBytes: 1000}, PlanParams{})
	if len(p.Candidates) != 0 {
		t.Fatalf("diagnostics must never be planned for eviction by this foundation, got %+v", p.Candidates)
	}
}

func TestPlan_ActiveItemNeverCandidate(t *testing.T) {
	items := []Item{
		{Name: "active-1", Status: StatusActive, SizeBytes: 5000},
	}
	p := Plan(cacheNamespace(), items, Quota{MaxBytes: 1000}, PlanParams{})
	if len(p.Candidates) != 0 {
		t.Fatalf("an active item must never be a candidate, got %+v", p.Candidates)
	}
	if p.TargetMet {
		t.Error("target cannot be met when only an active (unevictable) item is over quota")
	}
}

func TestPlan_UnmanagedItemNeverCandidate(t *testing.T) {
	items := []Item{{Name: "mystery", Status: StatusUnmanaged, SizeBytes: 5000}}
	p := Plan(cacheNamespace(), items, Quota{MaxBytes: 1000}, PlanParams{})
	if len(p.Candidates) != 0 {
		t.Fatalf("an unmanaged item must never be a candidate, got %+v", p.Candidates)
	}
	// Unmanaged items are also never counted toward CurrentUsageBytes -
	// this foundation's quota accounting is about what it owns.
	if p.CurrentUsageBytes != 0 {
		t.Errorf("expected unmanaged bytes excluded from CurrentUsageBytes, got %d", p.CurrentUsageBytes)
	}
}

func TestPlan_OldestFirstOrdering(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := []Item{
		{Name: "c", Status: StatusManaged, SizeBytes: 400, ModTime: t0.Add(2 * time.Hour)},
		{Name: "a", Status: StatusManaged, SizeBytes: 400, ModTime: t0},
		{Name: "b", Status: StatusManaged, SizeBytes: 400, ModTime: t0.Add(time.Hour)},
	}
	p := Plan(cacheNamespace(), items, Quota{MaxBytes: 500}, PlanParams{})
	if len(p.Candidates) < 2 {
		t.Fatalf("expected at least 2 candidates to reach quota, got %+v", p.Candidates)
	}
	if p.Candidates[0].Name != "a" || p.Candidates[1].Name != "b" {
		t.Errorf("expected oldest-first order [a b ...], got %+v", p.Candidates)
	}
}

func TestPlan_StableTieBreakOnEqualModTime(t *testing.T) {
	same := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := []Item{
		{Name: "z", Status: StatusManaged, SizeBytes: 100, ModTime: same},
		{Name: "a", Status: StatusManaged, SizeBytes: 100, ModTime: same},
	}
	p := Plan(cacheNamespace(), items, Quota{MaxBytes: 0}, PlanParams{}) // MaxBytes 0 = no-op, just checking determinism path is stable across repeated calls
	p2 := Plan(cacheNamespace(), items, Quota{MaxBytes: 1}, PlanParams{})
	if len(p2.Candidates) < 1 || p2.Candidates[0].Name != "a" {
		t.Errorf("expected the lexically-first name to break an equal-ModTime tie, got %+v", p2.Candidates)
	}
	_ = p
}

func TestPlan_MinRetentionSkipsRecentItemsWhenClockTrusted(t *testing.T) {
	ns := cacheNamespace()
	ns.MinRetention = 3600 // 1 hour
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	items := []Item{
		{Name: "old", Status: StatusManaged, SizeBytes: 100, ModTime: now.Add(-2 * time.Hour)},
		{Name: "new", Status: StatusManaged, SizeBytes: 100, ModTime: now.Add(-1 * time.Minute)},
	}
	p := Plan(ns, items, Quota{MaxBytes: 1}, PlanParams{NowWallClock: now})
	if len(p.Candidates) != 1 || p.Candidates[0].Name != "old" {
		t.Fatalf("expected only the old item to be a candidate, got %+v", p.Candidates)
	}
	foundSkip := false
	for _, s := range p.Skipped {
		if s.Name == "new" && s.Reason == SkipBelowMinRetention {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Errorf("expected the new item to be recorded as skipped for MinRetention, got %+v", p.Skipped)
	}
}

func TestPlan_MinRetentionConservativeWhenClockUntrusted(t *testing.T) {
	ns := cacheNamespace()
	ns.MinRetention = 3600
	items := []Item{
		{Name: "old", Status: StatusManaged, SizeBytes: 100, ModTime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	// NowWallClock is the zero value - clock untrusted, so even a very
	// old item must not be evicted via a MinRetention-gated namespace.
	p := Plan(ns, items, Quota{MaxBytes: 1}, PlanParams{})
	if len(p.Candidates) != 0 {
		t.Fatalf("expected zero candidates when the wall clock is untrusted, got %+v", p.Candidates)
	}
}

func TestPlan_TargetCannotBeMetReportsReason(t *testing.T) {
	items := []Item{{Name: "only", Status: StatusManaged, SizeBytes: 50}}
	p := Plan(cacheNamespace(), items, Quota{MaxBytes: 10}, PlanParams{})
	// Evicting the only eligible item still leaves ProjectedUsageAfterBytes
	// at 0 <= 10, so target IS technically met in this case - construct a
	// scenario where it truly cannot be met: an active item holds all the
	// usage.
	_ = p
	items2 := []Item{{Name: "active", Status: StatusActive, SizeBytes: 5000}}
	p2 := Plan(cacheNamespace(), items2, Quota{MaxBytes: 10}, PlanParams{})
	if p2.TargetMet {
		t.Fatal("expected TargetMet=false when only an active item accounts for the overage")
	}
	if p2.Reason == "" {
		t.Error("expected a non-empty Reason when the target cannot be met")
	}
}

func TestPlan_NoIntegerOverflowOnLargeSizes(t *testing.T) {
	items := []Item{{Name: "huge", Status: StatusManaged, SizeBytes: 1 << 62}}
	p := Plan(cacheNamespace(), items, Quota{MaxBytes: 1 << 61}, PlanParams{})
	if p.CurrentUsageBytes != 1<<62 {
		t.Errorf("expected exact large usage, got %d", p.CurrentUsageBytes)
	}
	if len(p.Candidates) != 1 {
		t.Fatalf("expected the huge item to be a candidate, got %+v", p.Candidates)
	}
}

func TestPlan_DeterministicForIdenticalInput(t *testing.T) {
	ns := cacheNamespace()
	items := []Item{
		{Name: "a", Status: StatusManaged, SizeBytes: 100, ModTime: time.Unix(1, 0)},
		{Name: "b", Status: StatusManaged, SizeBytes: 100, ModTime: time.Unix(2, 0)},
	}
	quota := Quota{MaxBytes: 50}
	params := PlanParams{NowMonotonic: 99}
	p1 := Plan(ns, items, quota, params)
	p2 := Plan(ns, items, quota, params)
	if len(p1.Candidates) != len(p2.Candidates) {
		t.Fatal("expected identical candidate counts across repeated calls")
	}
	for i := range p1.Candidates {
		if p1.Candidates[i] != p2.Candidates[i] {
			t.Errorf("expected identical candidate at index %d, got %+v vs %+v", i, p1.Candidates[i], p2.Candidates[i])
		}
	}
}

func TestPlan_ZeroValueSafety(t *testing.T) {
	// A completely zero-value call must never panic.
	_ = Plan(Namespace{}, nil, Quota{}, PlanParams{})
}
