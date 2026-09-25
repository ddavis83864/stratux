package common

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// EpaperStatusPath is the well-known runtime status file path both
// epaper_main and main/ agree on - the same RAM-backed /run convention as
// FanControllerStatusPath (see fanstatus.go's own doc comment for the
// full rationale). Under systemd, the containing directory is created by
// RuntimeDirectory= (see debian/stratux_epaper.service); the writer also
// creates it directly (MkdirAll) so behavior is identical run manually.
const EpaperStatusPath = "/run/stratux-epaper/status.json"

// WriteEpaperStatus atomically writes status (an epaper.Health value, but
// accepted as interface{} here so this leaf package need not import
// epaper) as JSON to path: a temp file in the same directory, then
// os.Rename, so a concurrent reader never observes a partially-written
// file - identical pattern to WriteFanControllerStatus.
func WriteEpaperStatus(path string, status interface{}) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(status)
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

// ReadEpaperStatus reads and unmarshals a status file written by
// WriteEpaperStatus into out (a pointer to an epaper.Health value). A
// missing file surfaces as a plain os.IsNotExist-satisfying error.
func ReadEpaperStatus(path string, out interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
