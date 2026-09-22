package epaper

import "fmt"

// PanelWidth/PanelHeight are the Waveshare 3.7in panel's native,
// physically-fixed pixel dimensions at rotation 0 - portrait, 280 wide by
// 480 tall. This is a real hardware-validation correction: the panel's
// own vendor reference driver (EPD_3in7.h) defines EPD_3IN7_WIDTH=280,
// EPD_3IN7_HEIGHT=480, and its own Driver Output Control command
// (SSD1677 0x01) programs the controller with a fixed gate count of 479
// (=480-1) regardless of how a caller wants content rotated - confirmed
// against the vendor's own EPD_3IN7_1Gray_Init() byte sequence. An
// earlier version of this constant reversed width and height (480x280,
// "landscape at rotation 0"), which was never checked against the
// vendor's own source; on real hardware this produced a fully-connected,
// error-free, zero-flicker blank panel, because the controller's RAM
// X/Y windowing and gate count were being programmed with the wrong
// axis entirely. See epaper_main/driver.go's Init() for the corrected,
// vendor-verified command sequence this feeds.
const (
	PanelWidth  = 280
	PanelHeight = 480
)

// Panel42V2Width/Panel42V2Height are the Waveshare 4.2in e-Paper Module
// (Rev2.2, "V2" controller generation)'s native, physically-fixed pixel
// dimensions at rotation 0 - landscape, 400 wide by 300 tall. Confirmed
// directly against the vendor's own reference driver (epd4in2_V2.py):
// EPD_WIDTH=400, EPD_HEIGHT=300, and its RAM X-address window (command
// 0x44) is programmed in byte units up to 0x31 (49 = 50 bytes - 1 =
// 400 pixels/8 - 1), while its RAM Y-address window (command 0x45) is
// programmed in pixel units up to 0x012B (299 = HEIGHT-1) - confirming
// the 400-pixel axis genuinely is this controller's X/byte-addressed
// direction, unlike PanelWidth/PanelHeight above (the 3.7in panel, whose
// own vendor reference turned out to be portrait-native despite an
// initial, uncorrected assumption otherwise - see that constant's own
// doc comment). Each panel's native axis convention was independently
// verified against its own vendor source rather than assumed from the
// other.
const (
	Panel42V2Width  = 400
	Panel42V2Height = 300
)

// panelDimensions returns the named panel's native (width, height) at
// rotation 0, before any rotation is applied. An unrecognized panel
// identifier falls back to the 3.7in panel's dimensions, matching
// Normalize's own "empty/unrecognized falls back to the shipped
// default" convention - callers are expected to pass an already-
// normalized, already-validated Config.Panel in practice.
func panelDimensions(panel string) (width, height int) {
	if panel == PanelWaveshare42V2 {
		return Panel42V2Width, Panel42V2Height
	}
	return PanelWidth, PanelHeight
}

// Dimensions returns the effective (width, height) for the named panel
// at rotation degrees (0/90/180/270) - 90 and 270 swap width/height.
// Rotation 0 is each panel's own native, physical orientation; this
// project's own dashboard/settings default to rotation 0, so that is
// the orientation validated first on real hardware for each panel.
func Dimensions(panel string, rotation int) (width, height int) {
	w, h := panelDimensions(panel)
	if rotation == 90 || rotation == 270 {
		return h, w
	}
	return w, h
}

// Line is one row of text to render, with a bounded, pre-formatted
// string - never a raw struct dump, coordinate, or credential.
type Line struct {
	Text string
}

// shortBuild truncates a full git commit hash to the standard 7-character
// short form. A real hardware-validation finding: the header line
// (which also carries the version string) was sized for this project's
// original, incorrect 480px-wide "landscape at rotation 0" assumption -
// on the panel's actual native 280px-wide portrait canvas, a full 40-
// character hash pushed the line well past the right edge, clipping it.
func shortBuild(build string) string {
	const shortLen = 7
	if len(build) <= shortLen {
		return build
	}
	return build[:shortLen]
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
		{Text: fmt.Sprintf("Stratux %s  %s", c.Version, shortBuild(c.Build))},
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
