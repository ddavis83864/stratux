package main

import "image"

// One-bit pictograms for the operating dashboard, drawn as supersampled
// vector shapes (4x) and thresholded, so they stay solid and recognizable
// at their small native sizes with no shading or gradients.

const iconSS = 4

// iconBroadcast: filled disc with a white antenna mast and radiating arcs
// (the overall "receiver" banner icon). n is the diameter.
func iconBroadcast(n int) *vcanvas {
	c := newVCanvas(n, iconSS, inkWhite)
	f := float64(n)
	c.circle(f/2, f/2, f/2, inkBlack)
	cx, top := f/2, f*0.38
	c.circle(cx, top, f*0.07, inkWhite)
	c.fillPoly([][2]float64{{cx - f*0.035, top + f*0.02}, {cx + f*0.035, top + f*0.02}, {cx + f*0.06, f * 0.80}, {cx - f*0.06, f * 0.80}}, inkWhite)
	for _, r := range []float64{f * 0.17, f * 0.28} {
		c.arc(cx, top, r, -50, 50, f*0.05, inkWhite)
		c.arc(cx, top, r, 130, 230, f*0.05, inkWhite)
	}
	return c
}

// iconWarn: outlined triangle with an exclamation mark.
func iconWarn(n int, thick float64) *vcanvas {
	c := newVCanvas(n, iconSS, inkWhite)
	f := float64(n)
	tri := [][2]float64{{f / 2, f * 0.08}, {f * 0.95, f * 0.90}, {f * 0.05, f * 0.90}}
	c.fillPoly(tri, inkBlack)
	in := f * thick
	c.fillPoly([][2]float64{{f / 2, f*0.08 + in*1.9}, {f*0.95 - in*1.7, f*0.90 - in*0.9}, {f*0.05 + in*1.7, f*0.90 - in*0.9}}, inkWhite)
	c.line(f/2, f*0.38, f/2, f*0.62, f*0.09, inkBlack)
	c.circle(f/2, f*0.75, f*0.05, inkBlack)
	return c
}

// iconFault: filled disc with a white X.
func iconFault(n int) *vcanvas {
	c := newVCanvas(n, iconSS, inkWhite)
	f := float64(n)
	c.circle(f/2, f/2, f/2, inkBlack)
	c.line(f*0.30, f*0.30, f*0.70, f*0.70, f*0.11, inkWhite)
	c.line(f*0.70, f*0.30, f*0.30, f*0.70, f*0.11, inkWhite)
	return c
}

// iconUnknown: ring with a question mark drawn from strokes.
func iconUnknown(n int) *vcanvas {
	c := newVCanvas(n, iconSS, inkWhite)
	f := float64(n)
	c.circle(f/2, f/2, f/2, inkBlack)
	c.circle(f/2, f/2, f/2-f*0.09, inkWhite)
	c.arc(f/2, f*0.40, f*0.14, 180, 400, f*0.08, inkBlack)
	c.line(f/2+f*0.10, f*0.50, f/2, f*0.60, f*0.08, inkBlack)
	c.line(f/2, f*0.60, f/2, f*0.66, f*0.08, inkBlack)
	c.circle(f/2, f*0.77, f*0.05, inkBlack)
	return c
}

// iconHourglass: startup.
func iconHourglass(n int) *vcanvas {
	c := newVCanvas(n, iconSS, inkWhite)
	f := float64(n)
	c.circle(f/2, f/2, f/2, inkBlack)
	c.fillPoly([][2]float64{{f * 0.30, f * 0.24}, {f * 0.70, f * 0.24}, {f * 0.53, f * 0.50}, {f * 0.70, f * 0.76}, {f * 0.30, f * 0.76}, {f * 0.47, f * 0.50}}, inkWhite)
	c.fillPoly([][2]float64{{f * 0.36, f * 0.30}, {f * 0.64, f * 0.30}, {f * 0.50, f * 0.48}}, inkBlack)
	return c
}

