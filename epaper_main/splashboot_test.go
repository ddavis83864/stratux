package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratux/stratux/epaper"
	"github.com/stratux/stratux/epaper/splash"
	"github.com/stratux/stratux/epaper/splash/assets"
)

const cfg42 = `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":0}`

func writeConf(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stratux.conf")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDecideBootSplash pins the enable decision. The reference behavior is
// main.readSettings: defaults (EpaperEnabled false), then the file's JSON
// overlaid, read as at most 10000 bytes, any parse failure => defaults.
func TestDecideBootSplash(t *testing.T) {
	pad := func(n int) string { return strings.Repeat("x", n) }
	exactly10000 := `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","P":"`
	exactly10000 += pad(10000-len(exactly10000)-2) + `"}`
	if len(exactly10000) != 10000 {
		t.Fatalf("test bug: %d bytes", len(exactly10000))
	}
	over := `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","P":"` + pad(10500) + `"}`

	cases := []struct {
		name    string
		conf    string // "" with missing=true means no file
		missing bool
		wantRun bool
		wantRot int
		wantWhy string // substring of Reason when !wantRun
	}{
		{name: "no config file", missing: true, wantWhy: "e-paper is disabled by default"},
		{name: "empty object", conf: `{}`, wantWhy: "EpaperEnabled is false"},
		{name: "explicitly disabled", conf: `{"EpaperEnabled":false,"EpaperPanel":"waveshare-4.2in-v2"}`, wantWhy: "EpaperEnabled is false"},
		{name: "enabled 4.2 rot 0", conf: cfg42, wantRun: true, wantRot: 0},
		{name: "enabled 4.2 rot 180", conf: `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":180}`, wantRun: true, wantRot: 180},
		{name: "enabled 4.2 rotation absent", conf: `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2"}`, wantRun: true, wantRot: 0},
		{name: "enabled, panel absent (defaults to 3.7in)", conf: `{"EpaperEnabled":true}`, wantWhy: "waveshare-3.7in"},
		{name: "enabled 3.7in", conf: `{"EpaperEnabled":true,"EpaperPanel":"waveshare-3.7in"}`, wantWhy: "waveshare-3.7in"},
		{name: "unknown panel (operational display refuses it too)", conf: `{"EpaperEnabled":true,"EpaperPanel":"nope"}`, wantWhy: "invalid"},
		{name: "rotation 90", conf: `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":90}`, wantWhy: "rotation 0 and 180 only"},
		{name: "rotation 270", conf: `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":270}`, wantWhy: "rotation 0 and 180 only"},
		{name: "rotation invalid", conf: `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":45}`, wantWhy: "invalid"},
		{name: "not JSON", conf: `EpaperEnabled=true`, wantWhy: "cannot parse"},
		{name: "truncated JSON", conf: `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2`, wantWhy: "cannot parse"},
		{name: "empty file", conf: ``, wantWhy: "cannot parse"},
		{name: "exactly 10000 bytes still parses", conf: exactly10000, wantRun: true},
		{name: "over 10000 bytes: daemon truncates and falls back to defaults", conf: over, wantWhy: "cannot parse"},
		{name: "unrelated field of the wrong type does not change the decision (daemon keeps the rest)", conf: `{"UAT_Enabled":"yes","EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2"}`, wantRun: true},
		{name: "json keys are case-insensitive like the daemon's", conf: `{"epaperenabled":true,"epaperpanel":"waveshare-4.2in-v2"}`, wantRun: true},
		{name: "EpaperEnabled of the wrong type stays disabled", conf: `{"EpaperEnabled":"yes","EpaperPanel":"waveshare-4.2in-v2"}`, wantWhy: "EpaperEnabled is false"},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "absent.conf")
		if !c.missing {
			path = writeConf(t, c.conf)
		}
		d := decideBootSplash(path)
		if d.Run != c.wantRun {
			t.Errorf("%s: Run=%v (reason %q), want %v", c.name, d.Run, d.Reason, c.wantRun)
			continue
		}
		if c.wantRun {
			if d.Panel != epaper.PanelWaveshare42V2 || d.Rotation != c.wantRot {
				t.Errorf("%s: panel=%q rotation=%d, want %q/%d", c.name, d.Panel, d.Rotation, epaper.PanelWaveshare42V2, c.wantRot)
			}
		} else if !strings.Contains(d.Reason, c.wantWhy) {
			t.Errorf("%s: reason %q lacks %q", c.name, d.Reason, c.wantWhy)
		}
	}
}

