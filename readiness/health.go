package readiness

import (
	"time"

	"github.com/stratux/stratux/sdrassign"
)

// HealthReport is the full JSON shape of the /getHealth API response and
// the source of truth for the readiness dashboard. Every field is either a
// plain value or a component health record carrying its own State/Reason,
// so a client can render an overall summary or drill into any one tile
// without a separate schema.
type HealthReport struct {
	GeneratedAt time.Time
	Overall     ComponentState

	UAT978           RadioHealth
	ES1090           RadioHealth
	GPS              GPSHealth
	GDL90            GDL90Health
	System           SystemHealth
	Storage          StorageHealth // persistent data partition
	TemporaryOverlay StorageHealth // the protected RAM-backed root overlay - NOT the recording capacity
	Time             TimeHealth

	// Future hardware: always StateNotInstalled until the physical board
	// exists. Never StateNotReady/StateDegraded for absent hardware - see
	// readiness.FutureHardware.
	AHRS FutureHardwareHealth
	Baro FutureHardwareHealth
	Fan  FutureHardwareHealth
}

// RadioHealth is the health record for one receiver band (978 UAT or 1090
// ES). It wraps sdrassign.BandStatus - the project's existing, tested
// per-band health/diagnosis logic - rather than re-deriving assignment,
// ambiguity, or conflict handling here; this package only adds the
// unified ComponentState classification and the counters the mission
// requires beyond what BandStatus already tracks (frame totals, message
// rates, tower/weather-product counts for UAT).
type RadioHealth struct {
	State  ComponentState
	Reason string
	Band   sdrassign.BandStatus

	TotalFrames           uint64
	MessageRateLastMinute float64
	MessageRatePeak       float64
	LastFrameTime         time.Time
	LastFrameAge          time.Duration

	// UAT-only counters; left at zero for the 1090 ES band.
	TowerCount           int
	WeatherProductCounts map[string]int
}

// StateFromBandStatus maps sdrassign's existing (Enabled/Assigned/
// Ambiguous/Conflict/ExternallySatisfied/Degraded) signals onto the
// unified ComponentState.
//
// A band the operator has intentionally disabled maps to StateNotInstalled
// (gray, "nothing to check here"), not StateNotReady - it is a
// configuration choice, not a failure, and must not render red or amber.
// Band.Enabled/Band.Reason still say plainly that it is disabled by
// configuration, so this is not ambiguous with genuinely absent hardware.
//
// ExternallySatisfied is checked before Assigned/Ambiguous/Conflict, not
// after: an externally-satisfied band (a non-SDR low-power radio already
// serving it, e.g. an external 978 UAT receiver) is never SDR-assigned by
// definition - sdrassign.BuildBandStatus's own Degraded formula already
// excludes ExternallySatisfied bands from being considered degraded for
// exactly this reason (`!a.ExternallySatisfied && (!a.Assigned || ...)`).
// Mapping !b.Assigned straight to StateNotReady without this check would
// misreport a healthy external receiver as a missing one.
func StateFromBandStatus(b sdrassign.BandStatus) ComponentState {
	switch {
	case !b.Enabled:
		return StateNotInstalled
	case b.ExternallySatisfied:
		return StateReady
	case b.Ambiguous, b.Conflict, !b.Assigned:
		return StateNotReady
	case b.Degraded:
		return StateDegraded
	default:
		return StateReady
	}
}

// BuildRadioHealth derives a RadioHealth from a band's existing status plus
// the counters BandStatus itself does not track.
func BuildRadioHealth(b sdrassign.BandStatus, totalFrames uint64, rateLastMinute, ratePeak float64, lastFrameTime, now time.Time, towerCount int, weatherProducts map[string]int) RadioHealth {
	h := RadioHealth{
		State:                 StateFromBandStatus(b),
		Reason:                b.Reason,
		Band:                  b,
		TotalFrames:           totalFrames,
		MessageRateLastMinute: rateLastMinute,
		MessageRatePeak:       ratePeak,
		LastFrameTime:         lastFrameTime,
		TowerCount:            towerCount,
		WeatherProductCounts:  weatherProducts,
	}
	if !lastFrameTime.IsZero() {
		h.LastFrameAge = now.Sub(lastFrameTime)
	}
	return h
}

