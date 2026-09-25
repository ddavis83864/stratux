// Package splash converts the owner-approved monochrome ARS splash
// artwork into the exact 1-bit bitmap the validated Waveshare 4.2in V2
// e-paper driver consumes, and validates the result.
//
// The conversion is pure integer arithmetic (no floating point, no
// third-party imaging library, no map iteration), so the same source
// image always produces a byte-identical bitmap on every platform and Go
// version. It performs only the transformations the approved-artwork
// brief permits: proportional scaling, centering with white padding,
// luminance conversion and a fixed threshold. It never dithers,
// sharpens, crops, stretches, or redraws anything.
//
// This package does no hardware I/O and embeds nothing; see the assets
// subpackage for the committed production bitmap and the splashgen
// command for regeneration. See docs/epaper-boot-splash.md.
package splash

import (
	"errors"
	"fmt"
	"image"
	"image/color"
)

// Panel geometry: the Waveshare 4.2in V2 panel's native, rotation-0
// dimensions (epaper.NativeDimensions(epaper.PanelWaveshare42V2)), which
// Driver42V2 addresses as Stride bytes per row and Height rows.
const (
	Width     = 400
	Height    = 300
	Stride    = (Width + 7) / 8
	BitmapLen = Stride * Height
)

// blackBelow is the luminance threshold on a 16-bit scale: a resampled
// pixel strictly darker than mid-gray (50%) becomes black. Because the
// resampling below is exact area-averaging, a 50% threshold keeps every
// stroke at its true proportional width instead of thickening or
// thinning line work.
const blackBelow = 0x8000

// maxSourceDim bounds source dimensions so every accumulator below fits
// comfortably in uint64.
const maxSourceDim = 16384

// Convert scales src proportionally to fit inside Width x Height,
// centers it on a white canvas, thresholds it to black/white, and
// returns the packed bitmap: BitmapLen bytes, row-major, MSB-first,
// Stride bytes per row, bit 1 = white, bit 0 = black - the exact layout
// PanelDriver.Update documents.
//
// Any alpha in src is composited over white first, so transparent
// regions become white rather than depending on undefined RGB values.
func Convert(src image.Image) ([]byte, error) {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, errors.New("splash: source image is empty")
	}
	if sw > maxSourceDim || sh > maxSourceDim {
		return nil, fmt.Errorf("splash: source image %dx%d exceeds %d px limit", sw, sh, maxSourceDim)
	}

	outW, outH := fitSize(sw, sh)
	offX, offY := (Width-outW)/2, (Height-outH)/2

	luma := make([]uint16, sw*sh)
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			luma[y*sw+x] = lumaOverWhite(src.At(b.Min.X+x, b.Min.Y+y))
		}
	}

	scaled := areaResample(luma, sw, sh, outW, outH)

	out := make([]byte, BitmapLen)
	for i := range out {
		out[i] = 0xFF // white canvas; margins stay white
	}
	for y := 0; y < outH; y++ {
		for x := 0; x < outW; x++ {
			if scaled[y*outW+x] < blackBelow {
				px := offX + x
				out[(offY+y)*Stride+px/8] &^= 0x80 >> uint(px%8)
			}
		}
	}
	return out, nil
}

// fitSize returns the largest proportional size (rounded half up) that
// fits within Width x Height without clipping.
func fitSize(sw, sh int) (w, h int) {
	if sw*Height >= sh*Width { // width-limited (or exact 4:3)
		w = Width
		h = (2*sh*Width + sw) / (2 * sw)
		if h < 1 {
			h = 1
		}
		if h > Height {
			h = Height
		}
		return w, h
	}
	h = Height
	w = (2*sw*Height + sh) / (2 * sh)
	if w < 1 {
		w = 1
	}
	if w > Width {
		w = Width
	}
	return w, h
}

// lumaOverWhite returns 16-bit luminance (ITU-R BT.601 weights, the same
// coefficients as color.GrayModel) of c composited over opaque white.
func lumaOverWhite(c color.Color) uint16 {
	r, g, b, a := c.RGBA() // alpha-premultiplied, 16-bit
	w := 0xFFFF - a
	r, g, b = r+w, g+w, b+w
	return uint16((19595*r + 38470*g + 7471*b + 1<<15) >> 16)
}

