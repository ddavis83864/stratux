package epaper

import "fmt"

// PanelWidth/PanelHeight are the Waveshare 3.7in panel's native pixel
// dimensions (landscape, rotation 0) - confirmed from Waveshare's own
// product documentation, see docs/waveshare-epaper-display.md.
const (
	PanelWidth  = 480
	PanelHeight = 280
)

// Dimensions returns the effective (width, height) for rotation degrees
// (0/90/180/270) - 90 and 270 swap width/height.
func Dimensions(rotation int) (width, height int) {
	if rotation == 90 || rotation == 270 {
		return PanelHeight, PanelWidth
	}
	return PanelWidth, PanelHeight
}

// Line is one row of text to render, with a bounded, pre-formatted
// string - never a raw struct dump, coordinate, or credential.
type Line struct {
	Text string
}

// boolWord renders a bool as a short, unambiguous word rather than
// "true"/"false", which reads poorly at a glance on a status panel.
func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// Layout turns a content snapshot plus the selected page into the
// ordered list of text lines to render - pure, so every page's exact
// wording is directly asserted by go test without ever touching a pixel
// buffer. The header line (build/version) and stale indicator are common
// to every page; the remaining lines depend on Config.Page.
func Layout(c Content, cfg Config, stale bool) []Line {
	lines := []Line{
		{Text: fmt.Sprintf("Stratux %s  build %s", c.Version, c.Build)},
	}
	if stale {
		lines = append(lines, Line{Text: "** STATUS DATA STALE / OFFLINE **"})
	}

	switch cfg.Page {
	case PageReceivers:
		lines = append(lines,
			Line{Text: "978 UAT:  " + boolWord(c.UATReceiving, "receiving", "no data")},
			Line{Text: "1090 ES:  " + boolWord(c.ESReceiving, "receiving", "no data")},
			Line{Text: fmt.Sprintf("GDL90 clients: %d", c.ConnectedGDL90Clients)},
			Line{Text: fmt.Sprintf("Traffic targets: %d", c.TrafficTargets)},
		)
	case PageHealth:
		lines = append(lines,
			Line{Text: "AHRS: " + c.AHRSState},
			Line{Text: "Baro: " + c.BaroState},
			Line{Text: "Fan:  " + c.FanState},
			Line{Text: fmt.Sprintf("CPU temp: %d C", c.CPUTempC)},
			Line{Text: "Power: " + boolWord(c.UndervoltageNow || c.ThrottledNow, "WARNING", "normal")},
		)
	default: // PageOverview
		lines = append(lines,
			Line{Text: "Readiness: " + c.OverallReady},
			Line{Text: "GPS fix: " + boolWord(c.GPSFix, "yes", "no")},
			Line{Text: "Trusted time: " + boolWord(c.TrustedTime, "yes", "no")},
			Line{Text: "Overlay: " + boolWord(c.OverlayProtected, "protected", "UNPROTECTED")},
			Line{Text: "Storage: " + c.StoragePressure},
			Line{Text: "Auto Record: " + boolWord(c.AutoRecordArmed, "armed", "off")},
			Line{Text: "Alerts: " + alertsWord(c.AlertsEnabled, c.AlertsMuted)},
		)
	}
	return lines
}

func alertsWord(enabled, muted bool) string {
	if !enabled {
		return "disabled"
	}
	if muted {
		return "muted"
	}
	return "enabled"
}

// ShutdownLines is the fixed content shown on a controlled shutdown,
// per docs/waveshare-epaper-display.md's startup/shutdown behavior -
// deliberately static (no live data) since nothing is being sampled once
// the main daemon is stopping.
func ShutdownLines() []Line {
	return []Line{
		{Text: "Stratux is shut down."},
		{Text: "Safe to remove power."},
	}
}

// StartupLines is shown immediately on epaper_main's own startup, before
// the first real content sample completes - so the panel never sits
// blank during startup.
func StartupLines() []Line {
	return []Line{
		{Text: "Stratux e-paper display"},
		{Text: "Starting..."},
	}
}

// DisclaimerLine is appended once, on every full refresh, to keep the
// aviation disclaimer visible on the physical panel itself, not only in
// documentation - see docs/waveshare-epaper-display.md.
const DisclaimerLine = "Supplemental only - not a certified instrument"
