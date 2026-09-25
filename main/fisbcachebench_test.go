package main

import (
	"fmt"
	"testing"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/storagelifecycle"
)

// BenchmarkFISBReserveAndEnqueue characterizes how the cost of ONE accepted
// capture on the live decode goroutine scales with the number of cached
// entries: reserveAndEnqueue copies the store snapshot (O(entries)) under the
// queue lock to compute the projected capacity. Run:
//
//	go test -run xxx -bench FISBReserveAndEnqueue -benchmem ./main/
//
// The numbers are for the machine that runs them; the target Raspberry Pi
// figure is a physical-acceptance measurement (PI_CACHE_COST_SCALING_PHYSICAL_MEASUREMENT_REQUIRED).
func BenchmarkFISBReserveAndEnqueue(b *testing.B) {
	for _, n := range []int{100, 500, 2000, 10000} {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			store := fisbcache.NewStore()
			for i := 0; i < n; i++ {
				store.Admit(fisbcache.Entry{Key: fisbcache.TextKey("METAR", fmt.Sprintf("K%05d", i)), ReceivedAtMonotonic: 1, SizeBytes: 100})
			}
			fisbCacheMu.Lock()
			origStore, origPending := fisbCacheStore, fisbCachePending
			fisbCacheStore = store
			fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
			q := fisbCachePending
			fisbCacheMu.Unlock()
			b.Cleanup(func() {
				fisbCacheMu.Lock()
				fisbCacheStore, fisbCachePending = origStore, origPending
				fisbCacheMu.Unlock()
			})
			settings := FISBCacheSettings{Enabled: true, MaxCacheBytes: 256 << 20, MaxEntries: FISBCacheMaxEntriesLimit}
			_ = storagelifecycle.PressureNormal
			keys := make([]fisbcache.Key, 64)
			for i := range keys {
				keys[i] = fisbcache.TextKey("METAR", fmt.Sprintf("K%05d", i))
			}
			item := newTestPendingItem("METAR body 091853Z AUTO 00000KT 10SM CLR 15/10 A3000")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				k := keys[i%len(keys)]
				if ok, _ := q.reserveAndEnqueue(k, item, settings); !ok {
					b.Fatal("reservation refused")
				}
				pk, _, _ := q.pop()
				q.releaseInFlight(pk)
			}
		})
	}
}
