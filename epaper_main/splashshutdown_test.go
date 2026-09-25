package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratux/stratux/epaper"
	"github.com/stratux/stratux/epaper/splash"
	"github.com/stratux/stratux/epaper/splash/assets"
)

// approvedBitmapSHA256 is the frozen production bitmap
// (docs/epaper-boot-splash.md). The shutdown splash reuses it verbatim.
const approvedBitmapSHA256 = "51039f8a375a2ecc44ed25fe7b6f373b31e7695e7a854bd192d605695b5b73dc"

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// jobsFn returns a jobLister that reports out (or err) and counts calls.
func jobsFn(out string, err error, calls *int) jobLister {
	return func(context.Context) (string, error) {
		*calls++
		return out, err
	}
}

// TestClassifyJobs pins the power-off / reboot / neither decision. The
// three testdata fixtures are the real `systemctl list-jobs` output that
// the ExecStop of stratux_epaper_shutdown.service saw on Debian 12
// (systemd 252) during `systemctl poweroff`, `halt` and `reboot`; the
// rest are hand-written edge cases.
func TestClassifyJobs(t *testing.T) {
	cases := []struct {
		name     string
		jobs     string
		want     shutdownKind
		wantJobs string
	}{
		{"real systemctl poweroff", fixture(t, "list-jobs-poweroff.txt"), shutdownPoweroff, "poweroff.target"},
		{"real systemctl halt", fixture(t, "list-jobs-halt.txt"), shutdownPoweroff, "halt.target"},
		{"real systemctl reboot", fixture(t, "list-jobs-reboot.txt"), shutdownReboot, "reboot.target"},
		{"kexec", "12 kexec.target start waiting\n13 systemd-kexec.service start waiting\n", shutdownReboot, "kexec.target"},
		{"soft-reboot", "12 soft-reboot.target start waiting\n", shutdownReboot, "soft-reboot.target"},
		{"empty queue", "", shutdownNone, ""},
		{"whitespace only", "\n  \n\t\n", shutdownNone, ""},
		{"ordinary service restart", "40 stratux_epaper.service stop running\n41 stratux_epaper.service start waiting\n", shutdownNone, ""},
		{"isolate rescue: a start job, but not a shutdown", "9 rescue.target start waiting\n10 stratux_epaper_shutdown.service stop running\n", shutdownNone, ""},
		{"a poweroff.target STOP job is not a shutdown", "9 poweroff.target stop waiting\n", shutdownNone, ""},
		{"poweroff's helper service alone is not the target job", "9 systemd-poweroff.service start waiting\n", shutdownNone, ""},
		{"unit name merely containing poweroff.target", "9 xpoweroff.target start waiting\n10 poweroff.target.bak start waiting\n", shutdownNone, ""},
		{"truncated rows are ignored", "poweroff.target\n7 poweroff.target\n", shutdownNone, ""},
		{"leading whitespace (--plain columns)", "   155 poweroff.target   start   waiting\n", shutdownPoweroff, "poweroff.target"},
		{"reboot wins if both are queued (uncertain which takes effect)", "1 poweroff.target start waiting\n2 reboot.target start waiting\n", shutdownReboot, "reboot.target,poweroff.target"},
		{"reboot wins regardless of row order", "2 reboot.target start waiting\n1 poweroff.target start waiting\n", shutdownReboot, "reboot.target,poweroff.target"},
	}
	for _, c := range cases {
		got, jobs := classifyJobs(c.jobs)
		if got != c.want || strings.Join(jobs, ",") != c.wantJobs {
			t.Errorf("%s: got kind=%d jobs=%v, want kind=%d jobs=%q", c.name, got, jobs, c.want, c.wantJobs)
		}
	}
}

func shutRun(t *testing.T, conf string, bus *fakeBus, status string, timeout time.Duration, openErr error, jobs string, jobsErr error) (code, opened, released, listed int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	open := func(epaper.GPIOMapping) (Bus, func(), error) {
		opened++
		if openErr != nil {
			return nil, nil, openErr
		}
		return bus, func() { released++ }, nil
	}
	code = runSplashShutdown(context.Background(), conf, status, timeout, open, jobsFn(jobs, jobsErr, &listed), &o, &e)
	return code, opened, released, listed, o.String(), e.String()
}

func poweroffJobs(t *testing.T) string { return fixture(t, "list-jobs-poweroff.txt") }

