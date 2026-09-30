package fisbrecorder

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// buildUplinkGDL90Record wraps payload in this fork's own relay header
// (message ID 0x07 + 3 zero reserved bytes) and frames it, matching
// main/gen_gdl90.go's relayMessage exactly.
func buildUplinkGDL90Record(payload []byte) []byte {
	body := append([]byte{GDL90MessageIDUplink, 0, 0, 0}, payload...)
	return buildGDL90Frame(body)
}

func writeSettingsSnapshotWithOutputs(t *testing.T, r *Recorder, outputs []map[string]interface{}) {
	t.Helper()
	r.RecordSnapshot("settings", map[string]interface{}{
		"NetworkOutputs": toInterfaceSlice(outputs),
	})
}

func toInterfaceSlice(outputs []map[string]interface{}) []interface{} {
	out := make([]interface{}, len(outputs))
	for i, o := range outputs {
		out[i] = o
	}
	return out
}

func recordWeatherSession(t *testing.T, connKey string, weatherPayloads [][]byte, heartbeatCount int, outputs []map[string]interface{}) string {
	t.Helper()
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())
	sid, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	writeSettingsSnapshotWithOutputs(t, r, outputs)
	for i := 0; i < heartbeatCount; i++ {
		r.RecordGDL90(connKey, buildGDL90Frame([]byte{0x00, byte(i)}))
	}
	for _, p := range weatherPayloads {
		r.RecordGDL90(connKey, buildUplinkGDL90Record(p))
	}
	if _, err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return filepath.Join(dir, sid)
}

// efbOutputs marshals through JSON and back so it round-trips the exact
// same float64-typed-numbers shape a real snapshot decoded from
// snapshots.jsonl.gz has (readSnapshotsAt unmarshals Data as
// interface{}, which for JSON numbers is always float64) - using a plain
// Go map with int literals in a test would NOT reproduce that and could
// hide a type-assertion bug efbCapableConnectionKeys actually has to
// handle against the real on-disk format.
func efbOutputs(t *testing.T, port int, capability int) []map[string]interface{} {
	t.Helper()
	raw := map[string]interface{}{"Ip": "", "Port": port, "Capability": capability}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return []map[string]interface{}{out}
}

func TestCompareWeatherGDL90_IsolatesWeatherFromHeartbeats(t *testing.T) {
	outputs := efbOutputs(t, 4000, 5) // NETWORK_GDL90_STANDARD | NETWORK_AHRS_GDL90
	dirA := recordWeatherSession(t, "192.168.10.22:4000", [][]byte{[]byte("weather-A-1"), []byte("weather-A-2")}, 50, outputs)
	dirB := recordWeatherSession(t, "192.168.10.22:4000", [][]byte{[]byte("weather-A-1"), []byte("weather-A-2")}, 10, outputs)

	rep, err := CompareWeatherGDL90(dirA, dirB)
	if err != nil {
		t.Fatalf("CompareWeatherGDL90: %v", err)
	}
	cc, ok := rep.PerConnection["192.168.10.22:4000"]
	if !ok {
		t.Fatal("no comparison entry for the connection that carried weather")
	}
	// Different heartbeat counts (50 vs 10) must NOT show up here at all -
	// only the 2 weather frames on each side.
	if cc.CountA != 2 || cc.CountB != 2 {
		t.Errorf("CountA=%d CountB=%d, want 2/2 (heartbeats must not leak into the weather-only count)", cc.CountA, cc.CountB)
	}
	if cc.Matched != 2 || len(cc.Mismatched) != 0 {
		t.Errorf("Matched=%d Mismatched=%v, want 2/none", cc.Matched, cc.Mismatched)
	}
	if !cc.IsEFBCapable {
		t.Error("IsEFBCapable = false for a port-4000 (NETWORK_GDL90_STANDARD) connection")
	}
}

