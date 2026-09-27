package main

// Drawing primitives for the 1-bit operating dashboard: exact integer
// shapes (rules, rounded borders), supersampled vector icons that are
// thresholded to one bit, and condensed bold text rendered from the Go
// fonts (BSD-licensed, already part of golang.org/x/image - no new
// dependency and no font file to ship). Everything here is deterministic
// pure Go (no cgo, no system fonts), so a frame is bit-for-bit
// reproducible on the build machine and on the Raspberry Pi.

import (
	"image"
	"image/color"
	"math"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomedium"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	inkBlack = 0
	inkWhite = 255
)

// ---- fonts ----

type faceKey struct {
	bold bool
	px   float64
}

var (
	fontOnce          sync.Once
	fontBold, fontMed *opentype.Font
	fontErr           error
	faceMu            sync.Mutex
	faceCache         = map[faceKey]font.Face{}
)

func loadFonts() error {
	fontOnce.Do(func() {
		fontBold, fontErr = opentype.Parse(gobold.TTF)
		if fontErr != nil {
			return
		}
		fontMed, fontErr = opentype.Parse(gomedium.TTF)
	})
	return fontErr
}

func faceFor(bold bool, px float64) font.Face {
	faceMu.Lock()
	defer faceMu.Unlock()
	k := faceKey{bold, px}
	if f, ok := faceCache[k]; ok {
		return f
	}
	if err := loadFonts(); err != nil {
		return nil
	}
	src := fontMed
	if bold {
		src = fontBold
	}
	f, err := opentype.NewFace(src, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil
	}
	faceCache[k] = f
	return f
}

// textStyle selects a face and the horizontal squeeze that gives the
// Go fonts the condensed look of the reference design.
type textStyle struct {
	bold bool
	px   float64
	sx   float64 // horizontal scale, 1.0 = natural width
	// cover is the ink coverage (0-255) at or above which an output pixel
	// becomes black. Lower = bolder. Small regular text uses a lower value
	// so thin strokes do not drop out at one bit.
	cover uint8
}

// textWidth is the exact drawn width in pixels of s in st.
func textWidth(st textStyle, s string) int {
	f := faceFor(st.bold, st.px)
	if f == nil {
		return 0
	}
	adv := font.MeasureString(f, s)
	return int(math.Ceil(float64(adv.Ceil()) * st.sx))
}

// fitStyle returns the largest style not exceeding maxW: it first
// squeezes (down to minSX), then shrinks the type (down to minPX). ok is
// false if even the smallest allowed style is too wide - callers then
// shorten the text; nothing is ever drawn clipped.
func fitStyle(st textStyle, s string, maxW int, minSX, minPX float64) (textStyle, bool) {
	for px := st.px; px >= minPX; px -= 1 {
		for sx := st.sx; sx >= minSX-1e-9; sx -= 0.02 {
			c := st
			c.px, c.sx = px, sx
			if textWidth(c, s) <= maxW {
				return c, true
			}
		}
	}
	c := st
	c.px, c.sx = minPX, minSX
	return c, false
}

// drawText draws s with its baseline at baseY, left edge at x, in the
// given ink (inkBlack or inkWhite). It returns the rectangle of pixels it
// touched (empty if nothing was drawn).
func drawText(dst *image.Gray, st textStyle, s string, x, baseY int, ink uint8) image.Rectangle {
	f := faceFor(st.bold, st.px)
	if f == nil || s == "" {
		return image.Rectangle{}
	}
	m := f.Metrics()
	asc, desc := m.Ascent.Ceil()+2, m.Descent.Ceil()+2
	adv := font.MeasureString(f, s).Ceil()
	tmp := image.NewGray(image.Rect(0, 0, adv+6, asc+desc))
	for i := range tmp.Pix {
		tmp.Pix[i] = 255
	}
	d := &font.Drawer{Dst: tmp, Src: image.NewUniform(color.Gray{0}), Face: f,
		Dot: fixed.Point26_6{X: fixed.I(3), Y: fixed.I(asc)}}
	d.DrawString(s)

	outW := int(math.Ceil(float64(adv+6) * st.sx))
	var bounds image.Rectangle
	for ox := 0; ox < outW; ox++ {
		// source span of this output column
		s0 := float64(ox) / st.sx
		s1 := float64(ox+1) / st.sx
		for oy := 0; oy < tmp.Bounds().Dy(); oy++ {
			var sum, wsum float64
			for sxp := int(math.Floor(s0)); sxp < int(math.Ceil(s1)) && sxp < tmp.Bounds().Dx(); sxp++ {
				lo, hi := math.Max(s0, float64(sxp)), math.Min(s1, float64(sxp+1))
				if hi <= lo {
					continue
				}
				w := hi - lo
				sum += w * float64(255-tmp.GrayAt(sxp, oy).Y)
				wsum += w
			}
			if wsum == 0 {
				continue
			}
			if uint8(sum/wsum+0.5) >= st.cover {
				px, py := x-3+ox, baseY-asc+oy
				if image.Pt(px, py).In(dst.Bounds()) {
					dst.SetGray(px, py, color.Gray{ink})
					bounds = bounds.Union(image.Rect(px, py, px+1, py+1))
				}
			}
		}
	}
	return bounds
}

