/*
alertinghealth.go: derives an alerting.HealthSnapshot from the existing,
already grace-aware readiness.HealthReport and preflight.Report - never
re-derives component health itself (see docs/alerting.md's
"Health-transition behavior").
*/
package main

import (
	"github.com/stratux/stratux/alerting"
	"github.com/stratux/stratux/calprofile"
	"github.com/stratux/stratux/preflight"
	"github.com/stratux/stratux/readiness"
)

// mapComponentState translates the existing, already-computed
// readiness.ComponentState into the alerting package's coarser
// ComponentLevel. NOT_INSTALLED (an optional component deliberately
// disabled) maps to Unknown, never a degradation - "optional disabled
// component" must never alert.
func mapComponentState(s readiness.ComponentState) alerting.ComponentLevel {
	switch s {
	case readiness.StateReady:
		return alerting.ComponentReady
	case readiness.StateDegraded:
		return alerting.ComponentCaution
	case readiness.StateNotReady:
		return alerting.ComponentNotReady
	default: // StateNotInstalled, StateUnknown, or anything unrecognized
		return alerting.ComponentUnknown
	}
}

// mapPreflightState translates preflight.State (this daemon's own
// already-grace-aware overall summary) the same way.
func mapPreflightState(s preflight.State) alerting.ComponentLevel {
	switch s {
	case preflight.StateReady:
		return alerting.ComponentReady
	case preflight.StateCaution:
		return alerting.ComponentCaution
	case preflight.StateNotReady:
		return alerting.ComponentNotReady
	default:
		return alerting.ComponentUnknown
	}
}

// mapTimeState translates readiness.TimeState into alerting.ComponentLevel.
// TimeUnsynchronized deliberately maps to ComponentCaution, not Unknown:
// unlike other components, "never yet synced" and "lost a prior sync" are
// the same wire value, and the alerting package's own first-observation
// exemption (see alerting.worse's doc comment) is what correctly keeps a
// fresh boot's not-yet-synced state quiet, while a later transition INTO
// this same state from a real GNSS/network-synced baseline still alerts -
// "trusted-time loss after previously being synchronized."
func mapTimeState(s readiness.TimeState) alerting.ComponentLevel {
	switch s {
	case readiness.TimeGNSSSynced, readiness.TimeNetworkSynced:
		return alerting.ComponentReady
	case readiness.TimeDegraded, readiness.TimeUnsynchronized:
		return alerting.ComponentCaution
	case readiness.TimeInvalid:
		return alerting.ComponentNotReady
	default:
		return alerting.ComponentUnknown
	}
}

// buildHealthSnapshotForAlerting assembles an alerting.HealthSnapshot from
// the current globalHealth and the current preflight report - a pure,
// lock-bounded read of already-computed state, no new health logic.
func buildHealthSnapshotForAlerting() alerting.HealthSnapshot {
	globalHealthMutex.Lock()
	h := globalHealth
	globalHealthMutex.Unlock()

	pf := buildPreflightReport()

	profileLevel := alerting.ComponentUnknown
	if info := activeProfileHealthInfo(); info.Available {
		if info.Kind != "" && info.Kind != calprofile.KindUncalibrated {
			profileLevel = alerting.ComponentReady
		} else {
			profileLevel = alerting.ComponentCaution
		}
	} else {
		profileLevel = alerting.ComponentNotReady
	}

	return alerting.HealthSnapshot{
		Overall:            mapPreflightState(pf.Overall),
		PersistentStorage:  mapComponentState(h.Storage.State),
		TemporaryOverlay:   mapComponentState(h.TemporaryOverlay.State),
		Thermal:            mapComponentState(h.System.State),
		UAT978:             mapComponentState(h.UAT978.State),
		ES1090:             mapComponentState(h.ES1090.State),
		GPS:                mapComponentState(h.GPS.State),
		TrustedTime:        mapTimeState(h.Time.State),
		GDL90Clients:       mapComponentState(h.GDL90.State),
		AHRS:               mapComponentState(h.AHRS.State),
		Baro:               mapComponentState(h.Baro.State),
		Fan:                mapComponentState(h.Fan.State),
		CalibrationProfile: profileLevel,
	}
}
