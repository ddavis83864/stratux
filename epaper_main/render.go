package main

import (
	"image"
	"image/draw"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/stratux/stratux/epaper"
)

// Render draws lines onto a width x height 1-bit (black/white) canvas and
// returns it packed MSB-first, row-major - exactly the RAM format
// Driver.Update expects. Uses only golang.org/x/image (already an
// indirect dependency of this repo via gonum's plotting support - see
// go.mod), so this introduces no new third-party dependency for text
// rendering.
func Render(lines []epaper.Line, width, height int) []byte {
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

	return packMonochrome(img)
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