// iconSatellite: body with two solar panels along a diagonal and signal
// arcs at the lower right.
func iconSatellite(n int) *vcanvas {
	c := newVCanvas(n, iconSS, inkWhite)
	f := float64(n)
	cx, cy := f*0.42, f*0.42
	body := [][2]float64{{cx - f*0.10, cy - f*0.07}, {cx + f*0.10, cy - f*0.07}, {cx + f*0.10, cy + f*0.07}, {cx - f*0.10, cy + f*0.07}}
	pa := [][2]float64{{cx - f*0.40, cy - f*0.12}, {cx - f*0.13, cy - f*0.12}, {cx - f*0.13, cy + f*0.12}, {cx - f*0.40, cy + f*0.12}}
	pb := [][2]float64{{cx + f*0.13, cy - f*0.12}, {cx + f*0.40, cy - f*0.12}, {cx + f*0.40, cy + f*0.12}, {cx + f*0.13, cy + f*0.12}}
	for _, p := range [][][2]float64{body, pa, pb} {
		c.fillPoly(rot(p, cx, cy, -45), inkBlack)
	}
	// dish stub and arcs toward the lower right
	ax, ay := cx+f*0.10, cy+f*0.10
	for _, r := range []float64{f * 0.16, f * 0.27, f * 0.38} {
		c.arc(ax, ay, r, 5, 85, f*0.055, inkBlack)
	}
	return c
}

// iconAircraft: top-view airplane with signal arcs at its nose.
func iconAircraft(n int) *vcanvas {
	c := newVCanvas(n, iconSS, inkWhite)
	f := float64(n)
	cx, cy := f*0.40, f*0.58
	// facing +x before rotation
	fus := [][2]float64{{cx + f*0.34, cy}, {cx + f*0.26, cy - f*0.05}, {cx - f*0.30, cy - f*0.045}, {cx - f*0.34, cy}, {cx - f*0.30, cy + f*0.045}, {cx + f*0.26, cy + f*0.05}}
	wingL := [][2]float64{{cx + f*0.08, cy - f*0.03}, {cx - f*0.10, cy - f*0.03}, {cx - f*0.20, cy - f*0.36}, {cx - f*0.10, cy - f*0.36}}
	wingR := [][2]float64{{cx + f*0.08, cy + f*0.03}, {cx - f*0.10, cy + f*0.03}, {cx - f*0.20, cy + f*0.36}, {cx - f*0.10, cy + f*0.36}}
	tailL := [][2]float64{{cx - f*0.24, cy - f*0.02}, {cx - f*0.34, cy - f*0.02}, {cx - f*0.38, cy - f*0.16}, {cx - f*0.32, cy - f*0.16}}
	tailR := [][2]float64{{cx - f*0.24, cy + f*0.02}, {cx - f*0.34, cy + f*0.02}, {cx - f*0.38, cy + f*0.16}, {cx - f*0.32, cy + f*0.16}}
	for _, p := range [][][2]float64{fus, wingL, wingR, tailL, tailR} {
		c.fillPoly(rot(p, cx, cy, -30), inkBlack)
	}
	ax, ay := cx+f*0.10, cy-f*0.10
	for _, r := range []float64{f * 0.20, f * 0.33} {
		c.arc(ax, ay, r, -75, -5, f*0.055, inkBlack)
	}
	return c
}

// iconTower: lattice antenna tower with radiating arcs.
func iconTower(n int) *vcanvas {
	c := newVCanvas(n, iconSS, inkWhite)
	f := float64(n)
	cx, top := f/2, f*0.36
	c.circle(cx, top, f*0.055, inkBlack)
	w := f * 0.075
	c.line(cx, top+f*0.04, f*0.24, f*0.95, w, inkBlack)
	c.line(cx, top+f*0.04, f*0.76, f*0.95, w, inkBlack)
	c.line(f*0.36, f*0.72, f*0.64, f*0.72, w*0.85, inkBlack)
	c.line(f*0.31, f*0.84, f*0.69, f*0.84, w*0.85, inkBlack)
	for _, r := range []float64{f * 0.17, f * 0.31} {
		c.arc(cx, top, r, -55, 55, f*0.05, inkBlack)
		c.arc(cx, top, r, 125, 235, f*0.05, inkBlack)
	}
	return c
}

