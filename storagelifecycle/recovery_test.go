package storagelifecycle

import (
	"errors"
	"testing"
	"time"
)

func recoveryNamespace() Namespace {
	return Namespace{ID: "cache", Root: "/data/cache", Criticality: CriticalityCache, ItemKind: ItemKindFile}
}

func TestPlanRecovery_NoTempFiles(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/a.json", []byte("x"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{})
	if len(plan.Items) != 0 {
		t.Fatalf("expected no recovery items, got %+v", plan.Items)
	}
}

func TestPlanRecovery_TempExistsDestinationAbsent_NoPromotion(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/.slctmp-a.json.abc", []byte("x"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{AllowPromotion: false})
	if len(plan.Items) != 1 || plan.Items[0].Action != RecoveryRemoveTemp {
		t.Fatalf("expected RecoveryRemoveTemp when promotion is not allowed, got %+v", plan.Items)
	}
}

func TestPlanRecovery_TempExistsDestinationAbsent_PromotedWhenValid(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/.slctmp-a.json.abc", []byte("valid"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{
		AllowPromotion: true,
		Validate:       func(tempPath string) error { return nil },
	})
	if len(plan.Items) != 1 || plan.Items[0].Action != RecoveryPromoteTemp {
		t.Fatalf("expected RecoveryPromoteTemp, got %+v", plan.Items)
	}
}

func TestPlanRecovery_ValidDestinationAlreadyExists_TempRemoved(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/a.json", []byte("committed"), 0o644, time.Now())
	fs.putFile("/data/cache/.slctmp-a.json.abc", []byte("stale"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{
		AllowPromotion: true,
		Validate:       func(tempPath string) error { return nil }, // even if it WOULD validate, destination wins
	})
	if len(plan.Items) != 1 || plan.Items[0].Action != RecoveryRemoveTemp {
		t.Fatalf("expected RecoveryRemoveTemp when a valid destination already exists, got %+v", plan.Items)
	}
}

func TestPlanRecovery_MultipleOwnedTempCandidates_OnlyOnePromotable(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/.slctmp-a.json.aaa", []byte("x"), 0o644, time.Now())
	fs.putFile("/data/cache/.slctmp-a.json.bbb", []byte("y"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{
		AllowPromotion: true,
		Validate:       func(tempPath string) error { return nil },
	})
	if len(plan.Items) != 2 {
		t.Fatalf("expected 2 items, got %+v", plan.Items)
	}
	promoted := 0
	for _, it := range plan.Items {
		if it.Action == RecoveryPromoteTemp {
			promoted++
		}
	}
	if promoted != 1 {
		t.Errorf("expected exactly 1 promotable candidate among duplicates, got %d", promoted)
	}
	// Deterministic: the lexically-first temp name is the one promoted.
	if plan.Items[0].TempName != ".slctmp-a.json.aaa" || plan.Items[0].Action != RecoveryPromoteTemp {
		t.Errorf("expected the lexically-first candidate to be the promotable one, got %+v", plan.Items[0])
	}
}

func TestPlanRecovery_TruncatedOrCorruptTempFailsValidation(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/.slctmp-a.json.abc", []byte("{not json"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{
		AllowPromotion: true,
		Validate:       func(tempPath string) error { return errors.New("invalid JSON") },
	})
	if len(plan.Items) != 1 || plan.Items[0].Action != RecoveryRemoveTemp {
		t.Fatalf("expected RecoveryRemoveTemp for a failed validation, got %+v", plan.Items)
	}
}

func TestPlanRecovery_InUseTempSkipped(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/.slctmp-a.json.abc", []byte("x"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{
		AllowPromotion: true,
		Validate:       func(tempPath string) error { return nil },
		InUse:          func(tempName string) bool { return true },
	})
	if len(plan.Items) != 1 || plan.Items[0].Action != RecoverySkipInUse {
		t.Fatalf("expected RecoverySkipInUse, got %+v", plan.Items)
	}
}

func TestPlanRecovery_UnknownTempNeverTouched(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/somebody-elses.tmp", []byte("x"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{AllowPromotion: true, Validate: func(string) error { return nil }})
	if len(plan.Items) != 0 {
		t.Fatalf("expected zero items - an unrecognized temp file must never be touched, got %+v", plan.Items)
	}
}