// GPSHealth is the health record for the GNSS receiver.
type GPSHealth struct {
	State  ComponentState
	Reason string

	Present    bool // a GPS device is known/detected at all
	DeviceType string
	FixType    string

	SatellitesInSolution uint16
	SatellitesSeen       uint16
	SatellitesTracked    uint16
	AccuracyMeters       float32

	LastUpdateTime time.Time
	LastUpdateAge  time.Duration

	ContributesTrustedTime bool
}

// BuildGPSHealth derives a GPSHealth from live GPS signals.
//
// It distinguishes "present without a fix" (State DEGRADED - the hardware
// is there and talking, it just has not resolved a position yet, which is
// routine at startup or indoors) from "missing device" (State NOT_READY -
// no data has arrived at all, which is a real problem for hardware the
// validated baseline requires) and from "present but gone silent"
// (State NOT_READY - it was working and has stopped).
func BuildGPSHealth(present bool, fixType string, deviceType string, satsSolution, satsSeen, satsTracked uint16, accuracyMeters float32, lastUpdateTime, now time.Time, staleAfter time.Duration, timeState TimeState) GPSHealth {
	h := GPSHealth{
		Present:                present,
		DeviceType:             deviceType,
		FixType:                fixType,
		SatellitesInSolution:   satsSolution,
		SatellitesSeen:         satsSeen,
		SatellitesTracked:      satsTracked,
		AccuracyMeters:         accuracyMeters,
		LastUpdateTime:         lastUpdateTime,
		ContributesTrustedTime: timeState == TimeGNSSSynced,
	}
	if !lastUpdateTime.IsZero() {
		h.LastUpdateAge = now.Sub(lastUpdateTime)
	}

	switch {
	case !present:
		h.State = StateNotReady
		h.Reason = "GPS device not detected"
	case lastUpdateTime.IsZero() || h.LastUpdateAge > staleAfter:
		h.State = StateNotReady
		h.Reason = "GPS device present but not reporting - no data received recently"
	case satsSolution == 0:
		h.State = StateDegraded
		h.Reason = "GPS present and reporting, no satellite solution yet"
	default:
		h.State = StateReady
		h.Reason = "GPS fix: " + fixType
	}
	return h
}

// GDL90Health is the health record for GDL90 generation/output.
//
// ForeFlightClientDetected must only be set true from evidence that
// actually identifies a ForeFlight client (e.g. a distinguishing query
// string/heartbeat ForeFlight itself sends) - the absence of such evidence
// means "unknown/not confirmed", not "ForeFlight is not connected", since
// most EFB clients that consume GDL90 do not identify themselves at all.
type GDL90Health struct {
	State  ComponentState
	Reason string

	Generating   bool
	OutputActive bool

	RecentClientCount        int
	LastClientActivity       time.Time
	ForeFlightClientDetected bool
}

// BuildGDL90Health derives a GDL90Health from live output signals.
func BuildGDL90Health(generating, outputActive bool, recentClientCount int, lastClientActivity time.Time, foreFlightDetected bool) GDL90Health {
	h := GDL90Health{
		Generating:               generating,
		OutputActive:             outputActive,
		RecentClientCount:        recentClientCount,
		LastClientActivity:       lastClientActivity,
		ForeFlightClientDetected: foreFlightDetected,
	}
	switch {
	case !generating || !outputActive:
		h.State = StateNotReady
		h.Reason = "GDL90 output is not active"
	case recentClientCount == 0:
		h.State = StateReady
		h.Reason = "GDL90 generating and available; no client currently connected"
	default:
		h.State = StateReady
		h.Reason = "GDL90 active with a connected client"
	}
	return h
}

