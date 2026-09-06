package common

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// FanControllerStatus is the fancontrol daemon's self-reported runtime
// status, written atomically to a small file under /run (tmpfs, RAM-backed,
// cleared on reboot) so the main stratuxrun daemon can report live
// fan-controller health without depending on an HTTP round trip to a fixed
// port. See fancontrol_main/fancontrol.go (writer), main/fancontrolstatus.go
// (reader/glue), and readiness.FanHealth (the derived health judgment).
//
// This is a low-rate (~1Hz) status snapshot, not telemetry - nothing here
// is written to the SD card or the persistent data partition, matching the
// mission's requirement that only bounded, RAM-backed runtime state be used
// for this purpose.
type FanControllerStatus struct {
	UpdatedAt time.Time

	// ControllerState is a short, human-readable word describing what the
	// control loop is currently doing: "STARTING" (the brief power-on fan
	// test), "COMMANDING" (PID output above the configured minimum and
	// being applied), "IDLE" (running at the configured minimum duty,
	// effectively steady-state), or "ERROR" (a hardware/GPIO problem -
	// see Error).
	ControllerState string
	// Error is non-empty only when the controller has a real problem to
	// report (e.g. it could not open GPIO). Empty means no error.
	Error string

	CPUTempC    float64
	TempTargetC float64

	PWMDutyMinPercent    uint32
	RequestedDutyPercent uint32
	PWMFrequencyHz       uint32
	PWMPin               int

	// TachometerSupported is always false: this hardware revision has no
	// tachometer or other rotation-feedback pin, so physical fan rotation
	// can never be confirmed from software - see readiness.FanHealth,
	// which must never claim the fan is confirmed spinning on the
	// strength of this status alone.
	TachometerSupported bool
}

// FanControllerStatusPath is the well-known runtime status file path both
// fancontrol_main and main/ agree on. It lives under /run - tmpfs,
// RAM-backed, cleared on reboot - never the SD card or the persistent
// partition. Under systemd, the containing directory is created ahead of
// time by RuntimeDirectory= (see debian/stratux_fancontrol.service);
// WriteFanControllerStatus also creates it directly (MkdirAll) so the
// daemon behaves the same way run manually, e.g. during development.
const FanControllerStatusPath = "/run/stratux-fancontrol/status.json"

// WriteFanControllerStatus atomically writes status as JSON to path: a
// temp file in the same directory, then os.Rename, so a concurrent reader
// (main/'s health builder) never observes a partially-written file.
func WriteFanControllerStatus(path string, status FanControllerStatus) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(&status)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ReadFanControllerStatus reads and parses a status file written by
// WriteFanControllerStatus. A missing file surfaces as a plain
// os.IsNotExist-satisfying error, so the caller can distinguish "never
// written yet" from "present but malformed" (a JSON error).
func ReadFanControllerStatus(path string) (FanControllerStatus, error) {
	var status FanControllerStatus
	data, err := os.ReadFile(path)
	if err != nil {
		return status, err
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return status, err
	}
	return status, nil
}
