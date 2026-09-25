package fisbcache

// State is the cache subsystem's own overall operational state - distinct
// from FreshnessState, which describes one entry. Naming follows this
// project's established UPPER_SNAKE string-state convention.
type State string

const (
	StateDisabled              State = "DISABLED"
	StateStartupGrace          State = "STARTUP_GRACE"
	StateWaitingForTrustedTime State = "WAITING_FOR_TRUSTED_TIME"
	StateLive                  State = "LIVE"
	StateDegraded              State = "DEGRADED"
	StateReadOnly              State = "READ_ONLY"
	StatePressureInhibited     State = "PRESSURE_INHIBITED"
	StateError                 State = "ERROR"
)

// StateInputs are the pure, caller-supplied facts DetermineState needs -
// never read from a global or a live clock internally, so identical
// inputs always produce an identical State.
type StateInputs struct {
	Enabled bool
	// StartupRecoveryComplete is false until this cache's own startup
	// recovery pass (see main/fisbcacherun.go) has finished inspecting
	// its namespace - never true merely because the process has been up
	// a while.
	StartupRecoveryComplete bool
	// TrustedTime mirrors readiness.TimeTrust's own GNSS/network-synced
	// determination (see main/'s glue) - this cache never serves
	// persisted cross-reboot data, and never computes a reconstructed
	// SourceTime, before this is true.
	TrustedTime bool
	// RecoveryError is non-nil if startup recovery encountered a
	// condition serious enough to prevent normal operation (e.g. the
	// cache-owned namespace itself could not be read at all) - distinct
	// from an individual corrupt entry, which is simply discarded, never
	// escalated to StateError.
	RecoveryError bool
	// StoragePressureProhibited mirrors storagelifecycle's own pressure
	// reading being severe enough that new writes are refused project-
	// wide (see storagelifecycle.RecordingSpaceDenied's own pattern) -
	// this cache stops admitting new entries but keeps serving already-
	// cached ones.
	StoragePressureProhibited bool
	// ReadOnly is set by the runtime layer when it has deliberately
	// stopped accepting new writes (e.g. during controlled shutdown) but
	// the store itself remains healthy and queryable.
	ReadOnly bool
	// HasNonFatalErrors is set when startup recovery or a later scan
	// found and safely quarantined/removed individual corrupt entries -
	// not fatal (RecoveryError), but worth surfacing as degraded rather
	// than silently reporting LIVE.
	HasNonFatalErrors bool
}

// DetermineState is the pure decision function behind this cache's
// exposed operational State - see StateInputs for exactly what it
// depends on. Ordering matters: disabled is checked first (an optional,
// disabled cache must never report anything else, regardless of any
// other input - see docs/fisb-weather-cache.md's readiness-integration
// requirement that a disabled cache never degrades overall readiness),
// then hard errors, then startup gating, then pressure/read-only, with
// StateLive only ever the last, most-permissive fallback.
func DetermineState(in StateInputs) State {
	if !in.Enabled {
		return StateDisabled
	}
	if in.RecoveryError {
		return StateError
	}
	if !in.StartupRecoveryComplete {
		return StateStartupGrace
	}
	if !in.TrustedTime {
		return StateWaitingForTrustedTime
	}
	if in.HasNonFatalErrors {
		return StateDegraded
	}
	if in.ReadOnly {
		return StateReadOnly
	}
	if in.StoragePressureProhibited {
		return StatePressureInhibited
	}
	return StateLive
}
