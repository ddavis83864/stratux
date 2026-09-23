package main

import (
	"testing"

	"github.com/stratux/stratux/epaper"
)

// TestNewPanelDriver_SelectsCorrectConcreteType is a direct regression
// test for the panel-selection factory: each supported panel identifier
// must construct its own matching concrete driver, an unrecognized
// identifier must fall back to the 3.7in driver (mirroring epaper.
// Normalize's and epaper.Dimensions's own convention), and every
// constructed driver must carry the width/height it was given.
func TestNewPanelDriver_SelectsCorrectConcreteType(t *testing.T) {
	bus := &fakeBus{}
	cases := []struct {
		name      string
		panel     string
		wantWidth int
	}{
		{"3.7in", epaper.PanelWaveshare37, 280},
		{"4.2in V2", epaper.PanelWaveshare42V2, 400},
		{"empty falls back to 3.7in", "", 280},
		{"unrecognized falls back to 3.7in", "some-future-panel", 280},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A PanelDriver must always be constructed with the panel's
			// fixed native dimensions (NativeDimensions), never the
			// rotation-swapped ones (Dimensions) - see NativeDimensions's
			// own doc comment for the real hardware-validation finding
			// this reflects.
			w, h := epaper.NativeDimensions(c.panel)
			drv := newPanelDriver(c.panel, bus, w, h)

			switch c.panel {
			case epaper.PanelWaveshare42V2:
				d, ok := drv.(*Driver42V2)
				if !ok {
					t.Fatalf("panel %q constructed %T, want *Driver42V2", c.panel, drv)
				}
				if d.WidthPx != c.wantWidth {
					t.Errorf("WidthPx = %d, want %d", d.WidthPx, c.wantWidth)
				}
			default:
				d, ok := drv.(*Driver)
				if !ok {
					t.Fatalf("panel %q constructed %T, want *Driver", c.panel, drv)
				}
				if d.WidthPx != c.wantWidth {
					t.Errorf("WidthPx = %d, want %d", d.WidthPx, c.wantWidth)
				}
			}
		})
	}
}
