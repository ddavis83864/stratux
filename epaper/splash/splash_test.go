package splash

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

func solid(w, h int, c color.Color) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func blackCount(bm []byte) int {
	s, _ := statsOf(bm)
	return s.Black
}

// statsOf counts populations without Validate's plausibility gate.
func statsOf(bm []byte) (Stats, error) {
	var s Stats
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			if Pixel(bm, x, y) {
				s.Black++
			} else {
				s.White++
			}
		}
	}
	return s, nil
}

func TestFitSize_PreservesAspectAndNeverExceedsPanel(t *testing.T) {
	cases := []struct{ sw, sh, w, h int }{
		{1448, 1086, 400, 300}, // the approved artwork: exact 4:3
		{800, 600, 400, 300},
		{200, 200, 300, 300},  // square: height-limited, side margins
		{1000, 500, 400, 200}, // wide: width-limited, top/bottom margins
		{100, 300, 100, 300},  // tall & narrow
		{400, 300, 400, 300},  // already native
		{40, 30, 400, 300},    // upscale
	}
	for _, c := range cases {
		w, h := fitSize(c.sw, c.sh)
		if w != c.w || h != c.h {
			t.Errorf("fitSize(%d,%d) = %dx%d, want %dx%d", c.sw, c.sh, w, h, c.w, c.h)
		}
		if w > Width || h > Height {
			t.Errorf("fitSize(%d,%d) = %dx%d exceeds panel", c.sw, c.sh, w, h)
		}
	}
}

func TestConvert_CentersWithWhiteMargins(t *testing.T) {
	// A solid black 200x200 square fits to 300x300 (height-limited), so
	// it must occupy x in [50,350) and y in [0,300), with white margins.
	bm, err := Convert(solid(200, 200, color.Black))
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			wantBlack := x >= 50 && x < 350
			if Pixel(bm, x, y) != wantBlack {
				t.Fatalf("pixel (%d,%d) black=%v, want %v", x, y, Pixel(bm, x, y), wantBlack)
			}
		}
	}
}

func TestConvert_IsExactlyDeterministic(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 333, 257))
	for y := 0; y < 257; y++ {
		for x := 0; x < 333; x++ {
			v := uint8((x*7 + y*13 + (x*y)%251) % 256)
			img.Set(x, y, color.NRGBA{v, v, v, 255})
		}
	}
	a, _ := Convert(img)
	b, _ := Convert(img)
	if !bytes.Equal(a, b) {
		t.Fatal("two conversions of the same image differ")
	}
}

func TestConvert_ThresholdIsFiftyPercentAreaCoverage(t *testing.T) {
	// 800x600 -> 400x300 is an exact 2x2 box. Left half of every 2x2
	// block black => 50% coverage => average exactly mid-gray (0x8000 or
	// 0x7FFF after rounding); verify the boundary behaves as documented
	// on both sides using 3 vs 1 black pixels per block.
	mk := func(blackPerBlock int) *image.Gray {
		img := image.NewGray(image.Rect(0, 0, 800, 600))
		for i := range img.Pix {
			img.Pix[i] = 255
		}
		for y := 0; y < 600; y++ {
			for x := 0; x < 800; x++ {
				idx := (y%2)*2 + x%2
				if idx < blackPerBlock {
					img.SetGray(x, y, color.Gray{0})
				}
			}
		}
		return img
	}
	for _, c := range []struct {
		black     int
		wantBlack bool
	}{{1, false}, {3, true}} {
		bm, err := Convert(mk(c.black))
		if err != nil {
			t.Fatal(err)
		}
		if got := Pixel(bm, 10, 10); got != c.wantBlack {
			t.Errorf("%d/4 black coverage: pixel black=%v, want %v", c.black, got, c.wantBlack)
		}
	}
}

func TestConvert_AlphaCompositesOverWhite(t *testing.T) {
	// Fully transparent pixels carrying black RGB must become white, not
	// black: no unintended alpha behavior.
	bm, err := Convert(solid(400, 300, color.NRGBA{0, 0, 0, 0}))
	if err != nil {
		t.Fatal(err)
	}
	if n := blackCount(bm); n != 0 {
		t.Errorf("transparent source produced %d black pixels, want 0", n)
	}
	bm, _ = Convert(solid(400, 300, color.NRGBA{0, 0, 0, 255}))
	if n := blackCount(bm); n != Width*Height {
		t.Errorf("opaque black source produced %d black pixels, want %d", n, Width*Height)
	}
}

func TestConvert_NonZeroOriginAndSubimage(t *testing.T) {
	base := solid(800, 600, color.Black)
	sub := base.SubImage(image.Rect(100, 100, 500, 400)) // 400x300, origin (100,100)
	bm, err := Convert(sub)
	if err != nil {
		t.Fatal(err)
	}
	if n := blackCount(bm); n != Width*Height {
		t.Errorf("subimage black count = %d, want %d", n, Width*Height)
	}
}

func TestConvert_RejectsEmptyAndHuge(t *testing.T) {
	if _, err := Convert(image.NewGray(image.Rect(0, 0, 0, 0))); err == nil {
		t.Error("empty image accepted")
	}
	if _, err := Convert(image.NewGray(image.Rect(0, 0, maxSourceDim+1, 1))); err == nil {
		t.Error("oversized image accepted")
	}
}

func TestValidate(t *testing.T) {
	blank := bytes.Repeat([]byte{0xFF}, BitmapLen)
	if _, err := Validate(blank); err == nil {
		t.Error("all-white bitmap accepted")
	}
	if _, err := Validate(make([]byte, BitmapLen)); err == nil {
		t.Error("all-black bitmap accepted")
	}
	if _, err := Validate(blank[:BitmapLen-1]); err == nil {
		t.Error("short bitmap accepted")
	}
	if _, err := Validate(append(blank, 0)); err == nil {
		t.Error("long bitmap accepted")
	}

	// 30% black: valid. 90% black (inverted-looking): rejected.
	mk := func(blackBytes int) []byte {
		b := bytes.Repeat([]byte{0xFF}, BitmapLen)
		for i := 0; i < blackBytes; i++ {
			b[i] = 0
		}
		return b
	}
	if s, err := Validate(mk(BitmapLen * 3 / 10)); err != nil || s.Black == 0 {
		t.Errorf("30%% black rejected: %v", err)
	}
	if _, err := Validate(mk(BitmapLen * 9 / 10)); err == nil {
		t.Error("90% black (inverted) bitmap accepted")
	}
}

func TestPreview_MatchesBitmapAndIsOpaque(t *testing.T) {
	bm := bytes.Repeat([]byte{0xFF}, BitmapLen)
	bm[0] = 0x7F // pixel (0,0) black
	bm[Stride+1] = 0xFE
	p := Preview(bm)
	for _, c := range p.Palette {
		if _, _, _, a := c.RGBA(); a != 0xFFFF {
			t.Fatal("preview palette has non-opaque entry")
		}
	}
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			r, _, _, _ := p.At(x, y).RGBA()
			if (r == 0) != Pixel(bm, x, y) {
				t.Fatalf("preview differs from bitmap at (%d,%d)", x, y)
			}
		}
	}
}
