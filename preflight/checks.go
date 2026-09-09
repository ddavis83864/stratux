package preflight

import "github.com/stratux/stratux/readiness"

// This file implements every individual automated check listed in
// docs/preflight-readiness.md, one function per report section. Each
// function is pure (readiness.HealthReport in, []CheckResult out) and
// intentionally does not re-derive any component-health judgment
// readiness has already made - it only reclassifies an existing
// readiness.ComponentState (plus a small amount of extra context, like a
// startup grace period) onto the preflight State/Severity vocabulary, per
// the documented rules below.

// fromComponentState is the default, most common reclassification: a
// confirmed readiness failure is a blocking preflight failure, a
// readiness DEGRADED is a preflight caution, an installed/working
// component is preflight-ready, and NOT_INSTALLED is preflight
// not-applicable (never affects the rollup). Individual checks below
// override this default wherever the documented policy calls for
// something more specific (e.g. "no current traffic is not a hardware
// failure").
func fromComponentState(s readiness.ComponentState) (State, Severity) {
	switch s {
	case readiness.StateReady:
		return StateReady, SeverityBlocking // Severity is irrelevant when State is Ready; kept Blocking so a caller who ignores State would still see "would be blocking if not ready," never silently Info.
	case readiness.StateDegraded:
		return StateCaution, SeverityCaution
	case readiness.StateNotReady:
		return StateNotReady, SeverityBlocking
	case readiness.StateNotInstalled:
		return StateNotApplicable, SeverityInfo
	default: // StateUnknown or an invalid value
		return StateUnknown, SeverityCaution
	}
}

// --- Core system -----------------------------------------------------

func coreSystemChecks(in Input) []CheckResult {
	sys := in.Health.System
	var out []CheckResult

	// Overall daemon health, independent of the specific failed-unit
	// list below (System.State already folds in throttling/undervoltage
	// too - see readiness.BuildSystemHealth - so this is a useful
	// at-a-glance item even before the more specific checks that follow).
	state, sev := fromComponentState(sys.State)
	out = append(out, newCheck("System", "stratux_service", "Stratux service health", state, sev, sys.Reason))

	// Critical daemon/service failure - any systemd unit reporting
	// "failed" is always blocking, regardless of what else is healthy,
	// per the mission's explicit NOT_READY rule.
	if len(sys.FailedServices) > 0 {
		out = append(out, newCheck("System", "failed_units", "System services", StateNotReady, SeverityBlocking,
			"one or more systemd units have failed: "+joinStrings(sys.FailedServices)))
	} else {
		out = append(out, newCheck("System", "failed_units", "System services", StateReady, SeverityBlocking, "no failed systemd units"))
	}

	// Active undervoltage or serious thermal throttling - blocking per
	// the mission's explicit rule (a Pi under active undervoltage cannot
	// be trusted to keep running reliably through a flight).
	switch {
	case sys.UndervoltageDetected:
		out = append(out, newCheck("System", "power_thermal", "Power and thermal", StateNotReady, SeverityBlocking, "active undervoltage detected"))
	case sys.Throttled:
		out = append(out, newCheck("System", "power_thermal", "Power and thermal", StateCaution, SeverityCaution, "CPU is currently throttled"))
	default:
		out = append(out, newCheck("System", "power_thermal", "Power and thermal", StateReady, SeverityBlocking, "no throttling or undervoltage detected"))
	}

	// Persistent storage - "expected but unavailable or read-only" is
	// blocking; a healthy-but-approaching-threshold DEGRADED reading
	// (readiness.CertifyPersistentStorage's WarnPercent) is a caution.
	out = append(out, storageCheck("Storage", "persistent_storage", "Persistent storage", in.Health.Storage))

	// Protected root overlay - "structurally invalid" is blocking, same
	// reasoning as persistent storage.
	out = append(out, storageCheck("Overlay", "protected_overlay", "Protected root overlay", in.Health.TemporaryOverlay))

	// Available recording capacity - an at-a-glance summary; the
	// Recording component below covers the full detail (current state,
	// the trusted-time requirement).
	if in.Health.Storage.RecordingAllowed {
		out = append(out, newCheck("System", "recording_capacity", "Recording capacity", StateReady, SeverityInfo, "sufficient persistent storage free for recording"))
	} else {
		out = append(out, newCheck("System", "recording_capacity", "Recording capacity", StateCaution, SeverityCaution, "persistent storage is too full to permit recording"))
	}

	return out
}

