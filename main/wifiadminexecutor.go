/*
wifiadminexecutor.go: the only file in this feature that touches real
network configuration files, interfaces, or processes - implements
wifiadmin.Executor. Every test in this repository injects a fake
wifiadmin.Executor instead (main/wifiadminapi_test.go,
wifiadmin/transaction_test.go), so no automated test run calls the real
network-command-invoking parts of this file (overlayctl, ifdown/ifup);
this file's own tests (main/wifiadminexecutor_test.go) exercise every
other part of it - the file-staging/rename/restore logic below - against
real temporary files, with only Reload/VerifyLive faked out.

Reuses main/networksettings.go's own existing template files exactly
(STRATUX_HOME+"/cfg/*.template") so this feature writes the SAME
configuration this project's existing /setSettings path already
manages - never a second, parallel configuration surface.

Why every physical file has two write targets
----------------------------------------------
Each of the four configuration files this project's network stack
consumes is read from a path under /etc (or /etc/network, /etc/
dnsmasq.d) that lives on the *live, currently-mounted* root - an
overlayfs whose upper (writable) layer is rebuilt fresh only on reboot.
The durable copy under /overlay/robase/etc/... is the same file on the
*real* underlying ext4 partition, reachable read-write only while
overlayctl has unlocked it, and is what a *future boot* will see once
the overlay's upper layer is rebuilt.

These are not interchangeable. Proven directly against real hardware
during this feature's own hardware-validation mission: a device whose
overlay upper layer already holds its own copy of one of these files
(which can happen from something as ordinary as first-boot provisioning
- confirmed present on a real test device, dated to that device's
original setup, unrelated to any code in this repository) will silently
ignore every future write to the durable-only copy, because reads
through the live merged path never see past that upper-layer shadow
copy - only a reboot clears it. The pre-existing /setSettings path
(main/networksettings.go) works around exactly this by always
rebooting after a network change (see web/plates/settings.html's own
"Stratux is rebooting" notice). This feature's entire safety model - a
two-step preview/apply/confirm/automatic-rollback cycle within a 90-
second window - depends on NOT requiring a reboot, so it cannot use that
workaround; it must instead keep both copies of every file correct on
every apply, always.

Transaction order (documented, not "atomic" - see below)
----------------------------------------------------------
Eight physical file writes (four configs x two locations) plus a real
interface/process reload cannot be made atomic - there is no primitive
on this filesystem, or across two different filesystems, that renames
eight files and restarts a process as a single indivisible operation.
Instead, every step from the first write onward is covered by an
in-memory snapshot of all eight files' exact prior byte content (or
absence), taken before anything is touched, so any failure at any point
converges the *entire* eight-file set back to the complete configuration
that was live before this attempt - never a mix of some old and some
new files. The steps, in order:

 1. Snapshot: read the current content (or absence) of all eight
    physical files. A failure here aborts before anything is touched.
 2. Render: render each of the four templates exactly once against the
    proposed configuration - the same rendered bytes go to both of that
    config's write targets, so the durable and live copies can never
    differ due to a second, independently-nondeterministic render.
 3. Stage: write all eight rendered outputs to "<path>.wifiadmin.tmp"
    siblings and fsync each. A failure here removes every tmp file
    created so far and leaves all eight real files untouched (nothing
    has been renamed into place yet).
 4. Activate, durable first: rename each config's four durable ".tmp"
    files into place. Durable-first means that if this process is
    killed or the device loses power immediately after this step,
    a subsequent boot (which rebuilds the overlay's upper layer from
    the durable copies) comes up already on the new configuration -
    which is exactly equivalent to a crash immediately after Apply
    returned successfully elsewhere in this codebase's crash-recovery
    model (wifiadmin.NewManager unconditionally rolls back to the
    transaction's own recorded previous configuration on restart,
    which itself goes through this same durable-first-then-live
    sequence again, converging regardless of which half completed).
 5. Activate, live: rename each config's four live ".tmp" files into
    place - this is what actually changes the running system's view of
    its own configuration.
 6. Reload: restart the real AP interface/process stack so it re-reads
    the files just activated.
 7. Verify: confirm the live configuration actually reflects what was
    just written (both by re-reading the live file and by an
    independent kernel-level HealthCheck) before ever reporting success
    to the caller - closing the exact gap that let a previous version
    of this file report success while the real device's broadcast
    configuration never changed.

A failure at step 4 or later restores every one of the eight files to
its step-1 snapshot (an old file gets its old content re-staged and
renamed back into place the same atomic way; a file that did not exist
before gets removed) and attempts a reload with the restored content,
so the live system never ends up stuck mid-transition even though this
attempt reports failure. If that restoration itself fails, the error
says so explicitly rather than claiming a recovery that did not happen.
*/
package main

