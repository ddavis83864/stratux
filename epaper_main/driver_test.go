package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBus is a complete, deterministic stand-in for real GPIO/SPI
// hardware. Every Driver test in this file uses only fakeBus - none
// requires a Raspberry Pi, GPIO, or SPI controller.
type fakeBus struct {
	commands []byte
	data     [][]byte
	power    []bool

	resetCalls int
	resetErr   error

	busyForCalls int32 // WaitIdle blocks (returns ctx.Err()) this many times before succeeding
	waitIdleErr  error

	sendCommandErr error
	sendDataErr    error
	setPowerErr    error
}

func (f *fakeBus) Reset(ctx context.Context) error {
	f.resetCalls++
	return f.resetErr
}

func (f *fakeBus) WaitIdle(ctx context.Context) error {
	if f.waitIdleErr != nil {
		return f.waitIdleErr
	}
	if atomic.LoadInt32(&f.busyForCalls) > 0 {
		atomic.AddInt32(&f.busyForCalls, -1)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeBus) SendCommand(b byte) error {
	if f.sendCommandErr != nil {
		return f.sendCommandErr
	}
	f.commands = append(f.commands, b)
	return nil
}

func (f *fakeBus) SendData(b ...byte) error {
	if f.sendDataErr != nil {
		return f.sendDataErr
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	f.data = append(f.data, cp)
	return nil
}

func (f *fakeBus) SetPower(on bool) error {
	if f.setPowerErr != nil {
		return f.setPowerErr
	}
	f.power = append(f.power, on)
	return nil
}

func newTestDriver(bus Bus) *Driver {
	return &Driver{Bus: bus, WidthPx: 480, HeightPx: 280, BusyTimeout: 50 * time.Millisecond}
}

func TestDriver_Init_PowersOnAndResetsBeforeSendingCommands(t *testing.T) {
	bus := &fakeBus{}
	d := newTestDriver(bus)
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if len(bus.power) == 0 || !bus.power[0] {
		t.Errorf("expected SetPower(true) as the first hardware action, got %v", bus.power)
	}
	if bus.resetCalls != 1 {
		t.Errorf("expected exactly one Reset call, got %d", bus.resetCalls)
	}
	if len(bus.commands) == 0 {
		t.Errorf("expected at least one command sent during Init")
	}
}

func TestDriver_Init_SoftwareResetIsTheFirstCommand(t *testing.T) {
	bus := &fakeBus{}
	d := newTestDriver(bus)
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if len(bus.commands) == 0 || bus.commands[0] != cmdSWReset {
		t.Errorf("expected the first command to be cmdSWReset (0x%02X), got %v", cmdSWReset, bus.commands)
	}
}

func TestDriver_Init_PropagatesResetFailure(t *testing.T) {
	bus := &fakeBus{resetErr: errors.New("gpio open failed")}
	d := newTestDriver(bus)
	if err := d.Init(context.Background()); err == nil {
		t.Errorf("expected Init to fail when Reset fails")
	}
}

func TestDriver_Init_BusyTimeoutSurfacesAsErrBusyTimeout(t *testing.T) {
	bus := &fakeBus{busyForCalls: 1000} // never becomes idle within the test's short timeout
	d := newTestDriver(bus)
	err := d.Init(context.Background())
	if !errors.Is(err, errBusyTimeout) {
		t.Errorf("expected errBusyTimeout, got %v", err)
	}
}

// TestDriver_Init_SendsVendorVerifiedCommandSequence is a direct
// regression test for a real hardware-validation finding: a fully
// wired, digitally error-free panel produced no visible output and no
// refresh flicker at all, because Init() was missing several required
// SSD1677 analog/RAM-setup commands (most critically VCOM voltage) and
// setRAMWindow encoded the X-address command incorrectly. This test
// asserts the exact command and data-byte sequence, verified
// byte-for-byte against Waveshare's own reference driver
// (EPD_3in7.c's EPD_3IN7_1Gray_Init()) for a 280x480 (native) panel -
// not merely "some commands were sent", which is exactly the class of
// test that existed before and never caught this.
func TestDriver_Init_SendsVendorVerifiedCommandSequence(t *testing.T) {
	bus := &fakeBus{}
	d := &Driver{Bus: bus, WidthPx: 280, HeightPx: 480, BusyTimeout: 50 * time.Millisecond}
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	wantCommands := []byte{
		cmdSWReset,
		cmdAutoWriteRAMBW, cmdAutoWriteRAMRed,
		cmdDriverOutputControl,
		cmdGateDrivingVoltage,
		cmdSourceDrivingVoltage,
		cmdDataEntryMode,
		cmdBorderWaveform,
		cmdBoosterSoftStart,
		cmdTemperatureSensor,
		cmdVCOMVoltage,
		cmdDisplayOption,
		cmdSetRAMXAddress, cmdSetRAMYAddress, cmdSetRAMXCounter, cmdSetRAMYCounter,
	}
	if len(bus.commands) != len(wantCommands) {
		t.Fatalf("command count = %d, want %d\ngot:  %v\nwant: %v", len(bus.commands), len(wantCommands), bus.commands, wantCommands)
	}
	for i, want := range wantCommands {
		if bus.commands[i] != want {
			t.Errorf("command[%d] = 0x%02X, want 0x%02X (full: %v)", i, bus.commands[i], want, bus.commands)
		}
	}

	wantData := [][]byte{
		{autoWriteRAMClearPattern},
		{autoWriteRAMClearPattern},
		{0xDF, 0x01, 0x00}, // gate count 479 (HeightPx-1), little-endian
		{0x00},             // gate driving voltage
		{0x41, 0xA8, 0x32}, // source driving voltage
		{0x03},             // data entry mode
		{0x03},             // border waveform
		{0xAE, 0xC7, 0xC3, 0xC0, 0xC0}, // booster soft-start
		{0x80},                         // temperature sensor
		{0x44},                         // VCOM voltage
		{0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0x4F, 0xFF, 0xFF, 0xFF, 0xFF}, // display option
		{0x00, 0x00, 0x17, 0x01}, // RAM X range: 0..279, little-endian (raw pixel, not byte-divided)
		{0x00, 0x00, 0xDF, 0x01}, // RAM Y range: 0..479, little-endian
		{0x00, 0x00},             // RAM X counter: 0, little-endian
		{0x00, 0x00},             // RAM Y counter: 0, little-endian
	}
	if len(bus.data) != len(wantData) {
		t.Fatalf("data payload count = %d, want %d\ngot:  %v\nwant: %v", len(bus.data), len(wantData), bus.data, wantData)
	}
	for i, want := range wantData {
		got := bus.data[i]
		if len(got) != len(want) {
			t.Errorf("data[%d] length = %d, want %d (got %v, want %v)", i, len(got), len(want), got, want)
			continue
		}
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("data[%d][%d] = 0x%02X, want 0x%02X (got %v, want %v)", i, j, got[j], want[j], got, want)
			}
		}
	}
}

