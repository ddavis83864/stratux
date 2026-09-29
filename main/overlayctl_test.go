package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestOverlayCtlMu_SerializesConcurrentCriticalSections proves the fix
// for issue #49: overlayCtlMu (main/gen_gdl90.go), used by
// requestOverlayDisable (main/ota.go), (realWifiExecutor).Apply
// (main/wifiadminexecutor.go) and applyNetworkSettings
// (main/networksettings.go) to guard their own unlock..lock critical
// sections against /overlay/robase's shared rw/ro toggle, actually
// serializes concurrent callers.
//
// Without a shared mutex, two of those critical sections running at once
// can interleave as "A unlocks; B unlocks (a no-op, already unlocked);
// B finishes its own work and locks; A's own write now sees the overlay
// locked again" - exactly issue #49's observed symptom (an OTA
// overlay-disable-marker write failing with "read-only file system"
// immediately after its own unlock had just succeeded).
//
// This test does not invoke the real overlayctl binary, requestOverlayDisable,
// Apply, or applyNetworkSettings themselves, and touches no filesystem -
// none of that exists off-device, and main/ota.go's own StatMount call
// would simply fail first in this environment, never reaching the mutex
// at all. What it proves instead is the synchronization primitive
// itself: many goroutines contending for overlayCtlMu, each simulating
// one caller's full critical section (lock, do some work, unlock),
// never observe two "inside" at the same time. That guarantee is the
// entire mechanism the fix relies on; the three real call sites simply
// wrap their existing overlayctl(unlock)/work/overlayctl(lock) sequences
// in exactly this Lock/defer Unlock pattern (verified by code review, not
// by this test, since exercising the real shell-out safely requires the
// actual device).
func TestOverlayCtlMu_SerializesConcurrentCriticalSections(t *testing.T) {
	const goroutines = 16
	const iterations = 500

	var inCriticalSection int32 // 0 or 1; flipped inside the lock, on purpose
	var overlapDetected int32   // set (atomically) if two callers are ever both "inside"
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				func() {
					overlayCtlMu.Lock()
					defer overlayCtlMu.Unlock()
					// Simulates one caller's whole critical section: enter,
					// do some work (a real caller's unlock/writes/lock),
					// leave. CompareAndSwap catches another goroutine
					// having entered before this one left, which could
					// only happen if the mutex failed to exclude it.
					if !atomic.CompareAndSwapInt32(&inCriticalSection, 0, 1) {
						atomic.StoreInt32(&overlapDetected, 1)
					}
					time.Sleep(time.Microsecond) // give a real overlap every chance to happen
					atomic.StoreInt32(&inCriticalSection, 0)
				}()
			}
		}()
	}
	wg.Wait()

	if atomic.LoadInt32(&overlapDetected) != 0 {
		t.Fatal("overlayCtlMu did not serialize concurrent critical sections: two callers were inside at once - this is exactly the class of race behind issue #49")
	}
}
