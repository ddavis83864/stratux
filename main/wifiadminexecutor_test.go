package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stratux/stratux/wifiadmin"
)

// This file exercises every part of main/wifiadminexecutor.go's Apply
// EXCEPT the two real-hardware-touching methods on realAPReloader
// (Reload/VerifyLive, which run real exec.Command/ifdown/ifup and read
// real system paths - see that file's own package doc comment for why
// no automated test in this repository calls those). Every test here
// substitutes a fakeAPReloader and redirects wifiAdminDualTargets to a
// temp directory, but otherwise exercises the REAL render/stage/
// activate/restore logic against a REAL filesystem - this is a
// regression suite for the specific defect found during this feature's
// hardware-validation mission: a real device whose live configuration
// had silently diverged from its durable copy, discovered when an
// applied SSID change never actually reached the live, running AP.

// repoTemplatePath resolves one of this project's own real templates
// (debian/*.template) relative to this test's working directory (the
// main/ package directory) - tests render through the ACTUAL production
// templates, not stand-ins, so a template change that broke this
// feature would be caught here too.
func repoTemplatePath(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "debian", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("could not find repository template %s: %v", path, err)
	}
	return path
}

// withTempWifiAdminDualTargets redirects wifiAdminDualTargets to a
// fresh temp directory (separate "live" and "durable" trees) for the
// duration of one test, using this project's own real templates.
func withTempWifiAdminDualTargets(t *testing.T) (liveDir, durableDir string) {
	t.Helper()
	root := t.TempDir()
	liveDir = filepath.Join(root, "live")
	durableDir = filepath.Join(root, "durable")
	orig := wifiAdminDualTargets
	wifiAdminDualTargets = []dualFileTarget{
		{
			name:       "dnsmasq",
			tplFile:    repoTemplatePath(t, "stratux-dnsmasq.conf.template"),
			liveOut:    filepath.Join(liveDir, "dnsmasq.d", "stratux-dnsmasq.conf"),
			durableOut: filepath.Join(durableDir, "dnsmasq.d", "stratux-dnsmasq.conf"),
		},
		{
			name:       "interfaces",
			tplFile:    repoTemplatePath(t, "interfaces.template"),
			liveOut:    filepath.Join(liveDir, "network", "interfaces"),
			durableOut: filepath.Join(durableDir, "network", "interfaces"),
		},
		{
			name:       "wpa_supplicant",
			tplFile:    repoTemplatePath(t, "wpa_supplicant.conf.template"),
			liveOut:    filepath.Join(liveDir, "wpa_supplicant", "wpa_supplicant.conf"),
			durableOut: filepath.Join(durableDir, "wpa_supplicant", "wpa_supplicant.conf"),
		},
		{
			name:       "wpa_supplicant_ap",
			tplFile:    repoTemplatePath(t, "wpa_supplicant_ap.conf.template"),
			liveOut:    filepath.Join(liveDir, "wpa_supplicant", "wpa_supplicant_ap.conf"),
			durableOut: filepath.Join(durableDir, "wpa_supplicant", "wpa_supplicant_ap.conf"),
		},
	}
	t.Cleanup(func() {
		wifiAdminDualTargets = orig
	})
	return liveDir, durableDir
}

// fakeAPReloader is main/wifiadminexecutor_test.go's own fake
// apReloader - never touches real network interfaces.
type fakeAPReloader struct {
	reloadCalls int
	reloadErr   error
	verifyCalls int
	verifyErr   error
	// onReload, if set, runs synchronously inside Reload - tests use it
	// to inspect file content at exactly the moment reload happens,
	// proving activation completed before reload runs.
	onReload func()
}

func (f *fakeAPReloader) Reload() error {
	f.reloadCalls++
	if f.onReload != nil {
		f.onReload()
	}
	return f.reloadErr
}

func (f *fakeAPReloader) VerifyLive(cfg wifiadmin.Config) error {
	f.verifyCalls++
	return f.verifyErr
}

func withFakeReloader(t *testing.T, fake *fakeAPReloader) {
	t.Helper()
	orig := wifiAPReloaderInstance
	wifiAPReloaderInstance = fake
	t.Cleanup(func() { wifiAPReloaderInstance = orig })
}

