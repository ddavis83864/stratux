package main

// splashshutdown.go: `epaperd -splash-shutdown`, the orderly-power-off ARS
// splash run by debian/stratux_epaper_shutdown.service as its ExecStop,
// after the operational renderer (and the boot splash) have stopped and
// released the panel.
//
// It is a thin lifecycle layer around the physically validated renderer
// (splash.go, via runSplash): the same embedded bitmap, panel driver,
// Init -> Clear -> Update(full) -> Sleep sequence, BUSY timeouts, rotation
// handling and SPI/GPIO release. It adds only two decisions:
//
//   - Enable: the same one the boot splash makes (decideBootSplash):
//     EpaperEnabled in /boot/firmware/stratux.conf, the Waveshare 4.2in V2
//     panel, rotation 0 or 180. There is no second switch.
//
//   - "Is this a power-off?" ExecStop runs on EVERY stop of the unit -
//     power-off, halt, reboot, kexec, `systemctl stop/restart`, isolating
//     another target - and systemd gives an ExecStop no direct way to tell
//     which. So the process asks systemd itself: the shutdown transaction
//     always has a start job for exactly one of poweroff.target /
//     halt.target (draw) or reboot.target / kexec.target (do not draw)
//     queued for as long as units are being stopped, and a plain service
//     stop or restart has none. See classifyJobs. Anything that cannot be
//     positively identified as a power-off does not draw: the splash is
//     cosmetic, and an unnecessary e-paper full refresh (a reboot would
//     otherwise refresh at shutdown and again at boot) is the cost we are
//     avoiding.
//
// Panel ownership is NOT enforced here; it is enforced by the unit's
// ordering (docs/epaper-shutdown-splash.md). This process only ever runs
// after systemd has finished stopping stratux_epaper.service and
// stratux_epaper_splash.service, and runSplash's own status-file guard
// stays on as a second, independent refusal.
//
// Bounded and cosmetic: any failure - display absent, SPI/GPIO
// unavailable, BUSY timeout, cannot query systemd - is logged, releases
// the hardware where it was opened, and lets the shutdown continue. The
// whole command has its own deadline, comfortably below the unit's
// TimeoutStopSec (asserted by a test).

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/stratux/stratux/common"
)

const (
	// shutdownSplashTimeout bounds the whole shutdown splash, including
	// the systemd query. Same figure and reasoning as bootSplashTimeout: a
	// healthy run is ~10-15 s, an absent display fails on the first BUSY
	// wait (~10 s), and the process must end itself - panel put to sleep -
	// before systemd's TimeoutStopSec has to kill it.
	shutdownSplashTimeout = 45 * time.Second

	// jobQueryTimeout bounds the `systemctl list-jobs` call. It talks to
	// PID 1 over a local socket and returns in milliseconds; this only
	// matters if PID 1 is wedged, in which case not drawing is right.
	jobQueryTimeout = 5 * time.Second

	// safeToRemovePowerPause is issue #43's fix: the operational
	// renderer's own "Stratux is shut down. / Safe to remove power."
	// screen was confirmed, on real hardware, to be drawn correctly and
	// to be perfectly legible when shown in isolation - the panel and
	// the bitmap content were never the problem. What was missing is
	// this: this unit's own Init()+Clear() (about to run) begins
	// overwriting that screen roughly 60-100ms after the operational
	// renderer's Update() call reports success, which is not enough
	// time for a human to ever perceive the settled frame before it is
	// cleared for the ARS splash - confirmed by two independent
	// owner-recorded real shutdowns, both showing the panel go directly
	// from the live dashboard through one continuous flash into the ARS
	// splash with no legible pause in between, even though a
	// synchronous, fsync'd diagnostic trace of a third real shutdown
	// showed the text draw completing with no error. Pausing here, once
	// a real power-off is already confirmed (never on a reboot or a
	// plain service stop, where nothing draws over this screen anyway),
	// gives that already-correct message time to actually be seen
	// before this process clears it.
	safeToRemovePowerPause = 3 * time.Second
)

// shutdownKind is what the systemd job queue says the system is doing.
type shutdownKind int

const (
	shutdownNone     shutdownKind = iota // no system shutdown transaction is queued
	shutdownPoweroff                     // poweroff or halt
	shutdownReboot                       // reboot or kexec
)

// Targets whose start job identifies the kind of system shutdown. systemctl
// poweroff/halt/reboot/kexec, shutdown(8), init(1) runlevels, logind's power
// key and Stratux's own `systemctl poweroff`/`systemctl reboot` all enqueue
// exactly one of these (aliases such as runlevel0.target and
// ctrl-alt-del.target are reported under their canonical names).
var (
	poweroffTargets = map[string]bool{"poweroff.target": true, "halt.target": true}
	rebootTargets   = map[string]bool{"reboot.target": true, "kexec.target": true, "soft-reboot.target": true}
)