// The configuration gate: the shutdown splash makes exactly the boot
// splash's enable decision, and a "not applicable" outcome is a clean
// exit 0 that touches no hardware and does not even ask systemd anything.
func TestRunSplashShutdown_ConfigGate(t *testing.T) {
	cases := []struct {
		name string
		conf string
		want bool // draws
		why  string
	}{
		{"enabled, 4.2in V2, rotation 0", cfg42, true, ""},
		{"enabled, 4.2in V2, rotation 180", `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":180}`, true, ""},
		{"EpaperEnabled false", `{"EpaperEnabled":false,"EpaperPanel":"waveshare-4.2in-v2"}`, false, "EpaperEnabled is false"},
		{"EpaperEnabled absent", `{"EpaperPanel":"waveshare-4.2in-v2"}`, false, "EpaperEnabled is false"},
		{"unsupported panel (3.7in)", `{"EpaperEnabled":true,"EpaperPanel":"waveshare-3.7in"}`, false, "waveshare-3.7in"},
		{"panel absent defaults to the unsupported 3.7in", `{"EpaperEnabled":true}`, false, "waveshare-3.7in"},
		{"unknown panel", `{"EpaperEnabled":true,"EpaperPanel":"nope"}`, false, "invalid"},
		{"rotation 90", `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":90}`, false, "rotation 0 and 180 only"},
		{"rotation 270", `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":270}`, false, "rotation 0 and 180 only"},
		{"malformed JSON", `{"EpaperEnabled":true,`, false, "cannot parse"},
		{"not JSON", `EpaperEnabled=true`, false, "cannot parse"},
		{"empty file", ``, false, "cannot parse"},
	}
	for _, c := range cases {
		bus := &fakeBus{}
		code, opened, released, listed, out, _ := shutRun(t, writeConf(t, c.conf), bus, absentStatus(t), time.Minute, nil, poweroffJobs(t), nil)
		if code != exitOK {
			t.Errorf("%s: exit %d, want 0", c.name, code)
		}
		if c.want {
			if opened != 1 || released != 1 || len(bus.commands) == 0 {
				t.Errorf("%s: opened=%d released=%d cmds=%d, want a drawn splash", c.name, opened, released, len(bus.commands))
			}
			continue
		}
		if opened != 0 || released != 0 || len(bus.commands) != 0 || bus.resetCalls != 0 {
			t.Errorf("%s: hardware touched (opened=%d cmds=%d resets=%d)", c.name, opened, len(bus.commands), bus.resetCalls)
		}
		if listed != 0 {
			t.Errorf("%s: queried systemd although the configuration already said no", c.name)
		}
		if !strings.Contains(out, "shutdown splash skipped") || !strings.Contains(out, c.why) {
			t.Errorf("%s: logged %q, want a skip naming %q", c.name, out, c.why)
		}
	}
	// Missing configuration file.
	bus := &fakeBus{}
	code, opened, _, listed, out, _ := shutRun(t, filepath.Join(t.TempDir(), "absent.conf"), bus, absentStatus(t), time.Minute, nil, poweroffJobs(t), nil)
	if code != exitOK || opened != 0 || listed != 0 || !strings.Contains(out, "disabled by default") {
		t.Errorf("missing config: exit=%d opened=%d listed=%d out=%q", code, opened, listed, out)
	}
}

// Power-off and halt draw the approved splash once, leave the panel asleep,
// and release SPI/GPIO.
func TestRunSplashShutdown_PoweroffDrawsExactSplashOnceAndReleases(t *testing.T) {
	for _, fx := range []string{"list-jobs-poweroff.txt", "list-jobs-halt.txt"} {
		bus := &fakeBus{}
		status := absentStatus(t)
		code, opened, released, listed, out, errOut := shutRun(t, writeConf(t, cfg42), bus, status, time.Minute, nil, fixture(t, fx), nil)
		if code != exitOK || opened != 1 || released != 1 || listed != 1 {
			t.Fatalf("%s: exit=%d opened=%d released=%d listed=%d (stderr %q), want 0/1/1/1", fx, code, opened, released, listed, errOut)
		}
		if last := bus.commands[len(bus.commands)-1]; last != cmdDeepSleep {
			t.Errorf("%s: panel not left asleep: last command 0x%02X", fx, last)
		}
		want := assets.Bitmap()
		n := 0
		for _, d := range bus.data {
			if bytes.Equal(d, want) {
				n++
			}
		}
		if n != 2 {
			t.Errorf("%s: production bitmap written %d times, want 2 (both planes of ONE full refresh)", fx, n)
		}
		if !strings.Contains(out, "power-off in progress") || !strings.Contains(out, "released SPI/GPIO") {
			t.Errorf("%s: journal lines missing: %q", fx, out)
		}
		if _, err := os.Stat(status); !os.IsNotExist(err) {
			t.Errorf("%s: shutdown splash touched the operational status file (%v)", fx, err)
		}
	}
}

