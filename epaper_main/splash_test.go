package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/epaper"
	"github.com/stratux/stratux/epaper/splash"
	"github.com/stratux/stratux/epaper/splash/assets"
)

func TestSplashBitmap_Rotations(t *testing.T) {
	base := assets.Bitmap()

	got0, err := splashBitmap(0)
	if err != nil || !bytes.Equal(got0, base) {
		t.Fatalf("rotation 0 must return the production bitmap unchanged (err=%v)", err)
	}

	got180, err := splashBitmap(180)
	if err != nil {
		t.Fatal(err)
	}
	if len(got180) != splash.BitmapLen {
		t.Fatalf("rotation 180 length = %d, want %d", len(got180), splash.BitmapLen)
	}
	for y := 0; y < splash.Height; y++ {
		for x := 0; x < splash.Width; x++ {
			if splash.Pixel(got180, x, y) != splash.Pixel(base, splash.Width-1-x, splash.Height-1-y) {
				t.Fatalf("rotation 180 pixel (%d,%d) is not the point-symmetric source pixel", x, y)
			}
		}
	}
	// Rotating twice is the identity.
	if bytes.Equal(got180, base) {
		t.Fatal("rotation 180 returned the unrotated bitmap")
	}

	for _, r := range []int{90, 270, 45, -1} {
		if _, err := splashBitmap(r); err == nil {
			t.Errorf("rotation %d accepted; only 0 and 180 are supported", r)
		}
	}
}

func TestSplashBitmap_ReturnsIndependentCopy(t *testing.T) {
	a, _ := splashBitmap(0)
	a[0] ^= 0xFF
	b, _ := splashBitmap(0)
	if a[0] == b[0] {
		t.Fatal("mutating one returned bitmap changed the embedded asset")
	}
}