func bootRun(t *testing.T, conf string, bus *fakeBus, status string, timeout time.Duration, openErr error) (code, opened, released int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	open := func(epaper.GPIOMapping) (Bus, func(), error) {
		opened++
		if openErr != nil {
			return nil, nil, openErr
		}
		return bus, func() { released++ }, nil
	}
	code = runSplashBoot(context.Background(), conf, status, timeout, open, &o, &e)
	return code, opened, released, o.String(), e.String()
}

func TestRunSplashBoot_DisabledTouchesNoHardware(t *testing.T) {
	for name, conf := range map[string]string{
		"disabled":     writeConf(t, `{"EpaperEnabled":false,"EpaperPanel":"waveshare-4.2in-v2"}`),
		"no config":    filepath.Join(t.TempDir(), "absent.conf"),
		"3.7in panel":  writeConf(t, `{"EpaperEnabled":true,"EpaperPanel":"waveshare-3.7in"}`),
		"rotation 90":  writeConf(t, `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":90}`),
		"corrupt conf": writeConf(t, `{{{`),
	} {
		bus := &fakeBus{}
		code, opened, released, out, _ := bootRun(t, conf, bus, absentStatus(t), time.Minute, nil)
		if code != exitOK {
			t.Errorf("%s: exit %d, want 0 (not applicable is not a failure)", name, code)
		}
		if opened != 0 || released != 0 || len(bus.commands) != 0 || bus.resetCalls != 0 {
			t.Errorf("%s: hardware touched (opened=%d cmds=%d resets=%d)", name, opened, len(bus.commands), bus.resetCalls)
		}
		if !strings.Contains(out, "boot splash skipped") {
			t.Errorf("%s: no logged reason: %q", name, out)
		}
	}
}

func TestRunSplashBoot_EnabledDrawsExactSplashAndReleases(t *testing.T) {
	bus := &fakeBus{}
	status := absentStatus(t)
	code, opened, released, out, _ := bootRun(t, writeConf(t, cfg42), bus, status, time.Minute, nil)
	if code != exitOK || opened != 1 || released != 1 {
		t.Fatalf("exit=%d opened=%d released=%d, want 0/1/1", code, opened, released)
	}
	if last := bus.commands[len(bus.commands)-1]; last != cmdDeepSleep {
		t.Errorf("panel not left asleep: last command 0x%02X", last)
	}
	want := assets.Bitmap()
	n := 0
	for _, d := range bus.data {
		if bytes.Equal(d, want) {
			n++
		}
	}
	if n != 2 {
		t.Errorf("production bitmap written %d times to controller RAM, want 2 (both planes of the full refresh)", n)
	}
	if !strings.Contains(out, "released SPI/GPIO") {
		t.Errorf("no release line in output: %q", out)
	}
	// The splash must never write the operational renderer's own status
	// file: it does not masquerade as, or pre-empt, the operational service.
	if _, err := os.Stat(status); !os.IsNotExist(err) {
		t.Errorf("boot splash created/modified the operational status file (%v)", err)
	}
}

func TestRunSplashBoot_Rotation180(t *testing.T) {
	bus := &fakeBus{}
	conf := writeConf(t, `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":180}`)
	if code, _, _, _, _ := bootRun(t, conf, bus, absentStatus(t), time.Minute, nil); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	want, _ := splashBitmap(180)
	for _, d := range bus.data {
		if bytes.Equal(d, want) {
			return
		}
	}
	t.Error("rotated (180) bitmap never reached the controller")
}