// areaResample resizes a sw x sh luminance plane to dw x dh by exact box
// (area) averaging with integer weights. Each destination pixel covers a
// fractional source footprint of sw/dw x sh/dh; source pixel i
// contributes in proportion to its overlap with that footprint. This is
// a true average, so it is order-independent and free of rounding drift.
func areaResample(src []uint16, sw, sh, dw, dh int) []uint16 {
	type tap struct{ idx, w int }
	taps := func(srcN, dstN int) [][]tap {
		// Work in units where a source pixel is dstN wide and a
		// destination pixel is srcN wide; every boundary is an integer.
		out := make([][]tap, dstN)
		for d := 0; d < dstN; d++ {
			lo, hi := d*srcN, (d+1)*srcN
			for s := lo / dstN; s*dstN < hi; s++ {
				a, b := s*dstN, (s+1)*dstN
				if a < lo {
					a = lo
				}
				if b > hi {
					b = hi
				}
				if b > a {
					out[d] = append(out[d], tap{s, b - a})
				}
			}
		}
		return out
	}
	xt, yt := taps(sw, dw), taps(sh, dh)

	// Horizontal pass: sums are scaled by sw (the total x-weight).
	mid := make([]uint64, dw*sh)
	for y := 0; y < sh; y++ {
		row := src[y*sw : (y+1)*sw]
		for x := 0; x < dw; x++ {
			var acc uint64
			for _, t := range xt[x] {
				acc += uint64(row[t.idx]) * uint64(t.w)
			}
			mid[y*dw+x] = acc
		}
	}

	// Vertical pass: total weight is sw*sh.
	total := uint64(sw) * uint64(sh)
	out := make([]uint16, dw*dh)
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var acc uint64
			for _, t := range yt[y] {
				acc += mid[t.idx*dw+x] * uint64(t.w)
			}
			out[y*dw+x] = uint16((acc + total/2) / total)
		}
	}
	return out
}

// Stats describes a bitmap's pixel populations.
type Stats struct {
	Black, White int
}

// BlackFraction is the share of pixels that are black.
func (s Stats) BlackFraction() float64 {
	return float64(s.Black) / float64(s.Black+s.White)
}

// Population bounds for a plausible ARS splash: it is predominantly
// white with substantial dark artwork. These catch a blank, inverted, or
// solid-black bitmap without pinning the exact artwork.
const (
	MinBlackFraction = 0.10
	MaxBlackFraction = 0.50
)

// Validate checks that bitmap is a well-formed, non-degenerate splash
// bitmap for the 4.2in V2 panel: exact length, both colors present in
// meaningful proportion, and not inverted. It cannot judge orientation
// or artwork fidelity - the assets package tests compare against the
// approved source for that.
func Validate(bitmap []byte) (Stats, error) {
	if len(bitmap) != BitmapLen {
		return Stats{}, fmt.Errorf("splash: bitmap is %d bytes, want %d (%dx%d, 1 bit/pixel, stride %d)",
			len(bitmap), BitmapLen, Width, Height, Stride)
	}
	var s Stats
	for _, by := range bitmap {
		for bit := 0; bit < 8; bit++ {
			if by&(0x80>>uint(bit)) != 0 {
				s.White++
			} else {
				s.Black++
			}
		}
	}
	if s.Black == 0 || s.White == 0 {
		return s, errors.New("splash: bitmap is blank (single color)")
	}
	if f := s.BlackFraction(); f < MinBlackFraction || f > MaxBlackFraction {
		return s, fmt.Errorf("splash: black fraction %.3f outside plausible range [%.2f, %.2f] (inverted or corrupt?)",
			f, MinBlackFraction, MaxBlackFraction)
	}
	return s, nil
}

// Pixel reports whether the bitmap pixel at (x, y) is black.
func Pixel(bitmap []byte, x, y int) (black bool) {
	return bitmap[y*Stride+x/8]&(0x80>>uint(x%8)) == 0
}

// Preview returns the bitmap as an opaque 1-bit (two-entry palette)
// image, for human review and PNG export. It is derived from, never a
// substitute for, the packed bitmap.
func Preview(bitmap []byte) *image.Paletted {
	img := image.NewPaletted(image.Rect(0, 0, Width, Height),
		color.Palette{color.Black, color.White})
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			if !Pixel(bitmap, x, y) {
				img.SetColorIndex(x, y, 1)
			}
		}
	}
	return img
}
