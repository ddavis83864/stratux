package preflight

import (
	"errors"
	"sync"
	"time"
)

// ManualCheckID identifies one of the fixed set of manual (human-observed)
// preflight items - see docs/preflight-readiness.md. The set is fixed
// (not user-definable) so the dashboard, the API, and diagnostics can all
// refer to the same stable identifiers.
type ManualCheckID string

const (
	CheckAntennasAttached      ManualCheckID = "antennas_attached"
	CheckMountSecure           ManualCheckID = "mount_secure"
	CheckVentsUnobstructed     ManualCheckID = "vents_unobstructed"
	CheckFansSpinning          ManualCheckID = "fans_spinning"
	CheckPowerSourceCharged    ManualCheckID = "power_source_charged"
	CheckIpadConnectedWifi     ManualCheckID = "ipad_connected_wifi"
	CheckForeFlightConnected   ManualCheckID = "foreflight_connected"
	CheckProfileSelected       ManualCheckID = "profile_selected_correct"
	CheckAHRSLevelReference    ManualCheckID = "ahrs_level_reference_appropriate"
)

// ManualCheckDefinition is the fixed, documented description of one
// manual check - never persisted, never user-editable; only the
// acknowledgement state (ManualAck) changes at runtime.
type ManualCheckDefinition struct {
	ID    ManualCheckID
	Label string
	// Reason is shown alongside the check as guidance on what the pilot
	// is actually confirming.
	Reason string
}

// ManualCheckDefinitions is the fixed, ordered list of every manual check
// this build supports. Order is the recommended review order, and is what
// the dashboard renders top-to-bottom.
var ManualCheckDefinitions = []ManualCheckDefinition{
	{CheckAntennasAttached, "Antennas attached", "Confirm both ADS-B antennas (and GPS antenna, if external) are properly connected."},
	{CheckMountSecure, "Stratux securely mounted", "Confirm the receiver is physically secured and will not shift or fall during flight."},
	{CheckVentsUnobstructed, "Case vents unobstructed", "Confirm the enclosure's cooling vents are not blocked by mounting material, cables, or debris."},
	{CheckFansSpinning, "Cooling fans physically spinning", "Visually or audibly confirm the fan(s) are turning. This hardware has no tachometer - the fan controller's commanded state can never electronically confirm physical rotation."},
	{CheckPowerSourceCharged, "Power source sufficiently charged", "Confirm the battery or aircraft power source has sufficient charge/capacity for the planned flight duration."},
	{CheckIpadConnectedWifi, "EFB device connected to Stratux Wi-Fi", "Confirm the iPad/EFB device is joined to the Stratux access point."},
	{CheckForeFlightConnected, "EFB application connection visually confirmed", "Confirm within the EFB app itself that it shows a connected GPS/ADS-B source. Stratux cannot identify ForeFlight or any other specific application from network activity alone."},
	{CheckProfileSelected, "Correct aircraft calibration profile selected", "Confirm the active named calibration profile matches the aircraft Stratux is currently mounted in."},
	{CheckAHRSLevelReference, "AHRS level reference appropriate for this mounting", "Confirm the AHRS was last leveled (Set Level) with the unit mounted the way it is mounted now."},
}

// manualCheckIndex supports O(1) validation of an incoming check ID
// without a linear scan of ManualCheckDefinitions on every request.
var manualCheckIndex = func() map[ManualCheckID]ManualCheckDefinition {
	m := make(map[ManualCheckID]ManualCheckDefinition, len(ManualCheckDefinitions))
	for _, d := range ManualCheckDefinitions {
		m[d.ID] = d
	}
	return m
}()

// IsKnownManualCheck reports whether id is one of the fixed manual checks
// this build supports.
func IsKnownManualCheck(id ManualCheckID) bool {
	_, ok := manualCheckIndex[id]
	return ok
}

// ErrUnknownManualCheck is returned by ManualAckStore methods for an id
// IsKnownManualCheck rejects.
var ErrUnknownManualCheck = errors.New("preflight: unknown manual check id")

// DefaultAckExpiration is how long a manual acknowledgement remains valid
// before it must be re-confirmed - see docs/preflight-readiness.md. Kept
// at or under the four-hour ceiling the design calls for.
const DefaultAckExpiration = 4 * time.Hour

