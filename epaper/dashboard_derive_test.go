package epaper

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// refStatus is the exact state illustrated in the reference image: GPS
// 3D fix (17 satellites), 1090 receiving, 978 radio connected with no
// messages yet, FIS-B with no uplink. The daemon has been up for ten
// minutes, so the startup grace is over.
func refStatus() StatusData {
	return StatusData{
		Version: "2.0.0~rc2", Build: "b26686c752ee",
		UptimeMs:      10 * 60 * 1000,
		GPSConnected:  true,
		GPSSolution:   "3D GPS",
		GPSSatsLocked: 17, GPSSatsSeen: 19,
		UAT: BandData{Enabled: true, ExternalConnected: true},
		ES:  BandData{Enabled: true, Detected: true, Assigned: true, DecoderRunning: true, Total: 500, LastMinute: 40},
	}
}

func refSample(at time.Time) Sample {
	st := refStatus()
	st.UptimeMs += int64(at.Sub(t0) / time.Millisecond)
	return Sample{
		At:      at,
		Status:  &st,
		Health:  &HealthData{CPUTempC: 55, TimeState: "GNSS_SYNCED"},
		Towers:  &TowerData{},
		Clients: &ClientData{Responding: 1},
		Power:   &PowerData{},
	}
}

// step observes a sample built by mutate at time at, advancing the daemon's
// uptime like the real one does.
func step(tr *Tracker, at time.Time, mutate func(*Sample)) {
	s := refSample(at)
	if mutate != nil {
		mutate(&s)
	}
	tr.Observe(s)
}

func newRef(t *testing.T) (*Tracker, Dashboard) {
	t.Helper()
	tr := NewTracker(DefaultThresholds(), t0)
	step(tr, t0, nil)
	return tr, tr.Derive(t0, t0)
}

func TestReferenceState(t *testing.T) {
	_, d := newRef(t)
	if d.Overall != OverallOnline || d.OverallTxt != "RECEIVER ONLINE" {
		t.Fatalf("overall = %v %q", d.Overall, d.OverallTxt)
	}
	want := map[string][2]string{
		"GPS":        {"3D FIX", "17 SAT"},
		"1090 ADS-B": {"ACTIVE", "TRAFFIC RECEIVED"},
		"978 UAT":    {"CONNECTED", "NO MESSAGES YET"},
		"FIS-B WX":   {"NO UPLINK", "AWAITING GROUND STATION"},
	}
	for _, tile := range []Tile{d.GPS, d.ES, d.UAT, d.FISB} {
		w, ok := want[tile.Label]
		if !ok || tile.Headline != w[0] || tile.Detail != w[1] {
			t.Errorf("tile %q = %q / %q, want %v", tile.Label, tile.Headline, tile.Detail, w)
		}
	}
	if d.Subtitle != "GPS FIX • 1090 ACTIVE • 978 NO MSGS" {
		t.Errorf("subtitle = %q", d.Subtitle)
	}
	if d.Clients != "1 CLIENT CONNECTED" {
		t.Errorf("clients = %q", d.Clients)
	}
	if d.Version != "2.0.0~rc2 b26686c" {
		t.Errorf("version = %q", d.Version)
	}
	if len(d.Warnings) != 0 || d.Stale {
		t.Errorf("unexpected warnings/stale: %v %v", d.Warnings, d.Stale)
	}
}

func TestVersionComesFromTheRunningDaemon(t *testing.T) {
	tr := NewTracker(DefaultThresholds(), t0)
	step(tr, t0, func(s *Sample) { s.Status.Version, s.Status.Build = "9.9.9", "abcdef1234" })
	if v := tr.Derive(t0, t0).Version; v != "9.9.9 abcdef1" {
		t.Errorf("version = %q", v)
	}
	if strings.Contains(newRefDash(t).Version, "v2.0 ") {
		t.Error("hard-coded version leaked")
	}
	// Before any reading there is no version to claim.
	if v := NewTracker(DefaultThresholds(), t0).Derive(t0, t0).Version; v != "VERSION ?" {
		t.Errorf("no-data version = %q", v)
	}
}

func newRefDash(t *testing.T) Dashboard { _, d := newRef(t); return d }

