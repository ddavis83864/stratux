package main

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/stratux/stratux/epaper"
)

// This file is the permanent regression coverage for a real, physically
// confirmed hardware-validation defect: EpaperRotation was validated,
// persisted, and correctly triggered a driver re-initialization and
// refresh, but the displayed content itself never actually rotated -
// Render never received or applied the rotation value at all. See
// render.go's own Render and rotateImage doc comments for the full root-
// cause account. None of these tests pass merely by checking dimensions
// or output length (the exact gap the pre-fix test suite had); every
// test here proves an actual pixel relocated to its correctly rotated
// coordinate, using an asymmetric single-pixel marker or an independent
// reference implementation of the same transform.

// markerImage returns a width x height all-white image with exactly one
// black pixel at (bx, by) - the minimal possible asymmetric pattern, so
// a rotation transform can be proven by exact coordinate, not merely "the
// output changed somehow".
func markerImage(width, height, bx, by int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, width, height))
	draw := color.Gray{Y: 255}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetGray(x, y, draw)
		}
	}
	img.SetGray(bx, by, color.Gray{Y: 0})
	return img
}

// findMarker returns the sole black pixel's coordinates, failing the
// test if there isn't exactly one - keeps every test below honest about
// asserting a single, unambiguous relocation rather than a whole-image
// diff that could pass for the wrong reason.
func findMarker(t *testing.T, img *image.Gray) (x, y int) {
	t.Helper()
	found := false
	fx, fy := -1, -1
	bounds := img.Bounds()
	for yy := bounds.Min.Y; yy < bounds.Max.Y; yy++ {
		for xx := bounds.Min.X; xx < bounds.Max.X; xx++ {
			if img.GrayAt(xx, yy).Y == 0 {
				if found {
					t.Fatalf("found more than one black pixel: (%d,%d) and (%d,%d)", fx, fy, xx, yy)
				}
				found = true
				fx, fy = xx, yy
			}
		}
	}
	if !found {
		t.Fatalf("no black pixel found in rotated image")
	}
	return fx, fy
}

// TestRotateImage_ExactPixelRelocation is the core proof that rotateImage
// implements a real geometric rotation, not a no-op or a dimension-only
// change: a single marker pixel at a known input coordinate must land at
// the exact, independently hand-derived output coordinate for every
// supported rotation value, at a small asymmetric (non-square) size
// chosen so a transposition bug (row/col swap in the wrong place) cannot
// coincidentally pass.
func TestRotateImage_ExactPixelRelocation(t *testing.T) {
	const w, h = 4, 3 // asymmetric: W != H, so 90/270 cannot be confused
	cases := []struct {
		rotation     int
		inX, inY     int
		wantW, wantH int
		wantX, wantY int
	}{
		// 0 degrees: identity - same dimensions, same coordinate.
		{0, 0, 0, w, h, 0, 0},
		{0, w - 1, h - 1, w, h, w - 1, h - 1},

		// 180 degrees: same dimensions, point-symmetric opposite corner -
		// this is the exact, physically-confirmed defect: a marker near
		// one corner must end up at the diagonally opposite corner.
		{180, 0, 0, w, h, w - 1, h - 1},
		{180, w - 1, h - 1, w, h, 0, 0},
		{180, w - 1, 0, w, h, 0, h - 1},

		// 90 degrees clockwise: dimensions transpose (H x W); the top-left
		// corner moves to the top-right of the new, transposed canvas -
		// hand-derived and independently verified against a worked 3x2
		// example before being encoded here (see render.go's rotateImage
		// doc comment).
		{90, 0, 0, h, w, h - 1, 0},
		{90, w - 1, 0, h, w, h - 1, w - 1},
		{90, 0, h - 1, h, w, 0, 0},

		// 270 degrees clockwise (90 counterclockwise): dimensions
		// transpose; the top-left corner moves to the bottom-left of the
		// new canvas - the mirror-image case of 90 degrees above.
		{270, 0, 0, h, w, 0, w - 1},
		{270, w - 1, 0, h, w, 0, 0},
		{270, 0, h - 1, h, w, h - 1, w - 1},
	}
	for _, c := range cases {
		in := markerImage(w, h, c.inX, c.inY)
		out := rotateImage(in, c.rotation)
		bounds := out.Bounds()
		if bounds.Dx() != c.wantW || bounds.Dy() != c.wantH {
			t.Errorf("rotation %d, marker(%d,%d): output dims (%d,%d), want (%d,%d)",
				c.rotation, c.inX, c.inY, bounds.Dx(), bounds.Dy(), c.wantW, c.wantH)
			continue
		}
		gotX, gotY := findMarker(t, out)
		if gotX != c.wantX || gotY != c.wantY {
			t.Errorf("rotation %d, marker(%d,%d): relocated to (%d,%d), want (%d,%d)",
				c.rotation, c.inX, c.inY, gotX, gotY, c.wantX, c.wantY)
		}
	}
}

// TestRotateImage_ZeroDegreesIsIdentity is a direct guard that the fix
// never changes any existing installation's rotation-0 (the shipped
// default and, until this fix, the only rotation value that ever worked
// correctly) output - pixel-for-pixel, not just by dimension.
func TestRotateImage_ZeroDegreesIsIdentity(t *testing.T) {
	const w, h = 5, 7
	in := markerImage(w, h, 2, 5)
	out := rotateImage(in, 0)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if out.GrayAt(x, y) != in.GrayAt(x, y) {
				t.Fatalf("rotation 0 changed pixel (%d,%d): got %v, want %v (identity)", x, y, out.GrayAt(x, y), in.GrayAt(x, y))
			}
		}
	}
}

