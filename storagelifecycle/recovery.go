package storagelifecycle

import (
	"fmt"
	"path/filepath"
	"sort"
)

// RecoveryAction is what PlanRecovery decided to do with one temp file it
// found - see PlanRecovery's doc comment for the full decision policy.
type RecoveryAction string

const (
	// RecoveryNone: not this package's concern at all (not an owned temp
	// file name) - never appears in a RecoveryPlan; PlanRecovery simply
	// does not create an item for it. Documented here only so the
	// RecoveryAction type lists every value PlanRecovery can produce.
	RecoveryNone RecoveryAction = "none"
	// RecoveryRemoveTemp: remove exactly this owned temp file - its
	// content is superseded, unvalidated, or otherwise not safe/allowed
	// to promote. Never touches anything but the temp file's own path.
	RecoveryRemoveTemp RecoveryAction = "remove_temp"
	// RecoveryPromoteTemp: rename this owned temp file onto its final
	// name - only ever proposed when a destination is absent, promotion
	// is explicitly allowed for the namespace, and the temp file passed
	// the caller-supplied validator.
	RecoveryPromoteTemp RecoveryAction = "promote_temp"
	// RecoverySkipInUse: an injected InUseChecker reported this temp name
	// as belonging to a write currently in progress in this process -
	// left completely alone.
	RecoverySkipInUse RecoveryAction = "skip_in_use"
)

// RecoveryItem is one owned temp file PlanRecovery found and its proposed
// disposition.
type RecoveryItem struct {
	Namespace string
	TempName  string
	TempPath  string
	FinalName string
	Action    RecoveryAction
	// Reason is sanitized (no file contents) and safe to log or surface
	// in diagnostics.
	Reason string
}

// RecoveryPlan is PlanRecovery's pure output for one namespace. Like
// RetentionPlan, producing a RecoveryPlan never mutates anything - see
// ExecuteRecovery for the separate, explicit step that acts on one.
type RecoveryPlan struct {
	Namespace            string
	GeneratedAtMonotonic float64
	Items                []RecoveryItem
}

// TempValidator validates one temp file's content before PlanRecovery
// will ever propose promoting it. Required whenever RecoveryOptions.
// AllowPromotion is true; if nil, PlanRecovery treats every otherwise-
// promotable candidate as unsafe to promote and proposes removing it
// instead - this package never promotes an unvalidated file, regardless
// of AllowPromotion.
type TempValidator func(tempPath string) error

// InUseChecker lets a caller report that a specific temp filename belongs
// to a write this same process currently has in flight, so PlanRecovery
// never proposes any action against it - see RecoverySkipInUse.
type InUseChecker func(tempName string) bool

// RecoveryOptions configures one PlanRecovery call.
type RecoveryOptions struct {
	NowMonotonic float64
	// AllowPromotion is a per-call decision, not a package default - a
	// caller recovering a namespace where an interrupted write's content
	// is never safe to reconstruct blind (most namespaces) should leave
	// this false, which makes PlanRecovery only ever propose removing
	// owned temp files, never promoting one.
	AllowPromotion bool
	Validate       TempValidator
	InUse          InUseChecker
}

// PlanRecovery inspects one namespace's raw directory entries (from
// FS.ReadDir - not a Scanner Inventory, since recovery must see every
// entry including ones a normal scan would already classify as
// unmanaged) for this package's own owned temp files (see
// ownedTempFinalName) and proposes what to do with each. Any entry whose
// name is not recognized as an owned temp file is completely ignored -
// PlanRecovery has no opinion about, and never touches, anything it does
// not provably own.
//
// Decision policy per owned temp file found:
//
//   - If InUse reports it as belonging to an in-flight write: RecoverySkipInUse.
//   - Else if a regular-file, non-symlink destination already exists at
//     FinalName: RecoveryRemoveTemp ("a valid destination already
//     exists") - a committed write always wins over an abandoned one.
//   - Else if more than one owned temp file targets the same FinalName:
//     only the lexically-first TempName may ever be considered for
//     promotion; every other one is RecoveryRemoveTemp
//     ("duplicate owned temp candidate for this final name").
//   - Else if !AllowPromotion, or Validate is nil, or Validate(tempPath)
//     returns an error: RecoveryRemoveTemp, with a reason identifying
//     which of those applied.
//   - Else: RecoveryPromoteTemp.
//
// PlanRecovery performs the validation calls (real file I/O, since
// TempValidator inspects real content) but no destructive filesystem
// operation - see ExecuteRecovery.
func PlanRecovery(ns Namespace, entries []DirEntry, opts RecoveryOptions) RecoveryPlan {
	plan := RecoveryPlan{Namespace: ns.ID, GeneratedAtMonotonic: opts.NowMonotonic}

	// destinationExists/tempsByFinalName: two passes over entries, since
	// deciding one temp file's fate requires knowing about every other
	// entry (its final name's destination, and sibling temp candidates)
	// first.
	destinationExists := make(map[string]bool)
	for _, e := range entries {
		if e.IsRegular && !e.IsSymlink {
			destinationExists[e.Name] = true
		}
	}

	tempsByFinal := make(map[string][]DirEntry)
	for _, e := range entries {
		if finalName, ok := ownedTempFinalName(e.Name); ok {
			tempsByFinal[finalName] = append(tempsByFinal[finalName], e)
		}
	}

	// Deterministic order: sort finalNames.
	finalNames := make([]string, 0, len(tempsByFinal))
	for fn := range tempsByFinal {
		finalNames = append(finalNames, fn)
	}
	sort.Strings(finalNames)

	for _, finalName := range finalNames {
		candidates := tempsByFinal[finalName]
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })

		path, err := SafeJoin(ns.Root, candidates[0].Name)
		_ = err // candidates[0].Name came from a real ReadDir result on ns.Root; SafeJoin cannot fail for it, but never assume

		if destinationExists[finalName] {
			for _, c := range candidates {
				plan.Items = append(plan.Items, removeItem(ns, c, finalName, "a valid destination already exists for this final name"))
			}
			continue
		}

		// candidates[0] is the only one ever eligible for promotion;
		// every other duplicate is always removed regardless of its own
		// individual validity.
		for i, c := range candidates {
			if i > 0 {
				plan.Items = append(plan.Items, removeItem(ns, c, finalName, "duplicate owned temp candidate for this final name - only one may ever be promoted"))
				continue
			}
			plan.Items = append(plan.Items, decideSoleCandidate(ns, c, finalName, path, opts))
		}
	}

	return plan
}

