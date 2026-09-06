package recording

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCSVExporter_HeaderAndBasicRow(t *testing.T) {
	var buf bytes.Buffer
	samples := []Sample{
		{
			UTC:           time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
			Latitude:      47.6,
			Longitude:     -122.3,
			GPSAltitudeFt: 1500,
			GroundspeedKt: 95,
		},
	}
	if err := (CSVExporter{}).Export(&buf, samples); err != nil {
		t.Fatalf("Export error: %v", err)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected a header line plus one data line, got %d lines: %q", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "UTC,TimeTrustState,Latitude,Longitude") {
		t.Errorf("unexpected header: %s", lines[0])
	}
	if !strings.Contains(lines[1], "2026-06-01T12:00:00.000Z") {
		t.Errorf("expected an RFC3339-ish UTC timestamp in the data row: %s", lines[1])
	}
}

func TestCSVExporter_NilAHRSFieldsAreEmptyNotZero(t *testing.T) {
	var buf bytes.Buffer
	samples := []Sample{{UTC: time.Now()}} // no AHRS fields set
	if err := (CSVExporter{}).Export(&buf, samples); err != nil {
		t.Fatalf("Export error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	fields := strings.Split(lines[1], ",")
	// PressureAltitudeFt is the 9th column (index 8).
	if fields[8] != "" {
		t.Errorf("nil PressureAltitudeFt should render as an empty CSV cell, got %q", fields[8])
	}
}

func TestCSVExporter_RealZeroValueIsNotEmpty(t *testing.T) {
	var buf bytes.Buffer
	zero := 0.0
	samples := []Sample{{UTC: time.Now(), BankDeg: &zero}}
	if err := (CSVExporter{}).Export(&buf, samples); err != nil {
		t.Fatalf("Export error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	fields := strings.Split(lines[1], ",")
	// BankDeg is the 11th column (index 10).
	if fields[10] != "0" {
		t.Errorf("a real zero BankDeg should render as \"0\", not empty, got %q", fields[10])
	}
}

func TestCSVExporter_TimeTrustStatePreserved(t *testing.T) {
	var buf bytes.Buffer
	samples := []Sample{{UTC: time.Now(), TimeTrustState: "GNSS_SYNCED"}}
	if err := (CSVExporter{}).Export(&buf, samples); err != nil {
		t.Fatalf("Export error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	fields := strings.Split(lines[1], ",")
	if fields[1] != "GNSS_SYNCED" {
		t.Errorf("TimeTrustState column = %q, want GNSS_SYNCED", fields[1])
	}
}

func TestCSVExporter_NewAHRSColumnsPresent(t *testing.T) {
	var buf bytes.Buffer
	vs := -300.0
	gMin, gMax := 0.85, 1.6
	status := uint8(0x1F)
	calState := "READY"
	age := 0.1
	samples := []Sample{{
		UTC:                       time.Now(),
		BaroVerticalSpeedFPM:      &vs,
		GLoadMin:                  &gMin,
		GLoadMax:                  &gMax,
		AHRSStatus:                &status,
		AHRSCalibrationState:      &calState,
		AHRSMeasurementAgeSeconds: &age,
	}}
	if err := (CSVExporter{}).Export(&buf, samples); err != nil {
		t.Fatalf("Export error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	header := strings.Split(lines[0], ",")
	fields := strings.Split(lines[1], ",")
	want := map[string]string{
		"GLoadMin":                  "0.85",
		"GLoadMax":                  "1.6",
		"BaroVerticalSpeedFPM":      "-300",
		"AHRSStatus":                "31",
		"AHRSCalibrationState":      "READY",
		"AHRSMeasurementAgeSeconds": "0.1",
	}
	for col, wantVal := range want {
		idx := -1
		for i, h := range header {
			if h == col {
				idx = i
				break
			}
		}
		if idx == -1 {
			t.Fatalf("column %q not found in header %v", col, header)
		}
		if fields[idx] != wantVal {
			t.Errorf("column %q = %q, want %q", col, fields[idx], wantVal)
		}
	}
}

func TestCSVExporter_NewAHRSColumnsEmptyWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	samples := []Sample{{UTC: time.Now()}}
	if err := (CSVExporter{}).Export(&buf, samples); err != nil {
		t.Fatalf("Export error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	header := strings.Split(lines[0], ",")
	fields := strings.Split(lines[1], ",")
	for _, col := range []string{"GLoadMin", "GLoadMax", "BaroVerticalSpeedFPM", "AHRSStatus", "AHRSCalibrationState", "AHRSMeasurementAgeSeconds"} {
		idx := -1
		for i, h := range header {
			if h == col {
				idx = i
				break
			}
		}
		if idx == -1 {
			t.Fatalf("column %q not found in header %v", col, header)
		}
		if fields[idx] != "" {
			t.Errorf("column %q should be an empty cell when unavailable, got %q", col, fields[idx])
		}
	}
}

func TestGPXExporter_NotYetImplemented(t *testing.T) {
	var buf bytes.Buffer
	err := (GPXExporter{}).Export(&buf, nil)
	if !errors.Is(err, ErrExportNotImplemented) {
		t.Errorf("GPXExporter.Export should return ErrExportNotImplemented, got %v", err)
	}
	if buf.Len() != 0 {
		t.Error("an unimplemented exporter should not write partial/misleading output")
	}
}

func TestKMLExporter_NotYetImplemented(t *testing.T) {
	err := (KMLExporter{}).Export(&bytes.Buffer{}, nil)
	if !errors.Is(err, ErrExportNotImplemented) {
		t.Errorf("KMLExporter.Export should return ErrExportNotImplemented, got %v", err)
	}
}

func TestExporter_InterfaceSatisfiedByAllThree(t *testing.T) {
	var _ Exporter = CSVExporter{}
	var _ Exporter = GPXExporter{}
	var _ Exporter = KMLExporter{}
}
