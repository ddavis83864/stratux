/*
driver.go: a from-scratch Go implementation of the control-line and SPI
command sequence needed to drive a Waveshare 3.7" e-paper panel
(SSD1677 controller) - reset, busy-wait, 1-bit black/white
initialization, partial/full display update, and deep sleep (this driver
never uses the controller's grayscale LUT modes or its red-RAM plane -
see render.go). This is an
original implementation, not a translation or port of any vendor's
source file - the underlying command bytes (which SPI command activates
which controller function) are factual, protocol-level interoperability
information about the SSD1677 chip, not anyone's copyrightable
expression, but the code structure, naming, comments, and error handling
here are entirely this project's own.

Every SPI/GPIO call goes through the Bus interface below, never a
package-level global, so unit tests exercise the whole init/refresh/sleep
sequence against a fake bus with no Raspberry Pi, no GPIO, and no SPI
controller attached - see driver_test.go.
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Bus is the minimal set of operations Driver needs from the real
// hardware. gpioBus (this package) implements it against go-rpio; a fake
// implementing the same interface backs every test.
type Bus interface {
	// Reset drives the RST line low then high with the panel's
	// documented timing.
	Reset(ctx context.Context) error
	// WaitIdle blocks until BUSY reports idle, or ctx is done - see
	// busyPolarity's own doc comment for the exact, verified polarity.
	WaitIdle(ctx context.Context) error
	// SendCommand/SendData drive DC low/high respectively before
	// clocking the byte(s) out over SPI with CS asserted.
	SendCommand(b byte) error
	SendData(b ...byte) error
	// SetPower drives the PWR control line (Rev2.3 driver board
	// convention - see docs/waveshare-epaper-display.md).
	SetPower(on bool) error
}

// busyPolarity documents a real discrepancy found while researching this
// panel: the Waveshare Driver HAT's own connector-pin description calls
// BUSY "low active", but the panel's own documented reference behavior
// waits *while BUSY reads HIGH* and proceeds once it reads LOW - i.e. the
// pin is actually high-while-busy in practice for this SSD1677-based
// panel, the opposite of the generic "low active" pin-description text.
// This driver follows the observed-behavior convention (high = busy),
// since that is what actually makes the panel work; a physical hardware
// test (Phase 10 of this feature's own validation plan) is the final
// check on this before the display is relied on for anything.
const busyMeansBusy = true

// Command bytes for the SSD1677 controller, as used by this panel. Named
// by function, not by hex value, so the meaning is clear without cross-
// referencing a datasheet inline.
const (
	cmdDriverOutputControl   = 0x01
	cmdGateDrivingVoltage    = 0x03
	cmdSourceDrivingVoltage  = 0x04
	cmdBoosterSoftStart      = 0x0C
	cmdDeepSleep             = 0x10
	cmdDataEntryMode         = 0x11
	cmdSWReset               = 0x12
	cmdTemperatureSensor     = 0x18
	cmdMasterActivate        = 0x20
	cmdDisplayUpdateControl1 = 0x21
	cmdDisplayUpdateControl2 = 0x22
	cmdVCOMVoltage           = 0x2C
	cmdBorderWaveform        = 0x3C
	cmdDisplayOption         = 0x37
	cmdSetRAMXAddress        = 0x44
	cmdSetRAMYAddress        = 0x45
	cmdAutoWriteRAMBW        = 0x46
	cmdAutoWriteRAMRed       = 0x47
	cmdSetRAMXCounter        = 0x4E
	cmdSetRAMYCounter        = 0x4F
	cmdWriteRAMBW            = 0x24
	cmdWriteRAMRed           = 0x26
)

// autoWriteRAMClearPattern is the data byte sent with cmdAutoWriteRAMBW
// and cmdAutoWriteRAMRed during Init - the vendor's own reference driver
// sends this exact value for both RAM planes to clear them to a known
// pattern before any other register write, and each write is followed by
// a real BUSY wait (this is an asynchronous controller operation, not an
// instant register write).
const autoWriteRAMClearPattern = 0xF7

// deepSleepModeRetainRAM is sent as cmdDeepSleep's data byte - the
// controller keeps register/RAM contents in this mode (as opposed to
// mode 2, which does not), matching this driver's own expectation that a
// subsequent power-on can resume without a full re-init if desired. This
// driver always does a full re-init on start regardless, so the specific
// retain/discard choice here only affects power draw during sleep, not
// correctness.
const deepSleepModeRetainRAM = 0x01

var errBusyTimeout = errors.New("epaper: BUSY line did not become idle within the configured timeout")

// Driver drives one SSD1677-based panel over a Bus. It is stateless
// beyond the panel's own current dimensions, so it holds no hardware
// handles itself - Bus owns those.
type Driver struct {
	Bus Bus

	WidthPx, HeightPx int
	BusyTimeout       time.Duration
}

// Init powers the panel on, resets it, and runs the documented
// monochrome initialization sequence. Must be called (and succeed)
// before any Update call.
//
// This exact command sequence - including the RAM auto-write clear,
// gate/source driving voltage, booster soft-start, VCOM voltage, and
// display option bytes - was verified byte-for-byte against Waveshare's
// own reference driver (EPD_3in7.c's EPD_3IN7_1Gray_Init()) after real
// hardware validation found the previous, shorter sequence left the
// panel fully unresponsive: digitally error-free (clean BUSY handshakes,
// zero reported faults) but with literally no visible output and no
// refresh flicker, on multiple full and partial refreshes and across a
// reboot. The commands this driver omitted - most critically VCOM
// voltage (0x2C), with no gate/source analog bias configured either -
// are not optional cosmetic steps; without them the panel's pixel
// elements have no correctly-biased waveform to transition against,
// which is consistent with "no flicker at all" rather than merely wrong
// content.
func (d *Driver) Init(ctx context.Context) error {
	if err := d.Bus.SetPower(true); err != nil {
		return fmt.Errorf("power on: %w", err)
	}
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

	// Auto-write RAM to a known clear pattern on both planes before any
	// other register write - the vendor's own driver does this first,
	// immediately after SW reset, and each is a real asynchronous
	// controller operation that must be waited out via BUSY.
	if err := d.cmdData(cmdAutoWriteRAMBW, autoWriteRAMClearPattern); err != nil {
		return err
	}
	if err := d.waitIdleWithTimeout(ctx); err != nil {
		return fmt.Errorf("post-ram-clear-bw busy-wait: %w", err)
	}
	if err := d.cmdData(cmdAutoWriteRAMRed, autoWriteRAMClearPattern); err != nil {
		return err
	}
	if err := d.waitIdleWithTimeout(ctx); err != nil {
		return fmt.Errorf("post-ram-clear-red busy-wait: %w", err)
	}

	// Driver output control: set the panel's own vertical resolution
	// (HeightPx-1, little-endian) and default scan direction.
	if err := d.cmdData(cmdDriverOutputControl, byte((d.HeightPx-1)&0xFF), byte(((d.HeightPx-1)>>8)&0xFF), 0x00); err != nil {
		return err
	}
	if err := d.cmdData(cmdGateDrivingVoltage, 0x00); err != nil {
		return err
	}
	if err := d.cmdData(cmdSourceDrivingVoltage, 0x41, 0xA8, 0x32); err != nil {
		return err
	}
	if err := d.cmdData(cmdDataEntryMode, 0x03); err != nil { // X/Y increment, X-then-Y
		return err
	}
	if err := d.cmdData(cmdBorderWaveform, 0x03); err != nil {
		return err
	}
	if err := d.cmdData(cmdBoosterSoftStart, 0xAE, 0xC7, 0xC3, 0xC0, 0xC0); err != nil {
		return err
	}
	if err := d.cmdData(cmdTemperatureSensor, 0x80); err != nil { // internal temperature sensor
		return err
	}
	if err := d.cmdData(cmdVCOMVoltage, 0x44); err != nil {
		return err
	}
	if err := d.cmdData(cmdDisplayOption, 0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0x4F, 0xFF, 0xFF, 0xFF, 0xFF); err != nil {
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

// Update writes a 1-bit-per-pixel bitmap (MSB-first, row-major, stride
// = ceil(WidthPx/8) bytes per row - the standard SSD1677 RAM packing)
// and triggers a display refresh. full selects a full (flashing, higher-
// quality, ghosting-clearing) refresh versus a faster partial one - see
// docs/waveshare-epaper-display.md's ghosting-mitigation policy for when
// each is used.
func (d *Driver) Update(ctx context.Context, bitmap []byte, full bool) error {
	stride := (d.WidthPx + 7) / 8
	want := stride * d.HeightPx
	if len(bitmap) != want {
		return fmt.Errorf("epaper: bitmap is %d bytes, want %d (stride %d x height %d)", len(bitmap), want, stride, d.HeightPx)
	}

	if err := d.setRAMWindow(0, 0, d.WidthPx-1, d.HeightPx-1); err != nil {
		return err
	}
	if err := d.cmd(cmdWriteRAMBW); err != nil {
		return err
	}
	if err := d.Bus.SendData(bitmap...); err != nil {
		return fmt.Errorf("write RAM: %w", err)
	}

	// DisplayUpdateControl2's own mode byte differs between a full
	// (0xF7 - clear + load LUT + display) and partial (0xFF in this
	// controller's own partial-mode convention) update; both trigger via
	// the same MasterActivate command afterward.
	mode := byte(0xFF)
	if full {
		mode = 0xF7
	}
	if err := d.cmdData(cmdDisplayUpdateControl2, mode); err != nil {
		return err
	}
	if err := d.cmd(cmdMasterActivate); err != nil {
		return err
	}
	if err := d.waitIdleWithTimeout(ctx); err != nil {
		return fmt.Errorf("post-update busy-wait: %w", err)
	}
	return nil
}

// Sleep puts the controller into deep sleep (register/RAM contents
// retained) and de-asserts PWR, per this Rev2.3 driver board's own
// power-control convention. Called on every controlled shutdown/stop -
// see docs/waveshare-epaper-display.md's shutdown-screen policy: Update
// with ShutdownLines should be called before Sleep, not after.
func (d *Driver) Sleep() error {
	if err := d.cmdData(cmdDeepSleep, deepSleepModeRetainRAM); err != nil {
		return err
	}
	// Deliberately ignore SetPower's own error here: sleep is a
	// best-effort power-down, called from shutdown/error paths where a
	// GPIO write failure must never become a new, escalating error.
	_ = d.Bus.SetPower(false)
	return nil
}

// setRAMWindow programs the controller's RAM X/Y address window and
// counters. A real hardware-validation finding: the X-address command
// (0x44) was previously sent as a single byte-divided-by-8 value (a
// byte-address convention) - but this SSD1677 panel's own vendor
// reference driver sends X exactly like Y: a raw, undivided pixel
// coordinate, as two little-endian bytes for both start and end (4 data
// bytes total), confirmed against the vendor's own EPD_3in7.c. Sending
// the wrong byte count/encoding here left the controller's RAM window
// and address counter in an undefined state, silently corrupting every
// subsequent RAM write - plausibly the primary cause of a fully-wired,
// error-free panel that produced no visible output and no refresh
// flicker at all.
func (d *Driver) setRAMWindow(x0, y0, x1, y1 int) error {
	if err := d.cmdData(cmdSetRAMXAddress, byte(x0&0xFF), byte((x0>>8)&0xFF), byte(x1&0xFF), byte((x1>>8)&0xFF)); err != nil {
		return err
	}
	if err := d.cmdData(cmdSetRAMYAddress, byte(y0&0xFF), byte((y0>>8)&0xFF), byte(y1&0xFF), byte((y1>>8)&0xFF)); err != nil {
		return err
	}
	if err := d.cmdData(cmdSetRAMXCounter, byte(x0&0xFF), byte((x0>>8)&0xFF)); err != nil {
		return err
	}
	return d.cmdData(cmdSetRAMYCounter, byte(y0&0xFF), byte((y0>>8)&0xFF))
}

func (d *Driver) cmd(c byte) error {
	return d.Bus.SendCommand(c)
}

func (d *Driver) cmdData(c byte, data ...byte) error {
	if err := d.Bus.SendCommand(c); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return d.Bus.SendData(data...)
}

func (d *Driver) waitIdleWithTimeout(ctx context.Context) error {
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

// defaultBusyTimeout bounds every BUSY wait - this panel's own documented
// full-refresh time is on the order of a few seconds (see
// docs/waveshare-epaper-display.md's refresh-time note), so 10 seconds
// gives ample margin without letting a genuinely stuck BUSY line block
// this process indefinitely.
const defaultBusyTimeout = 10 * time.Second
