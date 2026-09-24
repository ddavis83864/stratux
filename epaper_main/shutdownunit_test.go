package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests pin the systemd/packaging structure of the shutdown splash.
// Like bootunit_test.go they parse the real unit files in debian/, never
// copies. What they cannot prove - the actual behavior of a systemd
// shutdown transaction - is covered by test/epaper_shutdown_systemd_lab.sh
// (real Debian 12 systemd, real unit files, real epaperd) and by the
// recorded measurements in docs/epaper-shutdown-splash.md.

const shutdownUnitPath = "../debian/stratux_epaper_shutdown.service"

// nonCommentLines returns the unit file's directive lines.
func nonCommentLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, ";") {
			out = append(out, l)
		}
	}
	return out
}

func TestShutdownUnit_StructureAndBoundedTimeouts(t *testing.T) {
	u := parseUnit(t, shutdownUnitPath)

	if got := u.one(t, "Service", "Type"); got != "oneshot" {
		t.Errorf("Type=%s, want oneshot", got)
	}
	if got := u.one(t, "Service", "RemainAfterExit"); got != "yes" {
		t.Errorf("RemainAfterExit=%s, want yes: the unit must be ACTIVE from boot so systemd runs its ExecStop at shutdown", got)
	}
	// The arming step touches no hardware; the hardware work is ExecStop only.
	if got := u.one(t, "Service", "ExecStart"); got != "/bin/true" {
		t.Errorf("ExecStart=%q, want /bin/true (arming only; no hardware at start)", got)
	}
	if got := u.one(t, "Service", "ExecStop"); got != epaperdPath+" -splash-shutdown" {
		t.Errorf("ExecStop=%q, want %q", got, epaperdPath+" -splash-shutdown")
	}
	for _, key := range []string{"ExecStartPre", "ExecStartPost", "ExecStopPost", "ExecReload", "ExecCondition"} {
		if v := u["Service"][key]; len(v) != 0 {
			t.Errorf("%s=%v: the shutdown unit has exactly one action, ExecStop", key, v)
		}
	}
	if got := u.one(t, "Service", "Restart"); got != "no" {
		t.Errorf("Restart=%s, want no (bounded: a failed cosmetic splash is never retried)", got)
	}
	if got := u.words("Install", "WantedBy"); len(got) != 1 || got[0] != "multi-user.target" {
		t.Errorf("WantedBy=%v, want exactly multi-user.target (enabled like the other e-paper units; NOT pulled in by any shutdown target)", got)
	}

	// Bounded: the process's own deadline must expire strictly before
	// systemd's backstop, and the backstop itself must be modest - it is
	// the worst-case time this unit can add to a shutdown.
	stop, err := strconv.Atoi(u.one(t, "Service", "TimeoutStopSec"))
	if err != nil {
		t.Fatalf("TimeoutStopSec=%v must be a plain number of seconds", u["Service"]["TimeoutStopSec"])
	}
	if time.Duration(stop)*time.Second <= shutdownSplashTimeout {
		t.Errorf("TimeoutStopSec=%ds must exceed the process's own %v deadline so the process ends itself (panel put to sleep) before systemd kills it", stop, shutdownSplashTimeout)
	}
	if stop > 120 {
		t.Errorf("TimeoutStopSec=%ds: the worst-case added shutdown time must stay small", stop)
	}
	if start, err := strconv.Atoi(u.one(t, "Service", "TimeoutStartSec")); err != nil || start > 30 {
		t.Errorf("TimeoutStartSec must be a small bounded number of seconds, got %v", u["Service"]["TimeoutStartSec"])
	}
}