import (
	"bytes"
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

// apReloader is the one seam in this file real tests cannot cross: the
// real AP interface/process stack. wifiAPReloaderInstance is a package
// var (not a realWifiExecutor field, since wifiadmin.Executor's own
// interface has no room for injected dependencies) so
// main/wifiadminexecutor_test.go can substitute a fake and exercise
// every surrounding file-staging/restore path for real, without ever
// invoking a real network command.
type apReloader interface {
	// Reload tears down and recreates the real AP interface/process
	// stack so it picks up freshly-activated configuration files. This
	// project's own /etc/network/interfaces already fully destroys and
	// recreates the virtual ap0 interface (and, via ap0's own post-up
	// hook, the wpa_supplicant/dnsmasq processes backing it - see
	// debian/stratux-wifi.sh) as a side effect of cycling wlan0's own
	// pre-up/post-down hooks; that is what this reloads, not a
	// different, more targeted mechanism, because it is this project's
	// own already-established way of doing so.
	Reload() error
	// VerifyLive reports whether the live, already-reloaded
	// configuration genuinely reflects cfg. Apply must never report
	// success before this passes.
	VerifyLive(cfg wifiadmin.Config) error
}

var wifiAPReloaderInstance apReloader = realAPReloader{}

// wifiAdminRenameHook, when non-nil, is called immediately before each
// of Apply's eight activation renames, named by target and which half.
// A real filesystem fault cannot be injected at one precise point in
// this eight-rename sequence without also perturbing the step-1
// snapshot the test needs to stay trustworthy (e.g. pre-occupying a
// destination path would itself change what snapshotDualTargets reads
// before Apply ever starts) - this hook is the one purpose-built seam
// for main/wifiadminexecutor_test.go to inject a failure at an exact
// rename without that side effect. nil in production.
var wifiAdminRenameHook func(targetName string, durable bool) error

// wifiAdminRestoreWriteHook, when non-nil, is called immediately before
// restoreDualTargets writes or removes each individual physical path.
// restoreDualTargets's own writes reuse the identical ".tmp"-suffix
// convention the forward staging path uses, so a real-filesystem trick
// that injects a failure into a restore write would, if applied before
// Apply even starts, equally break the forward path's own staging step
// for that same path - this hook is the dedicated seam for testing a
// restore-specific failure in isolation. nil in production.
var wifiAdminRestoreWriteHook func(path string) error

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

// dualFileTarget names one logical configuration file's two physical
// locations - see this file's own package doc comment for why both are
// required.
type dualFileTarget struct {
	name       string
	tplFile    string
	liveOut    string
	durableOut string
}

// wifiAdminDualTargets is a var, not a const, solely so tests can
// redirect liveOut/durableOut to temporary directories - the same
// convention as wifiAdminLastKnownGoodPath in main/wifiadminsettings.go.
// Identified directly from the real device's own running processes and
// /etc/network/interfaces content during this feature's hardware-
// validation mission (debian/stratux-wifi.sh's own two wpa_supplicant/
// dnsmasq invocations, confirmed via `ps aux` against real hardware) -
// this is the complete, confirmed set of files this project's network
// stack ever reads for Wi-Fi configuration; nothing here was assumed.
var wifiAdminDualTargets = []dualFileTarget{
	{
		name:       "dnsmasq",
		tplFile:    STRATUX_HOME + "/cfg/stratux-dnsmasq.conf.template",
		liveOut:    "/etc/dnsmasq.d/stratux-dnsmasq.conf",
		durableOut: "/overlay/robase/etc/dnsmasq.d/stratux-dnsmasq.conf",
	},
	{
		name:       "interfaces",
		tplFile:    STRATUX_HOME + "/cfg/interfaces.template",
		liveOut:    "/etc/network/interfaces",
		durableOut: "/overlay/robase/etc/network/interfaces",
	},
	{
		name:       "wpa_supplicant",
		tplFile:    STRATUX_HOME + "/cfg/wpa_supplicant.conf.template",
		liveOut:    "/etc/wpa_supplicant/wpa_supplicant.conf",
		durableOut: "/overlay/robase/etc/wpa_supplicant/wpa_supplicant.conf",
	},
	{
		name:       "wpa_supplicant_ap",
		tplFile:    STRATUX_HOME + "/cfg/wpa_supplicant_ap.conf.template",
		liveOut:    "/etc/wpa_supplicant/wpa_supplicant_ap.conf",
		durableOut: "/overlay/robase/etc/wpa_supplicant/wpa_supplicant_ap.conf",
	},
}

// fileSnapshot is one physical file's exact prior state, captured
// before any write in this attempt - "existed=false" means the file was
// absent, so restoring it means removing it, never writing empty
// content in its place.
type fileSnapshot struct {
	path    string
	existed bool
	data    []byte
}

// snapshotFile reads path's current content. A missing file is not an
// error (existed=false); any other read error aborts the whole apply
// before anything is touched, since a snapshot this feature cannot
// trust is a snapshot it cannot safely restore from later.
func snapshotFile(path string) (fileSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileSnapshot{path: path, existed: false}, nil
		}
		return fileSnapshot{}, fmt.Errorf("reading %s: %w", path, err)
	}
	return fileSnapshot{path: path, existed: true, data: data}, nil
}

