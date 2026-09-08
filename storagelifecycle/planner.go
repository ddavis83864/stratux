package storagelifecycle

import (
	"sort"
	"time"
)

// SkipReason explains why an item was not proposed for eviction.
type SkipReason string

const (
	SkipActive               SkipReason = "active"
	SkipProtectedCriticality SkipReason = "namespace criticality does not permit automatic eviction"
	SkipUnmanaged            SkipReason = "unmanaged - not an owned item"
	SkipBelowMinRetention    SkipReason = "below the namespace's minimum retention"
	SkipTargetAlreadyMet     SkipReason = "target already met before considering this item"
)

// SkippedItem records one item the planner considered and did not
// propose for eviction, and why - included in every RetentionPlan so a
// caller (or a diagnostic bundle) can see the planner's full reasoning,
// not just its conclusions.
type SkippedItem struct {
	Namespace string
	Name      string
	Reason    SkipReason
}

// EvictionCandidate is one item the planner proposes evicting. Proposing
// is not executing - see manager.go for the explicit separation this
// mission requires, and note that a RetentionPlan by itself never deletes
// anything.
type EvictionCandidate struct {
	Namespace string
	Name      string
	Path      string
	SizeBytes int64
	// Rationale is a short, sanitized (no file contents, no coordinates)
	// human-readable reason this item was chosen and in what order -
	// safe to surface in diagnostics or a dashboard.
	Rationale string
}

// RetentionPlan is Plan's pure output for one namespace. It never mutates
// anything - see manager.go's Execute for the separate, explicit step
// that would ever act on a plan (unused by any production code path in
// this mission - see that file's doc comment).
type RetentionPlan struct {
	Namespace                string
	GeneratedAtMonotonic     float64
	CurrentUsageBytes        int64
	TargetBytes              int64 // 0 means "no quota configured"
	Candidates               []EvictionCandidate
	Skipped                  []SkippedItem
	ProjectedUsageAfterBytes int64
	// TargetMet is true if CurrentUsageBytes was already <= TargetBytes,
	// or if executing every proposed Candidate would bring usage to or
	// below TargetBytes. False means the namespace has no safe way to
	// reach its quota under current policy - see Reason.
	TargetMet bool
	Reason    string
}

// PlanParams are the caller-supplied context Plan needs beyond the raw
// Items - kept as explicit parameters (never read from a global or a
// live clock inside Plan itself) so Plan stays a pure function: identical
// arguments always produce an identical RetentionPlan.
type PlanParams struct {
	NowMonotonic float64
	// NowWallClock gates MinRetention: the zero value means "wall clock
	// not currently trusted," which conservatively excludes every item
	// from MinRetention-gated eligibility (see Plan's doc comment) rather
	// than guessing. A caller with trusted time should pass the real
	// current wall-clock time.
	NowWallClock time.Time
}

