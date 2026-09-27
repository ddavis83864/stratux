package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stratux/stratux/epaper"
)

// updateRecorder is a fake panel: it records every Update and can be made
// to fail.
type updateRecorder struct {
	updates []recordedUpdate
	fail    error
}

type recordedUpdate struct {
	full bool
	bmp  []byte
}

func (u *updateRecorder) Init(context.Context) error  { return nil }
func (u *updateRecorder) Clear(context.Context) error { return nil }
func (u *updateRecorder) Sleep() error                { return nil }
func (u *updateRecorder) Update(_ context.Context, bmp []byte, full bool) error {
	if u.fail != nil {
		return u.fail
	}
	u.updates = append(u.updates, recordedUpdate{full, append([]byte(nil), bmp...)})
	return nil
}
func (u *updateRecorder) fulls() (n int) {
	for _, x := range u.updates {
		if x.full {
			n++
		}
	}
	return
}

// scriptedSnap is a dashSnapshot backed by a real Tracker on a synthetic
// clock.
type scriptedSnap struct {
	tr *epaper.Tracker
}

func (s *scriptedSnap) Dashboard(now, wall time.Time) epaper.Dashboard { return s.tr.Derive(now, wall) }
func (s *scriptedSnap) dataAgeSeconds(now time.Time) float64 {
	if a, ok := s.tr.StatusAge(now); ok {
		return a.Seconds()
	}
	return -1
}

var dashCfg = epaper.Config{Enabled: true, Panel: epaper.PanelWaveshare42V2, Page: epaper.PageDashboard,
	RefreshIntervalSeconds: 30, FullRefreshEvery: 20}

type harness struct {
	t      *testing.T
	drv    *updateRecorder
	snap   *scriptedSnap
	pol    epaper.DashPolicyState
	health epaper.Health
	now    time.Time
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, drv: &updateRecorder{}, now: fx0}
	h.snap = &scriptedSnap{tr: epaper.NewTracker(epaper.DefaultThresholds(), fx0)}
	return h
}

// poll feeds a healthy reading at h.now, shaped by mutate.
func (h *harness) poll(mutate func(*epaper.Sample)) {
	st := baseStatus()
	st.UptimeMs += int64(h.now.Sub(fx0) / time.Millisecond)
	s := epaper.Sample{At: h.now, Status: &st, Health: &epaper.HealthData{TimeState: "GNSS_SYNCED"},
		Towers: &epaper.TowerData{}, Clients: &epaper.ClientData{Responding: 1}, Power: &epaper.PowerData{}}
	if mutate != nil {
		mutate(&s)
	}
	h.snap.tr.Observe(s)
}

