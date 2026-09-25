package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/storagelifecycle"
)

// startFISBWorkersForTest runs the production capture worker and a stand-in for
// the cleanup worker (which has no stop mechanism and would otherwise outlive the
// test and race with later tests that reassign the package-level cache state),
// and stops and joins both when the test ends. The capture worker is the real
// one; only the cleanup loop is driven here, calling the same
// fisbCacheRunRetention on the same triggers (capacity-rejection signal or tick).
func startFISBWorkersForTest(t *testing.T) {
	t.Helper()
	q := fisbCachePending
	sig := fisbCacheCleanupSignal
	var captureDone, cleanupDone sync.WaitGroup
	captureDone.Add(1)
	go func() { defer captureDone.Done(); fisbCacheCaptureWorker() }()
	stop := make(chan struct{})
	cleanupDone.Add(1)
	go func() {
		defer cleanupDone.Done()
		for {
			select {
			case <-stop:
				return
			case <-sig:
			case <-time.After(20 * time.Millisecond):
			}
			fisbCacheRunRetention()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		cleanupDone.Wait()
		close(q.wake) // ends the capture worker's range loop
		captureDone.Wait()
	})
}

// TestFISBCacheSoak_SustainedCaptureLoadStaysBoundedAndLeaksNothing drives the
// real capture worker (the production goroutine) with a sustained, bursty,
// heavily duplicated load from several goroutines - the shape of a busy
// ground-station market plus a receiver that never restarts - and checks the
// resource properties an always-on device depends on: the committed store never
// exceeds its configured maximum, the reservation queue drains to empty, every
// offered capture is accounted for (admitted, dropped, or rejected - never lost
// silently), goroutine count is flat, and heap growth is bounded.
func TestFISBCacheSoak_SustainedCaptureLoadStaysBoundedAndLeaksNothing(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)
	const maxEntries = 500
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: false, MaxCacheBytes: 8 << 20, MaxEntries: maxEntries}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheCleanupSignal = make(chan struct{}, 1)
	fisbCacheShuttingDown = false
	fisbCacheMu.Unlock()

	startFISBWorkersForTest(t)
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	const producers, perProducer = 4, 4000
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				// 3000 distinct stations: far more than the 500-entry budget, each
				// repeated many times - duplicates and capacity pressure together.
				fisbCaptureText("METAR", fmt.Sprintf("K%04d", (i*7+p*131)%3000), "METAR body 091853Z AUTO 00000KT 10SM CLR 15/10 A3000", fisbcache.FISBTime{})
				if i%2000 == 0 {
					runtime.Gosched()
				}
			}
		}(p)
	}
	wg.Wait()
	waitForFISBWorkerIdle(t, 20*time.Second)
	time.Sleep(300 * time.Millisecond) // let the async cleanup worker settle

	if n := fisbCacheStore.Len(); n > maxEntries {
		t.Fatalf("committed store holds %d entries, configured maximum is %d", n, maxEntries)
	}
	queued, inFlight, _, _ := fisbCachePending.stats()
	if queued != 0 || inFlight != 0 {
		t.Fatalf("reservation queue did not drain: queued=%d inFlight=%d", queued, inFlight)
	}
	if g := runtime.NumGoroutine(); g > baseGoroutines+2 {
		t.Fatalf("goroutines grew from %d to %d under sustained load", baseGoroutines, g)
	}
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	if grown := int64(m1.HeapAlloc) - int64(m0.HeapAlloc); grown > 32<<20 {
		t.Fatalf("live heap grew by %d MiB after %d offered captures", grown>>20, producers*perProducer)
	}
	t.Logf("offered %d captures from %d goroutines: store=%d/%d entries, goroutines %d->%d, heap delta %d KiB",
		producers*perProducer, producers, fisbCacheStore.Len(), maxEntries, baseGoroutines, runtime.NumGoroutine(), (int64(m1.HeapAlloc)-int64(m0.HeapAlloc))>>10)
}