func TestStartupBanner(t *testing.T) {
	tr := NewTracker(DefaultThresholds(), t0)
	// Daemon just started: radios and GPS are not up yet.
	step(tr, t0, func(s *Sample) {
		s.Status.UptimeMs = 20 * 1000
		s.Status.GPSConnected, s.Status.GPSSolution = false, "Disconnected"
		s.Status.UAT.ExternalConnected = false
		s.Status.ES.Detected = false
	})
	d := tr.Derive(t0, t0)
	if d.Overall != OverallStarting || d.OverallTxt != "STARTING" {
		t.Fatalf("overall = %v %q, want STARTING (no FAULT while the daemon is still starting)", d.Overall, d.OverallTxt)
	}
	// While starting, things that are not up yet are "starting", not failed:
	// no fault glyphs, and the subtitle says so too.
	for _, tile := range []Tile{d.GPS, d.ES, d.UAT, d.FISB} {
		if tile.Headline != "STARTING" || tile.Level == LevelFault {
			t.Errorf("tile %q while starting = %+v", tile.Label, tile)
		}
	}
	if d.Subtitle != "GPS STARTING \u2022 1090 STARTING \u2022 978 STARTING" {
		t.Errorf("subtitle = %q", d.Subtitle)
	}
	// Past the grace period the same state is a fault.
	step(tr, t0.Add(time.Minute), func(s *Sample) {
		s.Status.UptimeMs = 100 * 1000
		s.Status.GPSConnected, s.Status.GPSSolution = false, "Disconnected"
		s.Status.UAT.ExternalConnected = false
		s.Status.ES.Detected = false
	})
	d = tr.Derive(t0.Add(time.Minute), t0)
	if d.Overall != OverallFault || d.GPS.Headline != "NO GPS" || d.GPS.Level != LevelFault {
		t.Errorf("after grace: overall %v, gps %+v (the same state must now read as a fault)", d.Overall, d.GPS)
	}
}

func TestWaitingForFirstReading(t *testing.T) {
	tr := NewTracker(DefaultThresholds(), t0)
	d := tr.Derive(t0.Add(5*time.Second), t0)
	if d.Overall != OverallStarting || d.Stale {
		t.Errorf("first seconds: %v stale=%v", d.Overall, d.Stale)
	}
	d = tr.Derive(t0.Add(30*time.Second), t0)
	if d.Overall != OverallNoData || !d.Stale {
		t.Errorf("never reached the daemon: %v stale=%v", d.Overall, d.Stale)
	}
}

func TestGPSFixLossAndRecovery(t *testing.T) {
	tr, _ := newRef(t)
	at := t0
	advance := func(sol string, locked, seen uint16, connected bool) Dashboard {
		at = at.Add(5 * time.Second)
		step(tr, at, func(s *Sample) {
			s.Status.GPSSolution, s.Status.GPSSatsLocked, s.Status.GPSSatsSeen, s.Status.GPSConnected = sol, locked, seen, connected
		})
		return tr.Derive(at, t0)
	}
	// Fix lost.
	d := advance("No Fix", 0, 6, true)
	if d.GPS.Headline == "3D FIX" {
		t.Fatal("a lost fix must never keep showing 3D FIX")
	}
	if d.GPS.Headline != "NO FIX" || d.GPS.Detail != "6 SAT SEEN" || d.GPS.Level != LevelWarn {
		t.Errorf("no fix tile = %+v", d.GPS)
	}
	if d.Overall != OverallDegraded || !strings.Contains(d.Subtitle, "NO GPS FIX") {
		t.Errorf("overall/subtitle = %v %q", d.Overall, d.Subtitle)
	}
	// Receiver unplugged.
	d = advance("Disconnected", 0, 0, false)
	if d.GPS.Headline != "NO GPS" || d.GPS.Level != LevelFault || d.Overall != OverallFault {
		t.Errorf("disconnected: %+v overall %v", d.GPS, d.Overall)
	}
	// Recovery, including SBAS.
	d = advance("3D GPS + SBAS", 12, 14, true)
	if d.GPS.Headline != "3D FIX" || d.GPS.Detail != "12 SAT SBAS" || d.Overall != OverallOnline {
		t.Errorf("recovered: %+v overall %v", d.GPS, d.Overall)
	}
	// A "fix" with fewer than four satellites is not called 3D.
	d = advance("3D GPS", 3, 8, true)
	if d.GPS.Headline == "3D FIX" || d.GPS.Level != LevelWarn {
		t.Errorf("low-satellite fix = %+v", d.GPS)
	}
	// Dead reckoning is not a satellite fix.
	d = advance("Dead Reckoning", 0, 0, true)
	if d.GPS.Headline != "DEAD RECK" {
		t.Errorf("dead reckoning = %+v", d.GPS)
	}
}

