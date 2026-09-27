package main

import (
	"bytes"
	"flag"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the dashboard golden images in testdata/dashboard")

// previewDir, when set, also receives 1x and 3x previews of every fixture
// (used to produce the documentation previews).
var previewDir = os.Getenv("EPAPER_PREVIEW_DIR")

func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func scale(img *image.Gray, k int) *image.Gray {
	b := img.Bounds()
	out := image.NewGray(image.Rect(0, 0, b.Dx()*k, b.Dy()*k))
	for y := 0; y < b.Dy()*k; y++ {
		for x := 0; x < b.Dx()*k; x++ {
			out.SetGray(x, y, img.GrayAt(x/k, y/k))
		}
	}
	return out
}

func TestDashboardGoldenImages(t *testing.T) {
	for _, f := range allFixtures() {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			img, _ := renderDashboardImage(f.Dash)
			got := pngBytes(t, img)
			golden := filepath.Join("testdata", "dashboard", f.Name+".png")
			if previewDir != "" {
				_ = os.MkdirAll(previewDir, 0o755)
				_ = os.WriteFile(filepath.Join(previewDir, f.Name+".png"), got, 0o644)
				_ = os.WriteFile(filepath.Join(previewDir, f.Name+"@3x.png"), pngBytes(t, scale(img, 3)), 0o644)
			}
			docCopy := filepath.Join("..", "docs", "img", "epaper-dashboard", f.Name+".png")
			if *updateGolden {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(docCopy, got, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join("..", "docs", "img", "epaper-dashboard", f.Name+"@3x.png"), pngBytes(t, scale(img, 3)), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			// The preview shipped in the documentation must be the current render.
			if doc, err := os.ReadFile(docCopy); err != nil || !bytes.Equal(doc, got) {
				t.Errorf("%s is missing or stale (run go test ./epaper_main -run TestDashboardGoldenImages -update)", docCopy)
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden image (run go test -update): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("render of %q differs from %s (run go test -update after checking the preview)", f.Name, golden)
			}
		})
	}
}

func TestDashboardIsOneBitAndNativeSize(t *testing.T) {
	for _, f := range allFixtures() {
		img, _ := renderDashboardImage(f.Dash)
		if img.Bounds().Dx() != 400 || img.Bounds().Dy() != 300 {
			t.Fatalf("%s: size %v", f.Name, img.Bounds())
		}
		black := 0
		for _, p := range img.Pix {
			switch p {
			case 0:
				black++
			case 255:
			default:
				t.Fatalf("%s: non one-bit pixel value %d", f.Name, p)
			}
		}
		if frac := float64(black) / float64(len(img.Pix)); frac < 0.05 || frac > 0.6 {
			t.Errorf("%s: implausible ink coverage %.2f", f.Name, frac)
		}
	}
}

// Every text must stay inside its allotted region and the panel, and no
// two texts may overlap: nothing clipped, nothing colliding.
func TestDashboardTextNeverClippedOrOverlapping(t *testing.T) {
	canvas := image.Rect(0, 0, dashW, dashH)
	for _, f := range allFixtures() {
		_, placed := renderDashboardImage(f.Dash)
		for _, pl := range placed {
			if pl.Drawn.Empty() {
				if pl.Name != "chip" {
					t.Errorf("%s: %s drew nothing", f.Name, pl.Name)
				}
				continue
			}
			if !pl.Drawn.In(canvas) {
				t.Errorf("%s: %s drawn %v outside the panel", f.Name, pl.Name, pl.Drawn)
			}
			if !pl.Drawn.In(pl.Box.Inset(-1)) {
				t.Errorf("%s: %s drawn %v exceeds its box %v", f.Name, pl.Name, pl.Drawn, pl.Box)
			}
		}
		for i := range placed {
			for j := i + 1; j < len(placed); j++ {
				a, b := placed[i].Drawn, placed[j].Drawn
				if !a.Empty() && !b.Empty() && a.Overlaps(b) {
					t.Errorf("%s: %s %v overlaps %s %v", f.Name, placed[i].Name, a, placed[j].Name, b)
				}
			}
		}
	}
}

func TestDashboardRotation180IsExactPointSymmetry(t *testing.T) {
	f := allFixtures()[1]
	a := RenderDashboard(f.Dash, 0)
	b := RenderDashboard(f.Dash, 180)
	if len(a) != len(b) || len(a) != 50*300 {
		t.Fatalf("sizes %d %d", len(a), len(b))
	}
	if bytes.Equal(a, b) {
		t.Fatal("180 degree render identical to 0 degree")
	}
	// Point symmetry: pixel (x,y) of b equals pixel (399-x, 299-y) of a.
	bit := func(buf []byte, x, y int) bool { return buf[y*50+x/8]&(0x80>>uint(x%8)) != 0 }
	for y := 0; y < 300; y += 7 {
		for x := 0; x < 400; x += 5 {
			if bit(b, x, y) != bit(a, 399-x, 299-y) {
				t.Fatalf("pixel (%d,%d) mismatch", x, y)
			}
		}
	}
}

func BenchmarkRenderDashboard(b *testing.B) {
	d := allFixtures()[2].Dash
	for i := 0; i < b.N; i++ {
		RenderDashboard(d, 0)
	}
}