// tick runs one refresh cycle at h.now.
func (h *harness) tick() {
	h.health = refreshDashboard(context.Background(), h.drv, h.snap, dashCfg, &h.pol, h.health, h.now)
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func TestRefreshFirstFrameIsFullAndMatchesTheRender(t *testing.T) {
	h := newHarness(t)
	h.poll(nil)
	h.tick()
	if len(h.drv.updates) != 1 || !h.drv.updates[0].full {
		t.Fatalf("updates = %+v", h.drv.updates)
	}
	want := RenderDashboard(h.snap.Dashboard(h.now, h.now), 0)
	if !bytes.Equal(h.drv.updates[0].bmp, want) {
		t.Error("the frame written is not the render of the derived dashboard")
	}
	if h.health.State != epaper.StateRunning || h.health.FullRefreshCount != 1 || h.health.LastErrorCat != "" {
		t.Errorf("health = %+v", h.health)
	}
}

func TestRefreshDoesNotFireForPollsOrTheClockAlone(t *testing.T) {
	h := newHarness(t)
	h.poll(nil)
	h.tick()
	// Nine minutes of 5 s polls with nothing changing (not even the minute
	// in the footer may trigger a refresh).
	for i := 0; i < 108; i++ {
		h.advance(5 * time.Second)
		h.poll(nil)
		h.tick()
	}
	if len(h.drv.updates) != 1 {
		t.Fatalf("%d updates for an unchanging screen", len(h.drv.updates))
	}
}

func TestRefreshHeartbeatProvesLife(t *testing.T) {
	h := newHarness(t)
	h.poll(nil)
	h.tick()
	for h.now.Sub(fx0) < epaper.DashboardHeartbeat+10*time.Second {
		h.advance(5 * time.Second)
		h.poll(nil)
		h.tick()
	}
	if len(h.drv.updates) != 2 || h.drv.updates[1].full {
		t.Fatalf("updates = %d, want the initial full + one partial heartbeat", len(h.drv.updates))
	}
	// The heartbeat frame carries the newer footer time.
	if bytes.Equal(h.drv.updates[0].bmp, h.drv.updates[1].bmp) {
		t.Error("heartbeat frame identical to the first: the footer time did not advance")
	}
}

func TestRefreshCoalescesFlappingIntoOneUpdate(t *testing.T) {
	h := newHarness(t)
	h.poll(nil)
	h.tick()
	h.advance(40 * time.Second) // past the interval so only coalescing is in play
	// The GPS fix flaps every 2 s for 10 s, ending lost; ticks happen on
	// every flap but the minimum interval since the last refresh is met
	// only once, at the end.
	h.poll(func(s *epaper.Sample) { s.Status.GPSSolution, s.Status.GPSSatsLocked = "No Fix", 0 })
	h.tick()
	before := len(h.drv.updates)
	if before != 2 {
		t.Fatalf("first change after the interval should refresh once, got %d updates", before)
	}
	for i := 0; i < 5; i++ { // flap inside the next interval
		h.advance(2 * time.Second)
		lost := i%2 == 0
		h.poll(func(s *epaper.Sample) {
			if lost {
				s.Status.GPSSolution, s.Status.GPSSatsLocked = "No Fix", 0
			}
		})
		h.tick()
	}
	if len(h.drv.updates) != before {
		t.Fatalf("flapping inside the interval refreshed %d extra times", len(h.drv.updates)-before)
	}
	// When the interval is up, exactly one refresh shows the state as it is
	// then (the fix is back): none of the intermediate flaps was drawn.
	h.advance(30 * time.Second)
	h.poll(nil)
	h.tick()
	if len(h.drv.updates) != before+1 {
		t.Fatalf("updates = %d, want exactly one after the interval", len(h.drv.updates))
	}
}

func TestRefreshShowsNoDataAndRecovers(t *testing.T) {
	h := newHarness(t)
	h.poll(nil)
	h.tick()
	// The status source dies: no more polls succeed.
	for i := 0; i < 8; i++ {
		h.advance(5 * time.Second)
		h.tick()
	}
	if got := len(h.drv.updates); got != 2 {
		t.Fatalf("%d updates, want the initial frame plus one NO DATA frame", got)
	}
	last := h.drv.updates[1]
	if !last.full {
		t.Error("the NO DATA banner is a large polarity change and must be a full refresh")
	}
	// The NO DATA frame was drawn when the source first went stale (25 s
	// after the last reading), with the footer as of that moment.
	drawnAt := fx0.Add(25 * time.Second)
	nd := h.snap.Dashboard(drawnAt, drawnAt)
	if nd.Overall != epaper.OverallNoData || !bytes.Equal(last.bmp, RenderDashboard(nd, 0)) {
		t.Errorf("last frame is not the NO DATA screen (overall %v)", nd.Overall)
	}
	if h.health.LastErrorCat != epaper.ErrorStatusSourceUp || h.health.State != epaper.StateRunning {
		t.Errorf("health = %+v", h.health)
	}
	if h.health.DataAgeSeconds < 30 {
		t.Errorf("data age = %v", h.health.DataAgeSeconds)
	}
	// Recovery: the live screen returns and the error clears.
	h.advance(40 * time.Second)
	h.poll(nil)
	h.tick()
	if len(h.drv.updates) != 3 || !h.drv.updates[2].full {
		t.Fatalf("recovery updates = %d (last full=%v)", len(h.drv.updates), h.drv.updates[len(h.drv.updates)-1].full)
	}
	if h.health.LastErrorCat != "" {
		t.Errorf("error not cleared: %+v", h.health)
	}
	if !bytes.Equal(h.drv.updates[2].bmp, RenderDashboard(h.snap.Dashboard(h.now, h.now), 0)) {
		t.Error("recovered frame is not the live screen")
	}
}

func TestRefreshFailureIsReportedAndRetriedOncePerInterval(t *testing.T) {
	h := newHarness(t)
	h.poll(nil)
	h.drv.fail = errBusyTimeout
	h.tick()
	if h.health.State != epaper.StateError || h.health.LastErrorCat != epaper.ErrorBusyTimeout || h.health.BusyTimeoutCount != 1 {
		t.Fatalf("health after failed update = %+v", h.health)
	}
	// Not retried every poll: only after the interval.
	for i := 0; i < 2; i++ {
		h.advance(5 * time.Second)
		h.poll(nil)
		h.tick()
	}
	if h.health.BusyTimeoutCount != 1 {
		t.Errorf("retried inside the interval (busy count %d)", h.health.BusyTimeoutCount)
	}
	h.advance(25 * time.Second)
	h.poll(nil)
	h.drv.fail = errors.New("spi write")
	h.tick()
	if h.health.LastErrorCat != epaper.ErrorSPIWrite || h.health.ConsecutiveFailures < 2 {
		t.Errorf("second failure = %+v", h.health)
	}
	// The panel comes back: the owed first frame is written, in full.
	h.advance(40 * time.Second)
	h.poll(nil)
	h.drv.fail = nil
	h.tick()
	if len(h.drv.updates) != 1 || !h.drv.updates[0].full || h.health.State != epaper.StateRunning {
		t.Errorf("recovery: updates=%d health=%+v", len(h.drv.updates), h.health)
	}
}

func TestRefreshClientCountChangeIsMaterial(t *testing.T) {
	h := newHarness(t)
	h.poll(nil)
	h.tick()
	h.advance(40 * time.Second)
	h.poll(func(s *epaper.Sample) { s.Clients = &epaper.ClientData{Responding: 2} })
	h.tick()
	if len(h.drv.updates) != 2 {
		t.Fatalf("client connect did not refresh: %d", len(h.drv.updates))
	}
	h.advance(40 * time.Second)
	h.poll(func(s *epaper.Sample) { s.Clients = &epaper.ClientData{Responding: 0} })
	h.tick()
	if len(h.drv.updates) != 3 {
		t.Fatalf("client disconnect did not refresh: %d", len(h.drv.updates))
	}
}

func TestRefreshPanicIsContained(t *testing.T) {
	h := newHarness(t)
	h.poll(nil)
	var nilSnap dashSnapshot
	res := refreshDashboard(context.Background(), h.drv, nilSnap, dashCfg, &h.pol, h.health, h.now)
	if res.State != epaper.StateError {
		t.Errorf("a panic must become an ErrorCategory, got %+v", res)
	}
}

func TestDashboardSelectedRules(t *testing.T) {
	c := dashCfg
	if !dashboardSelected(c) {
		t.Error("4.2in V2, dashboard page, rotation 0 must select the dashboard")
	}
	c.Rotation = 180
	if !dashboardSelected(c) {
		t.Error("rotation 180 must be supported")
	}
	for _, mut := range []func(*epaper.Config){
		func(c *epaper.Config) { c.Rotation = 90 },
		func(c *epaper.Config) { c.Rotation = 270 },
		func(c *epaper.Config) { c.Panel = epaper.PanelWaveshare37 },
		func(c *epaper.Config) { c.Page = epaper.PageOverview },
	} {
		c := dashCfg
		mut(&c)
		if dashboardSelected(c) {
			t.Errorf("config %+v must fall back to the text pages", c)
		}
	}
}
