package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stratux/stratux/epaper"
)

func payload(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "pi-payloads", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// piServer serves the real payloads captured from the bench Pi; override
// replaces or removes (nil body + code) individual endpoints.
func piServer(t *testing.T, override map[string]func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, ep := range []string{"getStatus", "getHealth", "getTowers", "getClients", "getPowerHealth"} {
		ep := ep
		body := payload(t, ep+".json")
		mux.HandleFunc("/"+ep, func(w http.ResponseWriter, r *http.Request) {
			if f, ok := override[ep]; ok {
				f(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		})
	}
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func poll(t *testing.T, s *httptest.Server, timeout time.Duration) epaper.Sample {
	t.Helper()
	return NewDashSource(s.URL, timeout).Poll(context.Background(), time.Now)
}

func TestDashSourceParsesRealPiPayloads(t *testing.T) {
	smp := poll(t, piServer(t, nil), 2*time.Second)
	if smp.Status == nil || smp.Health == nil || smp.Towers == nil || smp.Clients == nil || smp.Power == nil {
		t.Fatalf("a member is missing: %+v", smp)
	}
	st := smp.Status
	if st.Version != "2.0.0~rc2" || st.Build[:7] != "b26686c" || st.UptimeMs != 7964060 {
		t.Errorf("identity/uptime = %+v", st)
	}
	if !st.GPSConnected || st.GPSSolution != "3D GPS" || st.GPSSatsLocked != 14 || st.GPSSatsSeen != 17 {
		t.Errorf("gps = %+v", st)
	}
	if !st.UAT.Enabled || !st.UAT.ExternalConnected || st.UAT.Total != 0 || st.UAT.LastMinute != 0 {
		t.Errorf("978 = %+v (the Pi was in the 'connected, no messages' state)", st.UAT)
	}
	if !st.ES.Enabled || !st.ES.Detected || !st.ES.Assigned || !st.ES.DecoderRunning || st.ES.Total != 13387 {
		t.Errorf("1090 = %+v", st.ES)
	}
	if smp.Health.TimeState != "GNSS_SYNCED" || smp.Health.CPUTempC < 40 || smp.Health.CPUTempC > 70 || len(smp.Health.FailedServices) != 0 {
		t.Errorf("health = %+v", smp.Health)
	}
	if smp.Towers.Active != 0 || smp.Towers.Known != 0 {
		t.Errorf("towers = %+v", smp.Towers)
	}
	// Every client in the capture was asleep, and two BLE entries have no
	// address at all: nobody is connected right now, whatever the daemon's
	// 15-minute "recent clients" counter (12 here) says.
	if len(smp.Clients.AwakeIPs) != 0 {
		t.Errorf("clients awake = %v, want none", smp.Clients.AwakeIPs)
	}
	if smp.Power.UndervoltageNow || smp.Power.ThrottledNow {
		t.Errorf("power = %+v", smp.Power)
	}

	// The whole thing, fed through the real tracker, is the reference
	// dashboard state (except the footer count, which is truthfully zero).
	tr := epaper.NewTracker(epaper.DefaultThresholds(), smp.At)
	tr.Observe(smp)
	d := tr.Derive(smp.At, smp.At)
	if d.Overall != epaper.OverallOnline || d.GPS.Headline != "3D FIX" || d.GPS.Detail != "14 SAT" ||
		d.ES.Headline != "QUIET" && d.ES.Headline != "ACTIVE" ||
		d.UAT.Headline != "CONNECTED" || d.UAT.Detail != "NO MESSAGES YET" ||
		d.FISB.Headline != "NO UPLINK" || d.Clients != "NO CLIENTS CONNECTED" {
		t.Errorf("real payloads -> %+v", d)
	}
}

func TestDashSourceCountsUniqueRespondingClients(t *testing.T) {
	var clients map[string]map[string]interface{}
	if err := json.Unmarshal(payload(t, "getClients.json"), &clients); err != nil {
		t.Fatal(err)
	}
	// Wake one phone (three UDP ports = one client) and one more device on a single port.
	for k, v := range clients {
		if v["Ip"] == "192.168.10.101" {
			v["SleepFlag"] = false
			clients[k] = v
		}
		if k == "192.168.10.102:4000" {
			v["SleepFlag"] = false
		}
	}
	body, _ := json.Marshal(clients)
	s := piServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"getClients": func(w http.ResponseWriter, r *http.Request) { w.Write(body) },
	})
	if ips := poll(t, s, 2*time.Second).Clients.AwakeIPs; len(ips) != 2 || ips[0] != "192.168.10.101" || ips[1] != "192.168.10.102" {
		t.Errorf("awake = %v, want the 2 unique addresses", ips)
	}
}

func TestDashSourceFailedEndpointsAreNilNotGuessed(t *testing.T) {
	fail := func(code int, body string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code); w.Write([]byte(body)) }
	}
	s := piServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"getStatus":      fail(500, "boom"),
		"getHealth":      fail(200, "not json"),
		"getTowers":      fail(404, ""),
		"getClients":     fail(200, `[1,2,3]`), // wrong shape
		"getPowerHealth": func(w http.ResponseWriter, r *http.Request) { w.Write(payload(t, "getPowerHealth.json")) },
	})
	smp := poll(t, s, 2*time.Second)
	if smp.Status != nil || smp.Health != nil || smp.Towers != nil || smp.Clients != nil {
		t.Errorf("failed reads must be nil: %+v", smp)
	}
	if smp.Power == nil {
		t.Error("one bad endpoint must not discard the good ones")
	}
}