// TestDriver_SetRAMWindow_UsesRawPixelAddressNotByteDivided is a focused
// regression test for the X-address encoding bug in isolation: the
// X-address command must carry the same raw-pixel, two-byte
// little-endian start/end encoding as Y - not a byte-address (divided
// by 8) value. A window ending at x1=279 must never appear as 279/8=34;
// it must appear as the raw value 279 (0x0117 little-endian).
func TestDriver_SetRAMWindow_UsesRawPixelAddressNotByteDivided(t *testing.T) {
	bus := &fakeBus{}
	d := &Driver{Bus: bus, WidthPx: 280, HeightPx: 480, BusyTimeout: 50 * time.Millisecond}
	if err := d.setRAMWindow(0, 0, 279, 479); err != nil {
		t.Fatalf("setRAMWindow failed: %v", err)
	}
	if len(bus.data) < 1 || len(bus.data[0]) != 4 {
		t.Fatalf("expected the first data payload (X-address) to be 4 bytes, got %v", bus.data)
	}
	xEnd := int(bus.data[0][2]) | int(bus.data[0][3])<<8
	if xEnd != 279 {
		t.Errorf("X-address end = %d, want 279 (raw pixel value, not 279/8=%d)", xEnd, 279/8)
	}
}

func TestDriver_Update_RejectsWrongSizedBitmap(t *testing.T) {
	bus := &fakeBus{}
	d := newTestDriver(bus)
	err := d.Update(context.Background(), []byte{0x00}, true)
	if err == nil {
		t.Errorf("expected an error for a wrong-sized bitmap")
	}
}

func TestDriver_Update_SendsCorrectlySizedBitmapAndActivates(t *testing.T) {
	bus := &fakeBus{}
	d := newTestDriver(bus)
	stride := (480 + 7) / 8
	bitmap := make([]byte, stride*280)
	if err := d.Update(context.Background(), bitmap, true); err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	found := false
	for _, c := range bus.commands {
		if c == cmdMasterActivate {
			found = true
		}
	}
	if !found {
		t.Errorf("expected cmdMasterActivate to be sent, got commands %v", bus.commands)
	}
}