// storageCheck reclassifies a readiness.StorageHealth the same way for
// both the persistent-data partition and the protected overlay: a
// confirmed problem (not present, not mounted, or unexpectedly
// read-only) is always blocking, even if the underlying State happened
// to read DEGRADED rather than NOT_READY, because those specific
// conditions are exactly the mission's documented NOT_READY triggers.
func storageCheck(component, id, label string, h readiness.StorageHealth) CheckResult {
	switch {
	case !h.Present, !h.Mounted, h.ReadOnly:
		return newCheck(component, id, label, StateNotReady, SeverityBlocking, h.Reason)
	default:
		state, sev := fromComponentState(h.State)
		return newCheck(component, id, label, state, sev, h.Reason)
	}
}

func joinStrings(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// --- GPS and time ------------------------------------------------------

func gpsTimeChecks(in Input) []CheckResult {
	gps := in.Health.GPS
	var out []CheckResult

	// GPS hardware presence.
	if !gps.Present {
		out = append(out, newCheck("GPS", "gps_hardware", "GPS hardware", StateNotReady, SeverityBlocking, "no GPS device detected"))
	} else {
		out = append(out, newCheck("GPS", "gps_hardware", "GPS hardware", StateReady, SeverityBlocking, "GPS device present: "+gps.DeviceType))
	}

	// GPS fix / satellite solution - required for intended operation,
	// but not yet having one during the acquisition grace period is
	// StateUnknown (initializing), not a failure. After the grace period
	// with still no fix, a present-but-fixless GPS is a caution (the
	// receiver itself is talking, matching readiness's own DEGRADED
	// classification for this case), while a genuinely absent GPS is
	// already covered as blocking above.
	hasFix := gps.FixType != "" && gps.FixType != "No Fix" && gps.SatellitesInSolution > 0
	switch {
	case hasFix:
		out = append(out, newCheck("GPS", "gps_fix", "GPS satellite solution", StateReady, SeverityBlocking,
			gps.FixType+" using "+itoaLocal(int(gps.SatellitesInSolution))+" satellites"))
	case !gps.Present:
		out = append(out, newCheck("GPS", "gps_fix", "GPS satellite solution", StateNotApplicable, SeverityInfo, "no GPS hardware to acquire a fix"))
	case in.UptimeSeconds < graceGPSAcquisitionSeconds:
		out = append(out, newCheck("GPS", "gps_fix", "GPS satellite solution", StateUnknown, SeverityCaution, "still within the GPS acquisition grace period"))
	default:
		out = append(out, newCheck("GPS", "gps_fix", "GPS satellite solution", StateCaution, SeverityCaution, "GPS is present and reporting but has not resolved a satellite solution"))
	}

	// Position accuracy - informational detail alongside the fix check,
	// never itself a separate blocking/caution trigger.
	if hasFix {
		out = append(out, newCheck("GPS", "gps_accuracy", "GPS position accuracy", StateReady, SeverityInfo,
			formatFloat(float64(gps.AccuracyMeters))+" m estimated accuracy"))
	} else {
		out = append(out, newCheck("GPS", "gps_accuracy", "GPS position accuracy", StateUnknown, SeverityInfo, "no current position solution"))
	}

	// Trusted time.
	timeH := in.Health.Time
	switch timeH.State {
	case readiness.TimeGNSSSynced, readiness.TimeNetworkSynced:
		out = append(out, newCheck("Time", "trusted_time", "Trusted time", StateReady, SeverityCaution, "time is synchronized: "+timeH.Source))
	case readiness.TimeDegraded:
		out = append(out, newCheck("Time", "trusted_time", "Trusted time", StateCaution, SeverityCaution, "time synchronization is degraded"))
	case readiness.TimeInvalid:
		out = append(out, newCheck("Time", "trusted_time", "Trusted time", StateCaution, SeverityCaution, "an implausible time source was rejected"))
	default: // TimeUnsynchronized
		if in.UptimeSeconds < graceGNSSTimeSeconds {
			out = append(out, newCheck("Time", "trusted_time", "Trusted time", StateUnknown, SeverityCaution, "still within the GNSS time-acquisition grace period"))
		} else {
			out = append(out, newCheck("Time", "trusted_time", "Trusted time", StateCaution, SeverityCaution, "time has not yet been synchronized to a trusted source"))
		}
	}

	// Time freshness - how long ago the last accepted sync was, only
	// meaningful once at least one sync has ever happened.
	if timeH.LastSyncTime.Valid {
		out = append(out, newCheck("Time", "time_freshness", "Time freshness", StateReady, SeverityInfo, "last synchronized recently"))
	} else {
		out = append(out, newCheck("Time", "time_freshness", "Time freshness", StateUnknown, SeverityInfo, "never synchronized this session"))
	}

	return out
}

// --- ADS-B ---------------------------------------------------------------

func adsbChecks(in Input) []CheckResult {
	var out []CheckResult
	out = append(out, radioChecks("978", "UAT978", in.Health.UAT978, in.UptimeSeconds)...)
	out = append(out, radioChecks("1090", "ES1090", in.Health.ES1090, in.UptimeSeconds)...)
	return out
}

// radioChecks reclassifies one RadioHealth (978 or 1090) into
// configuration/availability plus freshness/traffic checks. The single
// most important rule here (explicitly called out in the mission brief)
// is that a correctly detected, healthy receiver with zero *current*
// traffic is never itself a hardware failure - reception depends on
// what is actually flying nearby, which this daemon has no control over.
func radioChecks(band, component string, h readiness.RadioHealth, uptimeSeconds float64) []CheckResult {
	var out []CheckResult

	if !h.Band.Enabled {
		// readiness.RadioHealth.Band.Enabled is not populated from settings
		// directly - it is set by main/sdr.go's sdrWatcher(), which
		// deliberately delays configuring any SDR device until GPS gets a
		// valid fix (to reduce RF noise during acquisition) or up to
		// graceSDRDiscoverySeconds elapses, whichever comes first (see
		// sdrWatcher's own comment: "Delay SDR start for a bit to reduce
		// noise for the GPS to get a fix... give up waiting after 120s").
		// Confirmed live on hardware: with no GPS fix, Band.Enabled reads
		// false for the full ~120s, not some shorter guess - so within
		// that window, false is "not yet determined," not "deliberately
		// disabled," and must read UNKNOWN, never NOT_APPLICABLE. Only
		// after the grace window closes do we trust a still-false Enabled
		// as a genuine, settled "this band is off."
		if uptimeSeconds < graceSDRDiscoverySeconds {
			out = append(out, newCheck(component, band+"_config", band+" configuration", StateUnknown, SeverityCaution,
				"still within the SDR discovery grace period - the SDR watcher delays device configuration until GPS acquires a fix or the grace period elapses"))
			return out
		}
		out = append(out, newCheck(component, band+"_config", band+" configuration", StateNotApplicable, SeverityInfo, band+" is not enabled"))
		return out
	}

	// Configuration and receiver availability. An externally-satisfied
	// band (e.g. 978 served by a low-power external UAT radio rather
	// than a local SDR) is reported as such, honestly, rather than
	// pretending an SDR is present - this mirrors
	// sdrassign.BandStatus.ExternallySatisfied verbatim.
	switch {
	case h.Band.ExternallySatisfied:
		out = append(out, newCheck(component, band+"_config", band+" receiver", StateReady, SeverityBlocking,
			band+" is enabled and served by an external receiver, not a local SDR"))
	case h.Band.Assigned && !h.Band.Conflict:
		out = append(out, newCheck(component, band+"_config", band+" receiver", StateReady, SeverityBlocking, band+" SDR assigned: "+h.Band.DeviceSerial))
	case h.Band.Conflict:
		out = append(out, newCheck(component, band+"_config", band+" receiver", StateNotReady, SeverityBlocking, "conflicting "+band+" SDR assignment: "+h.Band.Reason))
	case uptimeSeconds < graceSDRDiscoverySeconds:
		out = append(out, newCheck(component, band+"_config", band+" receiver", StateUnknown, SeverityCaution, "still within the SDR discovery grace period"))
	default:
		out = append(out, newCheck(component, band+"_config", band+" receiver", StateNotReady, SeverityBlocking, band+" is enabled but no receiver is assigned: "+h.Band.Reason))
	}

	// Receiver freshness / current traffic. Only evaluated once the
	// receiver itself is confirmed present (handled above) - a missing
	// receiver already reads NOT_READY there and this check would only
	// duplicate that, so it reports NOT_APPLICABLE in that case.
	receiverPresent := h.Band.ExternallySatisfied || (h.Band.Assigned && !h.Band.Conflict)
	switch {
	case !receiverPresent:
		out = append(out, newCheck(component, band+"_traffic", band+" current traffic", StateNotApplicable, SeverityInfo, "no receiver to report traffic from"))
	case h.LastFrameAgeSeconds != nil:
		out = append(out, newCheck(component, band+"_traffic", band+" current traffic", StateReady, SeverityInfo,
			"receiving - most recent frame "+formatFloat(*h.LastFrameAgeSeconds)+"s ago"))
	case uptimeSeconds < graceSDRDiscoverySeconds:
		out = append(out, newCheck(component, band+"_traffic", band+" current traffic", StateUnknown, SeverityInfo, "still within the startup grace period"))
	default:
		// A healthy, correctly-detected receiver with zero traffic ever
		// received is CAUTION-at-most informational, never a hardware
		// failure - see this function's doc comment.
		out = append(out, newCheck(component, band+"_traffic", band+" current traffic", StateCaution, SeverityCaution,
			"receiver is healthy but has not decoded any "+band+" traffic yet - this depends on nearby aircraft/ground stations, not receiver health"))
	}

	return out
}

// --- GDL90 and clients -----------------------------------------------

func gdl90Checks(in Input) []CheckResult {
	g := in.Health.GDL90
	var out []CheckResult

	// Output itself - blocking when GDL90 generation/output is expected
	// but not active (readiness already only reports NOT_READY here when
	// output genuinely is not being produced).
	state, sev := fromComponentState(g.State)
	out = append(out, newCheck("GDL90", "gdl90_output", "GDL90 output", state, sev, g.Reason))

	// Recent clients - informational/caution, never blocking: EFB
	// software connecting is outside this daemon's control, and no
	// client at all is routine before an EFB app is opened.
	switch {
	case g.RecentClientCount > 0:
		out = append(out, newCheck("GDL90", "gdl90_clients", "Connected EFB clients", StateReady, SeverityInfo,
			itoaLocal(g.RecentClientCount)+" client(s) recently active"))
	case in.UptimeSeconds < graceNetworkClientSeconds:
		out = append(out, newCheck("GDL90", "gdl90_clients", "Connected EFB clients", StateUnknown, SeverityCaution, "still within the client-connection grace period"))
	default:
		out = append(out, newCheck("GDL90", "gdl90_clients", "Connected EFB clients", StateCaution, SeverityCaution, "no GDL90 client has connected recently"))
	}

	// Honest client-identification statement - always informational,
	// never a state judgment on its own; this exists so the report
	// itself carries the same disclosure the dashboard/API already make
	// (see readiness.GDL90Health.ForeFlightDetection) rather than a
	// consumer of just this report missing that context.
	identify := newCheck("GDL90", "gdl90_client_identity", "Client identification", StateNotApplicable, SeverityInfo,
		"Stratux cannot identify ForeFlight or any other specific EFB application from network activity alone - client presence is generic network-level liveness only.")
	out = append(out, identify)

	return out
}

// --- AHRS and calibration --------------------------------------------

func ahrsChecks(in Input) []CheckResult {
	a := in.Health.AHRS
	var out []CheckResult

	if !a.Enabled {
		out = append(out, newCheck("AHRS", "ahrs_hardware", "AHRS hardware", StateNotApplicable, SeverityInfo, "AHRS is disabled - optional equipment"))
		return out
	}

	// Hardware/fresh-data check - an uncalibrated AHRS is handled
	// separately below and must never make this (or any unrelated ADS-B/
	// GPS/weather check) NOT_READY on its own, per the mission's explicit
	// rule; this check is purely about the sensor itself being connected
	// and producing fresh data.
	state, sev := fromComponentState(a.State)
	// AHRS calibration incompleteness is deliberately downgraded here:
	// readiness.BuildAHRSHealth already reports DEGRADED for an
	// uncalibrated-but-connected AHRS (see readiness/sensor_health.go),
	// which fromComponentState would otherwise turn into a preflight
	// CAUTION - that is in fact exactly the outcome the mission wants
	// ("should normally produce CAUTION, not block"), so no override is
	// needed here; this comment exists only to make that intentional,
	// not accidental.
	out = append(out, newCheck("AHRS", "ahrs_hardware", "AHRS hardware", state, sev, a.Reason))

	// Active aircraft profile - a corrupt/unavailable profile subsystem
	// is blocking (it prevents safely interpreting whatever calibration
	// values happen to be loaded), while a merely-uncalibrated active
	// profile is a caution.
	if !a.Profile.Available {
		out = append(out, newCheck("AHRS", "ahrs_profile", "Active aircraft profile", StateNotReady, SeverityBlocking,
			"calibration-profile subsystem unavailable: "+a.Profile.Error))
	} else if !in.Profile.CalibrationValid {
		out = append(out, newCheck("AHRS", "ahrs_profile", "Active aircraft profile", StateCaution, SeverityCaution,
			"active profile \""+in.Profile.Name+"\" is not fully calibrated"))
	} else {
		out = append(out, newCheck("AHRS", "ahrs_profile", "Active aircraft profile", StateReady, SeverityCaution,
			"active profile \""+in.Profile.Name+"\" is calibrated"))
	}

	return out
}

// --- Barometer -----------------------------------------------------------

func baroCheck(in Input) []CheckResult {
	b := in.Health.Baro
	if !b.Enabled {
		return []CheckResult{newCheck("Baro", "baro_hardware", "Barometer", StateNotApplicable, SeverityInfo, "barometer is disabled - optional equipment")}
	}
	state, sev := fromComponentState(b.State)
	reason := b.Reason
	if b.NonFinite || b.Implausible {
		state, sev = StateCaution, SeverityCaution
		reason = "barometer reading is non-finite or implausible"
	}
	return []CheckResult{newCheck("Baro", "baro_hardware", "Barometer", state, sev, reason)}
}

// --- Fan controller ----------------------------------------------------

func fanChecks(in Input) []CheckResult {
	f := in.Health.Fan
	var out []CheckResult

	if !f.ServiceInstalled {
		out = append(out, newCheck("Fan", "fan_status", "Fan controller", StateNotApplicable, SeverityInfo, "fan-controller service is not installed on this build"))
		return out
	}

	switch {
	case !f.ServiceActive:
		out = append(out, newCheck("Fan", "fan_status", "Fan controller", StateCaution, SeverityCaution, "fan-controller service is installed but not active"))
	case !f.StatusAvailable:
		if in.UptimeSeconds < graceFanControllerSeconds {
			out = append(out, newCheck("Fan", "fan_status", "Fan controller", StateUnknown, SeverityCaution, "waiting for the fan-controller status file to appear"))
		} else {
			out = append(out, newCheck("Fan", "fan_status", "Fan controller", StateCaution, SeverityCaution, "fan-controller status file has not appeared"))
		}
	case f.Malformed:
		out = append(out, newCheck("Fan", "fan_status", "Fan controller", StateCaution, SeverityCaution, "fan-controller status file could not be parsed"))
	case f.ControllerError != "":
		out = append(out, newCheck("Fan", "fan_status", "Fan controller", StateCaution, SeverityCaution, "fan controller reports an error: "+f.ControllerError))
	default:
		out = append(out, newCheck("Fan", "fan_status", "Fan controller", StateReady, SeverityCaution, "fan controller state: "+f.ControllerState))
	}

	// Explicit, unconditional statement that physical rotation is never
	// electronically confirmed on this hardware - required verbatim by
	// the mission brief, and deliberately its own check (VERIFY) rather
	// than folded into fan_status, so a dashboard/API consumer cannot
	// miss it by only reading the summary state.
	out = append(out, newCheck("Fan", "fan_rotation", "Physical fan rotation", StateVerify, SeverityInfo,
		"this hardware has no tachometer - physical fan rotation can never be electronically confirmed; see the matching manual check"))

	return out
}

// --- Recording -----------------------------------------------------------

func recordingChecks(in Input) []CheckResult {
	var out []CheckResult

	if in.Recording.StorageAvailable {
		out = append(out, newCheck("Recording", "recording_storage", "Recording storage", StateReady, SeverityInfo, "persistent storage has room for a new recording"))
	} else {
		out = append(out, newCheck("Recording", "recording_storage", "Recording storage", StateCaution, SeverityCaution, "persistent storage is too full to start a new recording"))
	}

	if in.Recording.Permitted {
		out = append(out, newCheck("Recording", "recording_permitted", "Recording permitted", StateReady, SeverityInfo, "trusted time and storage requirements are both met"))
	} else if !in.Health.Time.RecordingAllowed {
		out = append(out, newCheck("Recording", "recording_permitted", "Recording permitted", StateCaution, SeverityCaution, "recording requires trusted time, which is not currently available"))
	} else {
		out = append(out, newCheck("Recording", "recording_permitted", "Recording permitted", StateCaution, SeverityCaution, "recording is not currently permitted"))
	}

	state := in.Recording.State
	if state == "" {
		state = "idle"
	}
	out = append(out, newCheck("Recording", "recording_state", "Current recording state", StateReady, SeverityInfo, "state: "+state))

	return out
}

// --- FIS-B -----------------------------------------------------------

func fisbChecks(in Input) []CheckResult {
	uat := in.Health.UAT978
	var out []CheckResult

	if !uat.Band.Enabled {
		// Same rationale as radioChecks() above - Band.Enabled can
		// legitimately read false for up to graceSDRDiscoverySeconds
		// while main/sdr.go's sdrWatcher() is still waiting on a GPS fix
		// before configuring any SDR device.
		if in.UptimeSeconds < graceSDRDiscoverySeconds {
			out = append(out, newCheck("FISB", "fisb_tower", "FIS-B tower reception", StateUnknown, SeverityCaution, "still within the SDR discovery grace period"))
			return out
		}
		out = append(out, newCheck("FISB", "fisb_tower", "FIS-B tower reception", StateNotApplicable, SeverityInfo, "978 UAT is not enabled"))
		return out
	}

	// Absence of a currently-receivable tower is an environmental/
	// service-availability condition, not a receiver failure - the
	// mission is explicit that this must never be NOT_READY.
	if uat.TowerCount > 0 {
		out = append(out, newCheck("FISB", "fisb_tower", "FIS-B tower reception", StateReady, SeverityInfo, itoaLocal(uat.TowerCount)+" tower(s) currently received"))
	} else {
		out = append(out, newCheck("FISB", "fisb_tower", "FIS-B tower reception", StateCaution, SeverityCaution, "no FIS-B ground tower is currently receivable"))
	}

	if uat.LastFrameAgeSeconds != nil {
		out = append(out, newCheck("FISB", "fisb_activity", "Last UAT activity", StateReady, SeverityInfo, formatFloat(*uat.LastFrameAgeSeconds)+"s ago"))
	} else {
		out = append(out, newCheck("FISB", "fisb_activity", "Last UAT activity", StateUnknown, SeverityInfo, "no UAT frame received yet"))
	}

	if uat.WeatherProductCounts != nil {
		total := 0
		for _, n := range uat.WeatherProductCounts {
			total += n
		}
		if total > 0 {
			out = append(out, newCheck("FISB", "fisb_weather", "Weather products received", StateReady, SeverityInfo, itoaLocal(total)+" product(s) received this session"))
		} else {
			out = append(out, newCheck("FISB", "fisb_weather", "Weather products received", StateCaution, SeverityInfo, "no weather products received yet"))
		}
	}

	return out
}

// --- Manual checks -----------------------------------------------------

// manualCheckResults renders every fixed ManualCheckDefinition, using
// in.ManualAcks (an already-filtered snapshot from ManualAckStore) to
// decide whether it is currently acknowledged. An acknowledged check is
// StateReady with Severity SeverityCaution (an unacknowledged required
// manual step is a caution, never a hard block, matching this feature's
// framing as a supplemental aid - see docs/preflight-readiness.md); an
// unacknowledged one is StateVerify (a human still needs to look at it).
func manualCheckResults(in Input) []CheckResult {
	out := make([]CheckResult, 0, len(ManualCheckDefinitions))
	for _, d := range ManualCheckDefinitions {
		ack := in.ManualAcks[d.ID]
		c := CheckResult{
			Component: "Manual",
			CheckID:   string(d.ID),
			Label:     d.Label,
			Reason:    d.Reason,
			Severity:  SeverityCaution,
			Source:    "manual",
		}
		if ack != nil {
			c.State = StateReady
			c.Blocking = false
			if ack.AckedAtUTC != nil {
				c.ObservedAt = ack.AckedAtUTC
				expires := ack.AckedAtUTC.Add(DefaultAckExpiration)
				c.ExpiresAt = &expires
			}
		} else {
			c.State = StateVerify
			c.Blocking = false
		}
		out = append(out, c)
	}
	return out
}

// --- Power / previous session ------------------------------------------

// powerSessionChecks reports whether the previous session recorded a
// clean shutdown or reboot - see power.EvaluatePreviousSession's doc
// comment for exactly what this can and cannot establish. Deliberately
// never blocking and never StateNotReady: an unclear previous-session
// close is ambiguous (it can also just mean this feature is new, or the
// daemon was restarted by systemd) and must never fail preflight on its
// own. This is a separate check from "System" -> "power_thermal" above,
// which covers the live/historical throttle-register reading; this one
// covers only the previous-session marker.
func powerSessionChecks(in Input) []CheckResult {
	if !in.PreviousSessionAvailable {
		return []CheckResult{newCheck("Power", "previous_session", "Previous session", StateNotApplicable, SeverityInfo, "no previous-session record available")}
	}
	if in.PreviousSessionEndedCleanly {
		return []CheckResult{newCheck("Power", "previous_session", "Previous session", StateReady, SeverityInfo, "previous session recorded a clean shutdown or reboot")}
	}
	return []CheckResult{newCheck("Power", "previous_session", "Previous session", StateCaution, SeverityInfo, in.PreviousSessionNote)}
}

// storageLifecycleChecks reports the storage-lifecycle inventory
// foundation's own pressure state - see readiness.StorageLifecycleHealth.
// Deliberately a single concise card (per this check's own mission
// requirement to avoid overwhelming the pilot with implementation
// detail): an unavailable inventory (still within startup grace, or the
// feature simply not yet populated) is NOT_APPLICABLE, never a caution -
// this is an unsupported-until-populated foundation, not a fault.
// Genuinely critical pressure is blocking, matching this project's
// existing "power_thermal"/storage treatment of real resource exhaustion.
func storageLifecycleChecks(in Input) []CheckResult {
	if !in.StorageLifecycleHasInventory {
		return []CheckResult{newCheck("Storage", "storage_lifecycle", "Storage lifecycle", StateNotApplicable, SeverityInfo, "inventory not yet available")}
	}
	switch in.StorageLifecyclePressure {
	case "CRITICAL":
		return []CheckResult{newCheck("Storage", "storage_lifecycle", "Storage lifecycle", StateNotReady, SeverityBlocking, in.StorageLifecycleReason)}
	case "HIGH", "ELEVATED", "UNKNOWN":
		return []CheckResult{newCheck("Storage", "storage_lifecycle", "Storage lifecycle", StateCaution, SeverityCaution, in.StorageLifecycleReason)}
	default:
		if in.StorageLifecycleStale {
			return []CheckResult{newCheck("Storage", "storage_lifecycle", "Storage lifecycle", StateCaution, SeverityCaution, "inventory is stale")}
		}
		return []CheckResult{newCheck("Storage", "storage_lifecycle", "Storage lifecycle", StateReady, SeverityInfo, "storage lifecycle nominal")}
	}
}

// autoRecordChecks reports Automatic Flight Recording's own state - a
// single concise card, mirroring storageLifecycleChecks' own restraint.
// Disabled (the default, opt-in-required posture) is ALWAYS purely
// informational (NOT_APPLICABLE/Info) - a deliberate choice to leave the
// feature off must never present as a caution or blocking condition, per
// this check's own explicit mission requirement. Severity never rises
// above Caution even for INHIBITED/ERROR: this is a supplemental
// recording feature, never authoritative for flight readiness, so its
// own trouble never blocks the overall report.
func autoRecordChecks(in Input) []CheckResult {
	if !in.AutoRecordEnabled {
		return []CheckResult{newCheck("Recording", "auto_record", "Automatic recording", StateNotApplicable, SeverityInfo, "automatic recording is disabled")}
	}
	switch in.AutoRecordMachineState {
	case "":
		return []CheckResult{newCheck("Recording", "auto_record", "Automatic recording", StateNotApplicable, SeverityInfo, "not yet initialized")}
	case "ERROR":
		return []CheckResult{newCheck("Recording", "auto_record", "Automatic recording", StateNotReady, SeverityCaution, in.AutoRecordReason)}
	case "INHIBITED":
		return []CheckResult{newCheck("Recording", "auto_record", "Automatic recording", StateCaution, SeverityCaution, in.AutoRecordReason)}
	default:
		return []CheckResult{newCheck("Recording", "auto_record", "Automatic recording", StateReady, SeverityInfo, "automatic recording armed")}
	}
}

// fisbCacheChecks reports the Rolling FIS-B Weather Cache's own state - a
// single concise card, mirroring autoRecordChecks' own restraint.
// Disabled (the default, opt-in-required posture) is ALWAYS purely
// informational (NOT_APPLICABLE/Info) - the absence of a tower, or the
// feature being off entirely, is never itself a failure (see this
// check's own mission requirement: "no tower is not automatically
// NOT_READY", "cache disabled is not a failure"). Severity never rises
// above Caution: this is a supplemental convenience cache, never
// authoritative for flight readiness, and cached-only weather being
// present is explicitly informational, not a substitute for a "current
// weather" check this project does not otherwise claim to make.
func fisbCacheChecks(in Input) []CheckResult {
	if !in.FISBCacheEnabled {
		return []CheckResult{newCheck("Recording", "fisb_cache", "FIS-B weather cache", StateNotApplicable, SeverityInfo, "FIS-B weather cache is disabled")}
	}
	switch in.FISBCacheState {
	case "", "STARTUP_GRACE", "WAITING_FOR_TRUSTED_TIME":
		return []CheckResult{newCheck("Recording", "fisb_cache", "FIS-B weather cache", StateNotApplicable, SeverityInfo, "not yet initialized")}
	case "ERROR":
		return []CheckResult{newCheck("Recording", "fisb_cache", "FIS-B weather cache", StateCaution, SeverityCaution, in.FISBCacheReason)}
	case "DEGRADED", "READ_ONLY", "PRESSURE_INHIBITED":
		return []CheckResult{newCheck("Recording", "fisb_cache", "FIS-B weather cache", StateCaution, SeverityCaution, in.FISBCacheReason)}
	default:
		return []CheckResult{newCheck("Recording", "fisb_cache", "FIS-B weather cache", StateReady, SeverityInfo, "FIS-B weather cache active")}
	}
}