// classifyJobs interprets `systemctl list-jobs --no-legend --plain` output
// (one "ID UNIT TYPE STATE" row per queued job). Reboot wins if both
// kinds are somehow queued (systemd does not reliably cancel an earlier
// request when a later, conflicting one arrives): which one takes effect
// is then uncertain, and skipping the splash is the harmless way to be
// wrong. The returned names are the matching jobs, for the log.
func classifyJobs(listJobs string) (shutdownKind, []string) {
	var power, reboot []string
	for _, line := range strings.Split(listJobs, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[2] != "start" {
			continue
		}
		switch {
		case poweroffTargets[f[1]]:
			power = append(power, f[1])
		case rebootTargets[f[1]]:
			reboot = append(reboot, f[1])
		}
	}
	switch {
	case len(reboot) > 0:
		return shutdownReboot, append(reboot, power...)
	case len(power) > 0:
		return shutdownPoweroff, power
	}
	return shutdownNone, nil
}

// jobLister returns the raw `systemctl list-jobs` output. Injected so tests
// need no systemd.
type jobLister func(ctx context.Context) (string, error)

// systemctlListJobs asks the running systemd manager for its job queue.
// PID 1 keeps serving this while it stops units, which is exactly when it
// is called.
func systemctlListJobs(ctx context.Context) (string, error) {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		path = "/usr/bin/systemctl"
	}
	ctx, cancel := context.WithTimeout(ctx, jobQueryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "list-jobs", "--no-legend", "--plain", "--full", "--no-pager")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("systemctl list-jobs: %w", err)
	}
	return string(out), nil
}

// runSplashShutdown is the whole `-splash-shutdown` command; it returns the
// process exit code with the same meaning as -splash-boot: 0 for a drawn
// splash or a clean "not applicable" skip, 1 for a hardware/driver/asset
// failure, 2 if refused because the operational service appears to own the
// panel.
func runSplashShutdown(ctx context.Context, configPath, statusPath string, timeout, pause time.Duration, open busOpener, listJobs jobLister, out, errOut io.Writer) int {
	d := decideBootSplash(configPath)
	if !d.Run {
		fmt.Fprintf(out, "shutdown splash skipped: %s\n", d.Reason)
		return exitOK
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	raw, err := listJobs(ctx)
	if err != nil {
		fmt.Fprintf(out, "shutdown splash skipped: cannot tell whether this is a power-off (%v)\n", err)
		return exitOK
	}
	kind, jobs := classifyJobs(raw)
	switch kind {
	case shutdownReboot:
		fmt.Fprintf(out, "shutdown splash skipped: the system is rebooting (%s); the boot splash follows\n", strings.Join(jobs, ", "))
		return exitOK
	case shutdownNone:
		fmt.Fprintln(out, "shutdown splash skipped: not a system power-off (the unit was stopped on its own)")
		return exitOK
	}
	fmt.Fprintf(out, "shutdown splash: power-off in progress (%s)\n", strings.Join(jobs, ", "))
	if pause > 0 {
		fmt.Fprintf(out, "shutdown splash: pausing %s so the operational renderer's \"safe to remove power\" screen can actually be seen before it is cleared\n", pause)
		// Deliberately unconditional (time.Sleep, not a ctx-cancellable
		// select): a first attempt made this pause cut-shortable by
		// ctx.Done(), reasoning that a bounded, cosmetic wait should
		// never be why systemd has to SIGKILL this unit. Physical
		// testing (two owner-recorded, frame-differenced real
		// shutdowns) showed that reasoning was wrong in practice - the
		// pause was being cut short to well under a second, consistent
		// with systemd's own very-late shutdown signal sweep (sent to
		// any processes still running as poweroff approaches) reaching
		// this process during the wait and firing ctx.Done()
		// immediately. 3 seconds is short enough, and every timeout in
		// this unit generous enough (60s TimeoutStopSec, 45s
		// shutdownSplashTimeout), that sleeping through that signal
		// here is the correct, deliberate choice - not an oversight.
		time.Sleep(pause)
	}

	fmt.Fprintln(out, "shutdown splash: drawing the ARS splash")
	// force=false: the ownership guard stays on, as for the boot splash.
	// Under the unit's ordering the operational renderer's status file
	// (in its RuntimeDirectory) is already gone, so this only trips if
	// something has broken that ordering - and then refusing is right.
	return runSplash(ctx, d.Panel, d.Rotation, false, statusPath, open, out, errOut)
}

// runSplashShutdownCommand wires runSplashShutdown to the real hardware,
// status file, systemd, and SIGINT/SIGTERM (systemd's TimeoutStopSec sends
// SIGTERM; cancelling the context makes the driver stop waiting and put
// the panel to sleep).
func runSplashShutdownCommand(configPath string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runSplashShutdown(ctx, configPath, common.EpaperStatusPath, shutdownSplashTimeout, safeToRemovePowerPause, openRealBus, systemctlListJobs, os.Stdout, os.Stderr)
}
