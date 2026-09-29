package main

// splash.go: the one-shot ARS splash renderer behind `epaperd -splash`.
//
// This is deliberately NOT part of the service loop (run in main.go):
// it is a manual, run-to-completion command whose only purpose so far is
// the first physical acceptance test of the approved production splash
// (docs/epaper-boot-splash.md, "Physical acceptance gate"). Nothing here
// is called at boot, and no systemd unit, postinst step, or setting
// invokes it. It draws the pre-generated bitmap embedded from
// epaper/splash/assets - no image conversion happens at runtime.
//
// Sequence (mirrors the validated service startup in run(): Init, then
// Clear, then one full refresh): Init -> Clear -> Update(full) -> Sleep,
// then release the SPI/GPIO mapping and exit. An e-paper panel is
// bistable, so the image persists after the process exits and the panel
// sleeps; the very next epaperd service start re-initializes the panel
// from scratch (that re-initialization is exactly what the takeover
// test exercises).

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/epaper"
	"github.com/stratux/stratux/epaper/splash"
	"github.com/stratux/stratux/epaper/splash/assets"
)

const (
	// splashTimeout bounds the whole command: two full refreshes (Clear
	// + Update) at a few seconds each, with generous margin, so a stuck
	// BUSY line can never hang the terminal indefinitely.
	splashTimeout = 90 * time.Second

	// statusFreshWindow is how recent the running service's self-reported
	// status must be to count as "the service currently owns the panel".
	// The service rewrites its status file every poll (5 s by default).
	statusFreshWindow = 30 * time.Second

	exitOK      = 0
	exitFailure = 1 // hardware/render failure
	exitRefused = 2 // bad usage, or refused before touching hardware
)

// loadBitmap returns the embedded production boot-splash bitmap (plain
// ARS logo). A variable so tests can substitute a corrupt asset.
var loadBitmap = assets.Bitmap

// loadShutdownBitmap returns the embedded production final-shutdown-
// splash bitmap (ARS logo plus the "Safe to remove power" message - see
// issue #43 and docs/epaper-shutdown-splash.md). A variable so tests can
// substitute a corrupt asset.
var loadShutdownBitmap = assets.ShutdownBitmap

// splashBitmap returns the production boot splash for the given content
// rotation.
func splashBitmap(rotation int) ([]byte, error) {
	return rotatedBitmap(loadBitmap(), rotation)
}

// shutdownSplashBitmap returns the production final shutdown splash
// (logo plus "Safe to remove power") for the given content rotation.
func shutdownSplashBitmap(rotation int) ([]byte, error) {
	return rotatedBitmap(loadShutdownBitmap(), rotation)
}

// rotatedBitmap applies the requested content rotation to an already-
// loaded 400x300 splash bitmap. 0 is the bitmap exactly as generated;
// 180 is a pure point-symmetric transform of it using the same
// rotateImage and packMonochrome the validated (rotation-fixed,
// c0dcd19c) status renderer uses. 90/270 are refused: every splash
// asset's approved artwork is 4:3 landscape, and a portrait 300x400
// logical canvas would need a separately generated and separately
// approved layout.
func rotatedBitmap(bm []byte, rotation int) ([]byte, error) {
	switch rotation {
	case 0:
		return bm, nil
	case 180:
		img := image.NewGray(image.Rect(0, 0, splash.Width, splash.Height))
		for y := 0; y < splash.Height; y++ {
			for x := 0; x < splash.Width; x++ {
				v := uint8(255)
				if splash.Pixel(bm, x, y) {
					v = 0
				}
				img.SetGray(x, y, color.Gray{Y: v})
			}
		}
		return packMonochrome(rotateImage(img, 180)), nil
	default:
		return nil, fmt.Errorf("rotation %d is not supported for the splash (only 0 and 180: the approved artwork is 4:3 landscape)", rotation)
	}
}

// guardOwnership refuses to proceed when the epaperd service appears to
// hold the panel: two processes driving one SPI bus/BUSY line would
// corrupt the display and confuse the takeover test. Evidence is the
// service's own self-reported status file - under systemd it lives in a
// RuntimeDirectory that is removed when the unit stops, so a stopped
// service leaves no file. A disabled service (EpaperEnabled false)
// touches no hardware and never blocks this command.
func guardOwnership(statusPath string, now time.Time) error {
	var h epaper.Health
	if err := common.ReadEpaperStatus(statusPath, &h); err != nil {
		return nil // no status file (or unreadable): no evidence of an owner
	}
	if h.State == epaper.StateDisabled || now.Sub(h.UpdatedAt) > statusFreshWindow {
		return nil
	}
	return fmt.Errorf("the epaperd service appears to be running and may own the panel (state %s, status updated %s ago); "+
		"stop it first with `sudo systemctl stop stratux_epaper`, or pass -splash-force if you are sure it is not driving the panel",
		h.State, now.Sub(h.UpdatedAt).Round(time.Second))
}

