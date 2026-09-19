package main

import (
	"context"
	"testing"
	"time"

	"github.com/stratux/stratux/epaper"
)

// TestRefreshOnce_PanicIsContained proves refreshOnce's own defer/recover
// (main.go) actually catches a real panic mid-refresh, rather than merely
// asserting the recover() call exists by inspection - a real
// hardware-validation-review finding: this codebase's fault-isolation
// story rests on this being genuine, and it previously had no test
// forcing an actual panic through the call graph. A nil *Driver forces a
// real, realistic nil-pointer-dereference panic inside driver.Update's
// own first statement (d.WidthPx) - exactly the class of programming
// error this recover() exists to contain, not a synthetic panic() call
// unrelated to any real code path.
func TestRefreshOnce_PanicIsContained(t *testing.T) {
	srv := fixtureServer(t, map[string]interface{}{
		"/getStatus":           map[string]interface{}{"Version": "v2.0.0", "Build": "0123456789abcdef"},
		"/getHealth":           map[string]interface{}{"Overall": "READY"},
		"/getStorageLifecycle": map[string]interface{}{"pressure": "NORMAL"},
		"/getAutoRecordStatus": map[string]interface{}{"snapshot": map[string]interface{}{"State": "ARMED_WAITING"}},
		"/getPowerHealth":      map[string]interface{}{},
		"/getAlertSettings":    map[string]interface{}{"masterEnabled": true},
		"/getAlerts":           map[string]interface{}{"muted": false},
	})
	defer srv.Close()

	src := NewStatusSource(srv.URL, 3*time.Second)
	cfg := epaper.Config{
		Enabled: true, Panel: epaper.PanelWaveshare37, Page: epaper.PageOverview,
		RefreshIntervalSeconds: epaper.DefaultRefreshIntervalSeconds,
		FullRefreshEvery:       epaper.DefaultFullRefreshEvery,
	}
	// A fresh, zero-value PolicyState's LastRefreshAt is the zero time -
	// maximally stale - so epaper.Decide is guaranteed to choose a real
	// refresh (never RefreshNone) on this first call, guaranteeing
	// refreshOnce reaches the driver.Update call this test targets.
	policy := &epaper.PolicyState{}
	prev := epaper.Health{}

	var driver *Driver // deliberately nil - forces a real panic in Update

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("refreshOnce did not contain its own panic - it escaped to the caller: %v", r)
			}
		}()
		result := refreshOnce(context.Background(), driver, src, cfg, policy, prev)
		if result.State != epaper.StateError {
			t.Errorf("refreshOnce after a contained panic: State = %q, want %q", result.State, epaper.StateError)
		}
		if result.LastErrorCat != epaper.ErrorSPIWrite {
			t.Errorf("refreshOnce after a contained panic: LastErrorCat = %q, want %q", result.LastErrorCat, epaper.ErrorSPIWrite)
		}
	}()
}
