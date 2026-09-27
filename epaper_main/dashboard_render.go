package main

import (
	"image"
	"image/color"

	"github.com/stratux/stratux/epaper"
)

// The operating dashboard's fixed 400 x 300 layout. All geometry is in
// native panel pixels; the hierarchy (header rule, large state banner,
// 2x2 tiles, footer rule) follows the approved reference design.
const (
	dashW = 400
	dashH = 300

	dashMargin  = 8
	headerRuleY = 29
	bannerTop   = 34
	bannerBot   = 102
	tileTop     = 108
	tileW       = 189
	tileH       = 78
	tileGap     = 6
	tileRowGap  = 6
	footerRuleY = 274
)

// placedText records where a piece of text was allowed to go and where
// ink actually landed, so tests can prove nothing is clipped or overlaps.
type placedText struct {
	Name  string
	Box   image.Rectangle // the region the text must stay inside
	Drawn image.Rectangle // pixels actually inked
}

var (
	styleBrand    = textStyle{bold: true, px: 16, sx: 0.84, cover: 110}
	styleVersion  = textStyle{px: 11, sx: 0.90, cover: 96}
	styleChip     = textStyle{bold: true, px: 10, sx: 0.90, cover: 110}
	styleBanner   = textStyle{bold: true, px: 32, sx: 0.84, cover: 110}
	styleSubtitle = textStyle{px: 12, sx: 0.88, cover: 96}
	styleLabel    = textStyle{px: 12, sx: 0.90, cover: 96}
	styleHead     = textStyle{bold: true, px: 26, sx: 0.84, cover: 110}
	styleDetail   = textStyle{px: 11, sx: 0.90, cover: 96}
	styleFooter   = textStyle{px: 12, sx: 0.90, cover: 96}
)

type painter struct {
	img    *image.Gray
	placed []placedText
}

// text draws s inside box, shrinking/squeezing to fit and trimming as a
// last resort, left-aligned at box.Min.X (or right-aligned when right).
func (p *painter) text(name string, st textStyle, s string, box image.Rectangle, baseY int, right bool, ink uint8, minSX, minPX float64) {
	fit, ok := fitStyle(st, s, box.Dx(), minSX, minPX)
	for !ok && len(s) > 1 {
		s = trimText(s)
		fit, ok = fitStyle(st, s, box.Dx(), minSX, minPX)
	}
	x := box.Min.X
	if right {
		x = box.Max.X - textWidth(fit, s)
	}
	drawn := drawText(p.img, fit, s, x, baseY, ink)
	p.placed = append(p.placed, placedText{Name: name, Box: box, Drawn: drawn})
}

// trimText drops the last rune and marks the cut, so an over-long value is
// visibly shortened rather than clipped mid-glyph.
func trimText(s string) string {
	r := []rune(s)
	if len(r) <= 2 {
		return string(r[:len(r)-1])
	}
	r = r[:len(r)-2]
	return string(r) + "."
}

// renderDashboardImage draws d onto a new 400 x 300 image (white paper,
// black ink) and reports the text placements.
func renderDashboardImage(d epaper.Dashboard) (*image.Gray, []placedText) {
	img := image.NewGray(image.Rect(0, 0, dashW, dashH))
	fillRect(img, img.Bounds(), inkWhite)
	p := &painter{img: img}

	p.header(d)
	p.banner(d)
	tw := tileW
	x0, x1 := dashMargin, dashMargin+tw+tileGap
	y0, y1 := tileTop, tileTop+tileH+tileRowGap
	p.tile(d.GPS, x0, y0, iconSatellite(36))
	p.tile(d.ES, x1, y0, iconAircraft(36))
	p.tile(d.UAT, x0, y1, iconTower(36))
	p.tile(d.FISB, x1, y1, iconCloud(36))
	p.footer(d)
	return img, p.placed
}

func (p *painter) header(d epaper.Dashboard) {
	blitIcon(p.img, iconBrand(28, 16), dashMargin, 6, 16, false)
	brandBox := image.Rect(40, 4, 160, 27)
	p.text("brand", styleBrand, "ARS STRATUX", brandBox, 22, false, inkBlack, 0.7, 12)

	right := dashW - dashMargin
	verBox := image.Rect(250, 8, right, 27)
	p.text("version", styleVersion, d.Version, verBox, 21, true, inkBlack, 0.7, 9)

	// Active warnings as inverted chips between the brand and the version,
	// most important first, as many as fit.
	x := 158
	limit := dashW - dashMargin - textWidth(styleVersion, d.Version) - 8
	for _, w := range d.Warnings {
		label := "! " + w.Text
		cw := textWidth(styleChip, label) + 10
		if x+cw > limit {
			break
		}
		fillRoundRect(p.img, x, 8, x+cw, 24, 4, inkBlack)
		box := image.Rect(x+5, 8, x+cw-5, 24)
		p.text("chip", styleChip, label, box, 20, false, inkWhite, 0.7, 9)
		x += cw + 5
	}
	fillRect(p.img, image.Rect(dashMargin, headerRuleY, dashW-dashMargin, headerRuleY+2), inkBlack)
}

