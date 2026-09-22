package main

import (
	"context"

	"github.com/stratux/stratux/epaper"
)

// PanelDriver is the minimal lifecycle every supported panel's controller
// driver implements - narrow and hardware-agnostic on purpose, so
// main.go's orchestration loop (settings poll, decide, render, refresh)
// never needs to know which physical controller it is actually talking
// to. Both *Driver (Waveshare 3.7in, SSD1677) and *Driver42V2 (Waveshare
// 4.2in V2, the same controller generation epd4in2_V2.py targets)
// implement this with no changes to either driver's own method set -
// this interface was added to formalize an already-matching set of
// signatures, not to force new ones.
//
// Deliberately excludes power control (Bus.SetPower): only the 3.7in
// panel's Rev2.3 driver HAT has a PWR line at all - the 4.2in panel's
// VCC is always-on, so its driver simply never calls Bus.SetPower, and
// no PanelDriver method needs to know the difference.
type PanelDriver interface {
	// Init powers up (if applicable), resets, and runs the panel's
	// documented initialization sequence. Must succeed before Clear or
	// Update.
	Init(ctx context.Context) error
	// Clear writes an all-white bitmap and activates it - establishing a
	// known-good baseline before any real content is drawn. See *Driver's
	// own Clear for why this is not merely cosmetic.
	Clear(ctx context.Context) error
	// Update writes bitmap (1-bit-per-pixel, MSB-first, row-major, stride
	// = ceil(width/8) bytes/row) and triggers a refresh - full selects a
	// full (flashing, ghosting-clearing) refresh versus a faster partial
	// one.
	Update(ctx context.Context, bitmap []byte, full bool) error
	// Sleep puts the controller into deep sleep and, if the panel has a
	// power-control line, de-asserts it. Called on every controlled stop.
	Sleep() error
}

// newPanelDriver constructs the concrete PanelDriver for panel, wired to
// width/height (from epaper.Dimensions(cfg.Panel, cfg.Rotation) - the
// caller's responsibility, not this function's, so this stays a pure
// selection/construction step with no dimension logic duplicated here).
// Neither concrete driver has its BusyTimeout set explicitly here,
// preserving this package's existing behavior exactly: each driver's own
// waitIdleWithTimeout falls back to its own documented default whenever
// BusyTimeout is the zero value. An unrecognized panel falls back to the
// 3.7in driver, mirroring epaper.Normalize's and epaper.Dimensions's own
// "unrecognized falls back to the shipped default" convention - in
// practice this is only ever called with an already-normalized,
// already-validated cfg.Panel.
func newPanelDriver(panel string, bus Bus, width, height int) PanelDriver {
	if panel == epaper.PanelWaveshare42V2 {
		return &Driver42V2{Bus: bus, WidthPx: width, HeightPx: height}
	}
	return &Driver{Bus: bus, WidthPx: width, HeightPx: height}
}