// SystemHealth is the health record for overall system vitals.
type SystemHealth struct {
	State  ComponentState
	Reason string

	Version string
	Commit  string
	Uptime  time.Duration

	CPUTempC             float64
	Throttled            bool
	UndervoltageDetected bool
	FailedServices       []string
}

// BuildSystemHealth derives a SystemHealth from live system signals.
func BuildSystemHealth(version, commit string, uptime time.Duration, cpuTempC float64, throttled, undervoltage bool, failedServices []string) SystemHealth {
	h := SystemHealth{
		Version:              version,
		Commit:               commit,
		Uptime:               uptime,
		CPUTempC:             cpuTempC,
		Throttled:            throttled,
		UndervoltageDetected: undervoltage,
		FailedServices:       failedServices,
	}
	switch {
	case len(failedServices) > 0:
		h.State = StateNotReady
		h.Reason = "one or more systemd units have failed"
	case undervoltage:
		h.State = StateDegraded
		h.Reason = "undervoltage detected"
	case throttled:
		h.State = StateDegraded
		h.Reason = "CPU is thermally throttled"
	default:
		h.State = StateReady
		h.Reason = "system nominal"
	}
	return h
}

// FutureHardwareHealth is the health record for a component whose physical
// hardware does not yet exist in this build (AHRS, barometer, fan
// controller integration). It is always StateNotInstalled - never
// StateNotReady/StateDegraded, which would misrepresent planned-but-absent
// hardware as a failure - so the interface can be replaced with a real
// health record once the hardware exists without any caller needing to
// change how it interprets the State field.
type FutureHardwareHealth struct {
	State  ComponentState
	Reason string
}

// NotInstalled builds a FutureHardwareHealth with the given explanation.
func NotInstalled(reason string) FutureHardwareHealth {
	return FutureHardwareHealth{State: StateNotInstalled, Reason: reason}
}

// BuildHealthReport assembles the full report and computes Overall via
// Rollup, so the aggregate always reflects the mission's color rules
// (StateNotInstalled/StateUnknown components never drag down an otherwise-
// healthy Overall; any real StateNotReady always shows).
func BuildHealthReport(now time.Time, uat978, es1090 RadioHealth, gps GPSHealth, gdl90 GDL90Health, system SystemHealth, storage, overlay StorageHealth, timeHealth TimeHealth, timeState TimeState, ahrs, baro, fan FutureHardwareHealth) HealthReport {
	r := HealthReport{
		GeneratedAt:      now,
		UAT978:           uat978,
		ES1090:           es1090,
		GPS:              gps,
		GDL90:            gdl90,
		System:           system,
		Storage:          storage,
		TemporaryOverlay: overlay,
		Time:             timeHealth,
		AHRS:             ahrs,
		Baro:             baro,
		Fan:              fan,
	}
	r.Overall = Rollup(
		uat978.State, es1090.State, gps.State, gdl90.State, system.State,
		storage.State, timeStateToComponentState(timeState),
		ahrs.State, baro.State, fan.State,
	)
	return r
}

// timeStateToComponentState maps the trusted-time state machine's TimeState
// onto the unified ComponentState for rollup purposes. Note the temporary
// overlay is deliberately excluded from the Overall rollup inputs above:
// its capacity is expected to run at high utilization by design (it is a
// small tmpfs holding transient runtime state), so folding its
// StorageHealth into Overall would make a perfectly normal system read as
// degraded. Its health is still reported in full in TemporaryOverlay for
// the dashboard's own tile.
func timeStateToComponentState(s TimeState) ComponentState {
	switch s {
	case TimeGNSSSynced, TimeNetworkSynced:
		return StateReady
	case TimeDegraded:
		return StateDegraded
	case TimeInvalid:
		return StateNotReady
	default: // TimeUnsynchronized
		return StateDegraded
	}
}
