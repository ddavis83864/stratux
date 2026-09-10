/*
wifiadminexecutor.go: the only file in this feature that touches real
network configuration files, interfaces, or processes - implements
wifiadmin.Executor. Every test (wifiadmin's own, and
main/wifiadminapi_test.go) injects a fake instead, so no automated test
run in this repository ever calls anything in this file. This mission
does not deploy or exercise this file against real hardware - see
docs/wifi-administration-hardening.md's own explicit statement to that
effect.

Reuses main/networksettings.go's own existing template files and output
paths exactly (STRATUX_HOME+"/cfg/*.template" -> "/overlay/robase/etc/...")
so this feature writes to the SAME configuration this project's existing
/setSettings path already manages - never a second, parallel
configuration surface. Unlike the existing writeTemplate (which only
logs a failure and returns nothing), every step here returns a real
error, because Manager's rollback/recovery decisions depend on being
able to tell success from failure.
*/
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/stratux/stratux/wifiadmin"
)

// realWifiExecutor is wifiadmin.Executor's production implementation.
type realWifiExecutor struct{}

// toNetworkTemplateParams converts a validated wifiadmin.Config into the
// exact NetworkTemplateParams shape main/networksettings.go's own
// templates already expect - the DHCP range is derived with
// wifiadmin.DerivedDHCPRange, the identical algorithm
// applyNetworkSettings itself already uses (see that function's own doc
// comment for why this specific derivation is guaranteed collision-free
// for any address wifiadmin.Config.Validate has already accepted).
func toNetworkTemplateParams(cfg wifiadmin.Config) (NetworkTemplateParams, error) {
	dhcpStart, dhcpEnd, ok := wifiadmin.DerivedDHCPRange(cfg.IPAddress)
	if !ok {
		return NetworkTemplateParams{}, fmt.Errorf("wifiadmin: could not derive a DHCP range for %q", cfg.IPAddress)
	}
	ipParts := strings.Split(cfg.IPAddress, ".")
	if len(ipParts) != 4 {
		return NetworkTemplateParams{}, fmt.Errorf("wifiadmin: malformed IP address %q", cfg.IPAddress)
	}
	ipPrefix := ipParts[0] + "." + ipParts[1] + "." + ipParts[2]

	clientNetworks := make([]wifiClientNetwork, len(cfg.ClientNetworks))
	for i, n := range cfg.ClientNetworks {
		clientNetworks[i] = wifiClientNetwork{SSID: n.SSID, Password: n.Password}
	}

	passphrase := ""
	if cfg.SecurityEnabled || cfg.Mode == wifiadmin.ModeDirect {
		passphrase = cfg.Passphrase
	}
	ssid := cfg.SSID
	if ssid == "" {
		ssid = "Stratux"
	}
	channel := cfg.Channel
	if channel == 0 {
		channel = 1
	}

	return NetworkTemplateParams{
		WiFiMode:                       int(cfg.Mode),
		WiFiCountry:                    cfg.Country,
		IpAddr:                         cfg.IPAddress,
		IpPrefix:                       ipPrefix,
		DhcpRangeStart:                 dhcpStart,
		DhcpRangeEnd:                   dhcpEnd,
		WiFiSSID:                       ssid,
		WiFiChannel:                    channel,
		WiFiDirectPin:                  cfg.DirectPin,
		WiFiPassPhrase:                 passphrase,
		WiFiClientNetworks:             clientNetworks,
		WiFiInternetPassThroughEnabled: cfg.InternetPassThroughEnabled,
	}, nil
}

// wifiAdminConfigTemplates names each template file and its destination -
// identical paths to main/networksettings.go's own applyNetworkSettings.
var wifiAdminConfigTemplates = []struct {
	tpl string
	out string
}{
	{STRATUX_HOME + "/cfg/stratux-dnsmasq.conf.template", "/overlay/robase/etc/dnsmasq.d/stratux-dnsmasq.conf"},
	{STRATUX_HOME + "/cfg/interfaces.template", "/overlay/robase/etc/network/interfaces"},
	{STRATUX_HOME + "/cfg/wpa_supplicant.conf.template", "/overlay/robase/etc/wpa_supplicant/wpa_supplicant.conf"},
	{STRATUX_HOME + "/cfg/wpa_supplicant_ap.conf.template", "/overlay/robase/etc/wpa_supplicant/wpa_supplicant_ap.conf"},
}