// The artwork is frozen: the bitmap the shutdown path draws is the exact
// approved production asset, not a copy.
func TestRunSplashShutdown_UsesTheApprovedAsset(t *testing.T) {
	sum := sha256.Sum256(assets.Bitmap())
	if got := hex.EncodeToString(sum[:]); got != approvedBitmapSHA256 {
		t.Fatalf("embedded production bitmap sha256 = %s, want the approved %s", got, approvedBitmapSHA256)
	}
	if _, err := splash.Validate(assets.Bitmap()); err != nil {
		t.Fatalf("approved bitmap fails validation: %v", err)
	}
	bus := &fakeBus{}
	shutRun(t, writeConf(t, cfg42), bus, absentStatus(t), time.Minute, nil, poweroffJobs(t), nil)
	want, _ := splashBitmap(0)
	for _, d := range bus.data {
		if bytes.Equal(d, want) {
			return
		}
	}
	t.Error("the approved bitmap never reached the controller")
}

func TestRunSplashShutdown_Rotation180(t *testing.T) {
	bus := &fakeBus{}
	conf := writeConf(t, `{"EpaperEnabled":true,"EpaperPanel":"waveshare-4.2in-v2","EpaperRotation":180}`)
	if code, _, _, _, _, _ := shutRun(t, conf, bus, absentStatus(t), time.Minute, nil, poweroffJobs(t), nil); code != exitOK {
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

// Not a power-off: no e-paper refresh, no hardware, exit 0. This is the
// reboot requirement (no shutdown refresh directly before the boot
// splash's) and the ordinary-stop requirement (restarting the unit, the
// package scripts, or isolating another target must never brand the panel).
func TestRunSplashShutdown_NotAPoweroffDrawsNothing(t *testing.T) {
	cases := []struct {
		name string
		jobs string
		err  error
		why  string
	}{
		{"reboot", fixture(t, "list-jobs-reboot.txt"), nil, "rebooting"},
		{"kexec", "1 kexec.target start waiting\n", nil, "rebooting"},
		{"poweroff then reboot both queued", "1 poweroff.target start waiting\n2 reboot.target start waiting\n", nil, "rebooting"},
		{"unit stopped on its own (systemctl stop/restart, package script)", "5 stratux_epaper_shutdown.service stop running\n", nil, "not a system power-off"},
		{"empty job queue", "", nil, "not a system power-off"},
		{"systemctl failed", "", errors.New("systemctl list-jobs: exit status 1"), "cannot tell whether this is a power-off"},
	}
	for _, c := range cases {
		bus := &fakeBus{}
		status := absentStatus(t)
		code, opened, released, listed, out, errOut := shutRun(t, writeConf(t, cfg42), bus, status, time.Minute, nil, c.jobs, c.err)
		if code != exitOK {
			t.Errorf("%s: exit %d, want 0 (a skip is never a failure)", c.name, code)
		}
		if listed != 1 {
			t.Errorf("%s: asked systemd %d times, want 1", c.name, listed)
		}
		if opened != 0 || released != 0 || len(bus.commands) != 0 || bus.resetCalls != 0 {
			t.Errorf("%s: hardware touched (opened=%d cmds=%d resets=%d)", c.name, opened, len(bus.commands), bus.resetCalls)
		}
		if !strings.Contains(out, "shutdown splash skipped") || !strings.Contains(out, c.why) || errOut != "" {
			t.Errorf("%s: stdout %q stderr %q, want a skip naming %q", c.name, out, errOut, c.why)
		}
		if _, err := os.Stat(status); !os.IsNotExist(err) {
			t.Errorf("%s: touched the operational status file", c.name)
		}
	}
}

// Every failure class is bounded, non-zero, releases whatever it opened, and
// writes a diagnostic - so the unit fails visibly in the journal while the
// shutdown itself simply continues.
func TestRunSplashShutdown_FailuresAreBoundedAndReleaseHardware(t *testing.T) {
	conf := writeConf(t, cfg42)
	cases := []struct {
		name         string
		bus          *fakeBus
		openErr      error
		wantReleased int
	}{
		{"GPIO/SPI unavailable", &fakeBus{}, errors.New("no /dev/gpiomem"), 0},
		{"display absent / BUSY never idle", &fakeBus{waitIdleErr: context.DeadlineExceeded}, nil, 1},
		{"SPI write error", &fakeBus{sendCommandErr: errors.New("spi: write failed")}, nil, 1},
		{"reset error", &fakeBus{resetErr: errors.New("gpio: reset failed")}, nil, 1},
	}
	for _, c := range cases {
		status := absentStatus(t)
		code, opened, released, _, _, errOut := shutRun(t, conf, c.bus, status, time.Minute, c.openErr, poweroffJobs(t), nil)
		if code != exitFailure {
			t.Errorf("%s: exit %d, want %d", c.name, code, exitFailure)
		}
		if opened != 1 || released != c.wantReleased {
			t.Errorf("%s: opened=%d released=%d, want 1/%d", c.name, opened, released, c.wantReleased)
		}
		if errOut == "" {
			t.Errorf("%s: no diagnostic on stderr", c.name)
		}
		if _, err := os.Stat(status); !os.IsNotExist(err) {
			t.Errorf("%s: touched the operational status file", c.name)
		}
	}
}

// A BUSY line that never idles must not hold the shutdown: the deadline
// cuts the run off, the panel is put to sleep, the hardware is released.
func TestRunSplashShutdown_TimeoutIsBounded(t *testing.T) {
	bus := &fakeBus{busyForCalls: 1 << 20} // every WaitIdle blocks until its context ends
	start := time.Now()
	code, _, released, _, _, _ := shutRun(t, writeConf(t, cfg42), bus, absentStatus(t), 150*time.Millisecond, nil, poweroffJobs(t), nil)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stuck BUSY held the shutdown for %v despite a 150ms deadline", took)
	}
	if code != exitFailure {
		t.Errorf("exit %d, want %d", code, exitFailure)
	}
	if released != 1 {
		t.Errorf("hardware released %d times, want 1", released)
	}
}

// A wedged systemd query is inside the same deadline: no hardware, exit 0.
func TestRunSplashShutdown_HungSystemdQueryIsBounded(t *testing.T) {
	var opened int
	open := func(epaper.GPIOMapping) (Bus, func(), error) { opened++; return &fakeBus{}, func() {}, nil }
	hang := func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	var o, e bytes.Buffer
	start := time.Now()
	code := runSplashShutdown(context.Background(), writeConf(t, cfg42), absentStatus(t), 150*time.Millisecond, open, hang, &o, &e)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("hung query held the shutdown for %v", took)
	}
	if code != exitOK || opened != 0 {
		t.Errorf("exit=%d opened=%d, want 0/0", code, opened)
	}
}