func writeStatus(t *testing.T, state epaper.ServiceState, age time.Duration) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "status.json")
	if err := common.WriteEpaperStatus(p, epaper.Health{State: state, UpdatedAt: time.Now().Add(-age)}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGuardOwnership(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		path    func() string
		wantErr bool
	}{
		{"no status file (service stopped)", func() string { return filepath.Join(t.TempDir(), "absent.json") }, false},
		{"fresh RUNNING", func() string { return writeStatus(t, epaper.StateRunning, 2*time.Second) }, true},
		{"fresh ERROR", func() string { return writeStatus(t, epaper.StateError, 2*time.Second) }, true},
		{"fresh NOT_DETECTED", func() string { return writeStatus(t, epaper.StateNotDetected, 2*time.Second) }, true},
		{"fresh DISABLED (touches no hardware)", func() string { return writeStatus(t, epaper.StateDisabled, 2*time.Second) }, false},
		{"stale RUNNING (service died)", func() string { return writeStatus(t, epaper.StateRunning, 5*time.Minute) }, false},
	}
	for _, c := range cases {
		if err := guardOwnership(c.path(), now); (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

// stubDriver records the lifecycle calls renderSplash makes.
type stubDriver struct {
	calls                               []string
	initErr, clearErr, updErr, sleepErr error
	gotBitmap                           []byte
	gotFull                             bool
}

func (s *stubDriver) Init(context.Context) error { s.calls = append(s.calls, "init"); return s.initErr }
func (s *stubDriver) Clear(context.Context) error {
	s.calls = append(s.calls, "clear")
	return s.clearErr
}
func (s *stubDriver) Update(_ context.Context, bm []byte, full bool) error {
	s.calls = append(s.calls, "update")
	s.gotBitmap, s.gotFull = bm, full
	return s.updErr
}
func (s *stubDriver) Sleep() error { s.calls = append(s.calls, "sleep"); return s.sleepErr }

func TestRenderSplash_Sequence(t *testing.T) {
	bm := assets.Bitmap()
	boom := errors.New("boom")
	cases := []struct {
		name      string
		d         *stubDriver
		wantCalls []string
		wantErr   bool
	}{
		{"success", &stubDriver{}, []string{"init", "clear", "update", "sleep"}, false},
		{"init fails: no sleep, nothing drawn", &stubDriver{initErr: boom}, []string{"init"}, true},
		{"clear fails: still sleeps", &stubDriver{clearErr: boom}, []string{"init", "clear", "sleep"}, true},
		{"update fails: still sleeps", &stubDriver{updErr: boom}, []string{"init", "clear", "update", "sleep"}, true},
		{"sleep fails after success is reported", &stubDriver{sleepErr: boom}, []string{"init", "clear", "update", "sleep"}, true},
	}
	for _, c := range cases {
		err := renderSplash(context.Background(), c.d, bm, io.Discard)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
		if got := len(c.d.calls); got != len(c.wantCalls) {
			t.Errorf("%s: calls %v, want %v", c.name, c.d.calls, c.wantCalls)
			continue
		}
		for i := range c.wantCalls {
			if c.d.calls[i] != c.wantCalls[i] {
				t.Errorf("%s: calls %v, want %v", c.name, c.d.calls, c.wantCalls)
				break
			}
		}
	}

	d := &stubDriver{}
	_ = renderSplash(context.Background(), d, bm, io.Discard)
	if !d.gotFull {
		t.Error("splash must use a full (ghost-clearing) refresh")
	}
	if !bytes.Equal(d.gotBitmap, bm) {
		t.Error("driver did not receive the exact production bitmap")
	}
}

// fakeOpener hands runSplash a fakeBus and counts releases.
func fakeOpener(bus *fakeBus, released *int, openErr error) busOpener {
	return func(epaper.GPIOMapping) (Bus, func(), error) {
		if openErr != nil {
			return nil, nil, openErr
		}
		return bus, func() { *released++ }, nil
	}
}

func absentStatus(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") }

// TestRunSplash_EndToEndThroughRealDriver drives the real Driver42V2
// against a fake bus and proves the exact production bytes reach the
// controller's RAM (both planes, on the full refresh), that the panel is
// put to deep sleep last, and that the hardware is released.
func TestRunSplash_EndToEndThroughRealDriver(t *testing.T) {
	bus := &fakeBus{}
	released := 0
	code := runSplash(context.Background(), epaper.PanelWaveshare42V2, 0, false, absentStatus(t), fakeOpener(bus, &released, nil), io.Discard, io.Discard)
	if code != exitOK {
		t.Fatalf("exit code %d, want %d", code, exitOK)
	}
	if released != 1 {
		t.Errorf("hardware released %d times, want exactly 1", released)
	}
	if len(bus.power) != 0 {
		t.Errorf("4.2in panel has no PWR line but SetPower was called: %v", bus.power)
	}
	if last := bus.commands[len(bus.commands)-1]; last != cmdDeepSleep {
		t.Errorf("last command 0x%02X, want deep sleep 0x%02X", last, cmdDeepSleep)
	}

	want := assets.Bitmap()
	var full [][]byte
	for _, d := range bus.data {
		if len(d) == splash.BitmapLen {
			full = append(full, d)
		}
	}
	// Clear writes blank to two planes; the full-refresh Update writes the
	// bitmap to two planes.
	if len(full) != 4 {
		t.Fatalf("saw %d full-frame RAM writes, want 4 (2 blank from Clear, 2 splash from Update)", len(full))
	}
	blank := bytes.Repeat([]byte{0xFF}, splash.BitmapLen)
	if !bytes.Equal(full[0], blank) || !bytes.Equal(full[1], blank) {
		t.Error("Clear did not write an all-white frame first")
	}
	if !bytes.Equal(full[2], want) || !bytes.Equal(full[3], want) {
		t.Error("the bytes written to controller RAM are not the production splash bitmap")
	}
}

func TestRunSplash_Rotation180SendsRotatedBitmap(t *testing.T) {
	bus := &fakeBus{}
	released := 0
	if code := runSplash(context.Background(), epaper.PanelWaveshare42V2, 180, false, absentStatus(t), fakeOpener(bus, &released, nil), io.Discard, io.Discard); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	want, _ := splashBitmap(180)
	found := false
	for _, d := range bus.data {
		if bytes.Equal(d, want) {
			found = true
		}
	}
	if !found {
		t.Error("rotated bitmap never reached the controller")
	}
}

func TestRunSplash_RefusalsHappenBeforeHardwareIsOpened(t *testing.T) {
	opened := 0
	open := func(epaper.GPIOMapping) (Bus, func(), error) {
		opened++
		return &fakeBus{}, func() {}, nil
	}
	running := writeStatus(t, epaper.StateRunning, time.Second)
	cases := []struct {
		name     string
		panel    string
		rotation int
		force    bool
		status   string
	}{
		{"wrong panel", epaper.PanelWaveshare37, 0, false, absentStatus(t)},
		{"unsupported rotation", epaper.PanelWaveshare42V2, 90, false, absentStatus(t)},
		{"service owns panel", epaper.PanelWaveshare42V2, 0, false, running},
	}
	for _, c := range cases {
		if code := runSplash(context.Background(), c.panel, c.rotation, c.force, c.status, open, io.Discard, io.Discard); code != exitRefused {
			t.Errorf("%s: exit %d, want %d", c.name, code, exitRefused)
		}
	}
	if opened != 0 {
		t.Errorf("hardware opened %d times by refused invocations, want 0", opened)
	}

	// -splash-force overrides the ownership guard only.
	if code := runSplash(context.Background(), epaper.PanelWaveshare42V2, 0, true, running, open, io.Discard, io.Discard); code != exitOK {
		t.Errorf("forced run exit %d, want %d", code, exitOK)
	}
}

func TestRunSplash_OpenFailureAndDriverFailureReleaseCorrectly(t *testing.T) {
	released := 0
	code := runSplash(context.Background(), epaper.PanelWaveshare42V2, 0, false, absentStatus(t),
		fakeOpener(nil, &released, errors.New("no /dev/gpiomem")), io.Discard, io.Discard)
	if code != exitFailure || released != 0 {
		t.Errorf("open failure: exit %d released %d, want %d and 0 (nothing was opened)", code, released, exitFailure)
	}

	// A BUSY timeout inside the driver must still release the hardware.
	bus := &fakeBus{waitIdleErr: context.DeadlineExceeded}
	released = 0
	code = runSplash(context.Background(), epaper.PanelWaveshare42V2, 0, false, absentStatus(t), fakeOpener(bus, &released, nil), io.Discard, io.Discard)
	if code != exitFailure {
		t.Errorf("BUSY timeout: exit %d, want %d", code, exitFailure)
	}
	if released != 1 {
		t.Errorf("BUSY timeout: hardware released %d times, want 1", released)
	}
}