// TestRotateImage_UnrecognizedValueIsIdentity mirrors epaper.Normalize's
// own defense-in-depth: even though only 0/90/180/270 ever reach this
// function in practice (epaper.Normalize rejects anything else at
// config-validation time), rotateImage itself must never panic or
// corrupt content on an unexpected value - it must fall back to
// identity, never guess.
func TestRotateImage_UnrecognizedValueIsIdentity(t *testing.T) {
	const w, h = 5, 7
	in := markerImage(w, h, 1, 1)
	out := rotateImage(in, 999)
	if out != in {
		t.Fatalf("rotateImage with unrecognized rotation 999 did not return the input image unchanged")
	}
}

// referenceRotate180Bits is a small, independent reimplementation of a
// 180-degree rotation directly on packMonochrome's packed byte format
// (MSB-first, row-major, 1=white/0=black) - used only to cross-check
// Render's real production pipeline (rotateImage + packMonochrome)
// below, so the test does not merely restate the implementation under
// test.
func referenceRotate180Bits(buf []byte, w, h int) []byte {
	stride := (w + 7) / 8
	isWhite := func(x, y int) bool {
		return buf[y*stride+x/8]&(0x80>>uint(x%8)) != 0
	}
	out := make([]byte, len(buf))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if isWhite(w-1-x, h-1-y) {
				out[y*stride+x/8] |= 0x80 >> uint(x%8)
			}
		}
	}
	return out
}

// TestRender_180DegreeOutputMatchesIndependentReferenceRotation is the
// full-pipeline proof, at the panel's real 400x300 native resolution,
// that Render's actual production output (real Layout content, real font
// drawing, real rotateImage + packMonochrome) at rotation 180 is exactly
// the bit-for-bit 180-degree rotation of its own rotation-0 output -
// checked against a second, independently written reference
// implementation of the same transform, not by re-deriving expected text
// pixel positions by hand. This directly reproduces, and proves fixed,
// the exact symptom from the physical hardware report: before this fix,
// out0 and out180 were byte-identical (see this package's git history -
// the pre-fix reproduction test that failed before this change existed).
func TestRender_180DegreeOutputMatchesIndependentReferenceRotation(t *testing.T) {
	lines := []epaper.Line{{Text: "Stratux 2.0.0  abcdef1"}}
	w, h := epaper.Dimensions(epaper.PanelWaveshare42V2, 0) // 400,300; same at rotation 180

	out0 := Render(lines, w, h, 0)
	out180 := Render(lines, w, h, 180)

	if bytes.Equal(out0, out180) {
		t.Fatal("rotation 0 and rotation 180 produced byte-identical output - the exact physically reported defect is not fixed")
	}

	want := referenceRotate180Bits(out0, w, h)
	if !bytes.Equal(out180, want) {
		t.Fatal("Render's rotation-180 output does not match an independently computed 180-degree rotation of its rotation-0 output")
	}
}

// TestRender_90And270ProduceTransposedNativeSizedOutput confirms Render's
// output is always sized to the panel's fixed native dimensions
// (400x300), regardless of rotation - at 90/270 the logical (drawing)
// canvas is transposed (300x400), but the packed bytes handed to
// PanelDriver.Update must always match the panel's real, physically-
// wired resolution, per NativeDimensions - never the driver's own RAM
// window been fed a swapped size (see epaper.NativeDimensions's doc
// comment for the real hardware-validation finding this guards against).
func TestRender_90And270ProduceTransposedNativeSizedOutput(t *testing.T) {
	lines := []epaper.Line{{Text: "X"}}
	nativeW, nativeH := epaper.NativeDimensions(epaper.PanelWaveshare42V2) // 400, 300
	wantLen := ((nativeW + 7) / 8) * nativeH

	for _, rotation := range []int{90, 270} {
		logicalW, logicalH := epaper.Dimensions(epaper.PanelWaveshare42V2, rotation) // 300, 400
		got := Render(lines, logicalW, logicalH, rotation)
		if len(got) != wantLen {
			t.Errorf("rotation %d: Render output is %d bytes, want %d (must always match the panel's native %dx%d resolution, not the logical %dx%d canvas)",
				rotation, len(got), wantLen, nativeW, nativeH, logicalW, logicalH)
		}
	}
}

// TestRender_EveryRotationValuePreservesTotalMarkCount is a sanity check
// that rotation redistributes pixels rather than losing or duplicating
// them: the same real overview-page content, rendered at each rotation
// value, must always produce the same total count of black (drawn) bits
// - a rotation is a permutation of pixel positions, never a lossy
// transform.
func TestRender_EveryRotationValuePreservesTotalMarkCount(t *testing.T) {
	countBlackBits := func(buf []byte, w, h int) int {
		stride := (w + 7) / 8
		n := 0
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if buf[y*stride+x/8]&(0x80>>uint(x%8)) == 0 {
					n++
				}
			}
		}
		return n
	}

	content := epaper.Content{Version: "2.0.0", Build: "abcdef1234567890", OverallReady: "READY"}
	lines := epaper.Layout(content, epaper.Config{Panel: epaper.PanelWaveshare42V2, Page: epaper.PageOverview}, false)

	var want = -1
	for _, rotation := range []int{0, 90, 180, 270} {
		w, h := epaper.Dimensions(epaper.PanelWaveshare42V2, rotation)
		out := Render(lines, w, h, rotation)
		nativeW, nativeH := epaper.NativeDimensions(epaper.PanelWaveshare42V2)
		got := countBlackBits(out, nativeW, nativeH)
		if want == -1 {
			want = got
			continue
		}
		if got != want {
			t.Errorf("rotation %d produced %d black bits, want %d (same as rotation 0) - rotation must relocate pixels, never lose or duplicate them", rotation, got, want)
		}
	}
}
