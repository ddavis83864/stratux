package assets

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stratux/stratux/epaper/splash"
)

// These tests are the automated acceptance gate (step 4A) for the
// committed production splash asset. They prove the asset is well
// formed, faithful to the approved source (including orientation),
// reproducible byte-for-byte, and unmodified since generation. They do
// not replace the physical acceptance test (step 4B).

func readSource(t *testing.T) ([]byte, image.Image) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("source", "ars-splash-source.png"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return raw, img
}

// The rectangular outer frame was removed from the approved artwork by
// owner decision. The source's outer band must stay clean white, while the
// oval artwork just inside it must still be present.
func TestSourceArtwork_HasNoOuterFrame(t *testing.T) {
	_, src := readSource(t)
	b := src.Bounds()
	const band = 25 // the frame lived 12-21 px in; the oval's first content is at 26
	dark := func(x, y int) bool {
		r, g, bl, _ := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
		return (19595*r+38470*g+7471*bl)>>16 < 0xF000
	}
	var oval int
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			inBand := x < band || x >= b.Dx()-band || y < band || y >= b.Dy()-band
			if inBand && dark(x, y) {
				t.Fatalf("non-white source pixel at (%d,%d) inside the %d px outer band: frame still present", x, y, band)
			}
			if !inBand && dark(x, y) {
				oval++
			}
		}
	}
	if oval < 100000 {
		t.Errorf("only %d dark pixels inside the band; oval artwork appears damaged", oval)
	}
}

func TestProductionBitmap_DimensionsAndDepth(t *testing.T) {
	bm := Bitmap()
	// 400 px * 1 bit/px = 50 bytes/row exactly (no padding bits), 300 rows.
	if len(bm) != 15000 || len(bm) != splash.BitmapLen {
		t.Fatalf("bitmap is %d bytes, want 15000 (400x300 at 1 bit/pixel)", len(bm))
	}
	if splash.Width != 400 || splash.Height != 300 || splash.Stride != 50 {
		t.Fatalf("geometry constants changed: %dx%d stride %d", splash.Width, splash.Height, splash.Stride)
	}
}

func TestProductionBitmap_NotBlankAndMeaningfulPopulations(t *testing.T) {
	s, err := splash.Validate(Bitmap())
	if err != nil {
		t.Fatal(err)
	}
	if s.Black < 10000 || s.White < 10000 {
		t.Errorf("populations too small: black=%d white=%d", s.Black, s.White)
	}
	t.Logf("black=%d white=%d (%.1f%% black)", s.Black, s.White, 100*s.BlackFraction())
}

func TestProductionBitmap_RegenerationIsByteIdentical(t *testing.T) {
	_, src := readSource(t)
	first, err := splash.Convert(src)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := splash.Convert(src)
	if !bytes.Equal(first, second) {
		t.Fatal("two regenerations of the same source differ")
	}
	if !bytes.Equal(first, Bitmap()) {
		t.Fatal("committed ars-splash-400x300.bin is not byte-identical to a fresh conversion of source/ars-splash-source.png; " +
			"run `go run ./epaper/splash/cmd/splashgen` (see docs/epaper-boot-splash.md)")
	}
}

func TestChecksumsMatchFiles(t *testing.T) {
	raw, err := os.ReadFile("CHECKSUMS.sha256")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("malformed checksum line %q", line)
		}
		data, err := os.ReadFile(f[1])
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != f[0] {
			t.Errorf("%s: sha256 %s, CHECKSUMS.sha256 says %s (file modified?)", f[1], got, f[0])
		}
		seen++
	}
	if seen != 2 {
		t.Errorf("CHECKSUMS.sha256 lists %d files, want 2 (source and production bitmap)", seen)
	}
}

func TestSourceArtwork_OpaqueAndFourByThree(t *testing.T) {
	_, src := readSource(t)
	if o, ok := src.(interface{ Opaque() bool }); !ok || !o.Opaque() {
		t.Error("approved source has an alpha channel with non-opaque pixels")
	}
	b := src.Bounds()
	// The approved artwork is exactly 4:3, so it fills the 400x300 panel
	// with uniform scale and no padding, stretch, or crop.
	if b.Dx()*splash.Height != b.Dy()*splash.Width {
		t.Errorf("source is %dx%d, not 4:3; the generator will letterbox it (allowed, but confirm this was intended)", b.Dx(), b.Dy())
	}
}