func TestCompareWeatherGDL90_DetectsByteMismatch(t *testing.T) {
	outputs := efbOutputs(t, 4000, 5)
	dirA := recordWeatherSession(t, "1.2.3.4:4000", [][]byte{[]byte("original-payload")}, 0, outputs)
	dirB := recordWeatherSession(t, "1.2.3.4:4000", [][]byte{[]byte("different-payload")}, 0, outputs)

	rep, err := CompareWeatherGDL90(dirA, dirB)
	if err != nil {
		t.Fatalf("CompareWeatherGDL90: %v", err)
	}
	cc := rep.PerConnection["1.2.3.4:4000"]
	if len(cc.Mismatched) != 1 {
		t.Fatalf("Mismatched = %v, want exactly 1 entry", cc.Mismatched)
	}
	if cc.Mismatched[0].FirstDiffOffset != 0 {
		t.Errorf("FirstDiffOffset = %d, want 0 (payloads differ from the first byte: 'o' vs 'd')", cc.Mismatched[0].FirstDiffOffset)
	}
}

func TestCompareWeatherGDL90_OnlyInOneSideCounted(t *testing.T) {
	outputs := efbOutputs(t, 4000, 5)
	dirA := recordWeatherSession(t, "1.1.1.1:4000", [][]byte{[]byte("p1"), []byte("p2"), []byte("p3")}, 0, outputs)
	dirB := recordWeatherSession(t, "1.1.1.1:4000", [][]byte{[]byte("p1")}, 0, outputs)

	rep, err := CompareWeatherGDL90(dirA, dirB)
	if err != nil {
		t.Fatalf("CompareWeatherGDL90: %v", err)
	}
	cc := rep.PerConnection["1.1.1.1:4000"]
	if cc.Matched != 1 {
		t.Errorf("Matched = %d, want 1", cc.Matched)
	}
	if cc.OnlyInA != 2 {
		t.Errorf("OnlyInA = %d, want 2", cc.OnlyInA)
	}
	if cc.OnlyInB != 0 {
		t.Errorf("OnlyInB = %d, want 0", cc.OnlyInB)
	}
}

func TestCompareWeatherGDL90_NonEFBConnectionStillReportedButNotFlagged(t *testing.T) {
	// Port 2000 with capability 8 (NETWORK_FLARM_NMEA only, no
	// NETWORK_GDL90_STANDARD bit) - a real weather-shaped frame sent
	// there would be unusual, but the comparison must still report it
	// (never silently drop a connection), just with IsEFBCapable=false.
	outputs := efbOutputs(t, 2000, 8)
	dirA := recordWeatherSession(t, "1.1.1.1:2000", [][]byte{[]byte("p1")}, 0, outputs)
	dirB := recordWeatherSession(t, "1.1.1.1:2000", [][]byte{[]byte("p1")}, 0, outputs)

	rep, err := CompareWeatherGDL90(dirA, dirB)
	if err != nil {
		t.Fatalf("CompareWeatherGDL90: %v", err)
	}
	cc, ok := rep.PerConnection["1.1.1.1:2000"]
	if !ok {
		t.Fatal("non-EFB connection missing from PerConnection - must still be reported")
	}
	if cc.IsEFBCapable {
		t.Error("IsEFBCapable = true for a NETWORK_FLARM_NMEA-only (no GDL90 bit) connection")
	}
	for _, k := range rep.EFBConnectionKeys {
		if k == "1.1.1.1:2000" {
			t.Error("non-EFB connection incorrectly listed in EFBConnectionKeys")
		}
	}
}

func TestCompareWeatherGDL90_NoWeatherFramesIsEmptyReport(t *testing.T) {
	outputs := efbOutputs(t, 4000, 5)
	dirA := recordWeatherSession(t, "1.1.1.1:4000", nil, 20, outputs)
	dirB := recordWeatherSession(t, "1.1.1.1:4000", nil, 5, outputs)

	rep, err := CompareWeatherGDL90(dirA, dirB)
	if err != nil {
		t.Fatalf("CompareWeatherGDL90: %v", err)
	}
	if len(rep.PerConnection) != 0 {
		t.Errorf("PerConnection = %v, want empty (no weather-bearing frames in either bundle)", rep.PerConnection)
	}
}

func TestCompareWeatherGDL90_MissingGDL90FileReturnsError(t *testing.T) {
	dirA := t.TempDir() // never recorded
	dirB := recordWeatherSession(t, "1.1.1.1:4000", [][]byte{[]byte("p1")}, 0, efbOutputs(t, 4000, 5))
	if _, err := CompareWeatherGDL90(dirA, dirB); err == nil {
		t.Fatal("CompareWeatherGDL90 with a missing gdl90.jsonl.gz returned no error")
	}
}