// banner never fills a large area solid black. An earlier design inverted
// the whole banner (white on black) for RECEIVER FAULT / NO STATUS DATA;
// owner-witnessed on the bench panel (three rounds, including two refresh-
// policy fixes - always-full-while-inverted, then Clear()-before-every-
// redraw), that large a solid fill visibly ghosts on this panel regardless
// of refresh policy. Severity is carried by border weight instead: a
// double-stroked border for the two most serious states, a single thicker
// one for degraded, matching this design's own "no shading, state carried by
// words and glyphs" rule (see the doc's Design rules) rather than fighting
// the hardware to make a large fill work.
func (p *painter) banner(d epaper.Dashboard) {
	x0, y0, x1, y1 := dashMargin, bannerTop, dashW-dashMargin, bannerBot
	switch d.Overall {
	case epaper.OverallFault, epaper.OverallNoData:
		strokeRoundRect(p.img, x0, y0, x1, y1, 7, 3, inkBlack)
		strokeRoundRect(p.img, x0+5, y0+5, x1-5, y1-5, 4, 2, inkBlack)
	case epaper.OverallDegraded:
		strokeRoundRect(p.img, x0, y0, x1, y1, 7, 3, inkBlack)
	default:
		strokeRoundRect(p.img, x0, y0, x1, y1, 7, 2, inkBlack)
	}

	const isz = 46
	iy := y0 + (y1-y0-isz)/2
	switch d.Overall {
	case epaper.OverallOnline:
		blitIcon(p.img, iconBroadcast(isz), x0+14, iy, isz, false)
	case epaper.OverallStarting:
		blitIcon(p.img, iconHourglass(isz), x0+14, iy, isz, false)
	case epaper.OverallDegraded:
		blitIcon(p.img, iconWarn(isz, 0.11), x0+14, iy, isz, false)
	case epaper.OverallFault:
		blitIcon(p.img, iconFault(isz), x0+14, iy, isz, false)
	default:
		blitIcon(p.img, iconUnknown(isz), x0+14, iy, isz, false)
	}

	tx := x0 + 14 + isz + 14
	box := image.Rect(tx, y0+6, x1-12, y0+44)
	p.text("banner", styleBanner, d.OverallTxt, box, y0+40, false, inkBlack, 0.66, 20)
	sub := image.Rect(tx, y0+46, x1-12, y1-6)
	p.text("subtitle", styleSubtitle, d.Subtitle, sub, y1-11, false, inkBlack, 0.72, 9)
}

func (p *painter) tile(t epaper.Tile, x, y int, icon *vcanvas) {
	thick := 2
	switch t.Level {
	case epaper.LevelFault:
		thick = 3
	case epaper.LevelWarn, epaper.LevelUnknown:
		thick = 2
	}
	strokeRoundRect(p.img, x, y, x+tileW, y+tileH, 6, thick, inkBlack)

	blitIcon(p.img, icon, x+9, y+(tileH-36)/2+4, 36, false)

	// Level marker, top right: never colour or shading, always a glyph.
	const msz = 16
	mx, my := x+tileW-msz-8, y+7
	markerW := 0
	switch t.Level {
	case epaper.LevelFault:
		blitIcon(p.img, iconFault(msz), mx, my, msz, false)
		markerW = msz + 6
	case epaper.LevelWarn:
		blitIcon(p.img, iconWarn(msz, 0.14), mx, my, msz, false)
		markerW = msz + 6
	case epaper.LevelUnknown:
		blitIcon(p.img, iconUnknown(msz), mx, my, msz, false)
		markerW = msz + 6
	}

	tx := x + 52
	right := x + tileW - 8
	p.text("label:"+t.Label, styleLabel, t.Label, image.Rect(tx, y+4, right-markerW, y+21), y+18, false, inkBlack, 0.7, 10)
	p.text("head:"+t.Label, styleHead, t.Headline, image.Rect(tx, y+22, right, y+52), y+48, false, inkBlack, 0.6, 14)
	p.text("detail:"+t.Label, styleDetail, t.Detail, image.Rect(tx, y+54, right, y+73), y+68, false, inkBlack, 0.72, 8)
}

func (p *painter) footer(d epaper.Dashboard) {
	fillRect(p.img, image.Rect(dashMargin, footerRuleY, dashW-dashMargin, footerRuleY+2), inkBlack)
	blitIcon(p.img, iconPeople(22, 14), dashMargin, 282, 14, false)
	right := dashW - dashMargin
	rw := textWidth(styleFooter, d.Footer.Text)
	rightBox := image.Rect(right-rw-1, 279, right, 298)
	p.text("footer-right", styleFooter, d.Footer.Text, rightBox, 293, true, inkBlack, 0.7, 9)
	leftBox := image.Rect(dashMargin+28, 279, rightBox.Min.X-8, 298)
	p.text("footer-left", styleFooter, d.Clients, leftBox, 293, false, inkBlack, 0.7, 9)
}

// RenderDashboard renders d for the panel: the landscape layout at
// rotation 0, or turned 180 degrees for a unit mounted upside down.
// (90/270 are portrait orientations the landscape design does not target;
// epaperd falls back to the text pages for those.)
func RenderDashboard(d epaper.Dashboard, rotation int) []byte {
	img, _ := renderDashboardImage(d)
	if rotation == 180 {
		img = rotateImage(img, 180)
	}
	return packMonochrome(img)
}

// dashboardRotationSupported reports whether the dashboard layout can be
// shown at rotation.
func dashboardRotationSupported(rotation int) bool { return rotation == 0 || rotation == 180 }

var _ = color.Gray{}