func Test1090QuietIsNotAFault(t *testing.T) {
	tr, _ := newRef(t)
	// No new 1090 messages for ten minutes: quiet airspace.
	at := t0
	for i := 0; i < 120; i++ {
		at = at.Add(5 * time.Second)
		step(tr, at, func(s *Sample) { s.Status.ES.LastMinute = 0 }) // total unchanged
	}
	d := tr.Derive(at, t0)
	if d.ES.Headline != "QUIET" || d.ES.Level != LevelIdle {
		t.Fatalf("1090 tile = %+v, want QUIET/idle", d.ES)
	}
	if d.Overall != OverallOnline {
		t.Errorf("silence alone changed the banner to %v", d.Overall)
	}
	if d.ES.Detail != "NONE IN LAST MIN" {
		t.Errorf("detail = %q (total>0 but no delta ever observed, so the age is unknown)", d.ES.Detail)
	}
	// New messages arrive -> ACTIVE, and later silence reports a real age.
	at = at.Add(5 * time.Second)
	step(tr, at, func(s *Sample) { s.Status.ES.Total, s.Status.ES.LastMinute = 503, 3 })
	if d := tr.Derive(at, t0); d.ES.Headline != "ACTIVE" {
		t.Errorf("after messages: %+v", d.ES)
	}
	for i := 0; i < 48; i++ { // four quiet minutes
		at = at.Add(5 * time.Second)
		step(tr, at, func(s *Sample) { s.Status.ES.Total, s.Status.ES.LastMinute = 503, 0 })
	}
	d = tr.Derive(at, t0)
	if d.ES.Headline != "QUIET" || d.ES.Detail != "LAST MSG <5 MIN AGO" || d.Overall != OverallOnline {
		t.Errorf("after messages then silence: %+v overall %v", d.ES, d.Overall)
	}
}

func Test978FirstMessageAndQuiet(t *testing.T) {
	tr, d := newRef(t)
	if d.UAT.Headline != "CONNECTED" || d.UAT.Detail != "NO MESSAGES YET" || d.UAT.Level != LevelIdle {
		t.Fatalf("connected/no messages must be representable: %+v", d.UAT)
	}
	at := t0.Add(5 * time.Second)
	step(tr, at, func(s *Sample) { s.Status.UAT.Total, s.Status.UAT.LastMinute = 1, 1 })
	d = tr.Derive(at, t0)
	if d.UAT.Headline != "ACTIVE" || d.UAT.Detail != "MESSAGES RECEIVED" || d.UAT.Level != LevelOK {
		t.Fatalf("first message: %+v", d.UAT)
	}
	// Two minutes of nothing: quiet with an honest age, still not a fault.
	for i := 0; i < 24; i++ {
		at = at.Add(5 * time.Second)
		step(tr, at, func(s *Sample) { s.Status.UAT.Total, s.Status.UAT.LastMinute = 1, 0 })
	}
	d = tr.Derive(at, t0)
	if d.UAT.Headline != "QUIET" || d.UAT.Detail != "LAST MSG <5 MIN AGO" || d.UAT.Level != LevelIdle {
		t.Errorf("quiet: %+v", d.UAT)
	}
	if d.Overall != OverallOnline {
		t.Errorf("978 quiet changed the banner: %v", d.Overall)
	}
}

func Test978DisconnectAndReconnect(t *testing.T) {
	tr, _ := newRef(t)
	at := t0.Add(5 * time.Second)
	step(tr, at, func(s *Sample) { s.Status.UAT.ExternalConnected = false }) // radio unplugged, no SDR assigned
	d := tr.Derive(at, t0)
	if d.UAT.Headline != "NO RADIO" || d.UAT.Level != LevelFault {
		t.Fatalf("disconnected: %+v", d.UAT)
	}
	if d.FISB.Headline != "NO RECEIVER" || d.FISB.Detail != "978 RADIO DOWN" || d.FISB.Level != LevelFault {
		t.Errorf("FIS-B must report the 978 receiver fault, got %+v", d.FISB)
	}
	if d.Overall != OverallFault || d.OverallTxt != "RECEIVER FAULT" || !strings.Contains(d.Subtitle, "978 DOWN") {
		t.Errorf("overall = %v %q %q", d.Overall, d.OverallTxt, d.Subtitle)
	}
	at = at.Add(5 * time.Second)
	step(tr, at, nil) // radio back
	d = tr.Derive(at, t0)
	if d.UAT.Headline != "CONNECTED" || d.Overall != OverallOnline {
		t.Errorf("reconnected: %+v overall %v", d.UAT, d.Overall)
	}
	// 978 turned off in settings is not a fault.
	at = at.Add(5 * time.Second)
	step(tr, at, func(s *Sample) { s.Status.UAT = BandData{} })
	d = tr.Derive(at, t0)
	if d.UAT.Headline != "OFF" || d.FISB.Headline != "OFF" || d.Overall != OverallOnline {
		t.Errorf("disabled: %+v %+v %v", d.UAT, d.FISB, d.Overall)
	}
}

func Test978SDRPath(t *testing.T) {
	tr := NewTracker(DefaultThresholds(), t0)
	step(tr, t0, func(s *Sample) {
		s.Status.UAT = BandData{Enabled: true, Detected: true, Assigned: true, DecoderRunning: false}
	})
	if d := tr.Derive(t0, t0); d.UAT.Headline != "NOT RUNNING" || d.UAT.Level != LevelFault {
		t.Errorf("SDR assigned but decoder stopped: %+v", d.UAT)
	}
	step(tr, t0.Add(5*time.Second), func(s *Sample) {
		s.Status.UAT = BandData{Enabled: true, Detected: true, Assigned: true, DecoderRunning: true}
	})
	if d := tr.Derive(t0.Add(5*time.Second), t0); d.UAT.Headline != "CONNECTED" {
		t.Errorf("SDR running: %+v", d.UAT)
	}
	step(tr, t0.Add(10*time.Second), func(s *Sample) {
		s.Status.UAT = BandData{Enabled: true, Detected: true, Assigned: true, DecoderRunning: true, Conflict: true}
	})
	if d := tr.Derive(t0.Add(10*time.Second), t0); d.UAT.Level != LevelFault {
		t.Errorf("conflicted assignment must not look healthy: %+v", d.UAT)
	}
}

