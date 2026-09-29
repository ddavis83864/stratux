package fisbcache

import "encoding/json"

// marshalPersistedEntryForTest is test-only glue: production code never
// marshals a PersistedEntry directly (main/fisbcachestorage.go streams it
// through storagelifecycle.WriteFunc instead) - this exists purely so
// schema_test.go can build the raw bytes DecodePersistedEntry expects
// without duplicating encoding/json.Marshal at every call site.
func marshalPersistedEntryForTest(p PersistedEntry) ([]byte, error) {
	return json.Marshal(p)
}
