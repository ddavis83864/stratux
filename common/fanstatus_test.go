package common

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadFanControllerStatus_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run", "status.json")
	want := FanControllerStatus{
		UpdatedAt:            time.Now().UTC().Truncate(time.Second),
		ControllerState:      "COMMANDING",
		CPUTempC:             55.5,
		TempTargetC:          50.0,
		PWMDutyMinPercent:    10,
		RequestedDutyPercent: 65,
		PWMFrequencyHz:       64000,
		PWMPin:               18,
		TachometerSupported:  false,
	}
	if err := WriteFanControllerStatus(path, want); err != nil {
		t.Fatalf("WriteFanControllerStatus: %v", err)
	}
	got, err := ReadFanControllerStatus(path)
	if err != nil {
		t.Fatalf("ReadFanControllerStatus: %v", err)
	}
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestWriteFanControllerStatus_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "run", "status.json")
	if err := WriteFanControllerStatus(path, FanControllerStatus{}); err != nil {
		t.Fatalf("WriteFanControllerStatus: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("status file was not created: %v", err)
	}
}

func TestWriteFanControllerStatus_NoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	if err := WriteFanControllerStatus(path, FanControllerStatus{ControllerState: "IDLE"}); err != nil {
		t.Fatalf("WriteFanControllerStatus: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file should not remain after a successful atomic write, stat err = %v", err)
	}
}

func TestReadFanControllerStatus_MissingFileIsNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadFanControllerStatus(filepath.Join(dir, "does-not-exist.json"))
	if !os.IsNotExist(err) {
		t.Errorf("expected an IsNotExist error for a never-written status file, got %v", err)
	}
}

func TestReadFanControllerStatus_MalformedFileIsDistinctError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadFanControllerStatus(path)
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if os.IsNotExist(err) {
		t.Error("a malformed file must not be reported as IsNotExist - the caller needs to tell these apart")
	}
}