// renderSplash runs Init -> Clear -> Update(full) -> Sleep against
// driver. If anything after a successful Init fails, it still attempts
// Sleep so the controller is never left mid-sequence and powered.
func renderSplash(ctx context.Context, driver PanelDriver, bitmap []byte, out io.Writer) (err error) {
	fmt.Fprintln(out, "initializing panel...")
	if err := driver.Init(ctx); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	defer func() {
		if serr := driver.Sleep(); serr != nil && err == nil {
			err = fmt.Errorf("sleep: %w", serr)
		}
	}()

	fmt.Fprintln(out, "clearing panel (full refresh)...")
	if err := driver.Clear(ctx); err != nil {
		return fmt.Errorf("clear: %w", err)
	}
	fmt.Fprintln(out, "drawing ARS splash (full refresh)...")
	if err := driver.Update(ctx, bitmap, true); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

// busOpener opens the hardware and returns a release func that must be
// called exactly once when done. Injected so tests can prove release
// happens without a Raspberry Pi.
type busOpener func(epaper.GPIOMapping) (Bus, func(), error)

func openRealBus(m epaper.GPIOMapping) (Bus, func(), error) {
	b, err := openGPIOBus(m)
	if err != nil {
		return nil, nil, err
	}
	return b, closeGPIOBus, nil
}

// splashSource pairs a rotation-aware bitmap accessor with a name, so
// runSplash can draw either the boot splash or the final shutdown splash
// without knowing which - each has its own committed, embedded asset (see
// epaper/splash/assets), never mixed at runtime. bitmap is a genuine
// top-level function reference (splashBitmap/shutdownSplashBitmap), not a
// closure over loadBitmap/loadShutdownBitmap: it re-reads those package
// vars fresh on every call, so tests overriding them (to inject a corrupt
// asset) take effect on the very next call, before any hardware is
// opened. A `load func() []byte` field would look equivalent but is not:
// copying the var's value into a struct literal snapshots it once, at
// package-init time, and a later test-time reassignment of the var would
// not reach that already-built struct.
type splashSource struct {
	bitmap func(rotation int) ([]byte, error)
	name   string // for error/log messages: "splash" or "shutdown splash"
}

var bootSplashSource = splashSource{bitmap: splashBitmap, name: "splash"}
var shutdownSplashSource = splashSource{bitmap: shutdownSplashBitmap, name: "shutdown splash"}

// runSplash draws one of the two committed splash assets (src); it
// returns a process exit code. Every check that can refuse runs before
// any hardware is opened.
func runSplash(ctx context.Context, src splashSource, panel string, rotation int, force bool, statusPath string, open busOpener, out, errOut io.Writer) int {
	if panel != epaper.PanelWaveshare42V2 {
		fmt.Fprintf(errOut, "%s: the approved artwork is generated for %q only, not %q\n", src.name, epaper.PanelWaveshare42V2, panel)
		return exitRefused
	}
	// The asset is embedded, so it cannot be "missing" at runtime, but
	// prove it is intact before any hardware is opened: a bad build must
	// fail here, not put garbage on the panel. Validate at rotation 0
	// (identity transform) rather than loading the raw asset separately:
	// src.bitmap is a genuine function reference and always re-reads the
	// current loader, where a separate `.load()` field would not (see
	// the splashSource doc comment).
	unrotated, err := src.bitmap(0)
	if err != nil {
		fmt.Fprintln(errOut, "splash:", err)
		return exitRefused
	}
	if _, err := splash.Validate(unrotated); err != nil {
		fmt.Fprintf(errOut, "%s: embedded splash asset is invalid: %v\n", src.name, err)
		return exitFailure
	}
	bitmap, err := src.bitmap(rotation)
	if err != nil {
		fmt.Fprintln(errOut, "splash:", err)
		return exitRefused
	}
	if !force {
		if err := guardOwnership(statusPath, time.Now()); err != nil {
			fmt.Fprintln(errOut, "splash:", err)
			return exitRefused
		}
	}

	nativeW, nativeH := epaper.NativeDimensions(panel)
	if nativeW != splash.Width || nativeH != splash.Height {
		fmt.Fprintf(errOut, "splash: panel is %dx%d but the asset is %dx%d\n", nativeW, nativeH, splash.Width, splash.Height)
		return exitRefused
	}

	ctx, cancel := context.WithTimeout(ctx, splashTimeout)
	defer cancel()

	bus, release, err := open(epaper.DefaultGPIOMapping())
	if err != nil {
		fmt.Fprintln(errOut, "splash: could not open GPIO/SPI:", err)
		return exitFailure
	}
	defer func() {
		release()
		fmt.Fprintln(out, "released SPI/GPIO; this process no longer owns the panel")
	}()

	driver := newPanelDriver(panel, bus, nativeW, nativeH)
	if err := renderSplash(ctx, driver, bitmap, out); err != nil {
		if errors.Is(err, errBusyTimeout) {
			fmt.Fprintln(errOut, "splash: panel BUSY line never went idle (is the display connected and wired per docs/waveshare-epaper-display.md?)")
		}
		fmt.Fprintln(errOut, "splash:", err)
		return exitFailure
	}
	fmt.Fprintln(out, "done: splash drawn, panel asleep")
	return exitOK
}

// runSplashCommand wires runSplash to the real hardware, the real status
// file, and SIGINT/SIGTERM.
func runSplashCommand(panel string, rotation int, force bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runSplash(ctx, bootSplashSource, panel, rotation, force, common.EpaperStatusPath, openRealBus, os.Stdout, os.Stderr)
}