// TestDriver_Update_LoadsVendorVerifiedLUTForFullVsPartial is a
// regression test for a real hardware-validation finding: Update()
// previously never loaded a custom waveform table at all, relying
// entirely on the controller's OTP-stored default - which produced a
// fully wired, digitally error-free, zero-flicker blank panel even
// after every other vendor-verified Init() command was added. This
// asserts the exact LUT command and byte-for-byte table content
// (verified against Waveshare's own EPD_3in7.c lut_1Gray_DU/lut_1Gray_A2
// arrays) for both full and partial refresh.
func TestDriver_Update_LoadsVendorVerifiedLUTForFullVsPartial(t *testing.T) {
	stride := (280 + 7) / 8
	bitmap := make([]byte, stride*480)

	for _, tc := range []struct {
		name    string
		full    bool
		wantLUT []byte
	}{
		{"full", true, lutFullRefresh1Gray},
		{"partial", false, lutPartialRefresh1Gray},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := &fakeBus{}
			d := &Driver{Bus: bus, WidthPx: 280, HeightPx: 480, BusyTimeout: 50 * time.Millisecond}
			if err := d.Update(context.Background(), bitmap, tc.full); err != nil {
				t.Fatalf("Update failed: %v", err)
			}
			lutIdx := -1
			for i, c := range bus.commands {
				if c == cmdLUTRegister {
					lutIdx = i
					break
				}
			}
			if lutIdx == -1 {
				t.Fatalf("expected cmdLUTRegister (0x32) to be sent, got commands %v", bus.commands)
			}
			got := bus.data[lutIdx]
			if len(got) != len(tc.wantLUT) {
				t.Fatalf("LUT length = %d, want %d", len(got), len(tc.wantLUT))
			}
			for i := range tc.wantLUT {
				if got[i] != tc.wantLUT[i] {
					t.Errorf("LUT byte[%d] = 0x%02X, want 0x%02X", i, got[i], tc.wantLUT[i])
				}
			}
		})
	}
}

func TestDriver_Update_FullVsPartialUseDifferentControlByte(t *testing.T) {
	stride := (480 + 7) / 8
	bitmap := make([]byte, stride*280)

	busFull := &fakeBus{}
	if err := newTestDriver(busFull).Update(context.Background(), bitmap, true); err != nil {
		t.Fatalf("full Update failed: %v", err)
	}
	busPartial := &fakeBus{}
	if err := newTestDriver(busPartial).Update(context.Background(), bitmap, false); err != nil {
		t.Fatalf("partial Update failed: %v", err)
	}

	// The only two things a full vs. partial Update should differ on are
	// (a) which DisplayUpdateControl2 mode byte gets sent, and (b) the
	// resulting MasterActivate side effects driven by the controller
	// itself (invisible to this fake bus). So: the full command sequence
	// must be identical (same commands, same order), but at least one
	// single-byte data payload recorded along the way must differ -
	// exactly the DisplayUpdateControl2 mode byte - without this test
	// needing to know which slice index that is.
	if len(busFull.commands) != len(busPartial.commands) {
		t.Fatalf("expected the same command sequence for full vs partial, got %v vs %v", busFull.commands, busPartial.commands)
	}
	for i := range busFull.commands {
		if busFull.commands[i] != busPartial.commands[i] {
			t.Fatalf("command sequence diverged at index %d: %v vs %v", i, busFull.commands, busPartial.commands)
		}
	}
	differs := false
	for i := range busFull.data {
		if i >= len(busPartial.data) {
			break
		}
		if len(busFull.data[i]) == 1 && len(busPartial.data[i]) == 1 && busFull.data[i][0] != busPartial.data[i][0] {
			differs = true
		}
	}
	if !differs {
		t.Errorf("expected exactly one differing single-byte data payload (the refresh-mode byte) between full and partial, found none")
	}
}

func TestDriver_Sleep_SendsDeepSleepAndPowersDown(t *testing.T) {
	bus := &fakeBus{}
	d := newTestDriver(bus)
	if err := d.Sleep(); err != nil {
		t.Fatalf("Sleep failed: %v", err)
	}
	if len(bus.commands) == 0 || bus.commands[len(bus.commands)-1] != cmdDeepSleep {
		t.Errorf("expected the last command to be cmdDeepSleep, got %v", bus.commands)
	}
	if len(bus.power) == 0 || bus.power[len(bus.power)-1] {
		t.Errorf("expected the final power state to be off, got %v", bus.power)
	}
}

func TestDriver_Sleep_NeverFailsOnAPowerWriteError(t *testing.T) {
	bus := &fakeBus{setPowerErr: errors.New("gpio write failed")}
	d := newTestDriver(bus)
	if err := d.Sleep(); err != nil {
		t.Errorf("Sleep must not fail just because the final power-down write failed, got: %v", err)
	}
}

func TestDriver_Update_PropagatesSPIWriteFailure(t *testing.T) {
	bus := &fakeBus{sendDataErr: errors.New("spi write failed")}
	d := newTestDriver(bus)
	stride := (480 + 7) / 8
	bitmap := make([]byte, stride*280)
	if err := d.Update(context.Background(), bitmap, true); err == nil {
		t.Errorf("expected Update to propagate an SPI write failure")
	}
}
