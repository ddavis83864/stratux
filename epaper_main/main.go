/*
epaper_main is the optional Waveshare 3.7" e-paper display service - a
separate binary and systemd unit (debian/stratux_epaper.service), never
linked into the main stratuxrun daemon, so a crash, panic, hang, or GPIO
fault here can never affect ADS-B decoding, GDL90 forwarding, AHRS, fan
control, GPS, alerting, recording, OTA, or shutdown. This is the same
fault-isolation pattern this codebase already uses for
fancontrol_main/fancontrol.go.

This process:
  - polls the main daemon's existing /getSettings over HTTP (never a new
    API) to read its own configuration and the enabled/disabled flag;
  - when disabled (the shipped default), touches no GPIO/SPI at all and
    simply idles, re-checking periodically;
  - when enabled, initializes the panel once, then loops: sample status
    from the main daemon's existing read-only APIs (statussource.go),
    decide whether a refresh is due (epaper.Decide, pure), and if so
    render (render.go) and write it to the panel (driver.go);
  - self-reports its own bounded health to common.EpaperStatusPath (a
    RAM-backed /run file) every cycle, exactly like fancontrol_main does
    for FanControllerStatus - the main daemon reads this for dashboard
    display, never the reverse;
  - on SIGTERM/SIGINT (a controlled stop, including a full system
    shutdown), renders the fixed shutdown screen and puts the panel to
    sleep before exiting - see epaper.ShutdownLines.

See docs/waveshare-epaper-display.md for the full design, GPIO ownership
matrix, wiring table, and aviation disclaimer.
*/
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/epaper"
)

func main() {
	baseURL := flag.String("baseurl", "http://127.0.0.1", "base URL of the main Stratux daemon's HTTP API")
	pollInterval := flag.Duration("poll", 5*time.Second, "how often to sample status and consider a refresh")
	settingsInterval := flag.Duration("settings-poll", 15*time.Second, "how often to re-check configuration")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	src := NewStatusSource(*baseURL, 3*time.Second)
	settingsSrc := NewStatusSource(*baseURL, 3*time.Second)

	run(ctx, src, settingsSrc, *pollInterval, *settingsInterval)
}

// run is the whole service lifecycle, factored out of main so it can be
// exercised by tests with fake sources and a cancellable context. It
// never returns an error - every failure inside it is caught, classified
// into a bounded epaper.ErrorCategory, and self-reported via the status
// file, never propagated as a process crash (a real panic anywhere in
// the refresh path is itself recovered around, one level up, in
// refreshOnce).
func run(ctx context.Context, statusSrc, settingsSrc *StatusSource, pollInterval, settingsInterval time.Duration) {
	cfg := epaper.Config{} // disabled zero value until the first settings poll succeeds
	var bus *gpioBus
	var driver *Driver
	var policy epaper.PolicyState
	health := epaper.Health{State: epaper.StateDisabled, UpdatedAt: time.Now()}
	writeHealth(health)

	lastSettingsPoll := time.Time{}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	shutdown := func() {
		if driver != nil {
			lines := append(epaper.ShutdownLines(), epaper.Line{Text: epaper.DisclaimerLine})
			w, h := epaper.Dimensions(cfg.Rotation)
			bmp := Render(lines, w, h)
			_ = driver.Update(context.Background(), bmp, true)
			_ = driver.Sleep()
		}
		if bus != nil {
			closeGPIOBus()
		}
	}

	for {
		select {
		case <-ctx.Done():
			shutdown()
			return
		case <-ticker.C:
		}

		if time.Since(lastSettingsPoll) >= settingsInterval || lastSettingsPoll.IsZero() {
			lastSettingsPoll = time.Now()
			newCfg, ok := pollConfig(settingsSrc)
			if ok && newCfg != cfg {
				if driver != nil {
					_ = driver.Sleep()
					closeGPIOBus()
					driver, bus = nil, nil
					policy = epaper.PolicyState{}
				}
				cfg = newCfg
			}
		}

		if !cfg.Enabled {
			health = epaper.Health{State: epaper.StateDisabled, UpdatedAt: time.Now()}
			writeHealth(health)
			continue
		}

		if driver == nil {
			var err error
			bus, err = openGPIOBus(cfg.GPIO)
			if err != nil {
				health = errorHealth(health, epaper.ErrorGPIOOpen)
				writeHealth(health)
				continue
			}
			w, h := epaper.Dimensions(cfg.Rotation)
			driver = &Driver{Bus: bus, WidthPx: w, HeightPx: h}
			if err := driver.Init(ctx); err != nil {
				health = errorHealth(health, classifyInitError(err))
				writeHealth(health)
				closeGPIOBus()
				driver, bus = nil, nil
				continue
			}
			startupLines := append(epaper.StartupLines(), epaper.Line{Text: epaper.DisclaimerLine})
			_ = driver.Update(ctx, Render(startupLines, w, h), true)
		}

		health = refreshOnce(ctx, driver, statusSrc, cfg, &policy, health)
		writeHealth(health)
	}
}

