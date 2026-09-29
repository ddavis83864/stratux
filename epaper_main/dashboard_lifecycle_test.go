package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stratux/stratux/epaper"
)

// lifecycleDriver is a fake panel that records the whole service lifecycle.
type lifecycleDriver struct {
	mu      sync.Mutex
	events  []string
	updates []recordedUpdate
	slept   bool
	initN   int
}

func (d *lifecycleDriver) Init(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initN++
	d.events = append(d.events, "init")
	return nil
}
func (d *lifecycleDriver) Clear(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, "clear")
	return nil
}
func (d *lifecycleDriver) Sleep() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.slept = true
	d.events = append(d.events, "sleep")
	return nil
}
func (d *lifecycleDriver) Update(_ context.Context, bmp []byte, full bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.updates = append(d.updates, recordedUpdate{full, append([]byte(nil), bmp...)})
	d.events = append(d.events, map[bool]string{true: "full", false: "partial"}[full])
	return nil
}
func (d *lifecycleDriver) snapshot() (ev []string, up []recordedUpdate) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.events...), append([]recordedUpdate(nil), d.updates...)
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The whole service against a fake panel and a fake daemon: it must draw the
// dashboard as its first frame, re-draw a fresh full frame when the panel is
// re-initialised by a configuration change, fall back to the text pages when
// the page setting says so, and shut down promptly with the shutdown screen.
func TestRunLifecycleWithFakePanelAndDaemon(t *testing.T) {
	var settings atomic.Value // map[string]interface{}
	interval := 30
	set := func(page string, rotation int) {
		settings.Store(map[string]interface{}{
			"EpaperEnabled": true, "EpaperPanel": "waveshare-4.2in-v2", "EpaperRotation": rotation,
			"EpaperRefreshIntervalSeconds": interval, "EpaperFullRefreshEvery": 20, "EpaperPage": page,
		})
	}
	set("dashboard", 0)

	daemon := piServer(t, map[string]func(http.ResponseWriter, *http.Request){})
	// wrap: serve /getSettings from the mutable map, everything else from
	// the real payloads
	mux := http.NewServeMux()
	mux.HandleFunc("/getSettings", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(settings.Load())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(daemon.URL + r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 1<<16)
		n, _ := resp.Body.Read(buf)
		w.Write(buf[:n])
	})
	front := httptest.NewServer(mux)
	defer front.Close()

	drv := &lifecycleDriver{}
	origOpen, origClose, origNew, origHealth := openBusFn, closeBusFn, newDriverFn, writeHealthFn
	defer func() { openBusFn, closeBusFn, newDriverFn, writeHealthFn = origOpen, origClose, origNew, origHealth }()
	var healthMu sync.Mutex
	var healths []epaper.Health
	writeHealthFn = func(h epaper.Health) { healthMu.Lock(); healths = append(healths, h); healthMu.Unlock() }
	var buses atomic.Int32
	openBusFn = func(epaper.GPIOMapping) (*gpioBus, error) { buses.Add(1); return &gpioBus{}, nil }
	closeBusFn = func() { buses.Add(-1) }
	newDriverFn = func(string, Bus, int, int) PanelDriver { return drv }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, NewStatusSource(front.URL, time.Second), NewStatusSource(front.URL, time.Second),
			NewDashSource(front.URL, time.Second), 20*time.Millisecond, 20*time.Millisecond)
	}()

	// 1. Initialised, cleared, then the first dashboard frame (full) - no
	// text startup frame in between.
	waitFor(t, "first dashboard frame", func() bool { _, up := drv.snapshot(); return len(up) >= 1 })
	ev, up := drv.snapshot()
	if len(ev) < 3 || ev[0] != "init" || ev[1] != "clear" || ev[2] != "full" {
		t.Fatalf("startup events = %v", ev)
	}
	if !up[0].full {
		t.Error("first frame must be a full refresh")
	}
	// It is a real dashboard (the daemon payloads are the reference-like state).
	first := up[0].bmp

	// 2. Rotation change: the panel is re-initialised and must be given a
	// fresh full frame, turned 180 degrees.
	set("dashboard", 180)
	waitFor(t, "re-initialisation", func() bool { _, up := drv.snapshot(); return len(up) >= 2 })
	ev, up = drv.snapshot()
	if drv.initN < 2 {
		t.Fatalf("panel was not re-initialised: %v", ev)
	}
	if !up[len(up)-1].full {
		t.Error("frame after re-initialisation must be full")
	}
	if string(up[len(up)-1].bmp) == string(first) {
		t.Error("rotation 180 frame identical to rotation 0 frame")
	}

	// 2b. A change that only re-initialises the panel (refresh interval):
	// the panel was cleared, so the dashboard must again start with a full
	// frame even though its page, rotation and state are unchanged.
	nBefore := len(up)
	interval = 45
	set("dashboard", 180)
	waitFor(t, "re-initialisation without a page change", func() bool { _, u := drv.snapshot(); return len(u) > nBefore })
	_, up = drv.snapshot()
	if drv.initN < 3 || !up[nBefore].full {
		t.Errorf("after an interval-only change: initN=%d, next frame full=%v", drv.initN, up[nBefore].full)
	}

	// 3. Page change to the text overview: the dashboard runner is stopped
	// and the legacy pages take over.
	nText := len(up)
	set("overview", 0)
	waitFor(t, "text page after the switch", func() bool { _, u := drv.snapshot(); return len(u) >= nText+2 })

	healthMu.Lock()
	sawRunning := false
	for _, h := range healths {
		if h.State == epaper.StateRunning && h.ConfiguredPanel == "waveshare-4.2in-v2" && h.FullRefreshCount >= 1 {
			sawRunning = true
		}
	}
	healthMu.Unlock()
	if !sawRunning {
		t.Error("service never reported RUNNING with a refresh count")
	}

	// 4. Shutdown (issue #43): the shutdown() closure itself no longer
	// draws anything - the panel is bistable, so whatever it was last
	// showing simply stays, and the closure only sleeps the panel and
	// releases the bus (its whole body is: stop the dashboard runner if
	// any, Sleep, release - nothing in between could draw). The single,
	// final retained shutdown splash (with its own "Safe to remove
	// power" message) is drawn separately, by
	// stratux_epaper_shutdown.service, not by this process - see
	// splashshutdown.go. "sleep" being the absolute last event, with
	// nothing after it, is exactly what proves this: a draw from
	// shutdown() itself could only appear before Sleep(), which is
	// already the last call in its body. (A legitimate, unrelated race
	// can still let one last in-flight periodic tick's own update land
	// just before cancel() is even observed - that update is not
	// asserted against here, since it predates shutdown() running at
	// all.)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return promptly after cancel")
	}
	ev, up = drv.snapshot()
	if ev[len(ev)-1] != "sleep" || !drv.slept {
		t.Errorf("panel not put to sleep: %v", ev)
	}
	if n := buses.Load(); n != 0 {
		t.Errorf("bus leak: %d still open", n)
	}
}