func TestFISBTransitions(t *testing.T) {
	tr, d := newRef(t)
	if d.FISB.Headline != "NO UPLINK" {
		t.Fatalf("start: %+v", d.FISB)
	}
	at := t0
	obs := func(dt time.Duration, mutate func(*Sample)) Dashboard {
		at = at.Add(dt)
		step(tr, at, mutate)
		return tr.Derive(at, t0)
	}

	// A ground station is heard (tower active), but no product decoded.
	d = obs(5*time.Second, func(s *Sample) { s.Towers = &TowerData{Active: 1, Known: 1} })
	if d.FISB.Headline != "UPLINK" || d.FISB.Detail != "NO WX PRODUCTS YET" {
		t.Errorf("uplink seen, no products: %+v", d.FISB)
	}
	// Only non-weather products (NOTAM/Other) decoded: still no weather.
	d = obs(5*time.Second, func(s *Sample) {
		s.Towers = &TowerData{Active: 1, Known: 1}
		s.Status.Products = ProductTotals{NOTAM: 2, Other: 3}
	})
	if d.FISB.Headline != "UPLINK" || d.FISB.Detail != "NO WX PRODUCTS YET" {
		t.Errorf("non-weather products must not read as weather: %+v", d.FISB)
	}
	// Weather products arrive.
	d = obs(5*time.Second, func(s *Sample) {
		s.Towers = &TowerData{Active: 1, Known: 1}
		s.Status.Products = ProductTotals{METAR: 4, NEXRAD: 20, NOTAM: 2, Other: 3}
	})
	if d.FISB.Headline != "WX CURRENT" || d.FISB.Level != LevelOK || d.FISB.Detail != "NEWEST <2 MIN AGO" {
		t.Errorf("current: %+v", d.FISB)
	}
	products := ProductTotals{METAR: 4, NEXRAD: 20, NOTAM: 2, Other: 3}
	quiet := func(s *Sample) { s.Status.Products = products } // no further products, no tower
	// Four minutes later: still inside one cycle.
	d = obs(4*time.Minute, quiet)
	if d.FISB.Headline != "WX CURRENT" || d.FISB.Detail != "NEWEST <5 MIN AGO" {
		t.Errorf("4 min: %+v", d.FISB)
	}
	// Seven minutes: aging.
	d = obs(3*time.Minute, quiet)
	if d.FISB.Headline != "WX AGING" || d.FISB.Level != LevelWarn {
		t.Errorf("7 min: %+v", d.FISB)
	}
	// Past fifteen minutes: stale, and never labeled current.
	d = obs(9*time.Minute, quiet)
	if d.FISB.Headline != "WX STALE" || d.FISB.Detail != "NEWEST <30 MIN AGO" {
		t.Errorf("16 min: %+v", d.FISB)
	}
	// Even with the tower still being heard, no new products means the
	// weather stays stale.
	d = obs(5*time.Second, func(s *Sample) { s.Status.Products = products; s.Towers = &TowerData{Active: 1, Known: 1} })
	if d.FISB.Headline != "WX STALE" {
		t.Errorf("uplink without new products: %+v", d.FISB)
	}
	// Fresh products bring it back.
	products.METAR++
	d = obs(5*time.Second, quiet)
	if d.FISB.Headline != "WX CURRENT" {
		t.Errorf("recovered: %+v", d.FISB)
	}
	// FIS-B staleness alone is information, not a receiver problem.
	d = obs(20*time.Minute, quiet)
	if d.FISB.Headline != "WX STALE" || d.Overall != OverallOnline {
		t.Errorf("stale weather changed the banner: %v", d.Overall)
	}
}

func TestFISBUplinkLost(t *testing.T) {
	tr, _ := newRef(t)
	at := t0.Add(5 * time.Second)
	step(tr, at, func(s *Sample) { s.Towers = &TowerData{Active: 1, Known: 1} })
	at = at.Add(3 * time.Minute)
	step(tr, at, func(s *Sample) { s.Towers = &TowerData{Active: 0, Known: 1} })
	d := tr.Derive(at, t0)
	if d.FISB.Headline != "UPLINK LOST" || d.FISB.Detail != "LAST <5 MIN AGO" {
		t.Errorf("uplink lost: %+v", d.FISB)
	}
	// A one-minute gap (the tower flip 1 -> 0 seen in the field) is not yet a loss.
	tr2, _ := newRef(t)
	step(tr2, t0.Add(5*time.Second), func(s *Sample) { s.Towers = &TowerData{Active: 1, Known: 1} })
	step(tr2, t0.Add(70*time.Second), func(s *Sample) { s.Towers = &TowerData{Active: 0, Known: 1} })
	if d := tr2.Derive(t0.Add(70*time.Second), t0); d.FISB.Headline != "UPLINK" {
		t.Errorf("60 s tower gap = %+v, want still UPLINK (within the 2 min window)", d.FISB)
	}
}