// TestProductionBitmap_OrientationMatchesSource compares coarse 40x30
// block means of the bitmap against the source and each of the flips
// that a wrong orientation would produce. Identity must be a near-exact
// match and every flip clearly worse - this is what proves the packed
// bitmap is upright and not mirrored/inverted relative to the artwork.
func TestProductionBitmap_OrientationMatchesSource(t *testing.T) {
	_, src := readSource(t)
	const bx, by = 10, 10 // block size -> 40 x 30 grid
	gw, gh := splash.Width/bx, splash.Height/by

	bmGrid := make([]float64, gw*gh)
	bm := Bitmap()
	for y := 0; y < splash.Height; y++ {
		for x := 0; x < splash.Width; x++ {
			if splash.Pixel(bm, x, y) {
				bmGrid[(y/by)*gw+x/bx] += 1.0 / float64(bx*by)
			}
		}
	}

	sb := src.Bounds()
	srcGrid := make([]float64, gw*gh)
	cellW, cellH := float64(sb.Dx())/float64(gw), float64(sb.Dy())/float64(gh)
	for gy := 0; gy < gh; gy++ {
		for gx := 0; gx < gw; gx++ {
			var dark, n float64
			for y := int(float64(gy) * cellH); y < int(float64(gy+1)*cellH); y++ {
				for x := int(float64(gx) * cellW); x < int(float64(gx+1)*cellW); x++ {
					r, g, b, _ := src.At(sb.Min.X+x, sb.Min.Y+y).RGBA()
					if (19595*r+38470*g+7471*b)>>16 < 0x8000 {
						dark++
					}
					n++
				}
			}
			srcGrid[gy*gw+gx] = dark / n
		}
	}

	meanAbsDiff := func(flipX, flipY, invert bool) float64 {
		var sum float64
		for gy := 0; gy < gh; gy++ {
			for gx := 0; gx < gw; gx++ {
				sx, sy := gx, gy
				if flipX {
					sx = gw - 1 - gx
				}
				if flipY {
					sy = gh - 1 - gy
				}
				v := srcGrid[sy*gw+sx]
				if invert {
					v = 1 - v
				}
				d := bmGrid[gy*gw+gx] - v
				if d < 0 {
					d = -d
				}
				sum += d
			}
		}
		return sum / float64(gw*gh)
	}

	identity := meanAbsDiff(false, false, false)
	if identity > 0.03 {
		t.Errorf("identity mean block error %.4f > 0.03: bitmap does not match the source", identity)
	}
	for _, v := range []struct {
		name               string
		flipX, flipY, invt bool
	}{
		{"mirrored left-right", true, false, false},
		{"upside-down", false, true, false},
		{"rotated 180", true, true, false},
		{"inverted", false, false, true},
	} {
		if d := meanAbsDiff(v.flipX, v.flipY, v.invt); d < identity*3 {
			t.Errorf("%s variant is not clearly worse than identity (%.4f vs %.4f): orientation not proven", v.name, d, identity)
		}
	}
}

// TestProductionBitmap_OvalCompleteAndNoFrame proves three things about
// the outline: (1) nothing is clipped - all artwork sits at least
// clearance px inside the panel edge on every side; (2) there is no
// rectangular frame - the outer band is entirely white (a frame would put
// black along every edge); (3) the oval border is a complete closed
// curve - white flood-filled in from the panel edge (4-connected) can
// never reach the interior, and the double-line oval is present when
// walking in from each of the four sides.
func TestProductionBitmap_OvalCompleteAndNoFrame(t *testing.T) {
	bm := Bitmap()
	const (
		clearX = 6  // columns
		clearY = 15 // rows
	)
	for y := 0; y < splash.Height; y++ {
		for x := 0; x < splash.Width; x++ {
			outer := x < clearX || x >= splash.Width-clearX || y < clearY || y >= splash.Height-clearY
			if outer && splash.Pixel(bm, x, y) {
				t.Fatalf("black pixel at (%d,%d) in the outer band: a frame, or artwork clipped at the panel edge", x, y)
			}
		}
	}

	// Flood-fill white inward from every edge pixel.
	seen := make([]bool, splash.Width*splash.Height)
	var queue [][2]int
	push := func(x, y int) {
		i := y*splash.Width + x
		if !seen[i] && !splash.Pixel(bm, x, y) {
			seen[i] = true
			queue = append(queue, [2]int{x, y})
		}
	}
	for x := 0; x < splash.Width; x++ {
		push(x, 0)
		push(x, splash.Height-1)
	}
	for y := 0; y < splash.Height; y++ {
		push(0, y)
		push(splash.Width-1, y)
	}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			x, y := p[0]+d[0], p[1]+d[1]
			if x >= 0 && x < splash.Width && y >= 0 && y < splash.Height {
				push(x, y)
			}
		}
	}
	for _, pt := range [][2]int{{200, 150}, {200, 60}, {200, 240}, {60, 150}, {340, 150}, {120, 100}, {280, 200}} {
		if seen[pt[1]*splash.Width+pt[0]] {
			t.Errorf("exterior white reaches interior point %v: the oval border has a gap", pt)
		}
	}

	runs := func(get func(i int) bool, from, to int) int {
		n, in := 0, false
		for i := from; i < to; i++ {
			if get(i) && !in {
				n++
			}
			in = get(i)
		}
		return n
	}
	cy, cx := splash.Height/2, splash.Width/2
	row := func(x int) bool { return splash.Pixel(bm, x, cy) }
	col := func(y int) bool { return splash.Pixel(bm, cx, y) }
	const strip = 30
	for name, n := range map[string]int{
		"left":   runs(row, 0, strip),
		"right":  runs(row, splash.Width-strip, splash.Width),
		"top":    runs(col, 0, strip),
		"bottom": runs(col, splash.Height-strip, splash.Height),
	} {
		if n < 2 {
			t.Errorf("%s side: %d oval strokes within %d px of the edge, want 2 (the double-line oval)", name, n, strip)
		}
	}
}

func TestPreviewPNG_IdenticalPixelsAndNoAlpha(t *testing.T) {
	f, err := os.Open("ars-splash-400x300.preview.png")
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
	if p.Bounds() != image.Rect(0, 0, 400, 300) {
		t.Errorf("preview bounds %v, want 400x300", p.Bounds())
	}
	bm := Bitmap()
	for y := 0; y < 300; y++ {
		for x := 0; x < 400; x++ {
			r, _, _, _ := p.At(x, y).RGBA()
			if (r == 0) != splash.Pixel(bm, x, y) {
				t.Fatalf("preview differs from production bitmap at (%d,%d)", x, y)
			}
		}
	}
}