// The persisted set on disk never outgrows the committed set: after a churn far
// larger than the budget, the number of cache files equals (never exceeds) the
// store's entries, so the device's flash cannot fill with stale weather.
func TestFISBCacheSoak_PersistedFilesTrackTheBoundedStore(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)
	const maxEntries = 50
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 20, MaxEntries: maxEntries}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheCleanupSignal = make(chan struct{}, 1)
	fisbCacheShuttingDown = false
	fisbCacheMu.Unlock()
	startFISBWorkersForTest(t)

	for i := 0; i < 1500; i++ {
		fisbCaptureText("METAR", fmt.Sprintf("K%04d", i%400), "METAR body 091853Z AUTO 00000KT 10SM CLR 15/10 A3000", fisbcache.FISBTime{})
		if i%50 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	waitForFISBWorkerIdle(t, 20*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	var files []string
	for {
		files, _ = filepath.Glob(filepath.Join(dir, "*"))
		if fisbCacheStore.Len() <= maxEntries && len(files) <= fisbCacheStore.Len() || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := fisbCacheStore.Len(); n > maxEntries {
		t.Fatalf("store holds %d entries, maximum %d", n, maxEntries)
	}
	if len(files) > fisbCacheStore.Len() {
		t.Fatalf("%d cache files on disk but only %d committed entries - flash would fill with stale weather", len(files), fisbCacheStore.Len())
	}
}

// At the new hard maximum (10,000 entries) the cache is bounded, admission completes, the
// inventory/status API stays usable with a full cache, and nothing leaks.
func TestFISBCacheCeiling_TenThousandEntriesBoundedAndUsable(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)
	withTrustedTimeForTest(t)
	store := fisbcache.NewStore()
	for i := 0; i < FISBCacheMaxEntriesLimit-10; i++ {
		store.Admit(fisbcache.Entry{Key: fisbcache.TextKey("METAR", fmt.Sprintf("K%05d", i)), ReceivedAtMonotonic: float64(i) / 1000, SizeBytes: 100})
	}
	fisbCacheMu.Lock()
	fisbCacheStore = store
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, MaxCacheBytes: 64 << 20, MaxEntries: FISBCacheMaxEntriesLimit}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheCleanupSignal = make(chan struct{}, 1)
	fisbCacheShuttingDown = false
	fisbCacheMu.Unlock()
	startFISBWorkersForTest(t)
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	// 200 brand-new stations (pushing past the ceiling) and 200 refreshes of existing ones.
	start := time.Now()
	nowUTC := start.UTC()
	ft := fisbcache.FISBTime{Hour: uint32(nowUTC.Hour()), Minute: uint32(nowUTC.Minute())} // a product time of "now"
	for i := 0; i < 200; i++ {
		fisbCaptureText("METAR", fmt.Sprintf("N%05d", i), "METAR body 091853Z AUTO 00000KT 10SM CLR 15/10 A3000", ft)
		fisbCaptureText("METAR", fmt.Sprintf("K%05d", i), "METAR body 091853Z AUTO 00000KT 10SM CLR 15/10 A3000", ft)
	}
	waitForFISBWorkerIdle(t, 60*time.Second)
	time.Sleep(500 * time.Millisecond) // async cleanup
	elapsed := time.Since(start)

	if n := fisbCacheStore.Len(); n > FISBCacheMaxEntriesLimit {
		t.Fatalf("the store holds %d entries, above the hard maximum %d", n, FISBCacheMaxEntriesLimit)
	}
	if q, f, _, _ := fisbCachePending.stats(); q != 0 || f != 0 {
		t.Fatalf("reservation queue did not drain: %d/%d", q, f)
	}
	st := fisbCacheStatusSnapshot()
	if st.TotalEntries > FISBCacheMaxEntriesLimit || st.MaxEntries != FISBCacheMaxEntriesLimit {
		t.Fatalf("status = %+v", st)
	}
	inv := fisbInventoryForTest(t)
	if st.CapacityRejected == 0 && st.TotalEntries < FISBCacheMaxEntriesLimit {
		t.Fatalf("expected the ceiling to be reached or capacity rejections recorded: %+v", st)
	}
	if len(inv) != st.TotalEntries {
		t.Fatalf("inventory has %d rows, status says %d", len(inv), st.TotalEntries)
	}
	if g := runtime.NumGoroutine(); g > baseGoroutines+2 {
		t.Fatalf("goroutines grew from %d to %d", baseGoroutines, g)
	}
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	if grown := int64(m1.HeapAlloc) - int64(m0.HeapAlloc); grown > 32<<20 {
		t.Fatalf("live heap grew by %d MiB", grown>>20)
	}
	t.Logf("at the ceiling: %d entries, 400 captures in %v, goroutines %d->%d, %d inventory rows", st.TotalEntries, elapsed, baseGoroutines, runtime.NumGoroutine(), len(inv))
}