func TestFISBAgeUnknownWhenWatchingStartsLate(t *testing.T) {
	// The display service starts after the daemon has decoded products:
	// their age is unknowable and must not be shown as current.
	tr := NewTracker(DefaultThresholds(), t0)
	step(tr, t0, func(s *Sample) {
		s.Status.Products = ProductTotals{METAR: 21, NEXRAD: 108}
		s.Towers = &TowerData{Active: 0, Known: 1}
	})
	d := tr.Derive(t0, t0)
	if d.FISB.Headline != "NO UPLINK NOW" || d.FISB.Detail != "WX AGE UNKNOWN" {
		t.Errorf("late start, no uplink: %+v", d.FISB)
	}
	step(tr, t0.Add(5*time.Second), func(s *Sample) {
		s.Status.Products = ProductTotals{METAR: 21, NEXRAD: 108}
		s.Towers = &TowerData{Active: 2, Known: 2}
	})
	if d := tr.Derive(t0.Add(5*time.Second), t0); d.FISB.Headline != "UPLINK" || d.FISB.Detail != "WX AGE UNKNOWN" {
		t.Errorf("late start, uplink: %+v", d.FISB)
	}
	// The first observed increase makes the age known.
	step(tr, t0.Add(10*time.Second), func(s *Sample) {
		s.Status.Products = ProductTotals{METAR: 22, NEXRAD: 108}
		s.Towers = &TowerData{Active: 2, Known: 2}
	})
	if d := tr.Derive(t0.Add(10*time.Second), t0); d.FISB.Headline != "WX CURRENT" {
		t.Errorf("after first delta: %+v", d.FISB)
	}
}

func TestDaemonRestartForgetsOldHistory(t *testing.T) {
	tr, _ := newRef(t)
	at := t0.Add(5 * time.Second)
	step(tr, at, func(s *Sample) {
		s.Status.Products = ProductTotals{METAR: 5}
		s.Towers = &TowerData{Active: 1, Known: 1}
	})
	at = at.Add(5 * time.Second)
	step(tr, at, func(s *Sample) {
		s.Status.Products = ProductTotals{METAR: 6}
		s.Towers = &TowerData{Active: 1, Known: 1}
	})
	if d := tr.Derive(at, t0); d.FISB.Headline != "WX CURRENT" {
		t.Fatalf("precondition: %+v", d.FISB)
	}
	// The daemon restarts: uptime and every counter go back to (near) zero.
	at = at.Add(5 * time.Second)
	step(tr, at, func(s *Sample) {
		s.Status.UptimeMs = 3000
		s.Status.Products = ProductTotals{}
		s.Status.ES.Total, s.Status.ES.LastMinute = 0, 0
		s.Status.UAT = BandData{Enabled: true, ExternalConnected: true}
		s.Towers = &TowerData{}
	})
	d := tr.Derive(at, t0)
	if d.FISB.Headline == "WX CURRENT" || d.FISB.Headline == "WX AGING" || d.FISB.Headline == "WX STALE" {
		t.Errorf("weather from before the restart is still presented: %+v", d.FISB)
	}
	if d.Overall != OverallStarting {
		t.Errorf("restarted daemon should read STARTING, got %v", d.Overall)
	}
	if d.ES.Headline != "CONNECTED" {
		t.Errorf("1090 history from the old run survived: %+v", d.ES)
	}
}