// Every failure class ends in a bounded, non-zero exit with the hardware
// released (so the operational renderer can open it next) - and never
// touches the operational status file.
func TestRunSplashBoot_FailuresAreBoundedAndReleaseHardware(t *testing.T) {
	conf := writeConf(t, cfg42)
	cases := []struct {
		name         string
		bus          *fakeBus
		openErr      error
		wantOpened   int
		wantReleased int
	}{
		{"GPIO/SPI unavailable", &fakeBus{}, errors.New("no /dev/gpiomem"), 1, 0},
		{"display absent / BUSY never idle", &fakeBus{waitIdleErr: context.DeadlineExceeded}, nil, 1, 1},
		{"SPI write error", &fakeBus{sendCommandErr: errors.New("spi: write failed")}, nil, 1, 1},
		{"reset error", &fakeBus{resetErr: errors.New("gpio: reset failed")}, nil, 1, 1},
	}
	for _, c := range cases {
		status := absentStatus(t)
		code, opened, released, _, errOut := bootRun(t, conf, c.bus, status, time.Minute, c.openErr)
		if code != exitFailure {
			t.Errorf("%s: exit %d, want %d", c.name, code, exitFailure)
		}
		if opened != c.wantOpened || released != c.wantReleased {
			t.Errorf("%s: opened=%d released=%d, want %d/%d", c.name, opened, released, c.wantOpened, c.wantReleased)
		}
		if errOut == "" {
			t.Errorf("%s: no diagnostic on stderr", c.name)
		}
		if _, err := os.Stat(status); !os.IsNotExist(err) {
			t.Errorf("%s: touched the operational status file", c.name)
		}
	}
}

// A BUSY line that never idles must not hold the boot: the whole run is
// cut off by the deadline, the hardware is released, and the exit is
// non-zero.
func TestRunSplashBoot_TimeoutIsBounded(t *testing.T) {
	bus := &fakeBus{busyForCalls: 1 << 20} // every WaitIdle blocks until its context ends
	start := time.Now()
	code, _, released, _, _ := bootRun(t, writeConf(t, cfg42), bus, absentStatus(t), 150*time.Millisecond, nil)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stuck BUSY held the run for %v despite a 150ms deadline", took)
	}
	if code != exitFailure {
		t.Errorf("exit %d, want %d", code, exitFailure)
	}
	if released != 1 {
		t.Errorf("hardware released %d times, want 1", released)
	}
}

func TestRunSplashBoot_CorruptEmbeddedAssetFailsBeforeHardware(t *testing.T) {
	orig := loadBitmap
	defer func() { loadBitmap = orig }()
	for name, bm := range map[string][]byte{
		"short": make([]byte, 100),
		"blank": bytes.Repeat([]byte{0xFF}, splash.BitmapLen),
		"inverted": func() []byte {
			b := orig()
			for i := range b {
				b[i] ^= 0xFF
			}
			return b
		}(),
	} {
		bm := bm
		loadBitmap = func() []byte { return bm }
		code, opened, _, _, errOut := bootRun(t, writeConf(t, cfg42), &fakeBus{}, absentStatus(t), time.Minute, nil)
		if code != exitFailure || opened != 0 {
			t.Errorf("%s asset: exit %d opened %d, want %d and 0", name, code, opened, exitFailure)
		}
		if !strings.Contains(errOut, "embedded splash asset is invalid") {
			t.Errorf("%s asset: stderr %q", name, errOut)
		}
	}
}

// No concurrent display ownership: if the operational renderer is (or
// appears to be) running, the splash refuses without opening anything.
func TestRunSplashBoot_NeverOpensPanelWhileOperationalRendererRuns(t *testing.T) {
	bus := &fakeBus{}
	running := writeStatus(t, epaper.StateRunning, time.Second)
	code, opened, _, _, errOut := bootRun(t, writeConf(t, cfg42), bus, running, time.Minute, nil)
	if code != exitRefused || opened != 0 || len(bus.commands) != 0 {
		t.Errorf("exit=%d opened=%d cmds=%d, want %d/0/0", code, opened, len(bus.commands), exitRefused)
	}
	if !strings.Contains(errOut, "may own the panel") {
		t.Errorf("stderr %q", errOut)
	}
}

func TestRunSplashBoot_DoesNotWriteOutsideItsOwnOutputs(t *testing.T) {
	// Sanity: the boot path is silent on the writer it is given except for
	// its progress lines (no panics on io.Discard either).
	code := runSplashBoot(context.Background(), writeConf(t, cfg42), absentStatus(t), time.Minute,
		func(epaper.GPIOMapping) (Bus, func(), error) { return &fakeBus{}, func() {}, nil }, io.Discard, io.Discard)
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
}
