package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTest42Driver(bus Bus) *Driver42V2 {
	return &Driver42V2{Bus: bus, WidthPx: 400, HeightPx: 300, BusyTimeout: 50 * time.Millisecond}
}

// TestDriver42V2_Init_SendsVendorVerifiedCommandSequence is a direct
// regression test for this driver's whole reason for existing: every
// command and data byte here is verified against Waveshare's own
// epd4in2_V2.py init(), not assumed or adapted from the 3.7in panel's
// own sequence - see driver_4in2v2.go's own doc comment for the two
// specific, confirmed differences (byte- vs pixel-addressed RAM X
// window, and no LUT load on the standard monochrome path).
func TestDriver42V2_Init_SendsVendorVerifiedCommandSequence(t *testing.T) {
	bus := &fakeBus{}
	d := newTest42Driver(bus)
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	wantCommands := []byte{
		cmdSWReset,
		cmdDisplayUpdateControl1,
		cmdBorderWaveform,
		cmdDataEntryMode,
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
		{0x40, 0x00},             // Display Update Control 1
		{0x05},                   // Border waveform
		{0x03},                   // Data entry mode
		{0x00, 0x31},             // RAM X window: byte 0 to byte 49 (400px/8-1) - byte-addressed, unlike the 3.7in panel
		{0x00, 0x00, 0x2B, 0x01}, // RAM Y window: 0 to 299 (300-1), little-endian pixel address
		{0x00},                   // RAM X counter: 1 byte, byte-addressed
		{0x00, 0x00},             // RAM Y counter: 2 bytes, little-endian pixel address
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

	// This panel has no PWR line - Init must never call Bus.SetPower.
	if len(bus.power) != 0 {
		t.Errorf("expected Bus.SetPower to never be called (this panel has no PWR line), got %v", bus.power)
	}
}

// TestDriver42V2_SetRAMWindow_UsesByteAddressForX is a focused regression
// test for the single most important, easiest-to-get-wrong difference
// from the 3.7in panel's own driver: this controller's RAM X-address
// window is a byte address, not a raw pixel address. A window ending at
// x1=399 (the last pixel column) must appear as byte 49 (399/8), never
// as the raw value 399.
func TestDriver42V2_SetRAMWindow_UsesByteAddressForX(t *testing.T) {
	bus := &fakeBus{}
	d := newTest42Driver(bus)
	if err := d.setRAMWindow(0, 0, 399, 299); err != nil {
		t.Fatalf("setRAMWindow failed: %v", err)
	}
	if len(bus.data) < 1 || len(bus.data[0]) != 2 {
		t.Fatalf("expected the first data payload (X-address) to be 2 bytes, got %v", bus.data)
	}
	xEnd := int(bus.data[0][1])
	if xEnd != 49 {
		t.Errorf("X-address end byte = %d, want 49 (399/8, byte-addressed, not the raw pixel value 399)", xEnd)
	}
}

func TestDriver42V2_Update_RejectsWrongSizedBitmap(t *testing.T) {
	bus := &fakeBus{}
	d := newTest42Driver(bus)
	if err := d.Update(context.Background(), []byte{0x00}, true); err == nil {
		t.Errorf("expected an error for a wrong-sized bitmap")
	}
}

func fullSizeBitmap42() []byte {
	stride := (400 + 7) / 8
	return make([]byte, stride*300)
}

func TestDriver42V2_Update_FullWritesBothPlanesAndActivates(t *testing.T) {
	bus := &fakeBus{}
	d := newTest42Driver(bus)
	if err := d.Update(context.Background(), fullSizeBitmap42(), true); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	wantCommands := []byte{cmdWriteRAMBW, cmdWriteRAMRed, cmdDisplayUpdateControl2, cmdMasterActivate}
	if len(bus.commands) != len(wantCommands) {
		t.Fatalf("command count = %d, want %d\ngot:  %v\nwant: %v", len(bus.commands), len(wantCommands), bus.commands, wantCommands)
	}
	for i, want := range wantCommands {
		if bus.commands[i] != want {
			t.Errorf("command[%d] = 0x%02X, want 0x%02X", i, bus.commands[i], want)
		}
	}
	// Last data payload is the Display Update Control 2 mode byte.
	last := bus.data[len(bus.data)-1]
	if len(last) != 1 || last[0] != displayUpdateModeFull {
		t.Errorf("Display Update Control 2 payload = %v, want [0x%02X]", last, displayUpdateModeFull)
	}
}

func TestDriver42V2_Update_PartialReprogramsWindowAndUsesDifferentControlValues(t *testing.T) {
	bus := &fakeBus{}
	d := newTest42Driver(bus)
	if err := d.Update(context.Background(), fullSizeBitmap42(), false); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	wantCommands := []byte{
		cmdBorderWaveform, cmdDisplayUpdateControl1, cmdBorderWaveform,
		cmdSetRAMXAddress, cmdSetRAMYAddress, cmdSetRAMXCounter, cmdSetRAMYCounter,
		cmdWriteRAMBW, cmdDisplayUpdateControl2, cmdMasterActivate,
	}
	if len(bus.commands) != len(wantCommands) {
		t.Fatalf("command count = %d, want %d\ngot:  %v\nwant: %v", len(bus.commands), len(wantCommands), bus.commands, wantCommands)
	}
	for i, want := range wantCommands {
		if bus.commands[i] != want {
			t.Errorf("command[%d] = 0x%02X, want 0x%02X", i, bus.commands[i], want)
		}
	}
	// Partial's border waveform (0x80) and Display Update Control 1
	// (0x00,0x00) values must differ from Init()'s own (0x05 / 0x40,0x00)
	// - verified directly against the vendor's display_Partial().
	if bus.data[0][0] != 0x80 {
		t.Errorf("partial border waveform = 0x%02X, want 0x80", bus.data[0][0])
	}
	if bus.data[1][0] != 0x00 || bus.data[1][1] != 0x00 {
		t.Errorf("partial Display Update Control 1 = %v, want [0x00,0x00]", bus.data[1])
	}
	last := bus.data[len(bus.data)-1]
	if len(last) != 1 || last[0] != displayUpdateModePartial {
		t.Errorf("Display Update Control 2 payload = %v, want [0x%02X]", last, displayUpdateModePartial)
	}
}

func TestDriver42V2_Clear_WritesAllWhiteToBothPlanes(t *testing.T) {
	bus := &fakeBus{}
	d := newTest42Driver(bus)
	if err := d.Clear(context.Background()); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}

	bwIdx, redIdx := -1, -1
	for i, c := range bus.commands {
		if c == cmdWriteRAMBW && bwIdx == -1 {
			bwIdx = i
		}
		if c == cmdWriteRAMRed && redIdx == -1 {
			redIdx = i
		}
	}
	if bwIdx == -1 || redIdx == -1 {
		t.Fatalf("expected both cmdWriteRAMBW and cmdWriteRAMRed, got commands %v", bus.commands)
	}
	wantLen := ((400 + 7) / 8) * 300
	for _, idx := range []int{bwIdx, redIdx} {
		got := bus.data[idx]
		if len(got) != wantLen {
			t.Fatalf("blank bitmap length = %d, want %d", len(got), wantLen)
		}
		for i, b := range got {
			if b != 0xFF {
				t.Fatalf("blank bitmap byte[%d] = 0x%02X, want 0xFF (all white)", i, b)
			}
		}
	}
}

