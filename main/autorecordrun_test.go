/*
autorecordrun_test.go: regression coverage for the persistent-data
mount-readiness race in initAutoRecord/autoRecordAwaitMountAndReload -
see their doc comments in autorecordrun.go for the full explanation.
*/
package main

import (
	"testing"
	"time"

	"github.com/stratux/stratux/autorecord"
)

// withTestAutoRecordMountReady overrides autoRecordMountReady for the
// duration of one test - mirrors withTestAutoRecordSettingsPath's own
// pattern - and restores the real, findmnt-backed implementation
// afterward.
func withTestAutoRecordMountReady(t *testing.T, fn func() bool) {
	t.Helper()
	orig := autoRecordMountReady
	autoRecordMountReady = fn
	t.Cleanup(func() { autoRecordMountReady = orig })
}

// TestAutoRecordAwaitMountAndReload_ReloadsOnceMountBecomesReady
// reproduces the real bug found on hardware: a settings file exists and
// is genuinely enabled, but the initial cache load happened while
// PersistentDataPath was not yet mounted (autoRecordMountReady reports
// false), so the cache was wrongly populated with the disabled default.
// Once the mount becomes ready, the background retry must reload and
// correct the cache without anyone calling /setAutoRecordSettings again.
func TestAutoRecordAwaitMountAndReload_ReloadsOnceMountBecomesReady(t *testing.T) {
	withTestAutoRecordSettingsPath(t)
	enabled := autorecord.DefaultSettings()
	enabled.Enabled = true
	if err := saveAutoRecordSettings(enabled); err != nil {
		t.Fatalf("saveAutoRecordSettings: %v", err)
	}

	// Simulate initAutoRecord's own one-time synchronous load having run
	// while the mount was not yet ready: the settings file above is
	// real and enabled, but the cache starts out wrongly disabled.
	autoRecordMu.Lock()
	autoRecordSettingsCache = autorecord.DefaultSettings()
	autoRecordMu.Unlock()

	var ready bool
	withTestAutoRecordMountReady(t, func() bool { return ready })

	done := make(chan struct{})
	go func() {
		autoRecordAwaitMountAndReload(2 * time.Second)
		close(done)
	}()

	// Let it observe "not ready" at least once before flipping the mount
	// to ready - proves it genuinely retries rather than only checking
	// once.
	time.Sleep(150 * time.Millisecond)
	autoRecordMu.Lock()
	stillDisabled := !autoRecordSettingsCache.Enabled
	autoRecordMu.Unlock()
	if !stillDisabled {
		t.Fatal("cache was corrected before the mount was ever reported ready")
	}
	ready = true

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("autoRecordAwaitMountAndReload did not return after the mount became ready")
	}

	autoRecordMu.Lock()
	got := autoRecordSettingsCache
	autoRecordMu.Unlock()
	if !got.Enabled {
		t.Fatalf("expected the cache to be corrected to the real (enabled) settings, got %+v", got)
	}
}

// TestAutoRecordAwaitMountAndReload_GivesUpAfterTimeout proves the retry
// is genuinely bounded: if the mount never becomes ready, the function
// returns on its own (never blocks forever) and leaves the safe default
// in place rather than reading a directory that may not really exist.
func TestAutoRecordAwaitMountAndReload_GivesUpAfterTimeout(t *testing.T) {
	withTestAutoRecordSettingsPath(t)
	withTestAutoRecordMountReady(t, func() bool { return false })

	autoRecordMu.Lock()
	autoRecordSettingsCache = autorecord.DefaultSettings()
	autoRecordMu.Unlock()

	done := make(chan struct{})
	go func() {
		autoRecordAwaitMountAndReload(200 * time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("autoRecordAwaitMountAndReload did not give up within its bounded timeout")
	}

	autoRecordMu.Lock()
	got := autoRecordSettingsCache
	autoRecordMu.Unlock()
	if got.Enabled {
		t.Fatalf("expected the safe default to remain in place when the mount never becomes ready, got %+v", got)
	}
}

// TestAutoRecordAwaitMountAndReload_NoOpWhenAlreadyCorrect confirms the
// no-op case doesn't log a spurious "reloaded" message or otherwise
// misbehave when the cache already matched (e.g. the mount really was
// ready immediately and initAutoRecord never even started this
// goroutine in the first place - this test exercises the function
// directly regardless).
func TestAutoRecordAwaitMountAndReload_NoOpWhenAlreadyCorrect(t *testing.T) {
	withTestAutoRecordSettingsPath(t)
	enabled := autorecord.DefaultSettings()
	enabled.Enabled = true
	if err := saveAutoRecordSettings(enabled); err != nil {
		t.Fatalf("saveAutoRecordSettings: %v", err)
	}
	autoRecordMu.Lock()
	autoRecordSettingsCache = enabled
	autoRecordMu.Unlock()

	withTestAutoRecordMountReady(t, func() bool { return true })

	done := make(chan struct{})
	go func() {
		autoRecordAwaitMountAndReload(2 * time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("autoRecordAwaitMountAndReload did not return promptly when already ready")
	}

	autoRecordMu.Lock()
	got := autoRecordSettingsCache
	autoRecordMu.Unlock()
	if got != enabled {
		t.Fatalf("expected the cache to remain exactly the already-correct value, got %+v", got)
	}
}
