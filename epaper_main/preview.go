package main

// preview.go: the one-shot frame previewer behind `epaperd -preview-png`.
//
// A manual, run-to-completion acceptance aid, structured exactly like the
// splash command (splash.go): it takes a 400 x 300 image - in practice a
// dashboard preview rendered by the tests (epaper_main/testdata/dashboard) -
// and shows it on the panel so an owner can review states that cannot be
// produced safely on a live unit (warning, degraded, fault, no-data). It
// touches only the panel: it reads nothing from and writes nothing to the
// Stratux daemon, GDL90 or any EFB, and it refuses to run while the epaperd
// service owns the panel (unless forced), just like -splash. Nothing calls
// it at boot or from any unit.

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/png" // registers the PNG decoder for image.Decode
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/epaper"
)

// previewBitmap decodes r (any image, 400 x 300) and packs it as the panel's
// 1-bit RAM format: darker than the midpoint is black. rotation is 0 or 180.
func previewBitmap(r io.Reader, rotation int) ([]byte, error) {
	src, _, err := image.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("could not decode the image: %w", err)
	}
	b := src.Bounds()
	if b.Dx() != epaper.Panel42V2Width || b.Dy() != epaper.Panel42V2Height {
		return nil, fmt.Errorf("image is %dx%d, want %dx%d", b.Dx(), b.Dy(), epaper.Panel42V2Width, epaper.Panel42V2Height)
	}
	gray := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(gray, gray.Bounds(), src, b.Min, draw.Src)
	switch rotation {
	case 0:
	case 180:
		gray = rotateImage(gray, 180)
	default:
		return nil, fmt.Errorf("rotation %d is not supported for previews (only 0 and 180)", rotation)
	}
	return packMonochrome(gray), nil
}

// runPreview is the whole `-preview-png` command; it returns a process exit
// code. Every check that can refuse runs before any hardware is opened.
func runPreview(ctx context.Context, path, panel string, rotation int, force bool, statusPath string, open busOpener, out, errOut io.Writer) int {
	if panel != epaper.PanelWaveshare42V2 {
		fmt.Fprintf(errOut, "preview: only %q is supported, not %q\n", epaper.PanelWaveshare42V2, panel)
		return exitRefused
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(errOut, "preview:", err)
		return exitRefused
	}
	bitmap, err := previewBitmap(f, rotation)
	f.Close()
	if err != nil {
		fmt.Fprintln(errOut, "preview:", err)
		return exitRefused
	}
	if !force {
		if err := guardOwnership(statusPath, time.Now()); err != nil {
			fmt.Fprintln(errOut, "preview:", err)
			return exitRefused
		}
	}

	ctx, cancel := context.WithTimeout(ctx, splashTimeout)
	defer cancel()
	bus, release, err := open(epaper.DefaultGPIOMapping())
	if err != nil {
		fmt.Fprintln(errOut, "preview: could not open GPIO/SPI:", err)
		return exitFailure
	}
	defer func() {
		release()
		fmt.Fprintln(out, "released SPI/GPIO; this process no longer owns the panel")
	}()

	nativeW, nativeH := epaper.NativeDimensions(panel)
	driver := newPanelDriver(panel, bus, nativeW, nativeH)
	if err := renderFrame(ctx, driver, bitmap, out); err != nil {
		if errors.Is(err, errBusyTimeout) {
			fmt.Fprintln(errOut, "preview: panel BUSY line never went idle")
		}
		fmt.Fprintln(errOut, "preview:", err)
		return exitFailure
	}
	fmt.Fprintln(out, "done: frame drawn, panel asleep")
	return exitOK
}

// renderFrame: Init -> Clear -> Update(full) -> Sleep, like renderSplash, and
// like it always attempts Sleep after a successful Init.
func renderFrame(ctx context.Context, driver PanelDriver, bitmap []byte, out io.Writer) (err error) {
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
	fmt.Fprintln(out, "drawing frame (full refresh)...")
	if err := driver.Update(ctx, bitmap, true); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

func runPreviewCommand(path, panel string, rotation int, force bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runPreview(ctx, path, panel, rotation, force, common.EpaperStatusPath, openRealBus, os.Stdout, os.Stderr)
}