func TestRunSplashShutdown_CorruptEmbeddedAssetFailsBeforeHardware(t *testing.T) {
	orig := loadBitmap
	defer func() { loadBitmap = orig }()
	loadBitmap = func() []byte { return make([]byte, 100) }
	code, opened, _, _, _, errOut := shutRun(t, writeConf(t, cfg42), &fakeBus{}, absentStatus(t), time.Minute, nil, poweroffJobs(t), nil)
	if code != exitFailure || opened != 0 || !strings.Contains(errOut, "embedded splash asset is invalid") {
		t.Errorf("exit=%d opened=%d stderr=%q", code, opened, errOut)
	}
}

// Second, independent line of defense for panel ownership (the first is
// the unit ordering): if the operational renderer appears to be running,
// refuse without opening anything.
func TestRunSplashShutdown_NeverOpensPanelWhileOperationalRendererRuns(t *testing.T) {
	bus := &fakeBus{}
	running := writeStatus(t, epaper.StateRunning, time.Second)
	code, opened, _, _, _, errOut := shutRun(t, writeConf(t, cfg42), bus, running, time.Minute, nil, poweroffJobs(t), nil)
	if code != exitRefused || opened != 0 || len(bus.commands) != 0 {
		t.Errorf("exit=%d opened=%d cmds=%d, want %d/0/0", code, opened, len(bus.commands), exitRefused)
	}
	if !strings.Contains(errOut, "may own the panel") {
		t.Errorf("stderr %q", errOut)
	}
}