func TestStatusSourceFailureNeverLooksLive(t *testing.T) {
	tr, d := newRef(t)
	if d.Overall != OverallOnline {
		t.Fatal("precondition")
	}
	// Polls fail from here on. Within 20 s the last reading is still trusted...
	if d := tr.Derive(t0.Add(15*time.Second), t0); d.Stale || d.Overall != OverallOnline {
		t.Errorf("15 s: %v stale=%v", d.Overall, d.Stale)
	}
	// ...after that nothing from the last reading may be presented.
	d = tr.Derive(t0.Add(25*time.Second), t0)
	if d.Overall != OverallNoData || d.OverallTxt != "NO STATUS DATA" || !d.Stale {
		t.Fatalf("25 s: %v %q stale=%v", d.Overall, d.OverallTxt, d.Stale)
	}
	for _, tile := range []Tile{d.GPS, d.ES, d.UAT, d.FISB} {
		if tile.Headline != "UNKNOWN" || tile.Level != LevelUnknown {
			t.Errorf("tile %q kept a stale state: %+v", tile.Label, tile)
		}
	}
	if d.Clients != "CLIENTS UNKNOWN" {
		t.Errorf("clients = %q", d.Clients)
	}
	if !strings.Contains(d.Subtitle, "NO DATA FOR >20 S") {
		t.Errorf("subtitle = %q", d.Subtitle)
	}
	d = tr.Derive(t0.Add(10*time.Minute), t0)
	if !strings.Contains(d.Subtitle, "NO DATA FOR >5 MIN") {
		t.Errorf("subtitle = %q", d.Subtitle)
	}
	// Recovery.
	step(tr, t0.Add(10*time.Minute+5*time.Second), nil)
	if d := tr.Derive(t0.Add(10*time.Minute+5*time.Second), t0); d.Overall != OverallOnline || d.Stale {
		t.Errorf("recovery: %v stale=%v", d.Overall, d.Stale)
	}
}

func TestFrozenStatusLoopIsDetected(t *testing.T) {
	tr, _ := newRef(t)
	// Polls keep succeeding but the daemon's uptime stops advancing.
	frozen := refStatus().UptimeMs
	var at time.Time
	for i := 1; i <= 6; i++ {
		at = t0.Add(time.Duration(i) * 5 * time.Second)
		step(tr, at, func(s *Sample) { s.Status.UptimeMs = frozen })
	}
	d := tr.Derive(at, t0) // 30 s frozen
	if d.Overall != OverallNoData || d.Subtitle != "STATUS STOPPED UPDATING • TILES NOT CURRENT" {
		t.Errorf("frozen: %v %q", d.Overall, d.Subtitle)
	}
}

func TestAuxiliarySourceFailureIsPartialNotFatal(t *testing.T) {
	tr := NewTracker(DefaultThresholds(), t0)
	step(tr, t0, nil)
	// From here the clients and towers endpoints fail; status keeps working.
	at := t0
	for i := 0; i < 9; i++ {
		at = at.Add(5 * time.Second)
		step(tr, at, func(s *Sample) { s.Clients, s.Towers = nil, nil })
	}
	d := tr.Derive(at, t0) // 45 s
	if d.Clients != "CLIENTS UNKNOWN" {
		t.Errorf("clients = %q", d.Clients)
	}
	if d.GPS.Headline != "3D FIX" || d.ES.Headline != "ACTIVE" {
		t.Error("core tiles must survive an auxiliary failure")
	}
	if d.FISB.Headline != "UNKNOWN" || d.FISB.Detail != "UPLINK STATUS ?" {
		t.Errorf("without tower data FIS-B cannot claim 'no uplink': %+v", d.FISB)
	}
	found := false
	for _, w := range d.Warnings {
		if w.Text == "DATA" && strings.Contains(w.Detail, "TOWERS") && strings.Contains(w.Detail, "CLIENTS") {
			found = true
		}
	}
	if !found || d.Overall != OverallDegraded {
		t.Errorf("warnings=%v overall=%v", d.Warnings, d.Overall)
	}
	// Recovery.
	at = at.Add(5 * time.Second)
	step(tr, at, nil)
	d = tr.Derive(at, t0)
	if d.Clients != "1 CLIENT CONNECTED" || len(d.Warnings) != 0 || d.Overall != OverallOnline {
		t.Errorf("recovered: %q %v %v", d.Clients, d.Warnings, d.Overall)
	}
}

func TestClientCountChanges(t *testing.T) {
	tr, _ := newRef(t)
	at := t0
	for _, c := range []struct {
		n    int
		want string
	}{{0, "NO CLIENTS CONNECTED"}, {1, "1 CLIENT CONNECTED"}, {2, "2 CLIENTS CONNECTED"}, {0, "NO CLIENTS CONNECTED"}} {
		at = at.Add(5 * time.Second)
		n := c.n
		step(tr, at, func(s *Sample) { s.Clients = &ClientData{Responding: n} })
		if d := tr.Derive(at, t0); d.Clients != c.want {
			t.Errorf("%d clients -> %q, want %q", n, d.Clients, c.want)
		}
	}
}

