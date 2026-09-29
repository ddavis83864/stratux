package fisbcache

import (
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodePersistedEntry_RoundTrip(t *testing.T) {
	e := Entry{
		Key:           TextKey(TextProductMETAR, "KSEA"),
		Source:        SourceTime{Trusted: true, UTC: mustUTC("2026-06-15T12:00:00Z")},
		ReceivedAtUTC: mustUTC("2026-06-15T12:00:05Z"),
	}
	p, err := EncodePersistedEntry(e, "METAR KSEA 151200Z 00000KT 10SM CLR 20/10 A3000")
	if err != nil {
		t.Fatalf("EncodePersistedEntry: %v", err)
	}
	raw, err := marshalPersistedEntryForTest(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, payload, err := DecodePersistedEntry(raw, mustUTC("2026-06-15T12:05:00Z"))
	if err != nil {
		t.Fatalf("DecodePersistedEntry: %v", err)
	}
	if got.Key != e.Key {
		t.Errorf("Key mismatch: got %+v, want %+v", got.Key, e.Key)
	}
	if !got.Source.Trusted || !got.Source.UTC.Equal(e.Source.UTC) {
		t.Errorf("Source mismatch: got %+v, want %+v", got.Source, e.Source)
	}
	if !got.ReceivedAtUTC.Equal(e.ReceivedAtUTC) {
		t.Errorf("ReceivedAtUTC mismatch: got %v, want %v", got.ReceivedAtUTC, e.ReceivedAtUTC)
	}
	if payload != "METAR KSEA 151200Z 00000KT 10SM CLR 20/10 A3000" {
		t.Errorf("payload mismatch: %q", payload)
	}
}

// TestDecodePersistedEntry_SizeBytesMatchesLiveAdmission guards against the
// SizeBytes semantics regressing to len(raw) (the whole persisted JSON
// envelope) instead of len(payload) (the decoded product content only) -
// found via a real cross-restart field comparison where every recovered
// entry's SizeBytes was inflated by the envelope's own fixed overhead
// relative to what live admission had recorded for the identical payload.
func TestDecodePersistedEntry_SizeBytesMatchesLiveAdmission(t *testing.T) {
	payload := "METAR KSEA 151200Z 00000KT 10SM CLR 20/10 A3000"
	admissionSizeBytes := int64(len(payload)) // mirrors main/fisbcacherun.go's live-admission call site

	e := Entry{Key: TextKey(TextProductMETAR, "KSEA")}
	p, err := EncodePersistedEntry(e, payload)
	if err != nil {
		t.Fatalf("EncodePersistedEntry: %v", err)
	}
	raw, err := marshalPersistedEntryForTest(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, decodedPayload, err := DecodePersistedEntry(raw, time.Time{})
	if err != nil {
		t.Fatalf("DecodePersistedEntry: %v", err)
	}
	if decodedPayload != payload {
		t.Fatalf("decoded payload mismatch: got %q, want %q", decodedPayload, payload)
	}
	if got.SizeBytes != admissionSizeBytes {
		t.Errorf("SizeBytes mismatch across recovery: got %d, want %d (live-admission semantics: len(payload))",
			got.SizeBytes, admissionSizeBytes)
	}
	if got.SizeBytes == int64(len(raw)) && len(raw) != len(payload) {
		t.Errorf("SizeBytes appears to be measuring the whole persisted envelope (len(raw)=%d) instead of the payload (len(payload)=%d)",
			len(raw), len(payload))
	}
}

func TestEncodePersistedEntry_OversizedPayloadRejected(t *testing.T) {
	e := Entry{Key: TextKey(TextProductMETAR, "KSEA")}
	huge := strings.Repeat("x", maxPersistedPayloadBytes+1)
	if _, err := EncodePersistedEntry(e, huge); err == nil {
		t.Fatal("expected an error for an oversized payload")
	}
}

func TestDecodePersistedEntry_RejectsMalformedJSON(t *testing.T) {
	if _, _, err := DecodePersistedEntry([]byte("{not valid json"), time.Time{}); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestDecodePersistedEntry_RejectsOversizedInput(t *testing.T) {
	huge := make([]byte, maxPersistedEntryBytes+1)
	if _, _, err := DecodePersistedEntry(huge, time.Time{}); err == nil {
		t.Fatal("expected an error for oversized input")
	}
}

func TestDecodePersistedEntry_RejectsSchemaVersionMismatch(t *testing.T) {
	e := Entry{Key: TextKey(TextProductMETAR, "KSEA")}
	p, err := EncodePersistedEntry(e, "text")
	if err != nil {
		t.Fatal(err)
	}
	p.SchemaVersion = SchemaVersion + 1
	raw, _ := marshalPersistedEntryForTest(p)
	if _, _, err := DecodePersistedEntry(raw, time.Time{}); err == nil {
		t.Fatal("expected rejection of a future/mismatched schema version, not a silent reinterpretation")
	}
}

func TestDecodePersistedEntry_RejectsWrongOriginMarker(t *testing.T) {
	e := Entry{Key: TextKey(TextProductMETAR, "KSEA")}
	p, err := EncodePersistedEntry(e, "text")
	if err != nil {
		t.Fatal(err)
	}
	p.Origin = "INTERNET"
	raw, _ := marshalPersistedEntryForTest(p)
	if _, _, err := DecodePersistedEntry(raw, time.Time{}); err == nil {
		t.Fatal("expected rejection of a document not marked as genuine FIS-B origin")
	}
}

func TestDecodePersistedEntry_RejectsChecksumMismatch(t *testing.T) {
	e := Entry{Key: TextKey(TextProductMETAR, "KSEA")}
	p, err := EncodePersistedEntry(e, "original text")
	if err != nil {
		t.Fatal(err)
	}
	p.Payload = "tampered text" // checksum now stale
	raw, _ := marshalPersistedEntryForTest(p)
	if _, _, err := DecodePersistedEntry(raw, time.Time{}); err == nil {
		t.Fatal("expected rejection of a payload/checksum mismatch")
	}
}

func TestDecodePersistedEntry_RejectsUnknownProductClass(t *testing.T) {
	e := Entry{Key: Key{Class: "made_up_class", Identity: "x"}}
	// Bypass EncodePersistedEntry's own (nonexistent) class validation by
	// constructing PersistedEntry directly, to prove DecodePersistedEntry
	// itself is the enforcement point.
	p := PersistedEntry{SchemaVersion: SchemaVersion, Origin: originMarker, ProductClass: string(e.Key.Class), Identity: e.Key.Identity, Payload: "x", PayloadChecksum: checksum("x")}
	raw, _ := marshalPersistedEntryForTest(p)
	if _, _, err := DecodePersistedEntry(raw, time.Time{}); err == nil {
		t.Fatal("expected rejection of a product class this build has no policy for")
	}
}

func TestDecodePersistedEntry_RejectsFutureDatedSourceTime(t *testing.T) {
	e := Entry{
		Key:    TextKey(TextProductMETAR, "KSEA"),
		Source: SourceTime{Trusted: true, UTC: mustUTC("2026-06-15T12:00:00Z")},
	}
	p, err := EncodePersistedEntry(e, "text")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := marshalPersistedEntryForTest(p)
	// "now" is well before the persisted source time - must be rejected.
	if _, _, err := DecodePersistedEntry(raw, mustUTC("2020-01-01T00:00:00Z")); err == nil {
		t.Fatal("expected rejection of an implausibly future-dated source time")
	}
}

func TestDecodePersistedEntry_UnknownFieldsRejected(t *testing.T) {
	raw := []byte(`{"schemaVersion":1,"origin":"FISB","productClass":"text","identity":"METAR KSEA","payload":"x","payloadChecksum":"` + checksum("x") + `","unexpectedField":true}`)
	if _, _, err := DecodePersistedEntry(raw, time.Time{}); err == nil {
		t.Fatal("expected rejection of an unknown field, per this schema's strict-parsing requirement")
	}
}
