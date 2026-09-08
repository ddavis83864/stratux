package autorecord

// GPSSample is one point-in-time GPS reading, as main/'s glue reads it
// from the live GPS subsystem (mySituation) - deliberately a small,
// plain struct rather than main.SituationData itself, so this package
// never imports main (impossible anyway - main imports this package) and
// every test constructs samples directly, with no hardware or global
// state involved.
type GPSSample struct {
	// FixValid mirrors this project's existing isGPSValid() criteria
	// (fix quality > 0, GPS connected) as observed by the caller - this
	// package does not re-derive GPS validity policy, it only consumes
	// the caller's own determination, since main/gps.go's isGPSValid has
	// its own established (if mutating - see main/autorecordapi.go's doc
	// comment) semantics this package must not duplicate or second-guess.
	FixValid bool
	// GroundSpeedKnots is only meaningful when FixValid is true.
	GroundSpeedKnots float64
	// SampleAgeSeconds is how long ago (monotonic) this sample was
	// obtained, as of the moment the caller built it - never itself a
	// wall-clock or GPS-reported timestamp for this package's own
	// purposes (see docs/automatic-flight-recording.md's clock-source
	// section).
	SampleAgeSeconds float64
}

// Conflicts describes daemon-wide states that must block a new automatic
// start (and, for Shutdown, force an immediate stop) - main/'s glue
// reads these from the OTA/configbackup/power subsystems' own existing
// status, never re-deriving them.
type Conflicts struct {
	OTABusy           bool
	ConfigRestoreBusy bool
	// ShutdownRequested is true once a confirmed controlled shutdown has
	// begun (see power.Manager) - the machine treats this as an
	// immediate, non-dwelled stop trigger, not merely a start-blocking
	// precondition, so an automatic recording is never left running
	// through a shutdown the owner has already confirmed.
	ShutdownRequested bool
}

// StorageDecision mirrors storagelifecycle.RecordingSpaceDecision's three
// values plus "unknown" as a plain string type, so this package does not
// need to import storagelifecycle just for one three-way enum (machine.go
// does import storagelifecycle directly for EvaluateRecordingSpace
// itself, where the full type is more useful than duplicating it) and a
// test can construct a MachineInput without ever touching that package.
type StorageDecision string

const (
	StorageAllowed StorageDecision = "allowed"
	StorageCaution StorageDecision = "caution"
	StorageDenied  StorageDecision = "denied"
	// StorageUnknown means the caller could not determine a decision at
	// all (Storage Lifecycle Manager uninitialized, or its inventory is
	// stale/erroring) - treated the same as StorageDenied for starting a
	// new recording (see machine.go), but reported with its own distinct
	// reason code so a diagnostic consumer can tell "no room" apart from
	// "could not tell."
	StorageUnknown StorageDecision = "unknown"
)

// MachineInput is everything Machine.Evaluate needs for one tick - see
// Machine.Evaluate's doc comment for the full decision policy this
// drives. Every field is caller-supplied; Evaluate reads no global state
// and calls no clock itself.
type MachineInput struct {
	NowMonotonic          float64
	Settings              Settings
	GPS                   GPSSample
	TrustedTime           bool
	ManualRecordingActive bool
	Conflicts             Conflicts
	Storage               StorageDecision
}