// iconCloud: outlined cloud (weather).
func iconCloud(n int) *vcanvas {
	c := newVCanvas(n, iconSS, inkWhite)
	f := float64(n)
	type disc struct{ x, y, r float64 }
	discs := []disc{{f * 0.28, f * 0.62, f * 0.19}, {f * 0.45, f * 0.42, f * 0.24}, {f * 0.68, f * 0.52, f * 0.20}, {f * 0.80, f * 0.66, f * 0.14}}
	stroke := f * 0.075
	for _, d := range discs {
		c.circle(d.x, d.y, d.r, inkBlack)
	}
	c.fillPoly([][2]float64{{f * 0.28, f * 0.62}, {f * 0.80, f * 0.66}, {f * 0.80, f * 0.80}, {f * 0.28, f * 0.81}}, inkBlack)
	for _, d := range discs {
		c.circle(d.x, d.y, d.r-stroke, inkWhite)
	}
	c.fillPoly([][2]float64{{f * 0.28, f * 0.62}, {f * 0.80, f * 0.66}, {f * 0.80, f*0.80 - stroke}, {f * 0.28, f*0.81 - stroke}}, inkWhite)
	return c
}

// iconPeople: three people (footer clients).
func iconPeople(w, h int) *vcanvas {
	// the canvas is square; the caller crops by choosing n = w and only
	// using the top h rows, so draw within the top part.
	c := newVCanvas(w, iconSS, inkWhite)
	f := float64(w)
	person := func(cx, headR, top float64) {
		c.circle(cx, top+headR, headR, inkBlack)
		c.fillPoly([][2]float64{{cx - headR*1.9, top + headR*3.2}, {cx - headR*1.5, top + headR*2.3}, {cx - headR*0.7, top + headR*2.05}, {cx + headR*0.7, top + headR*2.05}, {cx + headR*1.5, top + headR*2.3}, {cx + headR*1.9, top + headR*3.2}}, inkBlack)
	}
	hr := float64(h) * 0.16
	person(f*0.20, hr*0.85, float64(h)*0.20)
	person(f*0.80, hr*0.85, float64(h)*0.20)
	person(f*0.50, hr, float64(h)*0.04)
	return c
}

// iconBrand: the header's small delta-wing mark.
func iconBrand(w, h int) *vcanvas {
	c := newVCanvas(w, iconSS, inkWhite)
	f, hh := float64(w), float64(h)
	cy := hh / 2
	c.fillPoly([][2]float64{{f * 0.50, cy - hh*0.48}, {f * 0.60, cy + hh*0.12}, {f * 0.50, cy + hh*0.30}, {f * 0.40, cy + hh*0.12}}, inkBlack)
	c.line(f*0.05, cy+hh*0.10, f*0.42, cy, hh*0.13, inkBlack)
	c.line(f*0.95, cy+hh*0.10, f*0.58, cy, hh*0.13, inkBlack)
	return c
}

// blitIcon blits a canvas whose height is h (rows below h are ignored).
func blitIcon(dst *image.Gray, c *vcanvas, x, y, h int, invert bool) {
	tmp := image.NewGray(image.Rect(0, 0, c.n, c.n))
	c.blit(tmp, 0, 0, invert)
	for oy := 0; oy < h && oy < c.n; oy++ {
		for ox := 0; ox < c.n; ox++ {
			if image.Pt(x+ox, y+oy).In(dst.Bounds()) {
				dst.SetGray(x+ox, y+oy, tmp.GrayAt(ox, oy))
			}
		}
	}
}