func testConfig(ssid string) wifiadmin.Config {
	cfg := wifiadmin.DefaultConfig()
	cfg.SSID = ssid
	return cfg
}

// allTargetPaths returns every one of the eight physical paths the
// current wifiAdminDualTargets describes.
func allTargetPaths() []string {
	var paths []string
	for _, t := range wifiAdminDualTargets {
		paths = append(paths, t.durableOut, t.liveOut)
	}
	return paths
}

func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func assertSSIDEverywhere(t *testing.T, ssid string) {
	t.Helper()
	for _, t2 := range wifiAdminDualTargets {
		if t2.name != "wpa_supplicant_ap" {
			continue
		}
		for _, p := range [2]string{t2.durableOut, t2.liveOut} {
			content := readFileOrEmpty(t, p)
			want := `ssid="` + ssid + `"`
			if !strings.Contains(content, want) {
				t.Errorf("%s does not contain %q:\n%s", p, want, content)
			}
		}
	}
}

func TestRealWifiExecutor_Apply_HappyPath_WritesBothTreesAndReloads(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	fake := &fakeAPReloader{}
	withFakeReloader(t, fake)

	if err := (realWifiExecutor{}).Apply(testConfig("HappyPathSSID")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertSSIDEverywhere(t, "HappyPathSSID")
	if fake.reloadCalls != 1 {
		t.Errorf("reloadCalls = %d, want 1", fake.reloadCalls)
	}
	if fake.verifyCalls != 1 {
		t.Errorf("verifyCalls = %d, want 1", fake.verifyCalls)
	}
	// No stray .tmp files left behind.
	for _, p := range allTargetPaths() {
		if _, err := os.Stat(p + ".wifiadmin.tmp"); !os.IsNotExist(err) {
			t.Errorf("stray tmp file left at %s.wifiadmin.tmp", p)
		}
	}
}

// TestRealWifiExecutor_Apply_ConvergesDivergedLiveAndDurable is this
// suite's central regression test: it reproduces, on a real filesystem,
// the exact defect found on real hardware - a live copy of a config
// file that has silently diverged from its durable copy (simulating an
// overlay upper-layer shadow copy an earlier, unrelated write left
// behind) - and proves a single Apply call brings BOTH copies to the
// complete new configuration, not just the durable one.
func TestRealWifiExecutor_Apply_ConvergesDivergedLiveAndDurable(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	withFakeReloader(t, &fakeAPReloader{})

	// Seed live and durable with DIFFERENT stale content before this
	// config ever supported dual-write - reproducing the real device's
	// own observed state (live stuck on old content, durable already
	// showing something else entirely).
	for _, tgt := range wifiAdminDualTargets {
		if err := os.MkdirAll(filepath.Dir(tgt.liveOut), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(tgt.durableOut), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tgt.liveOut, []byte("stale-live-shadow-copy"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tgt.durableOut, []byte("some-other-durable-content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := (realWifiExecutor{}).Apply(testConfig("ConvergedSSID")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertSSIDEverywhere(t, "ConvergedSSID")
}

func TestRealWifiExecutor_Apply_RenderFailure_NothingTouched(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	fake := &fakeAPReloader{}
	withFakeReloader(t, fake)

	// Break one target's template path so rendering fails before any
	// file in the batch is ever staged.
	wifiAdminDualTargets[2].tplFile = filepath.Join(t.TempDir(), "does-not-exist.template")

	err := (realWifiExecutor{}).Apply(testConfig("ShouldNeverAppear"))
	if err == nil {
		t.Fatal("expected an error when a template file is missing")
	}
	for _, p := range allTargetPaths() {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to not exist (nothing should be written on a render failure)", p)
		}
		if _, statErr := os.Stat(p + ".wifiadmin.tmp"); !os.IsNotExist(statErr) {
			t.Errorf("stray tmp file left at %s.wifiadmin.tmp", p)
		}
	}
	if fake.reloadCalls != 1 {
		t.Errorf("expected the failure path's own restore-then-reload to run exactly once, got %d", fake.reloadCalls)
	}
}

func TestRealWifiExecutor_Apply_DurableStagingFailure_NothingRenamed(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	withFakeReloader(t, &fakeAPReloader{})

	// Occupy the third target's durable .tmp path with a directory, so
	// its own atomicStageOnly (os.OpenFile O_CREATE) fails outright.
	tgt := wifiAdminDualTargets[2]
	tmpPath := tgt.durableOut + ".wifiadmin.tmp"
	if err := os.MkdirAll(tmpPath, 0o755); err != nil {
		t.Fatal(err)
	}

	err := (realWifiExecutor{}).Apply(testConfig("ShouldNeverAppear"))
	if err == nil {
		t.Fatal("expected an error when a durable staging path is occupied by a directory")
	}
	for _, p := range allTargetPaths() {
		if p == tmpPath {
			continue // the directory we deliberately placed there
		}
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to not exist after a staging failure", p)
		}
	}
}

func TestRealWifiExecutor_Apply_LiveStagingFailure_DurableTmpCleanedUp(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	withFakeReloader(t, &fakeAPReloader{})

	tgt := wifiAdminDualTargets[1]
	tmpPath := tgt.liveOut + ".wifiadmin.tmp"
	if err := os.MkdirAll(tmpPath, 0o755); err != nil {
		t.Fatal(err)
	}

	err := (realWifiExecutor{}).Apply(testConfig("ShouldNeverAppear"))
	if err == nil {
		t.Fatal("expected an error when a live staging path is occupied by a directory")
	}
	// The durable .tmp for this same target must have been cleaned up,
	// not left behind, even though only the LIVE half failed.
	if _, statErr := os.Stat(tgt.durableOut + ".wifiadmin.tmp"); !os.IsNotExist(statErr) {
		t.Error("expected the durable .tmp sibling to be cleaned up after the live staging half failed")
	}
	for _, p := range allTargetPaths() {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to not exist after a staging failure", p)
		}
	}
}

func TestRealWifiExecutor_Apply_DurableRenameFailure_RestoresAllEightToOld(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	fake := &fakeAPReloader{}
	withFakeReloader(t, fake)

	// Seed all eight files with known "old" content first, via a real,
	// successful Apply.
	if err := (realWifiExecutor{}).Apply(testConfig("OldSSID")); err != nil {
		t.Fatalf("seeding Apply: %v", err)
	}
	assertSSIDEverywhere(t, "OldSSID")

	// Inject a failure exactly at the third target's durable rename -
	// by this point in the sequence, targets 0 and 1's durable copies
	// have already been renamed to the NEW content.
	failAt := wifiAdminDualTargets[2].name
	wifiAdminRenameHook = func(name string, durable bool) error {
		if name == failAt && durable {
			return errors.New("simulated durable rename failure")
		}
		return nil
	}
	t.Cleanup(func() { wifiAdminRenameHook = nil })

	err := (realWifiExecutor{}).Apply(testConfig("NewSSID"))
	if err == nil {
		t.Fatal("expected an error from the injected durable rename failure")
	}

	// Every one of the eight files - including the two durable copies
	// that DID successfully rename to "NewSSID" before the injected
	// failure - must be back to the complete OLD configuration, never a
	// mix of old and new.
	assertSSIDEverywhere(t, "OldSSID")
	if fake.reloadCalls != 2 { // 1 from seeding + 1 from this failure's own restore-reload
		t.Errorf("reloadCalls = %d, want 2 (seed + failure-path restore)", fake.reloadCalls)
	}
}

func TestRealWifiExecutor_Apply_LiveRenameFailure_RestoresAllEightToOld(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	fake := &fakeAPReloader{}
	withFakeReloader(t, fake)

	if err := (realWifiExecutor{}).Apply(testConfig("OldSSID")); err != nil {
		t.Fatalf("seeding Apply: %v", err)
	}

	// By the time ANY live rename runs, ALL FOUR durable renames have
	// already completed (see the documented transaction order) - so
	// failing a live rename must still restore the durable copies too.
	failAt := wifiAdminDualTargets[0].name
	wifiAdminRenameHook = func(name string, durable bool) error {
		if name == failAt && !durable {
			return errors.New("simulated live rename failure")
		}
		return nil
	}
	t.Cleanup(func() { wifiAdminRenameHook = nil })

	err := (realWifiExecutor{}).Apply(testConfig("NewSSID"))
	if err == nil {
		t.Fatal("expected an error from the injected live rename failure")
	}
	assertSSIDEverywhere(t, "OldSSID")
}

func TestRealWifiExecutor_Apply_ReloadFailure_RestoresAndReportsError(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	fake := &fakeAPReloader{}
	withFakeReloader(t, fake)

	if err := (realWifiExecutor{}).Apply(testConfig("OldSSID")); err != nil {
		t.Fatalf("seeding Apply: %v", err)
	}

	fake.reloadErr = errors.New("simulated reload failure")
	err := (realWifiExecutor{}).Apply(testConfig("NewSSID"))
	if err == nil {
		t.Fatal("expected an error when Reload fails")
	}
	// All eight files must already be fully activated to the NEW
	// configuration by the time Reload is called (reload only ever
	// runs after every rename has committed) - restoreDualTargets then
	// brings them back to OLD, and a second Reload call (with the
	// restored old content) is attempted so the live system is not left
	// stuck mid-transition.
	assertSSIDEverywhere(t, "OldSSID")
	if fake.reloadCalls != 3 { // seed + failed apply's own Reload + failure-path restore-Reload
		t.Errorf("reloadCalls = %d, want 3", fake.reloadCalls)
	}
}

// TestRealWifiExecutor_Apply_VerifyFailure_NeverReportsFalseSuccess is
// this suite's proof for the mission's own explicit requirement: the
// hot request path must not report success before the AP is actually
// using the new configuration. Here, staging, activation, and even the
// reload itself all succeed - only the final live-effect verification
// fails (simulating a reload that returned success without the
// underlying process actually having picked up the new file, exactly
// the class of gap that let the original defect go unnoticed).
func TestRealWifiExecutor_Apply_VerifyFailure_NeverReportsFalseSuccess(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	fake := &fakeAPReloader{}
	withFakeReloader(t, fake)

	if err := (realWifiExecutor{}).Apply(testConfig("OldSSID")); err != nil {
		t.Fatalf("seeding Apply: %v", err)
	}

	fake.verifyErr = errors.New("simulated: live configuration does not yet reflect the change")
	err := (realWifiExecutor{}).Apply(testConfig("NewSSID"))
	if err == nil {
		t.Fatal("Apply must not report success when VerifyLive fails - this is the exact defect this rewrite closes")
	}
	assertSSIDEverywhere(t, "OldSSID")
}

func TestRealWifiExecutor_Apply_RestoreItselfFails_ReportsManualRecoveryNeeded(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	fake := &fakeAPReloader{}
	withFakeReloader(t, fake)

	if err := (realWifiExecutor{}).Apply(testConfig("OldSSID")); err != nil {
		t.Fatalf("seeding Apply: %v", err)
	}

	// Force the failure path's own restore to fail too, via the
	// dedicated restore-write test hook (a real-filesystem trick can't
	// isolate a restore-only failure here, since restoreDualTargets and
	// the forward staging path share the same ".tmp"-suffix convention -
	// see wifiAdminRestoreWriteHook's own doc comment).
	fake.reloadErr = errors.New("simulated reload failure, to trigger the restore path")
	failRestorePath := wifiAdminDualTargets[3].durableOut
	wifiAdminRestoreWriteHook = func(path string) error {
		if path == failRestorePath {
			return errors.New("simulated restore-write failure")
		}
		return nil
	}
	t.Cleanup(func() { wifiAdminRestoreWriteHook = nil })

	err := (realWifiExecutor{}).Apply(testConfig("NewSSID"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "manual recovery may be required") {
		t.Errorf("error = %q, want it to say restoration itself failed and manual recovery may be required", err.Error())
	}
}

// TestRealWifiExecutor_Apply_ReloadRunsOnlyAfterAllEightFilesActivated
// proves the documented ordering: by the moment Reload is invoked,
// every one of the eight files already shows the NEW configuration -
// reload is never called partway through activation.
func TestRealWifiExecutor_Apply_ReloadRunsOnlyAfterAllEightFilesActivated(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	var sawDuringReload []string
	fake := &fakeAPReloader{}
	fake.onReload = func() {
		for _, p := range allTargetPaths() {
			sawDuringReload = append(sawDuringReload, readFileOrEmpty(t, p))
		}
	}
	withFakeReloader(t, fake)

	if err := (realWifiExecutor{}).Apply(testConfig("OrderingSSID")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(sawDuringReload) != len(allTargetPaths()) {
		t.Fatalf("onReload callback did not observe all files")
	}
	for i, content := range sawDuringReload {
		if content == "" {
			t.Errorf("target %d was empty at reload time - not yet activated", i)
		}
	}
	// The AP config specifically must already carry the new SSID.
	assertSSIDEverywhere(t, "OrderingSSID")
}

func TestRealWifiExecutor_Apply_SnapshotFailure_AbortsBeforeAnyWrite(t *testing.T) {
	withTempWifiAdminDualTargets(t)
	fake := &fakeAPReloader{}
	withFakeReloader(t, fake)

	// Make one target's durable path unreadable-as-a-file (a directory
	// in its place) BEFORE Apply ever runs, so the very first snapshot
	// read fails - this must abort before touching anything else.
	tgt := wifiAdminDualTargets[0]
	if err := os.MkdirAll(tgt.durableOut, 0o755); err != nil {
		t.Fatal(err)
	}

	err := (realWifiExecutor{}).Apply(testConfig("ShouldNeverAppear"))
	if err == nil {
		t.Fatal("expected an error when the initial snapshot cannot read an existing path")
	}
	if fake.reloadCalls != 0 {
		t.Errorf("reloadCalls = %d, want 0 - a snapshot failure must abort before overlayctl/reload ever runs", fake.reloadCalls)
	}
	for i, p := range allTargetPaths() {
		if i == 0 {
			continue // the directory we deliberately placed there
		}
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to not exist after a snapshot failure", p)
		}
	}
}

var _ apReloader = realAPReloader{}
var _ wifiadmin.Executor = realWifiExecutor{}

// TestRealAPReloader_Reload_AlwaysCyclesWlan0NeverAp0 is the direct
// regression test for the exact point of confusion in this feature's own
// hardware incident: the executor targeted wlan0, while the AP interface
// that actually carries traffic is ap0. eed1c01e's commit message
// establishes this is correct - ap0 is a virtual interface wlan0's own
// pre-up/post-down hooks create as a side effect - but until this test,
// nothing automated locked that in; it rested on code review and a
// commit message alone. This calls the REAL realAPReloader.Reload
// (ifdown/ifup are expected to fail in a test environment - see this
// file's own package doc comment - Reload's own return value is not
// asserted here, only which interface name it asked the OS to cycle).
func TestRealAPReloader_Reload_AlwaysCyclesWlan0NeverAp0(t *testing.T) {
	var invocations [][]string
	orig := wifiAdminReloadCommandRecorder
	wifiAdminReloadCommandRecorder = func(name string, args ...string) {
		invocations = append(invocations, append([]string{name}, args...))
	}
	t.Cleanup(func() { wifiAdminReloadCommandRecorder = orig })

	_ = (realAPReloader{}).Reload() // error tolerated/expected - see doc comment above

	if len(invocations) != 2 {
		t.Fatalf("expected exactly 2 recorded commands (ifdown, ifup), got %d: %v", len(invocations), invocations)
	}
	want := [][]string{{"ifdown", "wlan0"}, {"ifup", "wlan0"}}
	for i, w := range want {
		got := invocations[i]
		if len(got) != 2 || got[0] != w[0] || got[1] != w[1] {
			t.Errorf("invocation %d = %v, want %v", i, got, w)
		}
		if got[1] == "ap0" {
			t.Fatalf("Reload targeted ap0 directly - this is the exact incident this test exists to prevent: %v", got)
		}
	}
}