func TestShutdownSplashTimeoutMatchesBootPolicy(t *testing.T) {
	if shutdownSplashTimeout != bootSplashTimeout {
		t.Errorf("shutdownSplashTimeout=%v, bootSplashTimeout=%v: the shutdown splash reuses the validated boot bounds", shutdownSplashTimeout, bootSplashTimeout)
	}
	if jobQueryTimeout >= shutdownSplashTimeout {
		t.Errorf("jobQueryTimeout=%v must be a small part of the %v deadline", jobQueryTimeout, shutdownSplashTimeout)
	}
}

// fakeSystemctl puts a stub `systemctl` first on PATH that records its
// arguments and environment, then runs body.
func fakeSystemctl(t *testing.T, body string) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" > '" + argsFile + "'\necho \"LC_ALL=$LC_ALL\" >> '" + argsFile + "'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func TestSystemctlListJobs(t *testing.T) {
	args := fakeSystemctl(t, "echo '155 poweroff.target start waiting'")
	out, err := systemctlListJobs(context.Background())
	if err != nil || !strings.Contains(out, "poweroff.target") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	rec, _ := os.ReadFile(args)
	if !strings.Contains(string(rec), "list-jobs --no-legend --plain --full --no-pager") || !strings.Contains(string(rec), "LC_ALL=C") {
		t.Errorf("systemctl invoked as %q", rec)
	}

	fakeSystemctl(t, "echo 'Failed to connect to bus' >&2; exit 1")
	if _, err := systemctlListJobs(context.Background()); err == nil {
		t.Error("a failing systemctl must surface as an error, not as an empty (\"no shutdown\") queue")
	}
}

func TestSystemctlListJobs_HungSystemctlIsBoundedByCallerContext(t *testing.T) {
	fakeSystemctl(t, "exec sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := systemctlListJobs(ctx); err == nil {
		t.Error("hung systemctl returned no error")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("hung systemctl held the caller for %v", took)
	}
}

// --- CLI wiring: run the real main() in a subprocess ----------------------

// TestMain lets a test re-execute this binary as `epaperd <args>`.
func TestMain(m *testing.M) {
	if args := os.Getenv("EPAPERD_TEST_ARGS"); args != "" || os.Getenv("EPAPERD_TEST_MAIN") == "1" {
		os.Args = append([]string{"epaperd"}, strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runEpaperd(t *testing.T, args string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "EPAPERD_TEST_MAIN=1", "EPAPERD_TEST_ARGS="+args)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), o.String(), e.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, o.String(), e.String()
}

// The flag wiring, end to end, on hardware-free paths only (a missing or
// disabled config skips before any hardware or systemd is touched).
func TestCLI_SplashModesAreSeparateAndBootIsUnchanged(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.conf")
	disabled := writeConf(t, `{"EpaperEnabled":false}`)

	code, out, _ := runEpaperd(t, "-splash-shutdown -splash-config "+absent)
	if code != 0 || !strings.Contains(out, "shutdown splash skipped") {
		t.Errorf("-splash-shutdown: exit %d stdout %q", code, out)
	}
	code, out, _ = runEpaperd(t, "-splash-shutdown -splash-config "+disabled)
	if code != 0 || !strings.Contains(out, "EpaperEnabled is false") {
		t.Errorf("-splash-shutdown (disabled): exit %d stdout %q", code, out)
	}

	// Unchanged: -splash-boot still runs the boot decision and says "boot".
	code, out, _ = runEpaperd(t, "-splash-boot -splash-config "+absent)
	if code != 0 || !strings.Contains(out, "boot splash skipped") || strings.Contains(out, "shutdown") {
		t.Errorf("-splash-boot: exit %d stdout %q", code, out)
	}

	// Modes are mutually exclusive with -splash-shutdown.
	for _, args := range []string{"-splash-shutdown -splash-boot", "-splash-shutdown -splash"} {
		code, out, errOut := runEpaperd(t, args+" -splash-config "+absent)
		if code != exitRefused || out != "" || !strings.Contains(errOut, "cannot be combined") {
			t.Errorf("%q: exit %d stdout %q stderr %q, want a refusal with exit %d", args, code, out, errOut, exitRefused)
		}
	}
}
