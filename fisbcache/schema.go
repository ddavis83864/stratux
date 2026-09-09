package fisbcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// SchemaVersion is bumped whenever PersistedEntry gains, removes, or
// changes the meaning of a field a reader should notice - mirrors this
// project's established autorecord.SettingsSchemaVersion/
// configbackup.SchemaVersion convention. There is no earlier version:
// this is the first release of this cache, so DecodePersistedEntry
// rejects anything other than exactly this version rather than
// attempting to interpret a hypothetical future one.
const SchemaVersion = 1

// originMarker is a fixed, non-configurable string every persisted entry
// carries, proving (to this package's own decoder, and to a human
// inspecting a raw file) that the payload was written by this cache
// specifically as a genuinely-received FIS-B product - never a
// downloaded/internet-derived value, which this feature never fetches at
// all (see this package's own doc comment and docs/fisb-weather-cache.md's
// explicit non-goal statement).
const originMarker = "FISB"

// maxPersistedPayloadBytes bounds one entry's own payload (DLAC-decoded
// text, or a NEXRAD tile's intensity data) - generous for a single text
// report or one NEXRAD tile, small enough that this cache's own worst-
// case per-entry disk cost is always predictable regardless of what a
// misbehaving or malicious uplink might attempt to smuggle through the
// existing decoder.
const maxPersistedPayloadBytes = 65536

// maxPersistedEntryBytes bounds the fully-encoded JSON file - payload
// plus this schema's own bounded metadata fields, with headroom for JSON
// framing/escaping.
const maxPersistedEntryBytes = maxPersistedPayloadBytes + 4096

// PersistedEntry is the exact on-disk JSON shape for one cache entry -
// one file per entry (see main/fisbcachestorage.go), named deterministically
// from its own Key so a second write to the same Key is always a
// same-named replace (storagelifecycle.AtomicWriter's own atomic-rename
// semantics), never a second file.
type PersistedEntry struct {
	SchemaVersion int    `json:"schemaVersion"`
	Origin        string `json:"origin"`

	ProductClass string `json:"productClass"`
	Identity     string `json:"identity"`

	// SourceTimeTrusted/SourceTimeUTC mirror SourceTime - SourceTimeUTC
	// is the zero value whenever SourceTimeTrusted is false, and is
	// never treated as meaningful in that case (see DecodePersistedEntry).
	SourceTimeTrusted bool      `json:"sourceTimeTrusted"`
	SourceTimeUTC     time.Time `json:"sourceTimeUtc,omitempty"`

	// ReceivedAtUTCTrusted/ReceivedAtUTC mirror Entry.ReceivedAtUTC -
	// ReceivedAtMonotonic is deliberately NEVER persisted: a monotonic
	// reading from a prior process has no meaning after a restart (see
	// docs/fisb-weather-cache.md's time-model section) - only the
	// trusted wall-clock receive time, if one was ever recorded, crosses
	// a reboot.
	ReceivedAtUTCTrusted bool      `json:"receivedAtUtcTrusted"`
	ReceivedAtUTC        time.Time `json:"receivedAtUtc,omitempty"`

	// PayloadBase64-free: payload is a plain string (DLAC-decoded text is
	// already text; a NEXRAD tile's binary intensity data is encoded by
	// the caller before being placed here - see
	// main/fisbcachestorage.go) rather than a second nested encoding
	// layer.
	Payload string `json:"payload"`
	// PayloadChecksum detects accidental corruption only - explicitly
	// NOT an authentication mechanism, exactly like
	// configbackup.Document's own ContentChecksum (see
	// docs/configuration-backup-restore.md) - a deliberately-edited file
	// with an honestly-recomputed checksum is not something this schema
	// can or claims to detect.
	PayloadChecksum string `json:"payloadChecksum"`
}

