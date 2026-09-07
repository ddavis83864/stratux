package configbackup

import (
	"encoding/json"
	"testing"
)

// mustMarshalLen returns len(json.Marshal(v)), failing the test on error -
// used wherever a test needs Validate's rawSize argument for a Document
// it just built in-memory (never uploaded/downloaded as bytes).
func mustMarshalLen(t *testing.T, v interface{}) int {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return len(b)
}