func TestDriver42V2_Sleep_SendsDeepSleepAndNeverTouchesPower(t *testing.T) {
	bus := &fakeBus{}
	d := newTest42Driver(bus)
	if err := d.Sleep(); err != nil {
		t.Fatalf("Sleep failed: %v", err)
	}
	if len(bus.commands) != 1 || bus.commands[0] != cmdDeepSleep {
		t.Errorf("expected exactly cmdDeepSleep, got %v", bus.commands)
	}
	if len(bus.data) != 1 || len(bus.data[0]) != 1 || bus.data[0][0] != deepSleepModeRetainRAM {
		t.Errorf("expected deep sleep data byte 0x%02X, got %v", deepSleepModeRetainRAM, bus.data)
	}
	// This panel has no PWR line - Sleep must never call Bus.SetPower,
	// unlike the 3.7in driver's own Sleep.
	if len(bus.power) != 0 {
		t.Errorf("expected Bus.SetPower to never be called, got %v", bus.power)
	}
}

func TestDriver42V2_Init_BusyTimeoutSurfacesAsErrBusyTimeout(t *testing.T) {
	bus := &fakeBus{busyForCalls: 1000}
	d := newTest42Driver(bus)
	err := d.Init(context.Background())
	if !errors.Is(err, errBusyTimeout) {
		t.Errorf("expected errBusyTimeout, got %v", err)
	}
}

func TestDriver42V2_Update_PropagatesSPIWriteFailure(t *testing.T) {
	bus := &fakeBus{sendDataErr: errors.New("spi write failed")}
	d := newTest42Driver(bus)
	if err := d.Update(context.Background(), fullSizeBitmap42(), true); err == nil {
		t.Errorf("expected Update to propagate an SPI write failure")
	}
}

// TestDriver42V2_SatisfiesPanelDriver is a compile-time-adjacent check
// that this driver implements the same PanelDriver interface as the
// 3.7in panel's *Driver, with no method-set drift.
func TestDriver42V2_SatisfiesPanelDriver(t *testing.T) {
	var _ PanelDriver = (*Driver42V2)(nil)
}
