package main

// The operating dashboard's telemetry source: a decoupled HTTP client of
// the main daemon's existing, read-only status APIs (never a new API and
// never a Go-level import of the daemon's state), plus the small runner
// that polls it on a bounded cadence in its own goroutine so a slow or hung
// request can never delay a refresh decision or the shutdown path. See
// docs/epaper-operating-dashboard.md for the field-by-field mapping and
// the limitations of each source.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/stratux/stratux/epaper"
)

// maxStatusBody bounds every response read; the real bodies are a few KB.
const maxStatusBody = 1 << 20

// dashPoller is one poll cycle's worth of telemetry. The HTTP
// implementation is DashSource; tests use fakes.
type dashPoller interface {
	Poll(ctx context.Context, now func() time.Time) epaper.Sample
}

// DashSource polls the daemon over HTTP.
type DashSource struct {
	BaseURL string
	Client  *http.Client
}

// NewDashSource returns a DashSource with a bounded per-request timeout.
func NewDashSource(baseURL string, timeout time.Duration) *DashSource {
	return &DashSource{BaseURL: baseURL, Client: &http.Client{Timeout: timeout}}
}

func (s *DashSource) getInto(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxStatusBody)).Decode(out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ---- wire shapes (only the fields the dashboard uses) ----

type wireStatus struct {
	Version, Build string
	Uptime         int64 // milliseconds (stratuxClock.Milliseconds)

	GPS_connected            bool
	GPS_solution             string
	GPS_satellites_locked    uint16
	GPS_satellites_seen      uint16
	UATRadio_connected       bool
	UAT_Enabled              bool
	UAT_Detected             bool
	UAT_Assigned             bool
	UAT_DecoderRunning       bool
	UAT_Ambiguous            bool
	UAT_Conflict             bool
	UAT_messages_total       uint64
	UAT_messages_last_minute uint
	ES_Enabled               bool
	ES_Detected              bool
	ES_Assigned              bool
	ES_DecoderRunning        bool
	ES_Ambiguous             bool
	ES_Conflict              bool
	ES_messages_total        uint64
	ES_messages_last_minute  uint

	UAT_METAR_total  uint32
	UAT_TAF_total    uint32
	UAT_NEXRAD_total uint32
	UAT_SIGMET_total uint32
	UAT_PIREP_total  uint32
	UAT_NOTAM_total  uint32
	UAT_OTHER_total  uint32
}

func (w wireStatus) data() epaper.StatusData {
	return epaper.StatusData{
		Version: w.Version, Build: w.Build, UptimeMs: w.Uptime,
		GPSConnected: w.GPS_connected, GPSSolution: w.GPS_solution,
		GPSSatsLocked: w.GPS_satellites_locked, GPSSatsSeen: w.GPS_satellites_seen,
		UAT: epaper.BandData{
			Enabled: w.UAT_Enabled, Detected: w.UAT_Detected, Assigned: w.UAT_Assigned,
			DecoderRunning: w.UAT_DecoderRunning, Ambiguous: w.UAT_Ambiguous, Conflict: w.UAT_Conflict,
			ExternalConnected: w.UATRadio_connected,
			Total:             w.UAT_messages_total, LastMinute: w.UAT_messages_last_minute,
		},
		ES: epaper.BandData{
			Enabled: w.ES_Enabled, Detected: w.ES_Detected, Assigned: w.ES_Assigned,
			DecoderRunning: w.ES_DecoderRunning, Ambiguous: w.ES_Ambiguous, Conflict: w.ES_Conflict,
			Total: w.ES_messages_total, LastMinute: w.ES_messages_last_minute,
		},
		Products: epaper.ProductTotals{
			METAR: w.UAT_METAR_total, TAF: w.UAT_TAF_total, NEXRAD: w.UAT_NEXRAD_total,
			SIGMET: w.UAT_SIGMET_total, PIREP: w.UAT_PIREP_total, NOTAM: w.UAT_NOTAM_total, Other: w.UAT_OTHER_total,
		},
	}
}

type wireHealth struct {
	System struct {
		CPUTempC       float64
		FailedServices []string
	}
	Time struct{ State string }
}

type wireTower struct{ Messages_last_minute uint }

type wireClient struct {
	Ip        string
	SleepFlag bool
}

type wirePower struct {
	UndervoltageNow bool `json:"undervoltageNow"`
	ThrottledNow    bool `json:"throttledNow"`
}

// Poll reads every endpoint concurrently (each bounded by the client
// timeout) and returns the sample. A failed endpoint is left nil, never
// guessed: the tracker treats a missing member as "unknown".
func (s *DashSource) Poll(ctx context.Context, now func() time.Time) epaper.Sample {
	var (
		wg      sync.WaitGroup
		status  *epaper.StatusData
		health  *epaper.HealthData
		towers  *epaper.TowerData
		clients *epaper.ClientData
		power   *epaper.PowerData
	)
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	run(func() {
		var w wireStatus
		if err := s.getInto(ctx, "/getStatus", &w); err != nil {
			return
		}
		// A body that decodes but is not a Stratux status (a stray service
		// on the port, an empty object) is a failed read, not "all off".
		if w.Version == "" && w.Uptime == 0 {
			return
		}
		d := w.data()
		status = &d
	})
	run(func() {
		var w wireHealth
		if err := s.getInto(ctx, "/getHealth", &w); err != nil {
			return
		}
		health = &epaper.HealthData{CPUTempC: w.System.CPUTempC, TimeState: w.Time.State, FailedServices: w.System.FailedServices}
	})
	run(func() {
		var m map[string]wireTower
		if err := s.getInto(ctx, "/getTowers", &m); err != nil {
			return
		}
		t := epaper.TowerData{Known: len(m)}
		for _, tw := range m {
			if tw.Messages_last_minute > 0 {
				t.Active++
			}
		}
		towers = &t
	})
	run(func() {
		var m map[string]wireClient
		if err := s.getInto(ctx, "/getClients", &m); err != nil {
			return
		}
		seen := map[string]bool{}
		for _, c := range m {
			// Only UDP network clients carry an address; a connection
			// counts while the daemon's own liveness probe (ICMP ping/pong,
			// refreshed about once a second) says it is not asleep.
			if c.Ip != "" && !c.SleepFlag {
				seen[c.Ip] = true
			}
		}
		clients = &epaper.ClientData{Responding: len(seen)}
	})
	run(func() {
		var w wirePower
		if err := s.getInto(ctx, "/getPowerHealth", &w); err != nil {
			return
		}
		power = &epaper.PowerData{UndervoltageNow: w.UndervoltageNow, ThrottledNow: w.ThrottledNow}
	})
	wg.Wait()
	return epaper.Sample{At: now(), Status: status, Health: health, Towers: towers, Clients: clients, Power: power}
}

// ---- runner ----

// dashboardRunner owns a Tracker and feeds it from a poller in a
// background goroutine. Everything time-based stays in the pure Tracker;
// this type only adds the concurrency.
type dashboardRunner struct {
	mu     sync.Mutex
	tr     *epaper.Tracker
	cancel context.CancelFunc
	done   chan struct{}
}

func startDashboardRunner(parent context.Context, src dashPoller, th epaper.Thresholds, every time.Duration, now func() time.Time) *dashboardRunner {
	ctx, cancel := context.WithCancel(parent)
	r := &dashboardRunner{tr: epaper.NewTracker(th, now()), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		defer func() {
			// A bug in a poller must never take the display service down.
			_ = recover()
		}()
		poll := func() {
			pctx, pcancel := context.WithTimeout(ctx, 4*time.Second)
			s := src.Poll(pctx, now)
			pcancel()
			if ctx.Err() != nil {
				return
			}
			r.mu.Lock()
			r.tr.Observe(s)
			r.mu.Unlock()
		}
		poll()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				poll()
			}
		}
	}()
	return r
}

// Dashboard derives the current screen state.
func (r *dashboardRunner) Dashboard(now, wall time.Time) epaper.Dashboard {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tr.Derive(now, wall)
}

// dataAgeSeconds is how old the newest successful status reading is, or -1
// before the first one.
func (r *dashboardRunner) dataAgeSeconds(now time.Time) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if age, ok := r.tr.StatusAge(now); ok {
		return age.Seconds()
	}
	return -1
}

// Stop ends polling and waits for the goroutine.
func (r *dashboardRunner) Stop() {
	r.cancel()
	<-r.done
}