// ---- integer shapes ----

func fillRect(dst *image.Gray, r image.Rectangle, ink uint8) {
	r = r.Intersect(dst.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			dst.SetGray(x, y, color.Gray{ink})
		}
	}
}

// inRoundRect reports whether the pixel centre (x+.5, y+.5) is inside the
// rounded rectangle [x0,x1)x[y0,y1) with corner radius r.
func inRoundRect(x, y int, x0, y0, x1, y1 int, r float64) bool {
	px, py := float64(x)+0.5, float64(y)+0.5
	if px < float64(x0) || px > float64(x1) || py < float64(y0) || py > float64(y1) {
		return false
	}
	cx := math.Min(math.Max(px, float64(x0)+r), float64(x1)-r)
	cy := math.Min(math.Max(py, float64(y0)+r), float64(y1)-r)
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= r*r
}

// strokeRoundRect draws a rounded-rectangle border of the given thickness
// inside [x0,x1)x[y0,y1).
func strokeRoundRect(dst *image.Gray, x0, y0, x1, y1 int, r float64, thick int, ink uint8) {
	t := float64(thick)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if inRoundRect(x, y, x0, y0, x1, y1, r) &&
				!inRoundRect(x, y, x0+thick, y0+thick, x1-thick, y1-thick, math.Max(r-t, 0)) {
				dst.SetGray(x, y, color.Gray{ink})
			}
		}
	}
}

func fillRoundRect(dst *image.Gray, x0, y0, x1, y1 int, r float64, ink uint8) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if inRoundRect(x, y, x0, y0, x1, y1, r) {
				dst.SetGray(x, y, color.Gray{ink})
			}
		}
	}
}

// ---- supersampled vector icons ----

// vcanvas is a square hi-res canvas for one icon. Coordinates are in
// output pixels (float), so a shape is described in icon-pixel units and
// supersampled by ss on each axis.
type vcanvas struct {
	n   int // output size in pixels
	ss  int
	pix []uint8 // (n*ss) x (n*ss), 255 = white
}

func newVCanvas(n, ss int, bg uint8) *vcanvas {
	c := &vcanvas{n: n, ss: ss, pix: make([]uint8, n*ss*n*ss)}
	for i := range c.pix {
		c.pix[i] = bg
	}
	return c
}

func (c *vcanvas) side() int { return c.n * c.ss }

func (c *vcanvas) fillPoly(pts [][2]float64, ink uint8) {
	sd := float64(c.ss)
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		minY, maxY = math.Min(minY, p[1]*sd), math.Max(maxY, p[1]*sd)
	}
	for y := int(math.Floor(minY)); y <= int(math.Ceil(maxY)); y++ {
		if y < 0 || y >= c.side() {
			continue
		}
		fy := float64(y) + 0.5
		var xs []float64
		for i := range pts {
			a, b := pts[i], pts[(i+1)%len(pts)]
			ay, by := a[1]*sd, b[1]*sd
			if (ay <= fy && by > fy) || (by <= fy && ay > fy) {
				t := (fy - ay) / (by - ay)
				xs = append(xs, (a[0]+t*(b[0]-a[0]))*sd)
			}
		}
		for i := 0; i < len(xs); i++ { // tiny insertion sort
			for j := i + 1; j < len(xs); j++ {
				if xs[j] < xs[i] {
					xs[i], xs[j] = xs[j], xs[i]
				}
			}
		}
		for i := 0; i+1 < len(xs); i += 2 {
			for x := int(math.Ceil(xs[i] - 0.5)); float64(x)+0.5 < xs[i+1]; x++ {
				if x >= 0 && x < c.side() {
					c.pix[y*c.side()+x] = ink
				}
			}
		}
	}
}