func TestPlanRecovery_SymlinkTempNeverPromoted(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putSymlink("/data/cache/.slctmp-a.json.abc")
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{AllowPromotion: true, Validate: func(string) error { return nil }})
	if len(plan.Items) != 1 || plan.Items[0].Action != RecoveryRemoveTemp {
		t.Fatalf("expected RecoveryRemoveTemp for a symlinked temp name, got %+v", plan.Items)
	}
}

func TestPlanRecovery_NonRegularTempNeverPromoted(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.mkdir("/data/cache/.slctmp-a.json.abc") // a directory, not a file
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{AllowPromotion: true, Validate: func(string) error { return nil }})
	if len(plan.Items) != 1 || plan.Items[0].Action != RecoveryRemoveTemp {
		t.Fatalf("expected RecoveryRemoveTemp for a non-regular temp entry, got %+v", plan.Items)
	}
}

func TestPlanRecovery_RepeatedRecoveryIsIdempotent(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/.slctmp-a.json.abc", []byte("x"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	opts := RecoveryOptions{AllowPromotion: false}
	plan1 := PlanRecovery(recoveryNamespace(), entries, opts)
	ExecuteRecovery(fs, plan1)
	entries2, _ := fs.ReadDir("/data/cache")
	plan2 := PlanRecovery(recoveryNamespace(), entries2, opts)
	if len(plan2.Items) != 0 {
		t.Fatalf("expected a second recovery pass to find nothing left to do, got %+v", plan2.Items)
	}
}

func TestExecuteRecovery_PromoteRenamesToFinalName(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/.slctmp-a.json.abc", []byte("recovered"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{AllowPromotion: true, Validate: func(string) error { return nil }})
	results := ExecuteRecovery(fs, plan)
	if len(results) != 1 || results[0].Error != nil {
		t.Fatalf("unexpected results: %+v", results)
	}
	if !fs.exists("/data/cache/a.json") {
		t.Error("expected the temp file promoted to its final name")
	}
	if fs.exists("/data/cache/.slctmp-a.json.abc") {
		t.Error("expected the temp name to no longer exist after promotion")
	}
	if string(fs.content("/data/cache/a.json")) != "recovered" {
		t.Errorf("unexpected content: %s", fs.content("/data/cache/a.json"))
	}
}

func TestExecuteRecovery_RemoveDeletesOnlyTheTempFile(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/a.json", []byte("real"), 0o644, time.Now())
	fs.putFile("/data/cache/.slctmp-a.json.abc", []byte("stale"), 0o644, time.Now())
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{})
	results := ExecuteRecovery(fs, plan)
	if len(results) != 1 || results[0].Error != nil {
		t.Fatalf("unexpected results: %+v", results)
	}
	if fs.exists("/data/cache/.slctmp-a.json.abc") {
		t.Error("expected the temp file removed")
	}
	if !fs.exists("/data/cache/a.json") || string(fs.content("/data/cache/a.json")) != "real" {
		t.Error("the real destination file must be completely untouched")
	}
}

func TestExecuteRecovery_OneFailureDoesNotStopTheBatch(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/.slctmp-a.json.abc", []byte("x"), 0o644, time.Now())
	fs.putFile("/data/cache/.slctmp-b.json.abc", []byte("y"), 0o644, time.Now())
	fs.failRemove["/data/cache/.slctmp-a.json.abc"] = errors.New("permission denied")
	entries, _ := fs.ReadDir("/data/cache")
	plan := PlanRecovery(recoveryNamespace(), entries, RecoveryOptions{})
	results := ExecuteRecovery(fs, plan)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	var sawError, sawSuccess bool
	for _, r := range results {
		if r.Error != nil {
			sawError = true
		} else {
			sawSuccess = true
		}
	}
	if !sawError || !sawSuccess {
		t.Errorf("expected one failure and one success reported independently, got %+v", results)
	}
	if fs.exists("/data/cache/.slctmp-b.json.abc") {
		t.Error("the second item's removal should have succeeded despite the first item's failure")
	}
}
