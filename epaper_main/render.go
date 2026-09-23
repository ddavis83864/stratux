package main

import (
	"image"
	"image/draw"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/stratux/stratux/epaper"
)

// Render draws lines onto a width x height 1-bit (black/white) *logical*
// canvas (width/height as the viewer will read them once the configured
// rotation is applied - see epaper.Dimensions), rotates that canvas onto
// the panel's own physically-fixed native orientation (see
// epaper.NativeDimensions), and returns the result packed MSB-first,
// row-major - exactly the RAM format PanelDriver.Update expects, always
// sized to the panel's native dimensions regardless of rotation. Uses
// only golang.org/x/image (already an indirect dependency of this repo
// via gonum's plotting support - see go.mod), so this introduces no new
// third-party dependency for text rendering.
//
// A real hardware-validation finding: before this rotation step existed,
// EpaperRotation was validated and persisted correctly, and a
// configuration change was correctly detected and caused a real
// re-initialization and refresh (confirmed by physical flashing and
// rising refresh counters), but the displayed content itself never
// actually rotated - text was always drawn directly onto the native
// canvas in the same fixed screen-space orientation, regardless of
// rotation. This was not a 4.2in-panel-specific bug: the 3.7in panel's
// own render path shares this exact function and was equally affected
// at every non-zero rotation value.
func Render(lines []epaper.Line, width, height, rotation int) []byte {
	img := image.NewGray(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)

	face := basicfont.Face7x13
	lineHeight := face.Metrics().Height.Ceil() + 4
	d := &font.Drawer{
		Dst:  img,
		Src:  image.Black,
		Face: face,
	}
	y := lineHeight
	for _, l := range lines {
		if y > height {
			break // never draw past the panel's own bounds
		}
		d.Dot = fixed.Point26_6{X: fixed.I(4), Y: fixed.I(y)}
		d.DrawString(l.Text)
		y += lineHeight
	}

	return packMonochrome(rotateImage(img, rotation))
}

// rotateImage maps a logical (reading-orientation) image onto the
// panel's native pixel grid, rotated clockwise by rotation degrees -
// matching Config.Rotation's own "degrees, clockwise" documented
// convention (epaper/config.go). rotation must already be one of 0, 90,
// 180, 270 (epaper.Normalize's own validRotations); any other value is
// treated as 0 (identity), never panics or silently drops content.
//
// 0: identity, same dimensions - preserves every existing installation's
// pixel-for-pixel output exactly (no installation has ever configured a
// non-zero rotation successfully before this fix, since it was a no-op,
// so there is no prior non-zero-rotation behavior to preserve).
//
// 180: output has the same dimensions as the input; every pixel maps to
// its point-symmetric opposite (out[x,y] = in[W-1-x, H-1-y]) - this is
// the exact, physically-confirmed defect: a 400x300 (or any WxH) canvas
// stays WxH at 180 degrees, so getting this transform right is the
// entire fix for the reported symptom.
//
// 90/270: output dimensions are transposed (in's H becomes out's W and
// vice versa) - standard image-rotation formulas, each independently
// derived and verified by this file's own tests against a small,
// explicit, asymmetric example rather than assumed from the 180-degree
// case. Physical hardware validation of 90/270 has not been performed as
// of this fix - see docs/waveshare-epaper-display.md.
func rotateImage(img *image.Gray, rotation int) *image.Gray {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	switch rotation {
	case 180:
		out := image.NewGray(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out.SetGray(x, y, img.GrayAt(w-1-x, h-1-y))
			}
		}
		return out
	case 90:
		out := image.NewGray(image.Rect(0, 0, h, w))
		for y := 0; y < w; y++ {
			for x := 0; x < h; x++ {
				out.SetGray(x, y, img.GrayAt(y, h-1-x))
			}
		}
		return out
	case 270:
		out := image.NewGray(image.Rect(0, 0, h, w))
		for y := 0; y < w; y++ {
			for x := 0; x < h; x++ {
				out.SetGray(x, y, img.GrayAt(w-1-y, x))
			}
		}
		return out
	default: // 0, or any unrecognized value: identity
		return img
	}
}

// packMonochrome converts a grayscale image to the SSD1677's 1-bit RAM
// format: one bit per pixel, MSB first, row-major, each row padded to a
// whole byte - matching Driver.Update's own stride calculation exactly.
// A pixel darker than the midpoint is "black" (bit clear); this matches
// the controller's own documented BW-RAM convention where a 0 bit paints
// black.
func packMonochrome(img *image.Gray) []byte {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	stride := (width + 7) / 8
	out := make([]byte, stride*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			g := img.GrayAt(x, y)
			black := g.Y < 128
			if !black {
				// White pixel: set the bit (1 = white in this
				// controller's BW-RAM convention).
				out[y*stride+x/8] |= 0x80 >> uint(x%8)
			}
		}
	}
	return out
}

// blankMonochrome returns an all-white bitmap of the given dimensions -
// used only if rendering itself somehow produces a zero-length result,
// as a last-resort safe fallback that never sends a malformed buffer to
// Driver.Update.
func blankMonochrome(width, height int) []byte {
	stride := (width + 7) / 8
	out := make([]byte, stride*height)
	for i := range out {
		out[i] = 0xFF
	}
	return out
}