func (c *vcanvas) circle(cx, cy, r float64, ink uint8) {
	sd := float64(c.ss)
	x0, x1 := int(math.Floor((cx-r)*sd)), int(math.Ceil((cx+r)*sd))
	y0, y1 := int(math.Floor((cy-r)*sd)), int(math.Ceil((cy+r)*sd))
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			if x < 0 || y < 0 || x >= c.side() || y >= c.side() {
				continue
			}
			dx, dy := (float64(x)+0.5)/sd-cx, (float64(y)+0.5)/sd-cy
			if dx*dx+dy*dy <= r*r {
				c.pix[y*c.side()+x] = ink
			}
		}
	}
}

// line draws a round-capped stroke of width w.
func (c *vcanvas) line(x0, y0, x1, y1, w float64, ink uint8) {
	dx, dy := x1-x0, y1-y0
	l := math.Hypot(dx, dy)
	if l == 0 {
		c.circle(x0, y0, w/2, ink)
		return
	}
	nx, ny := -dy/l*w/2, dx/l*w/2
	c.fillPoly([][2]float64{{x0 + nx, y0 + ny}, {x1 + nx, y1 + ny}, {x1 - nx, y1 - ny}, {x0 - nx, y0 - ny}}, ink)
	c.circle(x0, y0, w/2, ink)
	c.circle(x1, y1, w/2, ink)
}

// arc strokes an arc of radius r about (cx,cy) from a0 to a1 degrees
// (0 = +x, counter-clockwise on screen with y down means increasing angle
// goes clockwise visually; callers pass whichever range they need).
func (c *vcanvas) arc(cx, cy, r, a0, a1, w float64, ink uint8) {
	steps := int(math.Abs(a1-a0)/4) + 2
	px, py := cx+r*math.Cos(a0*math.Pi/180), cy+r*math.Sin(a0*math.Pi/180)
	for i := 1; i <= steps; i++ {
		a := (a0 + (a1-a0)*float64(i)/float64(steps)) * math.Pi / 180
		x, y := cx+r*math.Cos(a), cy+r*math.Sin(a)
		c.line(px, py, x, y, w, ink)
		px, py = x, y
	}
}

// blit downsamples the canvas (box filter) and writes it, thresholded to
// one bit, onto dst with its top-left at (x, y). invert swaps black and
// white so the same icon can sit on a black banner.
func (c *vcanvas) blit(dst *image.Gray, x, y int, invert bool) {
	ss2 := c.ss * c.ss
	for oy := 0; oy < c.n; oy++ {
		for ox := 0; ox < c.n; ox++ {
			sum := 0
			for j := 0; j < c.ss; j++ {
				for i := 0; i < c.ss; i++ {
					sum += int(c.pix[(oy*c.ss+j)*c.side()+ox*c.ss+i])
				}
			}
			v := uint8(inkWhite)
			if sum/ss2 < 128 {
				v = inkBlack
			}
			if invert {
				v = 255 - v
			}
			if image.Pt(x+ox, y+oy).In(dst.Bounds()) {
				dst.SetGray(x+ox, y+oy, color.Gray{v})
			}
		}
	}
}

func rot(pts [][2]float64, cx, cy, deg float64) [][2]float64 {
	s, c := math.Sin(deg*math.Pi/180), math.Cos(deg*math.Pi/180)
	out := make([][2]float64, len(pts))
	for i, p := range pts {
		dx, dy := p[0]-cx, p[1]-cy
		out[i] = [2]float64{cx + dx*c - dy*s, cy + dx*s + dy*c}
	}
	return out
}
