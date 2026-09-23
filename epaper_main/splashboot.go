package main

// splashboot.go: `epaperd -splash-boot`, the once-per-boot ARS splash run
// by debian/stratux_epaper_splash.service strictly before the operational
// stratux_epaper.service.
//
// It reuses the physically validated manual renderer (splash.go) and adds
// only what an unattended boot needs:
//
//   - The enable decision. The splash must respect the same owner intent
//     as the operational display - EpaperEnabled - but it runs before the
//     main daemon's HTTP API exists (and must not wait for it: the logo
//     should appear at power-on, not after stratux.service's
//     ExecStartPre, which can run OTA installs). So it reads the durable
//     source of that same setting directly: the stratux.conf file the
//     daemon itself loads. There is no second switch; EpaperEnabled is
//     the only authority.
//   - A tighter deadline than the manual command.
//   - "Not applicable" outcomes (disabled, no config, panel/rotation the
//     artwork does not support) are a clean exit 0 with a logged reason,
//     never a failed unit.
//
// The splash is cosmetic. Nothing depends on this process succeeding: the
// unit is only ordered Before= the operational renderer (no Requires/
// Wants/After), so any failure here - display absent, SPI/GPIO
// unavailable, BUSY timeout, bad asset, nonzero exit, timeout - leaves
// the operational renderer and all of Stratux to start normally.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/epaper"
)

const (
	// defaultBootConfigPath is where the main daemon loads its settings
	// from when running as root (main/gen_gdl90.go configLocation).
	defaultBootConfigPath = "/boot/firmware/stratux.conf"

	// maxBootConfigBytes mirrors main.readSettings, which reads at most
	// 10000 bytes and treats anything that then fails to parse as "use
	// defaults" (EpaperEnabled false). Matching it keeps this decision
	// identical to the daemon's for every file, including oversized ones.
	maxBootConfigBytes = 10000

	// bootSplashTimeout bounds the whole boot splash. A healthy run is
	// Init + Clear + one full refresh (~10-15 s); an absent display fails
	// on the first BUSY wait (~10 s). It must stay comfortably below the
	// unit's TimeoutStartSec (asserted by a test) so the process ends
	// itself, panel put to sleep, before systemd has to kill it.
	bootSplashTimeout = 45 * time.Second
)

// bootConfig is the subset of the daemon's settings this decision needs.
// Field names match the daemon's (json is case-insensitive, like its own
// Unmarshal into globalSettings).
type bootConfig struct {
	EpaperEnabled  bool
	EpaperPanel    string
	EpaperRotation int
}

// bootDecision is the outcome of reading the owner's e-paper intent.
type bootDecision struct {
	Run      bool
	Panel    string
	Rotation int
	Reason   string // why not, when Run is false
}

// decideBootSplash reports whether the boot splash should run for the
// settings in configPath. It never errors: every "can't tell" case
// resolves to not running, exactly as the daemon resolves it to
// EpaperEnabled=false.
func decideBootSplash(configPath string) bootDecision {
	skip := func(format string, a ...interface{}) bootDecision {
		return bootDecision{Reason: fmt.Sprintf(format, a...)}
	}

	f, err := os.Open(configPath)
	if err != nil {
		return skip("cannot read %s (%v); e-paper is disabled by default", configPath, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxBootConfigBytes))
	if err != nil {
		return skip("cannot read %s (%v)", configPath, err)
	}

	var bc bootConfig
	if err := json.Unmarshal(raw, &bc); err != nil {
		// A type mismatch leaves the offending field at its zero value
		// and decodes the rest - the same partial result the daemon's own
		// Unmarshal keeps. Anything else (syntax error, truncation)
		// decoded nothing, which the daemon also treats as defaults.
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return skip("cannot parse %s (%v); e-paper is disabled by default", configPath, err)
		}
	}
	if !bc.EpaperEnabled {
		return skip("EpaperEnabled is false")
	}

	cfg, err := epaper.Normalize(epaper.Config{
		Enabled:  true,
		Panel:    bc.EpaperPanel,
		Rotation: bc.EpaperRotation,
	})
	if err != nil {
		return skip("e-paper configuration is invalid (%v); the operational display will not start either", err)
	}
	if cfg.Panel != epaper.PanelWaveshare42V2 {
		return skip("the ARS splash is generated for %s only; configured panel is %s", epaper.PanelWaveshare42V2, cfg.Panel)
	}
	if cfg.Rotation != 0 && cfg.Rotation != 180 {
		return skip("the ARS splash supports rotation 0 and 180 only; configured rotation is %d", cfg.Rotation)
	}
	return bootDecision{Run: true, Panel: cfg.Panel, Rotation: cfg.Rotation}
}

// runSplashBoot is the whole `-splash-boot` command; it returns the
// process exit code: 0 for a drawn splash or a clean "not applicable"
// skip, 1 for a hardware/driver/asset failure, 2 if refused because the
// operational service appears to own the panel.
func runSplashBoot(ctx context.Context, configPath, statusPath string, timeout time.Duration, open busOpener, out, errOut io.Writer) int {
	d := decideBootSplash(configPath)
	if !d.Run {
		fmt.Fprintf(out, "boot splash skipped: %s\n", d.Reason)
		return exitOK
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// force=false: the ownership guard stays on. At boot the status file
	// does not exist yet (/run is a fresh tmpfs), so this only ever trips
	// if someone starts the splash unit by hand while the operational
	// renderer is already running - which must not double-own the panel.
	return runSplash(ctx, d.Panel, d.Rotation, false, statusPath, open, out, errOut)
}

// runSplashBootCommand wires runSplashBoot to the real hardware, status
// file, and SIGINT/SIGTERM (systemd's timeout sends SIGTERM; cancelling
// the context makes the driver stop waiting and put the panel to sleep).
func runSplashBootCommand(configPath string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runSplashBoot(ctx, configPath, common.EpaperStatusPath, bootSplashTimeout, openRealBus, os.Stdout, os.Stderr)
}
