package main

import (
	"time"

	"github.com/stratux/stratux/epaper"
)

// The synthetic telemetry fixtures behind the dashboard previews and the
// golden-image tests. Every dashboard is produced by the real
// epaper.Tracker from a scripted sample stream - never a hand-built
// Dashboard - so a preview is exactly what the panel would show for that
// telemetry. Nothing here touches the live data path.

var fx0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func baseStatus() epaper.StatusData {
	return epaper.StatusData{
		Version: "2.0.0~rc2", Build: "b26686c752",
		UptimeMs:      10 * 60 * 1000,
		GPSConnected:  true,
		GPSSolution:   "3D GPS",
		GPSSatsLocked: 17, GPSSatsSeen: 19,
		UAT: epaper.BandData{Enabled: true, ExternalConnected: true},
		ES:  epaper.BandData{Enabled: true, Detected: true, Assigned: true, DecoderRunning: true, Total: 500, LastMinute: 40},
	}
}

// dashSample builds a tracker fed with `n` polls five seconds apart (the last
// at fx0+5n s), each shaped by mutate(i, sample).
func dashSample(n int, mutate func(i int, s *epaper.Sample)) epaper.Dashboard {
	tr := epaper.NewTracker(epaper.DefaultThresholds(), fx0)
	var at time.Time
	for i := 0; i <= n; i++ {
		at = fx0.Add(time.Duration(i) * 5 * time.Second)
		st := baseStatus()
		st.UptimeMs += int64(at.Sub(fx0) / time.Millisecond)
		s := epaper.Sample{
			At: at, Status: &st,
			Health:  &epaper.HealthData{CPUTempC: 56, TimeState: "GNSS_SYNCED"},
			Towers:  &epaper.TowerData{},
			Clients: epaper.ClientsOf(1),
			Power:   &epaper.PowerData{},
		}
		if mutate != nil {
			mutate(i, &s)
		}
		tr.Observe(s)
	}
	return tr.Derive(at, time.Date(2026, 9, 26, 12, 42, 0, 0, time.UTC))
}

type dashFixture struct {
	Name string
	Desc string
	Dash epaper.Dashboard
}

func allFixtures() []dashFixture {
	var out []dashFixture
	add := func(name, desc string, d epaper.Dashboard) { out = append(out, dashFixture{name, desc, d}) }

	add("01-startup", "Stratux daemon just started (uptime 20 s): radios and GPS not up yet",
		dashSample(0, func(i int, s *epaper.Sample) {
			s.Status.UptimeMs = 20 * 1000
			s.Status.GPSConnected, s.Status.GPSSolution, s.Status.GPSSatsLocked = false, "Disconnected", 0
			s.Status.UAT.ExternalConnected = false
			s.Status.ES.Detected, s.Status.ES.Total, s.Status.ES.LastMinute = false, 0, 0
			s.Clients = epaper.ClientsOf(0)
		}))

	add("02-reference", "the reference state: GPS 3D fix, 1090 receiving, 978 connected with no messages, FIS-B no uplink",
		dashSample(2, nil))

	add("03-fully-receiving", "everything receiving with fresh FIS-B weather, two clients",
		dashSample(6, func(i int, s *epaper.Sample) {
			s.Status.GPSSatsLocked = 14
			s.Status.UAT.Total, s.Status.UAT.LastMinute = uint64(100+10*i), 12
			s.Status.Products = epaper.ProductTotals{METAR: uint32(3 * i), TAF: uint32(i), NEXRAD: uint32(20 * i), NOTAM: 2, Other: 5}
			s.Towers = &epaper.TowerData{Active: 1, Known: 1}
			s.Clients = epaper.ClientsOf(2)
		}))

	add("04-no-gps-fix", "GPS receiver present but no fix",
		dashSample(3, func(i int, s *epaper.Sample) {
			s.Status.GPSSolution, s.Status.GPSSatsLocked, s.Status.GPSSatsSeen = "No Fix", 0, 7
		}))

	add("05-978-disconnected", "978 radio removed",
		dashSample(3, func(i int, s *epaper.Sample) { s.Status.UAT.ExternalConnected = false }))

	add("06-fisb-stale", "weather products stopped arriving 22 minutes ago",
		dashSample(300, func(i int, s *epaper.Sample) {
			s.Status.UAT.Total, s.Status.UAT.LastMinute = 200, 0
			p := epaper.ProductTotals{METAR: 20, NEXRAD: 90, Other: 4}
			if i < 60 {
				p = epaper.ProductTotals{METAR: uint32(i / 4), NEXRAD: uint32(i), Other: 4}
			}
			s.Status.Products = p
			s.Towers = &epaper.TowerData{Active: 0, Known: 1}
		}))

	add("07-status-unavailable", "the status source stopped answering 90 seconds ago",
		func() epaper.Dashboard {
			tr := epaper.NewTracker(epaper.DefaultThresholds(), fx0)
			st := baseStatus()
			tr.Observe(epaper.Sample{At: fx0, Status: &st, Health: &epaper.HealthData{TimeState: "GNSS_SYNCED"}, Towers: &epaper.TowerData{}, Clients: epaper.ClientsOf(1), Power: &epaper.PowerData{}})
			return tr.Derive(fx0.Add(90*time.Second), time.Date(2026, 9, 26, 12, 42, 0, 0, time.UTC))
		}())

	add("08-health-warning", "active under-voltage and high CPU temperature; receivers unaffected",
		dashSample(3, func(i int, s *epaper.Sample) {
			s.Power = &epaper.PowerData{UndervoltageNow: true}
			s.Health.CPUTempC = 83
		}))

	add("09-uplink-no-products", "ground station heard, no weather products assembled yet",
		dashSample(3, func(i int, s *epaper.Sample) {
			s.Status.UAT.Total, s.Status.UAT.LastMinute = uint64(20+i), 6
			s.Towers = &epaper.TowerData{Active: 1, Known: 1}
		}))

	add("10-clock-untrusted", "device clock not trusted: footer shows uptime, never a made-up time of day",
		dashSample(2, func(i int, s *epaper.Sample) { s.Health.TimeState = "UNSYNCHRONIZED" }))

	add("11-1090-down-fault", "1090 receiver missing while GPS and 978 are fine",
		dashSample(3, func(i int, s *epaper.Sample) { s.Status.ES.Detected = false }))

	add("12-worst-case-text", "longest realistic strings: 978 quiet, weather aging, several clients, two warnings",
		dashSample(200, func(i int, s *epaper.Sample) {
			s.Status.GPSSolution, s.Status.GPSSatsLocked = "3D GPS + SBAS", 12
			s.Status.UAT.Total, s.Status.UAT.LastMinute = 50, 0
			s.Status.ES.LastMinute = 0
			p := epaper.ProductTotals{METAR: 5, NEXRAD: 40}
			if i < 80 {
				p = epaper.ProductTotals{METAR: uint32(i / 20), NEXRAD: uint32(i / 2)}
			}
			s.Status.Products = p
			s.Clients = epaper.ClientsOf(12)
			s.Power = &epaper.PowerData{ThrottledNow: true}
			s.Health.CPUTempC = 81
			s.Health.FailedServices = []string{"stratux_ais"}
		}))
	return out
}