// Plan produces a deterministic RetentionPlan for one namespace: which of
// its Items to propose evicting (if any) to bring usage to at or below
// quota.MaxBytes, given only ns's own criticality/MinRetention and the
// namespace's own Items - it has no knowledge of any other namespace and
// makes no filesystem calls of its own (items must already be a scanned
// Inventory's namespace slice).
//
// Determinism: Plan reads no global or ambient state - only ns, items,
// quota, and params - so identical arguments always produce an identical
// plan, satisfying this package's mission requirement that planning
// never depend on filesystem enumeration order (items should already be
// sorted by the Scanner, but Plan re-sorts its own eligible subset
// defensively rather than trusting that) or on wall-clock timing beyond
// the single NowWallClock value the caller explicitly supplies.
//
// Eligibility, in order:
//  1. ns.Criticality must permit eviction at all (see Evictable) -
//     otherwise every Managed item is skipped with SkipProtectedCriticality
//     and TargetMet is false (this foundation never silently "succeeds"
//     by refusing to act - the caller can see exactly why nothing
//     happened).
//  2. Item.Status must be StatusManaged - StatusActive is always skipped
//     (SkipActive) and StatusUnmanaged is always skipped (SkipUnmanaged),
//     regardless of quota pressure.
//  3. If ns.MinRetention > 0 and params.NowWallClock is non-zero, an item
//     younger than MinRetention is skipped (SkipBelowMinRetention). If
//     NowWallClock is the zero value, every item with MinRetention > 0
//     configured is conservatively skipped the same way, rather than
//     assuming an untrusted clock means "old enough."
//
// Ordering among eligible candidates: oldest ModTime first (ties broken
// by Name, ascending, for full determinism) - see docs/storage-
// lifecycle.md's ordering section for the honest limitation this implies
// (relative order among already-existing items reflects whatever the
// filesystem recorded; Plan itself never calls a clock or reorders based
// on the current wall time, which is the concrete guarantee its own
// determinism rests on).
func Plan(ns Namespace, items []Item, quota Quota, params PlanParams) RetentionPlan {
	plan := RetentionPlan{
		Namespace:            ns.ID,
		GeneratedAtMonotonic: params.NowMonotonic,
		TargetBytes:          quota.MaxBytes,
	}

	var current int64
	for _, it := range items {
		if it.Status == StatusManaged || it.Status == StatusActive {
			current += it.SizeBytes
		}
	}
	plan.CurrentUsageBytes = current
	plan.ProjectedUsageAfterBytes = current

	if quota.MaxBytes <= 0 {
		plan.TargetMet = true
		plan.Reason = "no quota configured for this namespace"
		return plan
	}
	if current <= quota.MaxBytes {
		plan.TargetMet = true
		plan.Reason = "already within quota"
		return plan
	}

	if !Evictable(ns.Criticality) {
		plan.TargetMet = false
		plan.Reason = "namespace over quota, but its criticality does not permit automatic eviction"
		for _, it := range items {
			if it.Status == StatusManaged {
				plan.Skipped = append(plan.Skipped, SkippedItem{Namespace: ns.ID, Name: it.Name, Reason: SkipProtectedCriticality})
			}
		}
		return plan
	}

	var eligible []Item
	for _, it := range items {
		switch {
		case it.Status == StatusActive:
			plan.Skipped = append(plan.Skipped, SkippedItem{Namespace: ns.ID, Name: it.Name, Reason: SkipActive})
		case it.Status == StatusUnmanaged:
			plan.Skipped = append(plan.Skipped, SkippedItem{Namespace: ns.ID, Name: it.Name, Reason: SkipUnmanaged})
		case it.Status == StatusManaged:
			if ns.MinRetention > 0 {
				if params.NowWallClock.IsZero() || params.NowWallClock.Sub(it.ModTime).Seconds() < ns.MinRetention {
					plan.Skipped = append(plan.Skipped, SkippedItem{Namespace: ns.ID, Name: it.Name, Reason: SkipBelowMinRetention})
					continue
				}
			}
			eligible = append(eligible, it)
		}
	}

	sort.Slice(eligible, func(i, j int) bool {
		if !eligible[i].ModTime.Equal(eligible[j].ModTime) {
			return eligible[i].ModTime.Before(eligible[j].ModTime)
		}
		return eligible[i].Name < eligible[j].Name
	})

	remaining := current
	target := quota.MaxBytes
	for _, it := range eligible {
		if remaining <= target {
			plan.Skipped = append(plan.Skipped, SkippedItem{Namespace: ns.ID, Name: it.Name, Reason: SkipTargetAlreadyMet})
			continue
		}
		plan.Candidates = append(plan.Candidates, EvictionCandidate{
			Namespace: ns.ID,
			Name:      it.Name,
			Path:      it.Path,
			SizeBytes: it.SizeBytes,
			Rationale: "oldest eligible item in a namespace over its configured quota",
		})
		remaining -= it.SizeBytes
	}
	plan.ProjectedUsageAfterBytes = remaining

	if remaining <= target {
		plan.TargetMet = true
		plan.Reason = "quota met by proposed eviction of eligible items"
	} else {
		plan.TargetMet = false
		plan.Reason = "insufficient eligible data to safely reach quota under current policy"
	}
	return plan
}
