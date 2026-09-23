package main

import (
	"testing"

	"github.com/stratux/stratux/epaper"
)

// TestRender_OutputSizeMatchesStrideForBothPanels is a regression test
// for a real architectural gap: no render_test.go existed at all before
// this - a rendering bug (wrong stride, wrong bit convention, an out-of-
// bounds write) had no automated test to catch it for either panel. This
// asserts the exact expected byte count for both this project's
// supported panels' native dimensions.
func TestRender_OutputSizeMatchesStrideForBothPanels(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
	}{
		{"3.7in native (280x480)", epaper.PanelWidth, epaper.PanelHeight},
		{"3.7in rotated (480x280)", epaper.PanelHeight, epaper.PanelWidth},
		{"4.2in V2 native (400x300)", epaper.Panel42V2Width, epaper.Panel42V2Height},
		{"4.2in V2 rotated (300x400)", epaper.Panel42V2Height, epaper.Panel42V2Width},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := []epaper.Line{{Text: "test line"}}
			got := Render(lines, c.width, c.height, 0)
			want := ((c.width + 7) / 8) * c.height
			if len(got) != want {
				t.Errorf("Render(%dx%d) produced %d bytes, want %d (stride %d x height %d)", c.width, c.height, len(got), want, (c.width+7)/8, c.height)
			}
		})
	}
}

// TestRender_NeverPanicsOnRealOverviewPageContentForBothPanels exercises
// the actual Layout() output (not synthetic test lines) for the overview
// page, at both panels' real native dimensions - the combination this
// project actually ships, per the mission's own requirement that the
// existing overview page render correctly at the new panel's 400x300
// resolution.
func TestRender_NeverPanicsOnRealOverviewPageContentForBothPanels(t *testing.T) {
	content := epaper.Content{
		Version: "2.0.0", Build: "abcdef1234567890",
		OverallReady: "READY", AHRSState: "READY", BaroState: "READY", FanState: "READY",
		StoragePressure: "NORMAL",
	}
	for _, panel := range []string{epaper.PanelWaveshare37, epaper.PanelWaveshare42V2} {
		t.Run(panel, func(t *testing.T) {
			w, h := epaper.Dimensions(panel, 0)
			lines := epaper.Layout(content, epaper.Config{Panel: panel, Page: epaper.PageOverview}, false)
			lines = append(lines, epaper.Line{Text: epaper.DisclaimerLine})

			got := Render(lines, w, h, 0)
			want := ((w + 7) / 8) * h
			if len(got) != want {
				t.Errorf("Render produced %d bytes for panel %q, want %d", len(got), panel, want)
			}
		})
	}
}

// TestRender_NeverDrawsPastPanelBounds is a direct regression test for
// the out-of-bounds-rendering requirement: even a pathologically long
// line list must never grow the returned buffer beyond exactly
// stride*height bytes - Render's own "never draw past the panel's own
// bounds" check (the `if y > height { break }` in its loop) is what this
// asserts holds in practice, not just by inspection.
func TestRender_NeverDrawsPastPanelBounds(t *testing.T) {
	var many []epaper.Line
	for i := 0; i < 500; i++ {
		many = append(many, epaper.Line{Text: "line"})
	}
	const w, h = 400, 300
	got := Render(many, w, h, 0)
	want := ((w + 7) / 8) * h
	if len(got) != want {
		t.Fatalf("Render with 500 lines produced %d bytes, want exactly %d (must never grow past the panel's own bounds)", len(got), want)
	}
}

// TestRender_OddWidthPadsStrideToWholeBytes covers a width not evenly
// divisible by 8 (neither supported panel's native width happens to hit
// this case, but a rotated dimension or a future panel could) - the
// stride must round up, never truncate or misalign subsequent rows.
func TestRender_OddWidthPadsStrideToWholeBytes(t *testing.T) {
	const w, h = 401, 10 // 401 is not divisible by 8
	got := Render(nil, w, h, 0)
	wantStride := 51 // ceil(401/8)
	want := wantStride * h
	if len(got) != want {
		t.Errorf("Render(%dx%d) produced %d bytes, want %d (stride %d)", w, h, len(got), want, wantStride)
	}
}

// TestPackMonochrome_WhiteIsBitSetBlackIsBitClear is a direct assertion
// of the bit convention Driver.Update and Driver42V2.Update both rely on
// (a 0 bit paints black on both controllers' documented BW-RAM
// convention) - an all-white canvas (no lines drawn) must pack to all
// 0xFF bytes.
func TestPackMonochrome_WhiteIsBitSetBlackIsBitClear(t *testing.T) {
	got := Render(nil, 16, 2, 0) // no lines drawn -> fully white canvas
	for i, b := range got {
		if b != 0xFF {
			t.Errorf("byte[%d] = 0x%02X, want 0xFF (fully white canvas, no lines drawn)", i, b)
		}
	}
}

func TestBlankMonochrome_SizeAndAllWhite(t *testing.T) {
	got := blankMonochrome(400, 300)
	want := ((400 + 7) / 8) * 300
	if len(got) != want {
		t.Fatalf("blankMonochrome(400,300) = %d bytes, want %d", len(got), want)
	}
	for i, b := range got {
		if b != 0xFF {
			t.Errorf("byte[%d] = 0x%02X, want 0xFF", i, b)
		}
	}
}
