package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stratux/stratux/epaper/splash"
	"github.com/stratux/stratux/epaper/splash/assets"
)

// These tests pin the systemd/packaging structure of the boot splash. They
// parse the real unit files in debian/ (never copies), so the properties
// asserted here are the ones that ship.

const (
	splashUnitPath = "../debian/stratux_epaper_splash.service"
	epaperUnitPath = "../debian/stratux_epaper.service"
	epaperdPath    = "/opt/stratux/bin/epaperd"
)

// unit is a parsed systemd unit: section -> key -> every value given (keys
// can repeat; systemd concatenates them).
type unit map[string]map[string][]string

func parseUnit(t *testing.T, path string) unit {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	u := unit{}
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if u[section] == nil {
				u[section] = map[string][]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			t.Fatalf("%s: unparseable line %q", path, line)
		}
		u[section][strings.TrimSpace(k)] = append(u[section][strings.TrimSpace(k)], strings.TrimSpace(v))
	}
	return u
}

// words returns every whitespace-separated word across all occurrences of
// a list-valued key (After=, Before=, Wants=, ...).
func (u unit) words(section, key string) []string {
	var out []string
	for _, v := range u[section][key] {
		out = append(out, strings.Fields(v)...)
	}
	return out
}

func (u unit) one(t *testing.T, section, key string) string {
	t.Helper()
	v := u[section][key]
	if len(v) != 1 {
		t.Fatalf("[%s] %s: want exactly one value, got %v", section, key, v)
	}
	return v[0]
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestSplashUnit_StructureAndBoundedTimeouts(t *testing.T) {
	u := parseUnit(t, splashUnitPath)

	if got := u.one(t, "Service", "Type"); got != "oneshot" {
		t.Errorf("Type=%s, want oneshot: only a oneshot makes systemd hold stratux_epaper until the splash process has exited", got)
	}
	if got := u.one(t, "Service", "RemainAfterExit"); got != "yes" {
		t.Errorf("RemainAfterExit=%s, want yes (run once per boot; a stratux_epaper restart must not replay the splash)", got)
	}
	if got := u.one(t, "Service", "ExecStart"); got != epaperdPath+" -splash-boot" {
		t.Errorf("ExecStart=%q, want %q", got, epaperdPath+" -splash-boot")
	}
	if got := u.one(t, "Service", "Restart"); got != "no" {
		t.Errorf("Restart=%s, want no (bounded: a failed cosmetic splash is never retried)", got)
	}
	if !contains(u.words("Install", "WantedBy"), "multi-user.target") || len(u.words("Install", "WantedBy")) != 1 {
		t.Errorf("WantedBy=%v, want exactly multi-user.target (same target as stratux_epaper, so `systemctl enable` works and both are in the boot transaction)", u.words("Install", "WantedBy"))
	}

	// Bounded: the process's own deadline must expire strictly before
	// systemd's backstop, and the backstop itself must be modest.
	start := u.one(t, "Service", "TimeoutStartSec")
	secs, err := strconv.Atoi(start)
	if err != nil {
		t.Fatalf("TimeoutStartSec=%q must be a plain number of seconds", start)
	}
	if time.Duration(secs)*time.Second <= bootSplashTimeout {
		t.Errorf("TimeoutStartSec=%ds must exceed the process's own %v deadline so the process ends itself (panel put to sleep) before systemd kills it", secs, bootSplashTimeout)
	}
	if secs > 120 {
		t.Errorf("TimeoutStartSec=%ds: the boot-delay ceiling for the operational renderer must stay small", secs)
	}
	if stop, err := strconv.Atoi(u.one(t, "Service", "TimeoutStopSec")); err != nil || stop > 30 {
		t.Errorf("TimeoutStopSec must be a small bounded number of seconds, got %v", u["Service"]["TimeoutStopSec"])
	}
}

func TestSplashUnit_OrderedBeforeOperationalRendererAndNothingElse(t *testing.T) {
	u := parseUnit(t, splashUnitPath)

	if !contains(u.words("Unit", "Before"), "stratux_epaper.service") {
		t.Fatalf("Before=%v must include stratux_epaper.service: this is what guarantees the splash has released the panel before the operational renderer starts", u.words("Unit", "Before"))
	}
	// Minimum dependencies. Any of these would let a splash failure or
	// hang permanently block, fail, or stop something else. (Before= alone
	// still lets a hung splash delay stratux_epaper, but only up to the
	// bounded TimeoutStartSec, which TestSplashUnit_StructureAndBoundedTimeouts
	// pins.)
	for _, key := range []string{"Requires", "Requisite", "BindsTo", "PartOf", "Wants", "Upholds", "Conflicts", "OnFailure", "OnSuccess", "PropagatesStopTo", "PropagatesReloadTo"} {
		if v := u["Unit"][key]; len(v) != 0 {
			t.Errorf("[Unit] %s=%v: the cosmetic splash must not be coupled to any other unit", key, v)
		}
	}
	for _, key := range []string{"RequiredBy", "WantedBy", "UpheldBy"} {
		for _, w := range u.words("Install", key) {
			if key == "WantedBy" && w == "multi-user.target" {
				continue
			}
			t.Errorf("[Install] %s=%s: nothing may require the splash", key, w)
		}
	}
	// Not after the daemon or the network: the logo must appear at
	// power-on, not wait for stratux.service's ExecStartPre (OTA installs).
	for _, after := range u.words("Unit", "After") {
		t.Errorf("After=%s: the splash must start as early as possible (only the implicit basic.target ordering applies)", after)
	}
	if v := u["Unit"]["DefaultDependencies"]; len(v) != 0 && v[0] == "no" {
		t.Error("DefaultDependencies=no would let the splash start before /boot/firmware (its config) is mounted")
	}
}

func TestOperationalUnit_UnchangedAndNotCoupledToSplash(t *testing.T) {
	u := parseUnit(t, epaperUnitPath)
	// The physically-validated operational unit's dependencies are exactly
	// what they were before the splash existed.
	if got := u.words("Unit", "After"); len(got) != 1 || got[0] != "stratux.service" {
		t.Errorf("After=%v, want [stratux.service]", got)
	}
	if got := u.words("Unit", "Wants"); len(got) != 1 || got[0] != "stratux.service" {
		t.Errorf("Wants=%v, want [stratux.service]", got)
	}
	// It must not depend on the splash in any way, so a failed, hung, or
	// absent splash can never prevent it starting. (Ordering after the
	// splash comes only from the splash unit's own Before=, which can
	// delay it by at most the splash's bounded TimeoutStartSec.)
	raw, _ := os.ReadFile(epaperUnitPath)
	for _, line := range strings.Split(string(raw), "\n") {
		if l := strings.TrimSpace(line); !strings.HasPrefix(l, "#") && strings.Contains(l, "splash") {
			t.Errorf("stratux_epaper.service references the splash: %q", l)
		}
	}
	if got := u.one(t, "Service", "ExecStart"); got != epaperdPath {
		t.Errorf("operational ExecStart=%q, want %q", got, epaperdPath)
	}
}

// TestOrderingGraph is a small model of the systemd ordering both unit
// files declare: the splash is strictly before the operational renderer,
// nothing declares the reverse, and there is no cycle.
func TestOrderingGraph(t *testing.T) {
	splashU, opU := parseUnit(t, splashUnitPath), parseUnit(t, epaperUnitPath)
	edges := map[string][]string{} // a -> b means "a runs before b"
	add := func(name string, u unit) {
		for _, b := range u.words("Unit", "Before") {
			edges[name] = append(edges[name], b)
		}
		for _, a := range u.words("Unit", "After") {
			edges[a] = append(edges[a], name)
		}
	}
	add("stratux_epaper_splash.service", splashU)
	add("stratux_epaper.service", opU)

	var reaches func(from, to string, seen map[string]bool) bool
	reaches = func(from, to string, seen map[string]bool) bool {
		if from == to {
			return true
		}
		if seen[from] {
			return false
		}
		seen[from] = true
		for _, n := range edges[from] {
			if reaches(n, to, seen) {
				return true
			}
		}
		return false
	}
	if !reaches("stratux_epaper_splash.service", "stratux_epaper.service", map[string]bool{}) {
		t.Error("splash is not ordered before the operational renderer")
	}
	if reaches("stratux_epaper.service", "stratux_epaper_splash.service", map[string]bool{}) {
		t.Error("ordering cycle: the operational renderer is ordered before the splash")
	}
}

func TestBothUnitsRunTheSameInstalledBinary(t *testing.T) {
	sp, op := parseUnit(t, splashUnitPath), parseUnit(t, epaperUnitPath)
	spBin := strings.Fields(sp.one(t, "Service", "ExecStart"))[0]
	opBin := strings.Fields(op.one(t, "Service", "ExecStart"))[0]
	if spBin != opBin || spBin != epaperdPath {
		t.Errorf("splash runs %s, operational runs %s; both must be %s so they can never be from mismatched builds", spBin, opBin, epaperdPath)
	}
}

// TestPackagingInstallsUnitAndBinary: the unit reaches lib/systemd/system
// in the .deb, and the binary its ExecStart names is installed at that
// exact path by the same Makefile.
func TestPackagingInstallsUnitAndBinary(t *testing.T) {
	raw, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	mk := string(raw)
	for _, want := range []string{
		"cp debian/stratux_epaper_splash.service $(DEBPKG_BASE)/lib/systemd/system\n",
		"chmod 644 $(DEBPKG_BASE)/lib/systemd/system/stratux_epaper_splash.service\n",
		"cp -f epaperd $(STRATUX_HOME)/bin/\n",
	} {
		if !strings.Contains(mk, want) {
			t.Errorf("Makefile lacks %q", strings.TrimSpace(want))
		}
	}
	if !regexp.MustCompile(`(?m)^export STRATUX_HOME := /opt/stratux/$`).MatchString(mk) {
		t.Error("STRATUX_HOME is not /opt/stratux/; the units' ExecStart path would not match the installed binary")
	}
	if !regexp.MustCompile(`(?m)^PLATFORMDEPENDENT=.*\bepaperd\b`).MatchString(mk) {
		t.Error("epaperd is not built by `make all`")
	}
	if _, err := os.Stat(filepath.Clean(splashUnitPath)); err != nil {
		t.Errorf("unit file missing: %v", err)
	}
}

// TestSplashAssetIsEmbeddedNotInstalled: there is no asset file to
// install or lose - the bitmap is compiled into the binary the unit runs.
func TestSplashAssetIsEmbeddedNotInstalled(t *testing.T) {
	if _, err := splash.Validate(assets.Bitmap()); err != nil {
		t.Fatalf("embedded production bitmap is invalid: %v", err)
	}
	raw, _ := os.ReadFile(splashUnitPath)
	if strings.Contains(string(raw), ".bin") || strings.Contains(string(raw), ".png") {
		t.Error("unit references an asset file; the bitmap must be embedded in epaperd")
	}
}
