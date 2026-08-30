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
	if !strings.HasPrefix(lines[0], "UTC,Latitude,Longitude") {
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
	// PressureAltitudeFt is the 8th column (index 7).
	if fields[7] != "" {
		t.Errorf("nil PressureAltitudeFt should render as an empty CSV cell, got %q", fields[7])
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
	// BankDeg is the 10th column (index 9).
	if fields[9] != "0" {
		t.Errorf("a real zero BankDeg should render as \"0\", not empty, got %q", fields[9])
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