// ManualAck is one recorded acknowledgement. It is never written to disk
// and never becomes part of a calibration profile - see ManualAckStore's
// package comment. Both a monotonic and (when available) a trusted-UTC
// timestamp are kept: the monotonic value is authoritative for expiration
// (immune to a wall-clock step before GNSS time trust is established -
// see docs/readiness-and-time-trust.md), while the UTC value is carried
// only for human-readable display when it exists.
type ManualAck struct {
	CheckID     ManualCheckID `json:"checkId"`
	AckedAtMono time.Time     `json:"-"`
	AckedAtUTC  *time.Time    `json:"ackedAtUtc,omitempty"`
	SessionID   string        `json:"sessionId"`
}

// ManualAckStore holds every current manual acknowledgement in memory
// only. There is deliberately no persistence layer here (unlike
// calprofile.Store) - the whole point of a manual preflight check is that
// it must be re-confirmed after every reboot and after the documented
// expiration window, so surviving a restart would defeat its purpose.
// Safe for concurrent use.
type ManualAckStore struct {
	mu   sync.Mutex
	acks map[ManualCheckID]ManualAck
	// sessionID identifies the current process lifetime. Every
	// acknowledgement is stamped with the sessionID active when it was
	// made; a new process (any restart, including a reboot) gets a new
	// sessionID, which alone is sufficient to invalidate every prior
	// acknowledgement - see Snapshot, which treats a mismatched
	// sessionID exactly like an expired one.
	sessionID string
	// now returns the current monotonic instant. Overridable in tests;
	// production code (main/preflightapi.go) supplies stratuxClock.Time.
	now func() time.Time
}

// NewManualAckStore creates an empty store bound to sessionID (the
// current daemon boot/session identity) using nowFunc as its monotonic
// clock source.
func NewManualAckStore(sessionID string, nowFunc func() time.Time) *ManualAckStore {
	return &ManualAckStore{
		acks:      make(map[ManualCheckID]ManualAck),
		sessionID: sessionID,
		now:       nowFunc,
	}
}

// SessionID returns the session identity this store is bound to.
func (s *ManualAckStore) SessionID() string {
	return s.sessionID
}

// Ack records (or idempotently re-records) an acknowledgement for id,
// stamped with the current session and time. utcNow is nil when trusted
// time is unavailable.
func (s *ManualAckStore) Ack(id ManualCheckID, utcNow *time.Time) (ManualAck, error) {
	if !IsKnownManualCheck(id) {
		return ManualAck{}, ErrUnknownManualCheck
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := ManualAck{
		CheckID:     id,
		AckedAtMono: s.now(),
		AckedAtUTC:  utcNow,
		SessionID:   s.sessionID,
	}
	s.acks[id] = a
	return a, nil
}

// Clear removes one acknowledgement. Clearing an id that was never
// acknowledged (or is already expired/cleared) is not an error - the
// caller's desired end state (unacknowledged) already holds, matching
// the idempotent-delete convention calprofile.Store's Delete does not
// use, but that fits a manual checklist better: "make sure this box is
// unchecked" should never fail just because it already was.
func (s *ManualAckStore) Clear(id ManualCheckID) error {
	if !IsKnownManualCheck(id) {
		return ErrUnknownManualCheck
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.acks, id)
	return nil
}

// ResetAll clears every acknowledgement at once (the dashboard's
// "Reset checklist" control).
func (s *ManualAckStore) ResetAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks = make(map[ManualCheckID]ManualAck)
}

// Snapshot returns, for every known manual check, whether it currently
// has a live (correct session, not expired) acknowledgement, plus the
// acknowledgement itself when it does. A wrong-session or expired entry
// is reported as not-acknowledged here and is not implicitly deleted
// (Snapshot is a pure read) - ResetAll or a fresh Ack are the only ways
// the map itself changes size, but a stale entry can never make a check
// wrongly appear acknowledged again.
func (s *ManualAckStore) Snapshot(expiration time.Duration) map[ManualCheckID]*ManualAck {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make(map[ManualCheckID]*ManualAck, len(ManualCheckDefinitions))
	for _, d := range ManualCheckDefinitions {
		out[d.ID] = nil
		a, ok := s.acks[d.ID]
		if !ok {
			continue
		}
		if a.SessionID != s.sessionID {
			continue // a different process lifetime - never valid, regardless of age
		}
		if now.Sub(a.AckedAtMono) > expiration {
			continue // expired
		}
		aCopy := a
		out[d.ID] = &aCopy
	}
	return out
}