// The ordering that makes panel ownership exclusive. See the unit file and
// docs/epaper-shutdown-splash.md for why it is written for the STOP
// direction.
func TestShutdownUnit_OrderedBeforeBothRenderersAndNothingElse(t *testing.T) {
	u := parseUnit(t, shutdownUnitPath)

	before := u.words("Unit", "Before")
	if len(before) != 2 || !contains(before, "stratux_epaper.service") || !contains(before, "stratux_epaper_splash.service") {
		t.Fatalf("Before=%v, want exactly stratux_epaper.service and stratux_epaper_splash.service: "+
			"reverse stop order then puts this unit's ExecStop after BOTH have released the panel "+
			"(the boot splash must be named directly - naming only the operational unit was measured not to order against it)", before)
	}
	// After= would reverse the stop order: this unit's ExecStop would run
	// BEFORE the renderers stopped.
	if after := u.words("Unit", "After"); len(after) != 0 {
		t.Errorf("After=%v: any After= makes this unit stop BEFORE what it names, i.e. run while they still own the panel", after)
	}
	for _, key := range []string{"Requires", "Requisite", "BindsTo", "PartOf", "Wants", "Upholds", "Conflicts", "OnFailure", "OnSuccess", "PropagatesStopTo", "PropagatesReloadTo", "RequiresMountsFor", "JobTimeoutSec", "JobRunningTimeoutSec"} {
		if v := u["Unit"][key]; len(v) != 0 {
			t.Errorf("[Unit] %s=%v: the cosmetic splash must not be coupled to any other unit or filesystem", key, v)
		}
	}
	// Default dependencies stay ON: they are what order this unit's stop
	// before local filesystems (/boot/firmware) are unmounted, and what
	// include it in every shutdown transaction.
	if v := u["Unit"]["DefaultDependencies"]; len(v) != 0 && v[0] != "yes" {
		t.Errorf("DefaultDependencies=%s: must stay yes (stop ordered before the /boot/firmware unmount; stopped by shutdown.target)", v[0])
	}
	for _, key := range []string{"RequiredBy", "WantedBy", "UpheldBy", "Also", "Alias"} {
		for _, w := range u.words("Install", key) {
			if key == "WantedBy" && w == "multi-user.target" {
				continue
			}
			t.Errorf("[Install] %s=%s: nothing may require or pull in the shutdown splash", key, w)
		}
	}
}

// Nothing in the directive lines may name a shutdown/reboot target: the
// unit is stopped BY a shutdown (default deps), never started by one, and
// the poweroff-vs-reboot decision is made by the process, not by wiring.
func TestShutdownUnit_NoShutdownTargetWiring(t *testing.T) {
	for _, l := range nonCommentLines(t, shutdownUnitPath) {
		for _, target := range []string{"poweroff.target", "halt.target", "reboot.target", "kexec.target", "shutdown.target", "umount.target", "final.target"} {
			if strings.Contains(l, target) {
				t.Errorf("directive %q names %s: a unit started by a shutdown target is ordered AFTER related stop jobs (it measured losing /boot/firmware), and Conflicts/Before on these is the job of the default dependencies", l, target)
			}
		}
	}
}

// Model of the shutdown stop order. systemd starts units in Before=/After=
// order and stops them in the exact reverse; a unit's ExecStop therefore
// runs after the stop of everything that is ordered after it at start.
func TestShutdownUnit_StopOrderPutsExecStopAfterBothRenderers(t *testing.T) {
	units := map[string]unit{
		"stratux_epaper_shutdown.service": parseUnit(t, shutdownUnitPath),
		"stratux_epaper_splash.service":   parseUnit(t, splashUnitPath),
		"stratux_epaper.service":          parseUnit(t, epaperUnitPath),
	}
	start := map[string][]string{} // a -> b: a is started before b
	for name, u := range units {
		for _, b := range u.words("Unit", "Before") {
			start[name] = append(start[name], b)
		}
		for _, a := range u.words("Unit", "After") {
			start[a] = append(start[a], name)
		}
	}
	// stop order is the reverse relation: b is stopped before a.
	stopsBefore := func(b, a string) bool { // does b stop before a?
		seen := map[string]bool{}
		var reach func(from string) bool
		reach = func(from string) bool {
			if from == b {
				return true
			}
			if seen[from] {
				return false
			}
			seen[from] = true
			for _, n := range start[from] {
				if reach(n) {
					return true
				}
			}
			return false
		}
		return reach(a)
	}
	const s, b, e = "stratux_epaper_shutdown.service", "stratux_epaper_splash.service", "stratux_epaper.service"
	for name, want := range map[string]string{
		"operational renderer stops before the shutdown splash's ExecStop": e,
		"boot splash stops before the shutdown splash's ExecStop":          b,
	} {
		if !stopsBefore(want, s) {
			t.Errorf("%s: no stop-order path", name)
		}
	}
	if stopsBefore(s, e) || stopsBefore(s, b) {
		t.Error("the shutdown splash would stop BEFORE a renderer: ordering cycle or reversed edge")
	}
	// The measured requirement is a DIRECT edge to each renderer.
	for _, other := range []string{e, b} {
		if !contains(start[s], other) {
			t.Errorf("no direct Before= edge from the shutdown splash to %s", other)
		}
	}
	// And no edge in the other direction, from either renderer's unit.
	for _, other := range []string{e, b} {
		if contains(start[other], s) {
			t.Errorf("%s is ordered before the shutdown splash at start-up, which reverses the stop order", other)
		}
	}
}