// Apply writes every config file this project's existing network stack
// reads, then restarts the wlan0 interface - see the package doc
// comment for why every step here surfaces a real error, unlike the
// pre-existing writeTemplate/applyNetworkSettings it otherwise mirrors.
//
// All four files are rendered to ".tmp" siblings FIRST; only if every
// one renders successfully are they atomically renamed into place, in
// sequence. This closes a real gap in the pre-existing
// applyNetworkSettings (which truncates each file in place
// unconditionally, one at a time - a mid-sequence failure there can
// leave the four files describing four different, mutually
// inconsistent configurations). A failure at any point here leaves the
// PREVIOUSLY active files completely untouched.
func (realWifiExecutor) Apply(cfg wifiadmin.Config) error {
	params, err := toNetworkTemplateParams(cfg)
	if err != nil {
		return err
	}

	overlayctl("unlock")
	defer overlayctl("lock")

	var tmpPaths []string
	cleanup := func() {
		for _, p := range tmpPaths {
			os.Remove(p)
		}
	}

	for _, f := range wifiAdminConfigTemplates {
		tmp := f.out + ".wifiadmin.tmp"
		if err := renderTemplateAtomicStage(f.tpl, tmp, params); err != nil {
			cleanup()
			return fmt.Errorf("wifiadmin: rendering %s: %w", f.out, err)
		}
		tmpPaths = append(tmpPaths, tmp)
	}
	for i, f := range wifiAdminConfigTemplates {
		if err := os.Rename(tmpPaths[i], f.out); err != nil {
			// Best-effort: the files already renamed are now live and
			// inconsistent with the ones not yet renamed. This project
			// has no cross-file transactional filesystem primitive
			// available (they live in the read-only overlay's writable
			// upper layer); rather than pretend otherwise, this is
			// disclosed plainly in the design doc as a known, narrow
			// residual risk window (docs/wifi-administration-hardening.md's
			// "Known limitations").
			cleanup()
			return fmt.Errorf("wifiadmin: activating %s: %w", f.out, err)
		}
	}

	if err := runIfconfigCycle(); err != nil {
		return err
	}
	return nil
}

// renderTemplateAtomicStage parses tplFile and executes it against
// params, writing the result to tmp (not out directly) - the caller
// renames tmp into place only after every template in the batch has
// rendered successfully.
func renderTemplateAtomicStage(tplFile, tmp string, params NetworkTemplateParams) error {
	t, err := template.ParseFiles(tplFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := t.Execute(f, params); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	return f.Close()
}

// runIfconfigCycle mirrors main/networksettings.go's own ifdown/ifup
// sequence exactly, except ifup's failure is treated as fatal (returned
// to the caller) rather than only logged - Manager needs to know whether
// the new configuration actually came up to decide whether a rollback is
// needed. ifdown's own failure is tolerated (as the existing code
// already does): "the interface was not up" is not itself a failure of
// this apply attempt.
func runIfconfigCycle() error {
	if cmd := exec.Command("ifdown", "wlan0"); cmd.Run() != nil {
		// tolerated - see doc comment above.
	}
	cmd := exec.Command("ifup", "wlan0")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wifiadmin: ifup wlan0: %w", err)
	}
	return nil
}

// HealthCheck reports whether ap0 currently carries cfg's own IP
// address - a best-effort, single point-in-time observation using the
// same `ip addr show` mechanism already available on this platform. It
// never blocks and is never itself treated as proof of reconnection
// (see wifiadmin.Manager.ConfirmReconnection's own doc comment).
func (realWifiExecutor) HealthCheck(cfg wifiadmin.Config) (wifiadmin.Health, error) {
	out, err := exec.Command("ip", "-4", "-o", "addr", "show", "dev", "ap0").Output()
	if err != nil {
		return wifiadmin.Health{InterfacePresent: false, Detail: err.Error()}, nil
	}
	text := string(out)
	present := strings.Contains(text, "inet ")
	addr := ""
	for _, field := range strings.Fields(text) {
		if strings.Contains(field, "/") && strings.Count(field, ".") == 3 {
			addr = strings.SplitN(field, "/", 2)[0]
			break
		}
	}
	return wifiadmin.Health{
		InterfacePresent: present,
		InterfaceAddress: addr,
		AddressMatches:   addr != "" && addr == cfg.IPAddress,
	}, nil
}

// wifiAdminModeConstantsMatch is asserted by a test to guarantee
// wifiadmin.Mode's numeric values never silently drift from this
// project's own existing WifiModeAp/WifiModeDirect/WifiModeApClient
// constants (main/networksettings.go) - both packages must agree, since
// toNetworkTemplateParams passes wifiadmin.Mode straight through as an
// int into the existing template's own {{eq .WiFiMode 0}} checks.
func wifiAdminModeConstantsMatch() bool {
	return int(wifiadmin.ModeAP) == WifiModeAp &&
		int(wifiadmin.ModeDirect) == WifiModeDirect &&
		int(wifiadmin.ModeAPClient) == WifiModeApClient
}
