/*
	network_test.go: Regression coverage for the netMutex/clientConnections/
	networkGDL90Chan startup-order invariant (see the comment on their
	declarations in network.go).

	Before the fix, netMutex was a *sync.Mutex left nil until initNetwork()
	assigned it. healthUpdateLoop() (main/health.go) is started as a
	goroutine before initNetwork() runs in main(), and its very first tick
	calls lastNetworkClientActivityMono(), which locked that nil mutex -
	confirmed live on hardware as an intermittent boot crash
	(panic: runtime error: invalid memory address or nil pointer
	dereference, in sync.(*Mutex).Lock via main.lastNetworkClientActivityMono
	via main.updateHealth via main.healthUpdateLoop).

	These tests deliberately never call initNetwork() - the whole point of
	the fix is that clientConnections/networkGDL90Chan/netMutex are valid
	for the complete process lifetime from their package-level declaration,
	independent of whether or when initNetwork() runs. Against the pre-fix
	code, TestLastNetworkClientActivityMono_BeforeNetworkInit_NoPanic and
	TestUpdateHealth_BeforeNetworkInit_NoPanic below would nil-pointer-panic.

	initNetwork() itself is deliberately not invoked here: it binds real
	TCP/UDP sockets and starts unbounded background goroutines
	(monitorDHCPLeases, tcpNMEAInListener, initBluetooth, ...) that would
	outlive the test and risk port conflicts under `go test ./main/...` -
	that is an integration-test concern orthogonal to the initialization-
	order invariant covered here.
*/

package main

import (
	"sync"
	"testing"
	"time"
)

// ensureADSBTowerMutexForTest mirrors ensureSituationLocks() (see
// fancontrolstatus_test.go) for the one other main()-only-initialized lock
// updateHealth() depends on. Unlike netMutex, ADSBTowerMutex is genuinely
// safe in production - main() assigns it on the same goroutine several
// lines before it ever spawns healthUpdateLoop() - so it is nil here only
// because this test process never runs main(), not because of the bug
// under test.
func ensureADSBTowerMutexForTest() {
	if ADSBTowerMutex == nil {
		ADSBTowerMutex = &sync.Mutex{}
	}
}

// ensureStratuxClockForTest mirrors ensureSituationLocks() for stratuxClock.
// Like ADSBTowerMutex, stratuxClock is genuinely safe in production - it is
// the very first thing main() assigns, long before healthUpdateLoop() is
// started - so it is nil here only because this test process never runs
// main(). Only starts the underlying ticker once per test binary.
func ensureStratuxClockForTest() {
	if stratuxClock == nil {
		stratuxClock = NewMonotonic()
	}
}

// resetClientConnectionsForTest empties clientConnections and returns a
// restore function, so tests can inject fake clients without leaking state
// into other tests in this package.
func resetClientConnectionsForTest(t *testing.T) {
	t.Helper()
	orig := clientConnections
	clientConnections = make(map[string]connection)
	t.Cleanup(func() {
		netMutex.Lock()
		clientConnections = orig
		netMutex.Unlock()
	})
}

// TestLastNetworkClientActivityMono_BeforeNetworkInit_NoPanic is the direct
// regression test for the captured panic: call the exact function from the
// exact goroutine (a fresh goroutine, standing in for healthUpdateLoop's)
// without ever having called initNetwork() in this process.
func TestLastNetworkClientActivityMono_BeforeNetworkInit_NoPanic(t *testing.T) {
	done := make(chan time.Time, 1)
	go func() { done <- lastNetworkClientActivityMono() }()
	select {
	case got := <-done:
		if !got.IsZero() {
			t.Errorf("lastNetworkClientActivityMono() = %v, want zero time with no tracked clients", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lastNetworkClientActivityMono() did not return - deadlock?")
	}
}

// TestLastNetworkClientActivityMono_ReflectsLatestClientActivity confirms
// the fix didn't change the function's actual behavior - it still returns
// the most recent LastPingResponse/LastPongResponse across tracked clients.
func TestLastNetworkClientActivityMono_ReflectsLatestClientActivity(t *testing.T) {
	resetClientConnectionsForTest(t)

	now := time.Now()
	netMutex.Lock()
	clientConnections["a"] = &networkConnection{LastPingResponse: now.Add(-10 * time.Second)}
	clientConnections["b"] = &networkConnection{LastPongResponse: now.Add(-2 * time.Second)}
	clientConnections["c"] = &networkConnection{} // never responded - must not win
	netMutex.Unlock()

	got := lastNetworkClientActivityMono()
	want := now.Add(-2 * time.Second)
	if !got.Equal(want) {
		t.Errorf("lastNetworkClientActivityMono() = %v, want %v (client b's LastPongResponse)", got, want)
	}
}

// TestLastNetworkClientActivityMono_ConcurrentReadsAndWrites exercises the
// protected state from many goroutines at once - run with -race to confirm
// no data race, and it must return within the timeout to confirm no
// deadlock.
func TestLastNetworkClientActivityMono_ConcurrentReadsAndWrites(t *testing.T) {
	resetClientConnectionsForTest(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := "client-" + string(rune('A'+n))
			for {
				select {
				case <-stop:
					return
				default:
				}
				netMutex.Lock()
				clientConnections[key] = &networkConnection{LastPingResponse: time.Now()}
				netMutex.Unlock()
			}
		}(i)
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = lastNetworkClientActivityMono()
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent readers/writers did not finish - deadlock?")
	}
}

// TestUpdateHealth_BeforeNetworkInit_NoPanic is the integration-level
// regression test: updateHealth() is exactly what healthUpdateLoop() calls
// on its first, immediate tick (see health.go), and it is what actually
// panicked on real hardware. This confirms the full call chain
// (updateHealth -> lastNetworkClientActivityMono -> netMutex.Lock) survives
// running before initNetwork() with an honest "no client activity" result.
func TestUpdateHealth_BeforeNetworkInit_NoPanic(t *testing.T) {
	ensureSituationLocks()
	ensureADSBTowerMutexForTest()
	ensureStratuxClockForTest()
	withTestProfilesStore(t)
	resetClientConnectionsForTest(t)

	updateHealth()

	globalHealthMutex.Lock()
	report := globalHealth
	globalHealthMutex.Unlock()

	if report.GDL90.LastNetworkClientActivity.Valid {
		t.Errorf("LastNetworkClientActivity.Valid = true with no tracked clients, want false: %+v",
			report.GDL90.LastNetworkClientActivity)
	}
}
