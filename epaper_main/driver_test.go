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
