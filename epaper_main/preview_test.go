package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stratux/stratux/epaper"
)

func writePNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(255)
			if (x/8+y/8)%2 == 0 {
				v = 0
			}
			img.SetGray(x, y, color.Gray{v})
		}
	}
	p := filepath.Join(t.TempDir(), "f.png")
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunPreviewSendsExactlyTheImageAndReleasesTheBus(t *testing.T) {
	// A real dashboard golden must reach the controller unchanged.
	golden := filepath.Join("testdata", "dashboard", "02-reference.png")
	want := func() []byte {
		f, _ := os.Open(golden)
		defer f.Close()
		b, err := previewBitmap(f, 0)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}()
	bus := &fakeBus{}
	released := 0
	if code := runPreview(context.Background(), golden, epaper.PanelWaveshare42V2, 0, false, absentStatus(t), fakeOpener(bus, &released, nil), io.Discard, io.Discard); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if released != 1 {
		t.Errorf("released %d times", released)
	}
	if bus.commands[len(bus.commands)-1] != cmdDeepSleep {
		t.Error("panel not put to sleep last")
	}
	n := 0
	for _, d := range bus.data {
		if bytes.Equal(d, want) {
			n++
		}
	}
	if n != 2 {
		t.Errorf("the image reached the controller RAM %d times, want 2 (both planes of the full refresh)", n)
	}
	// The bitmap is exactly the dashboard render's own packing.
	fx := allFixtures()[1]
	if !bytes.Equal(want, RenderDashboard(fx.Dash, 0)) {
		t.Error("previewing the golden PNG does not equal the renderer's own bitmap for that state")
	}
}

func TestRunPreviewRefusesBeforeTouchingHardware(t *testing.T) {
	opened := 0
	open := func(epaper.GPIOMapping) (Bus, func(), error) { opened++; return &fakeBus{}, func() {}, nil }
	good := writePNG(t, 400, 300)
	cases := []struct {
		name, path, panel string
		rot               int
		status            string
	}{
		{"wrong panel", good, epaper.PanelWaveshare37, 0, absentStatus(t)},
		{"missing file", "/nonexistent.png", epaper.PanelWaveshare42V2, 0, absentStatus(t)},
		{"wrong size", writePNG(t, 300, 400), epaper.PanelWaveshare42V2, 0, absentStatus(t)},
		{"rotation 90", good, epaper.PanelWaveshare42V2, 90, absentStatus(t)},
		{"service owns the panel", good, epaper.PanelWaveshare42V2, 0, writeStatus(t, epaper.StateRunning, time.Second)},
	}
	for _, c := range cases {
		if code := runPreview(context.Background(), c.path, c.panel, c.rot, false, c.status, open, io.Discard, io.Discard); code != exitRefused {
			t.Errorf("%s: exit %d, want %d", c.name, code, exitRefused)
		}
	}
	if opened != 0 {
		t.Errorf("hardware opened %d times by refused runs", opened)
	}
}

func TestPreviewBitmapRotation180IsPointSymmetric(t *testing.T) {
	p := writePNG(t, 400, 300)
	read := func(rot int) []byte {
		f, _ := os.Open(p)
		defer f.Close()
		b, err := previewBitmap(f, rot)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a, b := read(0), read(180)
	bit := func(buf []byte, x, y int) bool { return buf[y*50+x/8]&(0x80>>uint(x%8)) != 0 }
	for y := 0; y < 300; y += 11 {
		for x := 0; x < 400; x += 7 {
			if bit(b, x, y) != bit(a, 399-x, 299-y) {
				t.Fatalf("mismatch at %d,%d", x, y)
			}
		}
	}
}