func TestWarnings(t *testing.T) {
	tr, _ := newRef(t)
	at := t0
	get := func(mutate func(*Sample)) Dashboard {
		at = at.Add(5 * time.Second)
		step(tr, at, mutate)
		return tr.Derive(at, t0)
	}
	warnTexts := func(d Dashboard) string {
		var s []string
		for _, w := range d.Warnings {
			s = append(s, w.Text)
		}
		return strings.Join(s, ",")
	}
	if d := get(nil); warnTexts(d) != "" {
		t.Errorf("baseline warnings = %q", warnTexts(d))
	}
	d := get(func(s *Sample) { s.Power = &PowerData{UndervoltageNow: true, ThrottledNow: true} })
	if warnTexts(d) != "UNDERVOLT" || d.Overall != OverallDegraded {
		t.Errorf("undervoltage: %q %v", warnTexts(d), d.Overall)
	}
	// Tiles are not falsely failed by a warning.
	if d.GPS.Level != LevelOK || d.ES.Level != LevelOK {
		t.Error("a device-health warning must not mark a subsystem failed")
	}
	if d := get(func(s *Sample) { s.Power = &PowerData{ThrottledNow: true} }); warnTexts(d) != "THROTTLED" {
		t.Errorf("throttled: %q", warnTexts(d))
	}
	// Recovered: no lingering warning (only *current* bits are consumed).
	if d := get(nil); warnTexts(d) != "" || d.Overall != OverallOnline {
		t.Errorf("cleared: %q %v", warnTexts(d), d.Overall)
	}
	if d := get(func(s *Sample) { s.Health.CPUTempC = 82.4 }); warnTexts(d) != "HOT 82C" {
		t.Errorf("hot: %q", warnTexts(d))
	}
	d = get(func(s *Sample) { s.Health.FailedServices = []string{"stratux_ais", "stratux_x"} })
	if warnTexts(d) != "SVC FAIL" || !strings.Contains(d.Warnings[0].Detail, "stratux_ais, stratux_x") {
		t.Errorf("failed services: %v", d.Warnings)
	}
	d = get(func(s *Sample) {
		s.Power = &PowerData{UndervoltageNow: true}
		s.Health.CPUTempC = 90
	})
	if warnTexts(d) != "UNDERVOLT,HOT 90C" {
		t.Errorf("multiple: %q", warnTexts(d))
	}
}

func TestFooterTimeBasis(t *testing.T) {
	tr, _ := newRef(t)
	wall := time.Date(2026, 9, 26, 12, 42, 30, 0, time.UTC)
	d := tr.Derive(t0, wall)
	if d.Footer.Text != "UPDATED 12:42Z" || d.Footer.Basis != BasisUTC {
		t.Errorf("trusted clock: %+v", d.Footer)
	}
	// An untrusted clock is never shown as a time of day.
	tr2 := NewTracker(DefaultThresholds(), t0)
	step(tr2, t0, func(s *Sample) { s.Health.TimeState = "UNSYNCHRONIZED" })
	d = tr2.Derive(t0, wall)
	if d.Footer.Basis != BasisUptime || d.Footer.Text != "UPDATED UP 00:10" {
		t.Errorf("untrusted clock: %+v", d.Footer)
	}
	if strings.Contains(d.Footer.Text, "12:42") {
		t.Error("untrusted wall clock leaked into the footer")
	}
	// Health source unavailable: fall back to uptime too.
	tr3 := NewTracker(DefaultThresholds(), t0)
	step(tr3, t0, func(s *Sample) { s.Health = nil })
	if d := tr3.Derive(t0, wall); d.Footer.Basis != BasisUptime {
		t.Errorf("no health: %+v", d.Footer)
	}
	// A wall-clock step (GPS time sync) does not disturb any freshness
	// window: the tracker only uses the monotonic 'now' it is given.
	step(tr, t0.Add(10*time.Second), nil)
	stepped := wall.Add(-6 * time.Hour)
	a := tr.Derive(t0.Add(10*time.Second), wall)
	b := tr.Derive(t0.Add(10*time.Second), stepped)
	if a.MaterialKey() != b.MaterialKey() {
		t.Error("a wall-clock step changed the material state")
	}
}

func TestMaterialKeyIgnoresOnlyTheFooterTime(t *testing.T) {
	tr, _ := newRef(t)
	a := tr.Derive(t0, time.Date(2026, 9, 26, 12, 42, 0, 0, time.UTC))
	b := tr.Derive(t0, time.Date(2026, 9, 26, 12, 43, 0, 0, time.UTC))
	if a.Footer.Text == b.Footer.Text {
		t.Fatal("test setup: footer should differ")
	}
	if a.MaterialKey() != b.MaterialKey() {
		t.Error("the minute ticking over must not be a material change")
	}
	c := a
	c.GPS.Headline = "NO FIX"
	if a.MaterialKey() == c.MaterialKey() {
		t.Error("a tile change must be material")
	}
	e := a
	e.Clients = "2 CLIENTS CONNECTED"
	if a.MaterialKey() == e.MaterialKey() {
		t.Error("a client count change must be material")
	}
	f := a
	f.Stale = true
	if a.MaterialKey() == f.MaterialKey() {
		t.Error("going stale must be material")
	}
}

