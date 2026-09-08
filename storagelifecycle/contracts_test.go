package storagelifecycle

import (
	"context"
	"io"
	"testing"
)

func TestEvaluateRecordingSpace_CriticalDenies(t *testing.T) {
	r := EvaluateRecordingSpace(PressureCritical, 0, Quota{}, RecordingSpaceRequest{})
	if r.Decision != RecordingSpaceDenied {
		t.Errorf("expected Denied under critical pressure, got %s", r.Decision)
	}
}

func TestEvaluateRecordingSpace_UnknownDenies(t *testing.T) {
	r := EvaluateRecordingSpace(PressureUnknown, 0, Quota{}, RecordingSpaceRequest{})
	if r.Decision != RecordingSpaceDenied {
		t.Errorf("expected Denied when pressure is unknown, got %s", r.Decision)
	}
}

func TestEvaluateRecordingSpace_HighAndElevatedCaution(t *testing.T) {
	for _, p := range []PressureState{PressureHigh, PressureElevated} {
		r := EvaluateRecordingSpace(p, 0, Quota{}, RecordingSpaceRequest{})
		if r.Decision != RecordingSpaceCaution {
			t.Errorf("expected Caution for %s pressure, got %s", p, r.Decision)
		}
	}
}

func TestEvaluateRecordingSpace_NormalAllows(t *testing.T) {
	r := EvaluateRecordingSpace(PressureNormal, 0, Quota{}, RecordingSpaceRequest{})
	if r.Decision != RecordingSpaceAllowed {
		t.Errorf("expected Allowed under normal pressure, got %s", r.Decision)
	}
}

func TestEvaluateRecordingSpace_ExpectedSizeExceedingQuotaDenies(t *testing.T) {
	r := EvaluateRecordingSpace(PressureNormal, 900, Quota{MaxBytes: 1000}, RecordingSpaceRequest{ExpectedBytes: 200})
	if r.Decision != RecordingSpaceDenied {
		t.Errorf("expected Denied when the expected size would exceed quota, got %s", r.Decision)
	}
}

func TestEvaluateRecordingSpace_UnestimatedRequestNeverDeniedOnSizeAlone(t *testing.T) {
	r := EvaluateRecordingSpace(PressureNormal, 900, Quota{MaxBytes: 1000}, RecordingSpaceRequest{ExpectedBytes: 0})
	if r.Decision != RecordingSpaceAllowed {
		t.Errorf("expected Allowed when no size estimate is given and pressure is normal, got %s", r.Decision)
	}
}

func TestIsCacheEntryExpired(t *testing.T) {
	meta := CacheEntryMetadata{ExpiresAtMonotonic: 100}
	if IsCacheEntryExpired(meta, 99) {
		t.Error("expected not yet expired at t=99")
	}
	if !IsCacheEntryExpired(meta, 100) {
		t.Error("expected expired at exactly the expiry time")
	}
	if !IsCacheEntryExpired(meta, 101) {
		t.Error("expected expired after the expiry time")
	}
}

func TestIsCacheEntryExpired_UnsetNeverExpires(t *testing.T) {
	meta := CacheEntryMetadata{}
	if IsCacheEntryExpired(meta, 1e9) {
		t.Error("an entry with no declared expiry must never report expired via this function alone")
	}
}

// fakeRecordingLifecycle is a minimal, test-only implementation of
// RecordingLifecycle, exercising only that the contract's shape is
// implementable and that NoAutomaticDeletion is honestly true - no
// production code implements this interface in this mission.
type fakeRecordingLifecycle struct {
	active    map[string]bool
	completed []CompletionReport
}

func (f *fakeRecordingLifecycle) ReserveSpace(req RecordingSpaceRequest) RecordingSpaceResult {
	return EvaluateRecordingSpace(PressureNormal, 0, Quota{}, req)
}
func (f *fakeRecordingLifecycle) RegisterActive(name string) {
	if f.active == nil {
		f.active = make(map[string]bool)
	}
	f.active[name] = true
}
func (f *fakeRecordingLifecycle) Complete(report CompletionReport) {
	delete(f.active, report.Name)
	f.completed = append(f.completed, report)
}
func (f *fakeRecordingLifecycle) NoAutomaticDeletion() bool { return true }

func TestRecordingLifecycleContract_Shape(t *testing.T) {
	var lc RecordingLifecycle = &fakeRecordingLifecycle{}
	lc.RegisterActive("rec-1")
	if !lc.(*fakeRecordingLifecycle).active["rec-1"] {
		t.Fatal("expected RegisterActive to mark the recording active")
	}
	lc.Complete(CompletionReport{Name: "rec-1", FinalSizeBytes: 100})
	if lc.(*fakeRecordingLifecycle).active["rec-1"] {
		t.Fatal("expected Complete to clear the active flag")
	}
	if !lc.NoAutomaticDeletion() {
		t.Fatal("a conforming implementation must report NoAutomaticDeletion() == true")
	}
}

// fakeCacheLifecycle is a minimal, test-only implementation of
// CacheLifecycle - no production code implements this interface in this
// mission.
type fakeCacheLifecycle struct {
	writer *AtomicWriter
	ns     Namespace
}

func (f *fakeCacheLifecycle) ReplaceProduct(meta CacheEntryMetadata, write WriteFunc) error {
	_, err := f.writer.Write(context.Background(), AtomicWriteOptions{
		Namespace: f.ns,
		Name:      meta.Key.ProductType + "-" + meta.Key.ProductID + ".bin",
		Mode:      0o644,
		Write:     write,
	})
	return err
}
func (f *fakeCacheLifecycle) IsExpired(meta CacheEntryMetadata, nowMonotonic float64) bool {
	return IsCacheEntryExpired(meta, nowMonotonic)
}
func (f *fakeCacheLifecycle) ReclaimPriority() Criticality { return CriticalityCache }

func TestCacheLifecycleContract_ReplaceProductUsesAtomicWrite(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	ns := Namespace{ID: "cache", Root: "/data/cache", Criticality: CriticalityCache, ItemKind: ItemKindFile}
	lc := &fakeCacheLifecycle{writer: &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}, ns: ns}

	meta := CacheEntryMetadata{Key: CacheProductKey{ProductType: "METAR", ProductID: "KXYZ"}}
	err := lc.ReplaceProduct(meta, func(w io.Writer) error {
		_, e := w.Write([]byte("synthetic-fixture-content"))
		return e
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(fs.content("/data/cache/METAR-KXYZ.bin")) != "synthetic-fixture-content" {
		t.Errorf("unexpected content: %s", fs.content("/data/cache/METAR-KXYZ.bin"))
	}
	if lc.ReclaimPriority() != CriticalityCache {
		t.Error("expected ReclaimPriority to always report CriticalityCache")
	}
}
