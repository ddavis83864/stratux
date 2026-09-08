package storagelifecycle

import (
	"sync"
	"testing"
	"time"

	"github.com/stratux/stratux/readiness"
)

func testManagerRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	must(t, r.Register(Namespace{ID: "diagnostics", Root: "/data/diagnostics", Criticality: CriticalityBounded, ItemKind: ItemKindFile, AllowedExtensions: []string{".json"}}))
	must(t, r.Register(Namespace{ID: "cache", Root: "/data/cache", Criticality: CriticalityCache, ItemKind: ItemKindFile}))
	return r
}

func newTestManager(t *testing.T, fs *fakeFS, clock *fakeClock) *Manager {
	t.Helper()
	return NewManager(ManagerConfig{
		Registry:             testManagerRegistry(t),
		Policy:               Policy{Quotas: map[string]Quota{"cache": {MaxBytes: 1000}}, RequiredConsecutivePressureSamples: 1},
		FS:                   fs,
		Clock:                clock.Now,
		FilesystemThresholds: readiness.DefaultPersistentStorageThresholds(),
	})
}

func TestManager_StatusBeforeAnyScan(t *testing.T) {
	fs := newFakeFS()
	m := newTestManager(t, fs, &fakeClock{})
	st := m.Status()
	if st.HasInventory {
		t.Error("expected HasInventory=false before any scan")
	}
	if st.Pressure != PressureUnknown {
		t.Errorf("expected PressureUnknown before any scan, got %s", st.Pressure)
	}
}

func TestManager_ScanThenStatusReportsUsage(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/a.json", make([]byte, 500), 0o644, time.Now())
	clock := &fakeClock{}
	m := newTestManager(t, fs, clock)
	m.Scan()
	st := m.Status()
	if !st.HasInventory {
		t.Fatal("expected HasInventory=true after Scan")
	}
	if st.Namespaces["cache"].ManagedBytes != 500 {
		t.Errorf("expected 500 managed bytes, got %+v", st.Namespaces["cache"])
	}
}

func TestManager_StaleInventoryDetected(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/cache")
	clock := &fakeClock{}
	m := NewManager(ManagerConfig{
		Registry:             testManagerRegistry(t),
		Policy:               Policy{StaleAfterSeconds: 60, RequiredConsecutivePressureSamples: 1},
		FS:                   fs,
		Clock:                clock.Now,
		FilesystemThresholds: readiness.DefaultPersistentStorageThresholds(),
	})
	m.Scan()
	clock.Advance(120)
	st := m.Status()
	if !st.Stale {
		t.Error("expected Stale=true after exceeding StaleAfterSeconds")
	}
	if st.Pressure != PressureUnknown {
		t.Errorf("expected PressureUnknown while stale, got %s", st.Pressure)
	}
}

func TestManager_NamespaceQuotaPressureReflected(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/a.json", make([]byte, 1200), 0o644, time.Now()) // over the 1000-byte quota
	m := newTestManager(t, fs, &fakeClock{})
	m.Scan()
	st := m.Status()
	if st.Pressure != PressureCritical {
		t.Errorf("expected CRITICAL pressure from the over-quota namespace, got %s", st.Pressure)
	}
}

func TestManager_FilesystemPressureCombinesWithNamespace(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/cache")
	m := NewManager(ManagerConfig{
		Registry:             testManagerRegistry(t),
		Policy:               Policy{RequiredConsecutivePressureSamples: 1},
		FS:                   fs,
		Clock:                (&fakeClock{}).Now,
		FilesystemPressure:   func() (float64, bool) { return 96, true }, // above RecordingProhibitedPercent (95)
		FilesystemThresholds: readiness.DefaultPersistentStorageThresholds(),
	})
	m.Scan()
	st := m.Status()
	if st.Pressure != PressureCritical {
		t.Errorf("expected CRITICAL from filesystem pressure, got %s", st.Pressure)
	}
}

func TestManager_ScanErrorReportedInStatus(t *testing.T) {
	fs := newFakeFS() // namespaces never created -> Lstat errors
	m := newTestManager(t, fs, &fakeClock{})
	m.Scan()
	st := m.Status()
	if len(st.ScanErrors) == 0 {
		t.Error("expected scan errors to be reported")
	}
	if st.Pressure != PressureUnknown {
		t.Errorf("expected PressureUnknown when scan errors exist, got %s", st.Pressure)
	}
}

func TestManager_UnmanagedTotalsAggregated(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.putFile("/data/diagnostics/notes.txt", []byte("hi"), 0o644, time.Now())
	fs.mkdir("/data/cache")
	m := newTestManager(t, fs, &fakeClock{})
	m.Scan()
	st := m.Status()
	if st.UnmanagedTotalCount != 1 {
		t.Errorf("expected 1 unmanaged item aggregated across namespaces, got %d", st.UnmanagedTotalCount)
	}
}

func TestManager_ProtectedTotalExcludesCache(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.putFile("/data/diagnostics/d.json", make([]byte, 300), 0o644, time.Now())
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/c.json", make([]byte, 400), 0o644, time.Now())
	m := newTestManager(t, fs, &fakeClock{})
	m.Scan()
	st := m.Status()
	if st.ProtectedTotalBytes != 300 {
		t.Errorf("expected only the bounded (diagnostics) namespace counted as protected, got %d", st.ProtectedTotalBytes)
	}
}

func TestManager_ConcurrentScansCoalesceWithoutDataRace(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/cache")
	m := newTestManager(t, fs, &fakeClock{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Scan()
			m.Status()
		}()
	}
	wg.Wait()
	if !m.Status().HasInventory {
		t.Error("expected at least one scan to have completed")
	}
}

func TestManager_PlanUnknownNamespaceErrors(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/cache")
	m := newTestManager(t, fs, &fakeClock{})
	m.Scan()
	if _, err := m.Plan("nonexistent", PlanParams{}); err == nil {
		t.Fatal("expected an error for an unregistered namespace")
	}
}

func TestManager_PlanBeforeScanErrors(t *testing.T) {
	fs := newFakeFS()
	m := newTestManager(t, fs, &fakeClock{})
	if _, err := m.Plan("cache", PlanParams{}); err == nil {
		t.Fatal("expected an error when no scan has ever completed")
	}
}

func TestManager_PlanUsesLastScan(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/diagnostics")
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/a.json", make([]byte, 1500), 0o644, time.Now())
	m := newTestManager(t, fs, &fakeClock{})
	m.Scan()
	plan, err := m.Plan("cache", PlanParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.TargetMet == false && len(plan.Candidates) == 0 {
		t.Errorf("expected a plan to have candidates when over quota, got %+v", plan)
	}
}