func TestSubtitleNeverContradictsTiles(t *testing.T) {
	// Exhaust the interesting combinations and check the subtitle tokens
	// against the tile levels they summarize.
	gpsStates := []struct {
		sol       string
		locked    uint16
		connected bool
	}{{"3D GPS", 12, true}, {"No Fix", 0, true}, {"Disconnected", 0, false}}
	for _, g := range gpsStates {
		for _, esOn := range []bool{true, false} {
			for _, uatConn := range []bool{true, false} {
				tr := NewTracker(DefaultThresholds(), t0)
				step(tr, t0, func(s *Sample) {
					s.Status.GPSSolution, s.Status.GPSSatsLocked, s.Status.GPSConnected = g.sol, g.locked, g.connected
					s.Status.ES.Detected = esOn
					s.Status.UAT.ExternalConnected = uatConn
				})
				d := tr.Derive(t0, t0)
				if gotFix := strings.HasPrefix(d.Subtitle, "GPS FIX \u2022"); gotFix != (d.GPS.Level == LevelOK) {
					t.Errorf("GPS level %v but subtitle %q", d.GPS.Level, d.Subtitle)
				}
				if !esOn && !strings.Contains(d.Subtitle, "1090 DOWN") {
					t.Errorf("1090 down but subtitle %q", d.Subtitle)
				}
				if !uatConn && !strings.Contains(d.Subtitle, "978 DOWN") {
					t.Errorf("978 down but subtitle %q", d.Subtitle)
				}
				if d.Overall == OverallOnline && (d.GPS.Level >= LevelWarn || d.ES.Level == LevelFault || d.UAT.Level == LevelFault) {
					t.Errorf("ONLINE with a failing tile: %+v", d)
				}
			}
		}
	}
}

func TestDeadSourceIsNotMistakenForAFrozenOne(t *testing.T) {
	// No poll succeeds after the first: at 17 s the source is late but the
	// screen must not claim "status stopped updating" (that means polls
	// keep succeeding with frozen values); it is simply not yet stale, and
	// at 25 s it reads as no data.
	tr, _ := newRef(t)
	if d := tr.Derive(t0.Add(17*time.Second), t0); d.Overall != OverallOnline {
		t.Errorf("17 s: %v", d.Overall)
	}
	d := tr.Derive(t0.Add(25*time.Second), t0)
	if d.Overall != OverallNoData || strings.Contains(d.Subtitle, "STOPPED UPDATING") {
		t.Errorf("25 s: %v %q", d.Overall, d.Subtitle)
	}
}

func TestSatelliteCountHysteresis(t *testing.T) {
	tr, _ := newRef(t) // 17 satellites
	at := t0
	detail := func(sats uint16) string {
		at = at.Add(5 * time.Second)
		step(tr, at, func(s *Sample) { s.Status.GPSSatsLocked = sats })
		return tr.Derive(at, t0).GPS.Detail
	}
	// Wobbles of one or two do not change what is shown (and so cost no refresh).
	for _, n := range []uint16{16, 17, 15, 18, 16, 17} {
		if d := detail(n); d != "17 SAT" {
			t.Fatalf("%d satellites shown as %q, want the steady 17 SAT", n, d)
		}
	}
	// A real change of three or more is followed.
	if d := detail(14); d != "14 SAT" {
		t.Errorf("14 satellites -> %q", d)
	}
	if d := detail(13); d != "14 SAT" {
		t.Errorf("13 (a wobble from 14) -> %q", d)
	}
	// Crossing the usable-solution line always shows the truth.
	if d := detail(4); d != "4 SAT" {
		t.Errorf("4 satellites -> %q", d)
	}
	d := func() Dashboard {
		at = at.Add(5 * time.Second)
		step(tr, at, func(s *Sample) { s.Status.GPSSatsLocked = 3 })
		return tr.Derive(at, t0)
	}()
	if d.GPS.Detail != "3 SAT \u2022 LOW" {
		t.Errorf("3 satellites -> %+v", d.GPS)
	}
}

// After a daemon restart the uptime falls to near zero and then climbs from
// there: that is a live daemon, not a frozen one, however long the previous
// run had been up. (Found on the bench panel: the screen stayed NO STATUS
// DATA after a restart until the new uptime overtook the old.)
func TestRestartedDaemonIsNotMistakenForAFrozenOne(t *testing.T) {
	tr, _ := newRef(t) // uptime ~10 min
	// The daemon is down for a minute (no reading succeeds), then comes
	// back with its uptime near zero.
	at := t0.Add(60 * time.Second)
	up := int64(2000)
	var d Dashboard
	for i := 0; i < 8; i++ { // 40 s of polling after the restart
		u := up
		step(tr, at, func(s *Sample) { s.Status.UptimeMs = u })
		d = tr.Derive(at, t0)
		if d.Overall == OverallNoData || d.Stale {
			t.Fatalf("%d s after the restart the screen reads %v (stale=%v): %q", i*5, d.Overall, d.Stale, d.Subtitle)
		}
		at = at.Add(5 * time.Second)
		up += 5000
	}
	if d.Overall != OverallStarting {
		t.Errorf("overall = %v, want STARTING while the new run is under 90 s", d.Overall)
	}
}