// snapshotDualTargets captures every physical file's current state, in
// a fixed order (durable then live, per target, in target-list order) -
// see restoreDualTargets for how this is used.
func snapshotDualTargets(targets []dualFileTarget) ([]fileSnapshot, error) {
	snapshots := make([]fileSnapshot, 0, len(targets)*2)
	for _, t := range targets {
		for _, p := range [2]string{t.durableOut, t.liveOut} {
			s, err := snapshotFile(p)
			if err != nil {
				return nil, err
			}
			snapshots = append(snapshots, s)
		}
	}
	return snapshots, nil
}

// atomicWriteFile writes data to path via the project's own established
// temp-file+fsync+atomic-rename pattern (see main/wifiadminsettings.go's
// atomicWriteJSON, main/alertsettings.go's saveAlertSettings).
func atomicWriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".wifiadmin.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// restoreDualTargets writes every snapshot's prior content back to its
// own path (or removes the path, if it did not previously exist),
// unconditionally - it does not try to determine which files were
// actually changed by the failed attempt, since re-writing a file back
// to its own current content is always harmless. Every restoration is
// attempted even if an earlier one fails, so one bad path never blocks
// recovery of the other seven; every individual failure is collected
// into the returned error rather than only reporting the first.
func restoreDualTargets(snapshots []fileSnapshot) error {
	var errs []string
	for _, s := range snapshots {
		if wifiAdminRestoreWriteHook != nil {
			if err := wifiAdminRestoreWriteHook(s.path); err != nil {
				errs = append(errs, fmt.Sprintf("restoring %s: %v", s.path, err))
				continue
			}
		}
		if s.existed {
			if err := atomicWriteFile(s.path, s.data); err != nil {
				errs = append(errs, fmt.Sprintf("restoring %s: %v", s.path, err))
			}
		} else if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Sprintf("removing %s: %v", s.path, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// renderTemplateBytes parses tplFile and executes it against params,
// returning the rendered content - rendered once per config and reused
// for both of that config's write targets (see this file's own package
// doc comment for why).
func renderTemplateBytes(tplFile string, params NetworkTemplateParams) ([]byte, error) {
	t, err := template.ParseFiles(tplFile)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, params); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Apply writes every config file this project's existing network stack
// reads, to both their durable and live locations, reloads the real AP
// stack, and verifies the live result before reporting success - see
// this file's own package doc comment for the full, documented
// transaction order and why it is not (and cannot be) a single atomic
// operation.
func (realWifiExecutor) Apply(cfg wifiadmin.Config) error {
	params, err := toNetworkTemplateParams(cfg)
	if err != nil {
		return err
	}

	// Step 1: snapshot every physical file's current state before
	// anything is touched.
	snapshot, err := snapshotDualTargets(wifiAdminDualTargets)
	if err != nil {
		return fmt.Errorf("wifiadmin: could not snapshot the current configuration before applying: %w", err)
	}

	overlayctl("unlock")
	defer overlayctl("lock")

	// fail restores every file to its step-1 snapshot and attempts a
	// reload with that restored content, so the live system never sits
	// on a half-applied intermediate state just because this attempt
	// reports failure. If restoration itself fails, that is stated
	// plainly rather than folded into a claim of successful recovery.
	fail := func(reason error) error {
		if restoreErr := restoreDualTargets(snapshot); restoreErr != nil {
			return fmt.Errorf("wifiadmin: %w (restoring the previous configuration also failed, manual recovery may be required: %v)", reason, restoreErr)
		}
		if reloadErr := wifiAPReloaderInstance.Reload(); reloadErr != nil {
			return fmt.Errorf("wifiadmin: %w (restored the previous configuration files but could not reload them: %v)", reason, reloadErr)
		}
		return reason
	}

	// Step 2+3: render each config once, stage both of its targets.
	var stagedFiles []stagedFilePair
	for _, t := range wifiAdminDualTargets {
		data, err := renderTemplateBytes(t.tplFile, params)
		if err != nil {
			cleanupStaged(stagedFiles)
			return fail(fmt.Errorf("rendering %s: %w", t.name, err))
		}
		if err := atomicStageOnly(t.durableOut, data); err != nil {
			cleanupStaged(stagedFiles)
			return fail(fmt.Errorf("staging %s (durable): %w", t.name, err))
		}
		if err := atomicStageOnly(t.liveOut, data); err != nil {
			cleanupStaged(stagedFiles)
			os.Remove(t.durableOut + ".wifiadmin.tmp")
			return fail(fmt.Errorf("staging %s (live): %w", t.name, err))
		}
		stagedFiles = append(stagedFiles, stagedFilePair{durableOut: t.durableOut, liveOut: t.liveOut})
	}

	// Step 4: activate durable first - see the package doc comment for
	// why this order.
	for _, t := range wifiAdminDualTargets {
		if wifiAdminRenameHook != nil {
			if err := wifiAdminRenameHook(t.name, true); err != nil {
				return fail(fmt.Errorf("activating %s (durable): %w", t.name, err))
			}
		}
		if err := os.Rename(t.durableOut+".wifiadmin.tmp", t.durableOut); err != nil {
			return fail(fmt.Errorf("activating %s (durable): %w", t.name, err))
		}
	}

	// Step 5: activate live.
	for _, t := range wifiAdminDualTargets {
		if wifiAdminRenameHook != nil {
			if err := wifiAdminRenameHook(t.name, false); err != nil {
				return fail(fmt.Errorf("activating %s (live): %w", t.name, err))
			}
		}
		if err := os.Rename(t.liveOut+".wifiadmin.tmp", t.liveOut); err != nil {
			return fail(fmt.Errorf("activating %s (live): %w", t.name, err))
		}
	}

	// Step 6: reload the real AP stack.
	if err := wifiAPReloaderInstance.Reload(); err != nil {
		return fail(fmt.Errorf("reloading the AP interface: %w", err))
	}

	// Step 7: verify the live result before ever reporting success -
	// this is the gap a previous version of this file left open.
	if err := wifiAPReloaderInstance.VerifyLive(cfg); err != nil {
		return fail(fmt.Errorf("the AP does not yet reflect the new configuration after reload: %w", err))
	}

	return nil
}

// atomicStageOnly writes data to path+".wifiadmin.tmp" (fsync'd) but
// does not rename it into place - the caller renames every staged file
// only after every render+stage in the batch has succeeded.
func atomicStageOnly(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".wifiadmin.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
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

// stagedFilePair names one target's two ".tmp" siblings already staged
// so far in a batch that has not yet finished staging every target.
type stagedFilePair struct {
	durableOut string
	liveOut    string
}

// cleanupStaged removes every ".tmp" sibling created so far for a batch
// that failed before every target could be staged.
func cleanupStaged(staged []stagedFilePair) {
	for _, s := range staged {
		os.Remove(s.durableOut + ".wifiadmin.tmp")
		os.Remove(s.liveOut + ".wifiadmin.tmp")
	}
}

// realAPReloader is apReloader's real, hardware-touching implementation.
type realAPReloader struct{}

// Reload mirrors main/networksettings.go's own ifdown/ifup sequence
// exactly, except ifup's failure is treated as fatal (returned to the
// caller) rather than only logged - Manager needs to know whether the
// new configuration actually came up to decide whether a rollback is
// needed. ifdown's own failure is tolerated (as the existing code
// already does): "the interface was not up" is not itself a failure of
// this apply attempt. Cycling wlan0, not ap0, is intentional - see this
// file's own package doc comment on apReloader.
func (realAPReloader) Reload() error {
	if cmd := exec.Command("ifdown", "wlan0"); cmd.Run() != nil {
		// tolerated - see doc comment above.
	}
	cmd := exec.Command("ifup", "wlan0")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wifiadmin: ifup wlan0: %w", err)
	}
	return nil
}

// VerifyLive confirms the live configuration actually reflects cfg
// before Apply is allowed to report success: the live AP config file
// itself must contain cfg's own SSID, and an independent kernel-level
// check (ap0's actual address) must also agree - two different sources
// of truth, neither of which is the durable-only file this project's
// own hardware validation proved insufficient on its own.
func (r realAPReloader) VerifyLive(cfg wifiadmin.Config) error {
	livePath := "/etc/wpa_supplicant/wpa_supplicant_ap.conf"
	if cfg.Mode == wifiadmin.ModeDirect {
		livePath = "/etc/wpa_supplicant/wpa_supplicant.conf"
	}
	data, err := os.ReadFile(livePath)
	if err != nil {
		return fmt.Errorf("could not read the live configuration at %s: %w", livePath, err)
	}
	wantSSID := cfg.SSID
	if wantSSID == "" {
		wantSSID = "Stratux"
	}
	if !strings.Contains(string(data), "ssid=\""+wantSSID+"\"") {
		return fmt.Errorf("the live configuration at %s does not yet contain the expected ssid %q", livePath, wantSSID)
	}
	health, err := (realWifiExecutor{}).HealthCheck(cfg)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	if !health.InterfacePresent || !health.AddressMatches {
		return fmt.Errorf("the AP interface does not yet carry the expected address: %+v", health)
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