// pollConfig reads Epaper* fields from /getSettings, normalizes them,
// and reports whether a usable config was obtained at all (a request
// failure returns ok=false, leaving the caller's previous cfg
// untouched - a transient HTTP hiccup must never itself disable an
// already-running display).
func pollConfig(src *StatusSource) (epaper.Config, bool) {
	m, err := src.getJSON("/getSettings")
	if err != nil {
		return epaper.Config{}, false
	}
	raw := epaper.Config{
		Enabled:                bl(m, "EpaperEnabled"),
		Panel:                  str(m, "EpaperPanel"),
		Rotation:               in(m, "EpaperRotation"),
		RefreshIntervalSeconds: in(m, "EpaperRefreshIntervalSeconds"),
		FullRefreshEvery:       in(m, "EpaperFullRefreshEvery"),
		Page:                   str(m, "EpaperPage"),
	}
	cfg, err := epaper.Normalize(raw)
	if err != nil {
		return epaper.Config{}, false
	}
	return cfg, true
}

// refreshOnce samples status, decides, and (if due) renders and writes
// one frame - all wrapped in a recover() so a genuine bug anywhere in
// this path (a rendering edge case, an unexpected driver panic) is
// caught and reported as ErrorCategory, never a process crash that would
// need systemd to restart it mid-refresh with the panel in an unknown
// electrical state.
func refreshOnce(ctx context.Context, driver *Driver, src *StatusSource, cfg epaper.Config, policy *epaper.PolicyState, prev epaper.Health) (result epaper.Health) {
	result = prev
	defer func() {
		if r := recover(); r != nil {
			log.Printf("epaper: recovered panic in refresh cycle: %v", r)
			result = errorHealth(prev, epaper.ErrorSPIWrite)
		}
	}()

	content, err := src.Gather(time.Now())
	if err != nil {
		h := errorHealth(prev, epaper.ErrorStatusSourceUp)
		h.DataAgeSeconds = time.Since(policy.LastRefreshAt).Seconds()
		return h
	}

	stale := epaper.ShouldShowStale(time.Since(policy.LastRefreshAt))
	interval := time.Duration(cfg.RefreshIntervalSeconds) * time.Second
	kind, newPolicy := epaper.Decide(*policy, content, interval, cfg.FullRefreshEvery, time.Now())
	*policy = newPolicy

	h := epaper.Health{
		UpdatedAt:             time.Now(),
		State:                 epaper.StateRunning,
		ConfiguredPanel:       cfg.Panel,
		PanelDetected:         true,
		LastSuccessfulRefresh: prev.LastSuccessfulRefresh,
		ConsecutiveFailures:   0,
		BusyTimeoutCount:      prev.BusyTimeoutCount,
		FullRefreshCount:      prev.FullRefreshCount,
		PartialRefreshCount:   prev.PartialRefreshCount,
		DataAgeSeconds:        time.Since(content.SampledAt).Seconds(),
	}

	if kind == epaper.RefreshNone {
		return h
	}

	w, hgt := epaper.Dimensions(cfg.Rotation)
	lines := epaper.Layout(content, cfg, stale)
	if kind == epaper.RefreshFull {
		lines = append(lines, epaper.Line{Text: epaper.DisclaimerLine})
	}
	bmp := Render(lines, w, hgt)

	if err := driver.Update(ctx, bmp, kind == epaper.RefreshFull); err != nil {
		cat := epaper.ErrorSPIWrite
		if err == errBusyTimeout {
			cat = epaper.ErrorBusyTimeout
			h.BusyTimeoutCount = prev.BusyTimeoutCount + 1
		}
		h = errorHealth(prev, cat)
		h.ConsecutiveFailures = prev.ConsecutiveFailures + 1
		return h
	}

	h.LastSuccessfulRefresh = time.Now()
	if kind == epaper.RefreshFull {
		h.FullRefreshCount = prev.FullRefreshCount + 1
	} else {
		h.PartialRefreshCount = prev.PartialRefreshCount + 1
	}
	return h
}

func classifyInitError(err error) epaper.ErrorCategory {
	if err == errBusyTimeout {
		return epaper.ErrorBusyTimeout
	}
	return epaper.ErrorPanelNotFound
}

func errorHealth(prev epaper.Health, cat epaper.ErrorCategory) epaper.Health {
	h := prev
	h.UpdatedAt = time.Now()
	h.State = epaper.StateError
	if cat == epaper.ErrorPanelNotFound {
		h.State = epaper.StateNotDetected
		h.PanelDetected = false
	}
	h.LastErrorCat = cat
	h.ConsecutiveFailures = prev.ConsecutiveFailures + 1
	return h
}

func writeHealth(h epaper.Health) {
	if err := common.WriteEpaperStatus(common.EpaperStatusPath, h); err != nil {
		log.Printf("epaper: could not write status file: %v", err)
	}
}
