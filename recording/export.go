package recording

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
)

// Exporter converts a slice of Sample into some external track-log format.
// This interface is the mission's requested "eventual CSV, GPX and KML
// export interfaces" - CSVExporter is a real, tested implementation;
// GPXExporter and KMLExporter are defined now so callers/tests can depend
// on the interface shape, but deliberately return a clear
// ErrExportNotImplemented rather than a silent no-op or a half-correct
// format, consistent with not claiming complete ForeFlight track-log
// integration ahead of it actually existing.
type Exporter interface {
	Export(w io.Writer, samples []Sample) error
}

// ErrExportNotImplemented is returned by an Exporter that defines its
// interface but does not yet implement real output.
var ErrExportNotImplemented = errors.New("recording: export format not yet implemented")

// CSVExporter writes samples as CSV. AHRS/pressure-altitude fields that
// are nil for a given sample render as an empty cell, never as 0 - an
// empty cell in a spreadsheet reads unambiguously as "no data", where a
// 0 could be mistaken for a real reading.
type CSVExporter struct{}

var csvHeader = []string{
	"UTC", "TimeTrustState", "Latitude", "Longitude", "GPSAltitudeFt", "GPSAccuracyMeters",
	"GroundspeedKt", "CourseDeg", "PressureAltitudeFt", "PitchDeg", "BankDeg",
	"VerticalAccelG", "GLoad", "GLoadMin", "GLoadMax", "BaroVerticalSpeedFPM",
	"AHRSStatus", "AHRSCalibrationState", "AHRSMeasurementAgeSeconds",
	"UAT978MessageRateLastMinute",
	"ES1090MessageRateLastMinute", "FISBTowerCount",
	"SystemHealthTransition", "TimeSourceTransition",
}

func optionalFloat(f *float64) string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%g", *f)
}

func optionalUint8(u *uint8) string {
	if u == nil {
		return ""
	}
	return fmt.Sprintf("%d", *u)
}

func optionalString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Export writes samples as CSV to w.
func (CSVExporter) Export(w io.Writer, samples []Sample) error {
	writer := csv.NewWriter(w)
	if err := writer.Write(csvHeader); err != nil {
		return fmt.Errorf("could not write CSV header: %w", err)
	}
	for _, s := range samples {
		record := []string{
			s.UTC.UTC().Format("2006-01-02T15:04:05.000Z"),
			s.TimeTrustState,
			fmt.Sprintf("%g", s.Latitude),
			fmt.Sprintf("%g", s.Longitude),
			fmt.Sprintf("%g", s.GPSAltitudeFt),
			fmt.Sprintf("%g", s.GPSAccuracyMeters),
			fmt.Sprintf("%g", s.GroundspeedKt),
			fmt.Sprintf("%g", s.CourseDeg),
			optionalFloat(s.PressureAltitudeFt),
			optionalFloat(s.PitchDeg),
			optionalFloat(s.BankDeg),
			optionalFloat(s.VerticalAccelG),
			optionalFloat(s.GLoad),
			optionalFloat(s.GLoadMin),
			optionalFloat(s.GLoadMax),
			optionalFloat(s.BaroVerticalSpeedFPM),
			optionalUint8(s.AHRSStatus),
			optionalString(s.AHRSCalibrationState),
			optionalFloat(s.AHRSMeasurementAgeSeconds),
			fmt.Sprintf("%g", s.UAT978MessageRateLastMinute),
			fmt.Sprintf("%g", s.ES1090MessageRateLastMinute),
			fmt.Sprintf("%d", s.FISBTowerCount),
			s.SystemHealthTransition,
			s.TimeSourceTransition,
		}
		if err := writer.Write(record); err != nil {
			return fmt.Errorf("could not write CSV record: %w", err)
		}
	}
	writer.Flush()
	return writer.Error()
}

// GPXExporter is a placeholder for future GPX (GPS Exchange Format)
// export. Not yet implemented - see ErrExportNotImplemented.
type GPXExporter struct{}

// Export always returns ErrExportNotImplemented today.
func (GPXExporter) Export(w io.Writer, samples []Sample) error {
	return ErrExportNotImplemented
}

// KMLExporter is a placeholder for future KML (Keyhole Markup Language)
// export. Not yet implemented - see ErrExportNotImplemented.
type KMLExporter struct{}

// Export always returns ErrExportNotImplemented today.
func (KMLExporter) Export(w io.Writer, samples []Sample) error {
	return ErrExportNotImplemented
}
