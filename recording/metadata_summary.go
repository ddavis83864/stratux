package recording

// RecordingRef identifies one recording directory for SummarizeMetadata -
// deliberately just an ID/Dir pair, not the full recordingListEntry shape
// main/recordingapi.go already has, so this package stays independent of
// that HTTP-facing type.
type RecordingRef struct {
	ID  string
	Dir string
}

// MetadataDiagnosticsSummary is a bounded, diagnostics-safe summary of
// every known recording's metadata status - counts only, plus the single
// most-recent recording's identity and start-state availability. It never
// carries the contents of any individual recording's metadata (no
// Preflight check results, no calibration-profile identity), so it is
// always safe to embed directly in a diagnostic bundle regardless of how
// many recordings exist.
type MetadataDiagnosticsSummary struct {
	RecordingCount  int `json:"recordingCount"`
	WithMetadata    int `json:"withMetadata"`
	Legacy          int `json:"legacy"`
	Incomplete      int `json:"incomplete"`
	CorruptMetadata int `json:"corruptMetadata"`

	// MostRecentRecordingID is already a server-generated, non-sensitive
	// identifier (a timestamp-shaped string - see main/recordingapi.go's
	// recordingIDPattern), the same value already exposed by
	// GET /getRecordings, so including it here discloses nothing new.
	MostRecentRecordingID         string `json:"mostRecentRecordingId,omitempty"`
	MostRecentSchemaVersion       int    `json:"mostRecentSchemaVersion,omitempty"`
	MostRecentStartStateAvailable bool   `json:"mostRecentStartStateAvailable"`
}

// SummarizeMetadata builds a MetadataDiagnosticsSummary from refs, which
// must already be sorted newest-first by ID (the same order
// handleListRecordingsRequest already produces) so the MostRecent* fields
// reflect the actual most recent recording rather than an arbitrary one.
func SummarizeMetadata(refs []RecordingRef) MetadataDiagnosticsSummary {
	var sum MetadataDiagnosticsSummary
	sum.RecordingCount = len(refs)
	for i, ref := range refs {
		result := ReadMetadata(ref.Dir)
		switch result.Status {
		case MetadataOK:
			sum.WithMetadata++
			if !result.Metadata.Finalization.Complete {
				sum.Incomplete++
			}
			if i == 0 {
				sum.MostRecentRecordingID = ref.ID
				sum.MostRecentSchemaVersion = result.Metadata.SchemaVersion
				sum.MostRecentStartStateAvailable = true
			}
		case MetadataCorrupt:
			sum.CorruptMetadata++
			if i == 0 {
				sum.MostRecentRecordingID = ref.ID
			}
		default: // MetadataUnavailable: legacy recording, not an error
			sum.Legacy++
			if i == 0 {
				sum.MostRecentRecordingID = ref.ID
			}
		}
	}
	return sum
}
