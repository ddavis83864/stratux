package assets

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stratux/stratux/epaper/splash"
)

// These tests are the automated acceptance gate for the committed final
// shutdown splash asset (issue #43): the ARS logo plus a prominent "Safe
// to remove power" message, the single retained image an orderly
// power-off leaves on the panel. They mirror the boot-splash tests above
// and additionally prove the message region actually carries ink,
// distinct from (and below) the logo, since a byte-perfect but blank or
// truncated render would otherwise pass every other check here. They do
// not replace the physical acceptance test (docs/epaper-shutdown-splash.md).

func readShutdownSource(t *testing.T) image.Image {
	t.Helper()
	f, err := os.Open(filepath.Join("source", "ars-shutdown-source.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestShutdownBitmap_DimensionsAndDepth(t *testing.T) {
	bm := ShutdownBitmap()
	if len(bm) != 15000 || len(bm) != splash.BitmapLen {
		t.Fatalf("shutdown bitmap is %d bytes, want 15000 (400x300 at 1 bit/pixel)", len(bm))
	}
}

func TestShutdownBitmap_NotBlankAndMeaningfulPopulations(t *testing.T) {
	s, err := splash.Validate(ShutdownBitmap())
	if err != nil {
		t.Fatal(err)
	}
	if s.Black < 10000 || s.White < 10000 {
		t.Errorf("populations too small: black=%d white=%d", s.Black, s.White)
	}
	t.Logf("black=%d white=%d (%.1f%% black)", s.Black, s.White, 100*s.BlackFraction())
}

func TestShutdownBitmap_RegenerationIsByteIdentical(t *testing.T) {
	src := readShutdownSource(t)
	first, err := splash.Convert(src)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := splash.Convert(src)
	if !bytes.Equal(first, second) {
		t.Fatal("two regenerations of the same source differ")
	}
	if !bytes.Equal(first, ShutdownBitmap()) {
		t.Fatal("committed ars-shutdown-400x300.bin is not byte-identical to a fresh conversion of source/ars-shutdown-source.png; " +
			"run `go run ./epaper/splash/cmd/splashgen -name ars-shutdown` (see docs/epaper-shutdown-splash.md)")
	}
}

func TestShutdownBitmap_DistinctFromBootSplash(t *testing.T) {
	if bytes.Equal(ShutdownBitmap(), Bitmap()) {
		t.Fatal("the shutdown splash bitmap is byte-identical to the boot splash: the 'Safe to remove power' message is missing (issue #43)")
	}
}

// TestShutdownBitmap_MessageBelowLogoBothCarryInk proves the two-part
// layout the source generation script produces: recognisable logo
// artwork in the upper region of the panel, and a distinct, substantial
// block of text (the safety message) in the lower region below it -
// never all the ink concentrated in one place, which would mean either
// the logo or the message failed to render. Row bands are generous
// (not tied to exact font metrics) so the test survives minor future
// copy edits.
func TestShutdownBitmap_MessageBelowLogoBothCarryInk(t *testing.T) {
	bm := ShutdownBitmap()
	blackInRows := func(fromY, toY int) int {
		n := 0
		for y := fromY; y < toY; y++ {
			for x := 0; x < splash.Width; x++ {
				if splash.Pixel(bm, x, y) {
					n++
				}
			}
		}
		return n
	}
	const (
		logoBand    = 130 // upper band: the ARS logo
		messageFrom = 170 // lower band: "SAFE TO REMOVE POWER" and its companion line
	)
	logoInk := blackInRows(0, logoBand)
	messageInk := blackInRows(messageFrom, splash.Height)
	if logoInk < 500 {
		t.Errorf("only %d black pixels in the upper %d rows: logo artwork appears missing", logoInk, logoBand)
	}
	if messageInk < 500 {
		t.Errorf("only %d black pixels in the lower rows (from %d): 'Safe to remove power' message appears missing", messageInk, messageFrom)
	}
	// A gap band between the two must stay comparatively light, proving
	// they are visually separated (not one merged blob of ink).
	gapInk := blackInRows(logoBand, messageFrom)
	gapRows := messageFrom - logoBand
	if gapRows > 0 && gapInk > (messageFrom-logoBand)*splash.Width/4 {
		t.Errorf("gap band (rows %d-%d) is %d%% black: logo and message are not visually separated",
			logoBand, messageFrom, gapInk*100/(gapRows*splash.Width))
	}
}

func TestShutdownPreviewPNG_IdenticalPixelsAndNoAlpha(t *testing.T) {
	f, err := os.Open("ars-shutdown-400x300.preview.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := img.(*image.Paletted)
	if !ok {
		t.Fatalf("preview decoded as %T, want *image.Paletted (1-bit)", img)
	}
	if len(p.Palette) != 2 {
		t.Errorf("preview palette has %d entries, want 2", len(p.Palette))
	}
	if !p.Opaque() {
		t.Error("preview has non-opaque pixels")
	}
	if p.Bounds() != image.Rect(0, 0, splash.Width, splash.Height) {
		t.Errorf("preview bounds %v, want %dx%d", p.Bounds(), splash.Width, splash.Height)
	}
	bm := ShutdownBitmap()
	for y := 0; y < splash.Height; y++ {
		for x := 0; x < splash.Width; x++ {
			wantBlack := splash.Pixel(bm, x, y)
			r, _, _, _ := p.At(x, y).RGBA()
			gotBlack := r == 0
			if gotBlack != wantBlack {
				t.Fatalf("preview pixel (%d,%d) does not match the bitmap (black=%v, want %v)", x, y, gotBlack, wantBlack)
			}
		}
	}
}