// EncodePersistedEntry builds and validates a PersistedEntry for e/
// payload - the one path that ever produces bytes for
// storagelifecycle.WriteFunc to stream out (see
// main/fisbcachestorage.go). Returns an error (never a partially-valid
// result) if payload exceeds maxPersistedPayloadBytes.
func EncodePersistedEntry(e Entry, payload string) (PersistedEntry, error) {
	if len(payload) > maxPersistedPayloadBytes {
		return PersistedEntry{}, fmt.Errorf("fisbcache: payload of %d bytes exceeds the %d byte bound", len(payload), maxPersistedPayloadBytes)
	}
	p := PersistedEntry{
		SchemaVersion:        SchemaVersion,
		Origin:               originMarker,
		ProductClass:         string(e.Key.Class),
		Identity:             e.Key.Identity,
		SourceTimeTrusted:    e.Source.Trusted,
		ReceivedAtUTCTrusted: !e.ReceivedAtUTC.IsZero(),
		ReceivedAtUTC:        e.ReceivedAtUTC,
		Payload:              payload,
		PayloadChecksum:      checksum(payload),
	}
	if e.Source.Trusted {
		p.SourceTimeUTC = e.Source.UTC
	}
	return p, nil
}

// DecodePersistedEntry parses and strictly validates raw JSON previously
// produced by EncodePersistedEntry. Rejection (a non-nil error, always
// with no partial Entry returned) covers: malformed JSON, oversized
// input, a schema version other than exactly SchemaVersion (never a
// silent best-effort reinterpretation of an unknown version - see this
// package's own doc comment), a missing/wrong origin marker, an unknown
// ProductClass/Identity combination this build's own PolicyFor does not
// recognize, a payload checksum mismatch, and a source or receive time
// implausibly far in the future (a corrupt or tampered file must never
// be trusted as "current").
func DecodePersistedEntry(raw []byte, nowUTC time.Time) (Entry, string, error) {
	if len(raw) > maxPersistedEntryBytes {
		return Entry{}, "", fmt.Errorf("fisbcache: persisted entry of %d bytes exceeds the %d byte bound", len(raw), maxPersistedEntryBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p PersistedEntry
	if err := dec.Decode(&p); err != nil {
		return Entry{}, "", fmt.Errorf("fisbcache: malformed persisted entry: %w", err)
	}
	if p.SchemaVersion != SchemaVersion {
		return Entry{}, "", fmt.Errorf("fisbcache: persisted entry schema version %d, want %d", p.SchemaVersion, SchemaVersion)
	}
	if p.Origin != originMarker {
		return Entry{}, "", fmt.Errorf("fisbcache: persisted entry missing the expected FIS-B origin marker")
	}
	if len(p.Payload) > maxPersistedPayloadBytes {
		return Entry{}, "", fmt.Errorf("fisbcache: persisted payload of %d bytes exceeds the %d byte bound", len(p.Payload), maxPersistedPayloadBytes)
	}
	if checksum(p.Payload) != p.PayloadChecksum {
		return Entry{}, "", fmt.Errorf("fisbcache: persisted entry failed its payload checksum")
	}
	key := Key{Class: ProductClass(p.ProductClass), Identity: p.Identity}
	if !PolicyFor(key).known {
		return Entry{}, "", fmt.Errorf("fisbcache: persisted entry names an unsupported product class %q", p.ProductClass)
	}
	if p.SourceTimeTrusted {
		if !nowUTC.IsZero() && p.SourceTimeUTC.After(nowUTC.Add(maxFutureSkew)) {
			return Entry{}, "", fmt.Errorf("fisbcache: persisted entry's source time is implausibly in the future")
		}
	}
	if p.ReceivedAtUTCTrusted {
		if !nowUTC.IsZero() && p.ReceivedAtUTC.After(nowUTC.Add(maxFutureSkew)) {
			return Entry{}, "", fmt.Errorf("fisbcache: persisted entry's receive time is implausibly in the future")
		}
	}

	e := Entry{
		Key:       key,
		SizeBytes: int64(len(raw)),
	}
	if p.SourceTimeTrusted {
		e.Source = SourceTime{Trusted: true, UTC: p.SourceTimeUTC}
	}
	if p.ReceivedAtUTCTrusted {
		e.ReceivedAtUTC = p.ReceivedAtUTC
	}
	// ReceivedAtMonotonic is intentionally left zero here - the caller
	// (main/fisbcacherun.go's recovery pass) must set it explicitly from
	// the CURRENT process's own monotonic clock once trusted time is
	// available, per this package's "never calculate persistence age
	// from an untrusted wall clock" requirement; DecodePersistedEntry
	// itself has no monotonic clock to consult.
	return e, p.Payload, nil
}
