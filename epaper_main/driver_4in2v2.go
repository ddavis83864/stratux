/*
driver_4in2v2.go: a from-scratch Go implementation of the control-line
and SPI command sequence needed to drive a Waveshare 4.2" e-Paper Module
(PCB Rev2.2, 400x300, "V2" controller generation) - reset, busy-wait,
1-bit black/white initialization, full/partial display update, and deep
sleep. This is an original implementation, not a translation or port of
any vendor's source file - the underlying command bytes (which SPI
command activates which controller function) are factual, protocol-
level interoperability information about this controller, not anyone's
copyrightable expression, but the code structure, naming, comments, and
error handling here are entirely this project's own, mirroring driver.go
(the Waveshare 3.7in/SSD1677 driver)'s own existing conventions.

Every exact command byte, data byte, and sequence in this file was
verified directly against Waveshare's own reference driver
(RaspberryPi_JetsonNano/python/lib/waveshare_epd/epd4in2_V2.py, the file
this mission names as the manufacturer's own proven-working
implementation for this exact panel) - never guessed or adapted from the
3.7in panel's own sequence, since the two controllers, while sharing the
same command byte set (confirmed: every command this driver uses -
0x10/0x11/0x12/0x20/0x21/0x22/0x24/0x26/0x3C/0x44/0x45/0x4E/0x4F - has an
identical, already-shared package-level constant in driver.go), differ
in the specific values and sequence needed, and in one structurally
important way: this controller's RAM X-address window (commands 0x44/
0x4E) is a byte address (ceil(width/8) bytes), not the 3.7in panel's own
raw-pixel address - confirmed directly from the vendor's own init(),
which programs the X-window end as 0x31 (49 = 400/8 - 1), not 399.
Getting this distinction wrong was exactly the class of bug a real
hardware-validation investigation found and fixed for the 3.7in panel
(see driver.go's own Init() doc comment) - this driver was written to
avoid repeating it by verifying against this panel's own vendor source
directly, not by assuming the 3.7in panel's convention transfers.

A second, deliberate difference from driver.go: this controller's
standard 1-bit monochrome init()/display() path in the vendor's own
reference never loads a custom LUT waveform table (Lut() is called only
from the vendor's separate, unused-here Init_4Gray() path) - relying
entirely on the controller's OTP-stored default waveform. This is a real
difference from the 3.7in panel, which required an explicit LUT load
(see driver.go's own hardware-validation history) - this driver
faithfully matches the 4.2in panel's own vendor reference rather than
assuming the 3.7in panel's requirement applies here too. If real
hardware validation of this panel finds the OTP default insufficient
(the same symptom pattern documented in driver.go), that finding belongs
here, verified against this panel's own hardware - not assumed in
advance.

This panel has no PWR control line (see epaper/gpio.go's own PWR
documentation, which is specific to the 3.7in panel's Rev2.3 driver
board) - its VCC is always-on, so this driver never calls Bus.SetPower.
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Driver42V2 drives a Waveshare 4.2in e-Paper Module (Rev2.2, 400x300,
// "V2" controller generation) over a Bus - the same hardware abstraction
// driver.go's *Driver uses, so this driver is exercised by fakeBus in
// tests exactly the same way, with no Raspberry Pi, GPIO, or SPI
// controller attached. Implements PanelDriver (see paneldriver.go).
type Driver42V2 struct {
	Bus Bus

	WidthPx, HeightPx int
	BusyTimeout       time.Duration
}

// Init resets the panel and runs the documented monochrome
// initialization sequence, verified byte-for-byte against the vendor's
// own epd4in2_V2.py init(). Must be called (and succeed) before Clear or
// Update. Never calls Bus.SetPower - this panel has no PWR line.
func (d *Driver42V2) Init(ctx context.Context) error {
	if err := d.Bus.Reset(ctx); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	if err := d.waitIdleWithTimeout(ctx); err != nil {
		return fmt.Errorf("post-reset busy-wait: %w", err)
	}

	if err := d.cmd(cmdSWReset); err != nil {
		return err
	}
	if err := d.waitIdleWithTimeout(ctx); err != nil {
		return fmt.Errorf("post-swreset busy-wait: %w", err)
	}

	if err := d.cmdData(cmdDisplayUpdateControl1, 0x40, 0x00); err != nil {
		return err
	}
	if err := d.cmdData(cmdBorderWaveform, 0x05); err != nil {
		return err
	}
	if err := d.cmdData(cmdDataEntryMode, 0x03); err != nil { // X/Y increment, X-then-Y
		return err
	}
	if err := d.setRAMWindow(0, 0, d.WidthPx-1, d.HeightPx-1); err != nil {
		return err
	}
	if err := d.waitIdleWithTimeout(ctx); err != nil {
		return fmt.Errorf("post-init busy-wait: %w", err)
	}
	return nil
}

// Clear writes an all-white bitmap to both RAM planes and activates it -
// the vendor's own Clear() does exactly this, unconditionally, before
// any real content is ever drawn; see driver.go's Clear for the fuller
// rationale (this panel was not itself hardware-validated with this step
// omitted, so this driver includes it from the start rather than
// discovering the need for it the same way the 3.7in panel's driver
// did).
func (d *Driver42V2) Clear(ctx context.Context) error {
	blank := d.blankBitmap()
	if err := d.cmd(cmdWriteRAMBW); err != nil {
		return err
	}
	if err := d.Bus.SendData(blank...); err != nil {
		return fmt.Errorf("write blank RAM (BW): %w", err)
	}
	if err := d.cmd(cmdWriteRAMRed); err != nil {
		return err
	}
	if err := d.Bus.SendData(blank...); err != nil {
		return fmt.Errorf("write blank RAM (secondary plane): %w", err)
	}
	return d.turnOnDisplay(ctx, displayUpdateModeFull)
}

// Update writes a 1-bit-per-pixel bitmap (MSB-first, row-major, stride =
// ceil(WidthPx/8) bytes per row) and triggers a display refresh. full
// selects a full (flashing, higher-quality, ghosting-clearing) refresh
// versus a faster partial one, matching the vendor's own display() vs
// display_Partial() - both take a full-frame-sized image in the vendor's
// own reference (this controller's own "partial" mode is a faster
// waveform applied to the whole frame, not a sub-region update), exactly
// matching this project's own Update contract as already used by
// driver.go and main.go.
func (d *Driver42V2) Update(ctx context.Context, bitmap []byte, full bool) error {
	want := d.stride() * d.HeightPx
	if len(bitmap) != want {
		return fmt.Errorf("epaper: bitmap is %d bytes, want %d (stride %d x height %d)", len(bitmap), want, d.stride(), d.HeightPx)
	}

	if full {
		if err := d.cmd(cmdWriteRAMBW); err != nil {
			return err
		}
		if err := d.Bus.SendData(bitmap...); err != nil {
			return fmt.Errorf("write RAM (BW): %w", err)
		}
		if err := d.cmd(cmdWriteRAMRed); err != nil {
			return err
		}
		if err := d.Bus.SendData(bitmap...); err != nil {
			return fmt.Errorf("write RAM (secondary plane): %w", err)
		}
		return d.turnOnDisplay(ctx, displayUpdateModeFull)
	}

	// Partial refresh: the vendor's own display_Partial() reprograms the
	// border waveform, Display Update Control 1, and the RAM window/
	// counters before writing - values that differ from Init()'s own
	// (border 0x80 here vs 0x05 in Init; control-1 0x00,0x00 here vs
	// 0x40,0x00 in Init) and must be set exactly this way each time, not
	// left over from Init or a previous full refresh. The border
	// waveform command is sent twice, matching the vendor's own sequence
	// verbatim rather than an unverified "simplification".
	if err := d.cmdData(cmdBorderWaveform, 0x80); err != nil {
		return err
	}
	if err := d.cmdData(cmdDisplayUpdateControl1, 0x00, 0x00); err != nil {
		return err
	}
	if err := d.cmdData(cmdBorderWaveform, 0x80); err != nil {
		return err
	}
	if err := d.setRAMWindow(0, 0, d.WidthPx-1, d.HeightPx-1); err != nil {
		return err
	}
	if err := d.cmd(cmdWriteRAMBW); err != nil {
		return err
	}
	if err := d.Bus.SendData(bitmap...); err != nil {
		return fmt.Errorf("write RAM (partial): %w", err)
	}
	return d.turnOnDisplay(ctx, displayUpdateModePartial)
}

// Sleep puts the controller into deep sleep (register/RAM contents
// retained, matching driver.go's own choice - see its doc comment).
// Never touches Bus.SetPower - this panel has no PWR line, so there is
// nothing to de-assert.
func (d *Driver42V2) Sleep() error {
	return d.cmdData(cmdDeepSleep, deepSleepModeRetainRAM)
}

// displayUpdateModeFull/Partial are the Display Update Control 2 (0x22)
// mode bytes the vendor's own TurnOnDisplay()/TurnOnDisplay_Partial()
// use - deliberately distinct constants from driver.go's own full/
// partial mode bytes (which happen to share the 0xF7 full value but not
// the partial one: this controller uses 0xFF for partial, matching the
// 3.7in panel's own partial value coincidentally, not by assumption -
// each was independently verified against its own vendor source).
const (
	displayUpdateModeFull    = 0xF7
	displayUpdateModePartial = 0xFF
)

func (d *Driver42V2) turnOnDisplay(ctx context.Context, mode byte) error {
	if err := d.cmdData(cmdDisplayUpdateControl2, mode); err != nil {
		return err
	}
	if err := d.cmd(cmdMasterActivate); err != nil {
		return err
	}
	return d.waitIdleWithTimeout(ctx)
}

// setRAMWindow programs this controller's RAM X/Y address window and
// counters. X is a byte address here (ceil(WidthPx/8) bytes) - not the
// 3.7in panel's raw-pixel address - and the X counter (0x4E) is a single
// byte, not two; both confirmed directly against the vendor's own
// init(), which sends 0x44+[0x00,0x31] (49 = 400/8-1 bytes) and
// 0x4E+[0x00] (one byte). Y remains a raw pixel address, two bytes
// little-endian, exactly like the 3.7in panel.
func (d *Driver42V2) setRAMWindow(x0, y0, x1, y1 int) error {
	xByteStart := byte(x0 / 8)
	xByteEnd := byte(x1 / 8)
	if err := d.cmdData(cmdSetRAMXAddress, xByteStart, xByteEnd); err != nil {
		return err
	}
	if err := d.cmdData(cmdSetRAMYAddress, byte(y0&0xFF), byte((y0>>8)&0xFF), byte(y1&0xFF), byte((y1>>8)&0xFF)); err != nil {
		return err
	}
	if err := d.cmdData(cmdSetRAMXCounter, xByteStart); err != nil {
		return err
	}
	return d.cmdData(cmdSetRAMYCounter, byte(y0&0xFF), byte((y0>>8)&0xFF))
}

func (d *Driver42V2) stride() int {
	return (d.WidthPx + 7) / 8
}

func (d *Driver42V2) blankBitmap() []byte {
	blank := make([]byte, d.stride()*d.HeightPx)
	for i := range blank {
		blank[i] = 0xFF
	}
	return blank
}

func (d *Driver42V2) cmd(c byte) error {
	return d.Bus.SendCommand(c)
}

func (d *Driver42V2) cmdData(c byte, data ...byte) error {
	if err := d.Bus.SendCommand(c); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return d.Bus.SendData(data...)
}

func (d *Driver42V2) waitIdleWithTimeout(ctx context.Context) error {
	timeout := d.BusyTimeout
	if timeout <= 0 {
		timeout = defaultBusyTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := d.Bus.WaitIdle(waitCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		return errBusyTimeout
	}
	return err
}
