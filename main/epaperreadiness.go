/*
epaperreadiness.go: gathers the optional e-paper display service's own
self-reported status (common.EpaperStatusPath, written by the separate
epaper_main process - see epaper_main/main.go) and the stratux_epaper
systemd unit's load/active state, and derives a readiness.EpaperHealth.
Mirrors main/fancontrolstatus.go's buildFanHealth exactly - all I/O
happens here so the readiness package itself stays pure and testable
without a real display or systemd.
*/
package main

import (
	"os"
	"time"

	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/epaper"
	"github.com/stratux/stratux/readiness"
)

const (
	// epaperServiceName is the systemd unit installed by
	// debian/stratux_epaper.service.
	epaperServiceName = "stratux_epaper"
	// epaperStaleAfter allows a couple of missed status-file writes (see
	// epaper_main/main.go's default 5-second poll interval) before
	// reading DEGRADED, without masking a genuinely wedged/dead process
	// for long - the same margin buildFanHealth applies for its own,
	// faster-cadence status file.
	epaperStaleAfter = 30 * time.Second
)

// buildEpaperHealth reads the epaper_main daemon's own self-reported
// runtime status and the stratux_epaper systemd unit's state, and derives
// a readiness.EpaperHealth. now is a plain wall-clock reading: epaper_main
// is a separate process with no access to stratuxrun's stratuxClock, so
// its status file's UpdatedAt is necessarily wall-clock, like
// FanHealth.LastUpdateTime.
func buildEpaperHealth(now time.Time) readiness.EpaperHealth {
	state := readiness.UnitActiveState(epaperServiceName)
	installed := readiness.UnitInstalled(state)
	active := state == "active"

	var status epaper.Health
	err := common.ReadEpaperStatus(common.EpaperStatusPath, &status)
	available := err == nil
	malformed := err != nil && !os.IsNotExist(err)

	return readiness.BuildEpaperHealth(
		globalSettings.EpaperEnabled, installed, active, available, malformed,
		string(status.State), status.PanelDetected, string(status.LastErrorCat), status.ConfiguredPanel,
		uint64(status.FullRefreshCount), uint64(status.PartialRefreshCount),
		uint64(status.BusyTimeoutCount), uint64(status.ConsecutiveFailures),
		status.UpdatedAt, now, epaperStaleAfter,
	)
}