// The validated boot and operational units gain no coupling to the
// shutdown unit: restarting or stopping either can never run it, and
// neither names it.
func TestShutdownUnit_ExistingUnitsUnchangedAndUncoupled(t *testing.T) {
	boot, op := parseUnit(t, splashUnitPath), parseUnit(t, epaperUnitPath)

	// Boot splash: exactly the physically validated semantics.
	if got := boot.words("Unit", "Before"); len(got) != 1 || got[0] != "stratux_epaper.service" {
		t.Errorf("boot splash Before=%v, want exactly [stratux_epaper.service]", got)
	}
	if got := boot.words("Unit", "After"); len(got) != 0 {
		t.Errorf("boot splash After=%v, want none", got)
	}
	if boot.one(t, "Service", "Type") != "oneshot" || boot.one(t, "Service", "RemainAfterExit") != "yes" ||
		boot.one(t, "Service", "ExecStart") != epaperdPath+" -splash-boot" {
		t.Errorf("boot splash Type/RemainAfterExit/ExecStart changed: %v", boot["Service"])
	}
	if v := boot["Service"]["ExecStop"]; len(v) != 0 {
		t.Errorf("boot splash gained ExecStop=%v: the boot unit must not also run at shutdown", v)
	}

	// Operational unit: exactly as validated, no ExecStop/After on the new unit.
	if got := op.words("Unit", "After"); len(got) != 1 || got[0] != "stratux.service" {
		t.Errorf("operational After=%v, want [stratux.service]", got)
	}
	if v := op["Service"]["ExecStop"]; len(v) != 0 {
		t.Errorf("operational unit gained ExecStop=%v", v)
	}
	if got := op.one(t, "Service", "ExecStart"); got != epaperdPath {
		t.Errorf("operational ExecStart=%q, want the unchanged %q", got, epaperdPath)
	}

	for _, path := range []string{splashUnitPath, epaperUnitPath} {
		for _, l := range nonCommentLines(t, path) {
			if strings.Contains(l, "shutdown") {
				t.Errorf("%s directive %q references the shutdown splash", path, l)
			}
		}
	}
}

func TestShutdownUnit_RunsTheSameInstalledBinary(t *testing.T) {
	u := parseUnit(t, shutdownUnitPath)
	bin := strings.Fields(u.one(t, "Service", "ExecStop"))[0]
	if bin != epaperdPath {
		t.Errorf("ExecStop runs %s, want %s (the same binary as the boot splash and operational renderer)", bin, epaperdPath)
	}
}

func TestShutdownUnit_PackagingInstallsUnit(t *testing.T) {
	raw, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	mk := string(raw)
	for _, want := range []string{
		"cp debian/stratux_epaper_shutdown.service $(DEBPKG_BASE)/lib/systemd/system\n",
		"chmod 644 $(DEBPKG_BASE)/lib/systemd/system/stratux_epaper_shutdown.service\n",
		// The already-validated units keep shipping unchanged.
		"cp debian/stratux_epaper_splash.service $(DEBPKG_BASE)/lib/systemd/system\n",
		"cp debian/stratux_epaper.service $(DEBPKG_BASE)/lib/systemd/system\n",
	} {
		if !strings.Contains(mk, want) {
			t.Errorf("Makefile lacks %q", strings.TrimSpace(want))
		}
	}
	if _, err := os.Stat(shutdownUnitPath); err != nil {
		t.Errorf("unit file missing: %v", err)
	}
}