func TestDashSourceRejectsAnEmptyOrForeignStatus(t *testing.T) {
	for _, body := range []string{`{}`, `{"hello":"world"}`, `null`} {
		body := body
		s := piServer(t, map[string]func(http.ResponseWriter, *http.Request){
			"getStatus": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) },
		})
		if smp := poll(t, s, 2*time.Second); smp.Status != nil {
			t.Errorf("status body %q accepted as a real status", body)
		}
	}
}

func TestDashSourcePollIsBoundedWhenTheDaemonHangs(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	hang := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}
	over := map[string]func(http.ResponseWriter, *http.Request){}
	for _, ep := range []string{"getStatus", "getHealth", "getTowers", "getClients", "getPowerHealth"} {
		over[ep] = hang
	}
	s := piServer(t, over)
	start := time.Now()
	smp := poll(t, s, 300*time.Millisecond)
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("poll took %v against a hung daemon", el)
	}
	if smp.Status != nil {
		t.Error("hung status produced a reading")
	}
}

// ---- runner ----

type scriptedPoller struct {
	calls atomic.Int32
	f     func(n int32, now func() time.Time) epaper.Sample
}

func (p *scriptedPoller) Poll(ctx context.Context, now func() time.Time) epaper.Sample {
	return p.f(p.calls.Add(1), now)
}

func TestRunnerPollsAndDerivesThenStops(t *testing.T) {
	src := &scriptedPoller{f: func(n int32, now func() time.Time) epaper.Sample {
		st := baseStatus()
		st.UptimeMs += int64(n) * 5000
		return epaper.Sample{At: now(), Status: &st, Health: &epaper.HealthData{TimeState: "GNSS_SYNCED"},
			Towers: &epaper.TowerData{}, Clients: epaper.ClientsOf(1), Power: &epaper.PowerData{}}
	}}
	r := startDashboardRunner(context.Background(), src, epaper.DefaultThresholds(), 20*time.Millisecond, time.Now)
	deadline := time.Now().Add(2 * time.Second)
	for src.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if src.calls.Load() < 3 {
		t.Fatal("runner did not poll repeatedly")
	}
	d := r.Dashboard(time.Now(), time.Now())
	if d.Overall != epaper.OverallOnline || d.GPS.Headline != "3D FIX" {
		t.Errorf("dashboard = %+v", d)
	}
	if age := r.dataAgeSeconds(time.Now()); age < 0 || age > 1 {
		t.Errorf("data age = %v", age)
	}
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
	n := src.calls.Load()
	time.Sleep(80 * time.Millisecond)
	if src.calls.Load() != n {
		t.Error("polling continued after Stop")
	}
}

func TestRunnerSurvivesAPanickingPollerAndStopsCleanly(t *testing.T) {
	src := &scriptedPoller{f: func(n int32, now func() time.Time) epaper.Sample { panic("poller bug") }}
	r := startDashboardRunner(context.Background(), src, epaper.DefaultThresholds(), 10*time.Millisecond, time.Now)
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung after a poller panic")
	}
	// With no successful reading the screen is NO DATA once the wait is over.
	if d := r.Dashboard(time.Now().Add(time.Minute), time.Now()); d.Overall != epaper.OverallNoData {
		t.Errorf("overall = %v", d.Overall)
	}
}