func decideSoleCandidate(ns Namespace, c DirEntry, finalName, tempPath string, opts RecoveryOptions) RecoveryItem {
	if opts.InUse != nil && opts.InUse(c.Name) {
		return RecoveryItem{Namespace: ns.ID, TempName: c.Name, TempPath: tempPath, FinalName: finalName, Action: RecoverySkipInUse, Reason: "belongs to a write currently in progress in this process"}
	}
	if c.IsSymlink {
		return removeItem(ns, c, finalName, "owned temp name matched but the entry is a symlink - never promoted")
	}
	if !c.IsRegular {
		return removeItem(ns, c, finalName, "owned temp name matched but the entry is not a regular file - never promoted")
	}
	if !opts.AllowPromotion {
		return removeItem(ns, c, finalName, "promotion is not permitted for this namespace")
	}
	if opts.Validate == nil {
		return removeItem(ns, c, finalName, "no validator configured - an unvalidated temp file is never promoted")
	}
	if err := opts.Validate(tempPath); err != nil {
		return removeItem(ns, c, finalName, "owned temp file failed validation: "+sanitizeError(err))
	}
	return RecoveryItem{Namespace: ns.ID, TempName: c.Name, TempPath: tempPath, FinalName: finalName, Action: RecoveryPromoteTemp, Reason: "owned temp file validated successfully - promoting to complete an interrupted write"}
}

func removeItem(ns Namespace, c DirEntry, finalName, reason string) RecoveryItem {
	path, _ := SafeJoin(ns.Root, c.Name)
	return RecoveryItem{Namespace: ns.ID, TempName: c.Name, TempPath: path, FinalName: finalName, Action: RecoveryRemoveTemp, Reason: reason}
}

// sanitizeError renders err's message only - never any file content a
// caller's Validate might have read on its way to failing.
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// RecoveryResult is ExecuteRecovery's per-item outcome.
type RecoveryResult struct {
	Item  RecoveryItem
	Error error
}

// ExecuteRecovery performs the filesystem operations RecoveryPlan
// describes: RecoveryRemoveTemp calls FS.Remove on exactly the temp
// path, RecoveryPromoteTemp calls FS.Rename from the temp path to the
// item's final path, and RecoverySkipInUse/RecoveryNone do nothing.
//
// One item's failure never stops the rest of the batch (matching this
// package's failure-isolation convention - a daemon startup calling this
// must never fail to start because one recovery action could not
// complete) - every item's outcome is reported in the returned slice, in
// the same order as plan.Items, so a caller can log every failure
// without any of them being silently dropped.
func ExecuteRecovery(fs FS, plan RecoveryPlan) []RecoveryResult {
	results := make([]RecoveryResult, 0, len(plan.Items))
	for _, item := range plan.Items {
		var err error
		switch item.Action {
		case RecoveryRemoveTemp:
			err = fs.Remove(item.TempPath)
		case RecoveryPromoteTemp:
			finalPath, joinErr := SafeJoin(filepath.Dir(item.TempPath), item.FinalName)
			if joinErr != nil {
				err = fmt.Errorf("storagelifecycle: %w", joinErr)
			} else {
				err = fs.Rename(item.TempPath, finalPath)
			}
		case RecoverySkipInUse, RecoveryNone:
			// no-op
		}
		results = append(results, RecoveryResult{Item: item, Error: err})
	}
	return results
}
