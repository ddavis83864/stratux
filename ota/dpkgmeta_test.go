package ota

import (
	"bytes"
	"testing"
)

const sampleStatus = `Package: libncurses6
Status: install ok installed
Version: 6.4-4

Package: stratux
Status: install ok unpacked
Version: 2.0-pre5
Conffiles:
 /lib/systemd/system/stratux.service dac00f0ef66eda1ee697da5bd7449ef7

Package: librtlsdr0
Status: install ok installed
Version: 0.6.0-6
`

func TestExtractStanza_Found(t *testing.T) {
	s, ok := ExtractStanza([]byte(sampleStatus), "stratux")
	if !ok {
		t.Fatal("expected to find stratux stanza")
	}
	if !bytes.Contains(s, []byte("Status: install ok unpacked")) {
		t.Errorf("extracted stanza missing expected content: %q", s)
	}
	if !bytes.Contains(s, []byte("Conffiles:")) {
		t.Errorf("extracted stanza should preserve continuation lines: %q", s)
	}
	if bytes.Contains(s, []byte("libncurses6")) || bytes.Contains(s, []byte("librtlsdr0")) {
		t.Errorf("extracted stanza leaked neighboring stanza content: %q", s)
	}
}

func TestExtractStanza_NotFound(t *testing.T) {
	_, ok := ExtractStanza([]byte(sampleStatus), "nonexistent-package")
	if ok {
		t.Fatal("expected package not to be found")
	}
}

func TestExtractStanza_EmptyStatus(t *testing.T) {
	_, ok := ExtractStanza([]byte(""), "stratux")
	if ok {
		t.Fatal("expected not found on empty status")
	}
}

func TestExtractStanza_ExactNameMatch_NoSubstringCollision(t *testing.T) {
	status := "Package: stratux-foo\nStatus: install ok installed\nVersion: 1.0\n"
	_, ok := ExtractStanza([]byte(status), "stratux")
	if ok {
		t.Fatal("stratux-foo must not match a lookup for stratux")
	}
}

func TestReplaceStanza_ReplacesInPlace_PreservesNeighbors(t *testing.T) {
	newStanza := []byte("Package: stratux\nStatus: install ok installed\nVersion: 1.9-old\n")
	result := ReplaceStanza([]byte(sampleStatus), "stratux", newStanza)

	got, ok := ExtractStanza(result, "stratux")
	if !ok {
		t.Fatal("expected stratux stanza to still exist after replace")
	}
	if !bytes.Contains(got, []byte("1.9-old")) {
		t.Errorf("replacement did not take effect: %q", got)
	}
	if bytes.Contains(got, []byte("unpacked")) {
		t.Errorf("old stanza content leaked into replacement: %q", got)
	}

	// Neighbors must be byte-for-byte untouched.
	before, _ := ExtractStanza([]byte(sampleStatus), "libncurses6")
	after, _ := ExtractStanza(result, "libncurses6")
	if !bytes.Equal(before, after) {
		t.Errorf("neighboring stanza libncurses6 was altered:\nbefore=%q\nafter=%q", before, after)
	}
	before2, _ := ExtractStanza([]byte(sampleStatus), "librtlsdr0")
	after2, _ := ExtractStanza(result, "librtlsdr0")
	if !bytes.Equal(before2, after2) {
		t.Errorf("neighboring stanza librtlsdr0 was altered:\nbefore=%q\nafter=%q", before2, after2)
	}

	// Stanza count must be unchanged (replace, not append+orphan).
	if len(splitStanzas(result)) != 3 {
		t.Errorf("expected 3 stanzas after replace, got %d: %q", len(splitStanzas(result)), result)
	}
}

func TestReplaceStanza_RemoveWhenNewIsNil(t *testing.T) {
	result := ReplaceStanza([]byte(sampleStatus), "stratux", nil)
	if _, ok := ExtractStanza(result, "stratux"); ok {
		t.Fatal("expected stratux stanza to be removed")
	}
	if len(splitStanzas(result)) != 2 {
		t.Errorf("expected 2 remaining stanzas, got %d", len(splitStanzas(result)))
	}
}

func TestReplaceStanza_AppendWhenMissing(t *testing.T) {
	status := "Package: libncurses6\nStatus: install ok installed\nVersion: 6.4-4\n"
	newStanza := []byte("Package: stratux\nStatus: install ok installed\nVersion: 2.0-pre5\n")
	result := ReplaceStanza([]byte(status), "stratux", newStanza)

	got, ok := ExtractStanza(result, "stratux")
	if !ok {
		t.Fatal("expected stratux stanza to be appended")
	}
	if !bytes.Contains(got, []byte("2.0-pre5")) {
		t.Errorf("appended stanza wrong content: %q", got)
	}
	if len(splitStanzas(result)) != 2 {
		t.Errorf("expected 2 stanzas after append, got %d", len(splitStanzas(result)))
	}
}

func TestReplaceStanza_MissingAndNil_NoOp(t *testing.T) {
	status := "Package: libncurses6\nStatus: install ok installed\nVersion: 6.4-4\n"
	result := ReplaceStanza([]byte(status), "stratux", nil)
	if !bytes.Equal(bytes.TrimSpace(result), bytes.TrimSpace([]byte(status))) {
		t.Errorf("expected no-op, got %q", result)
	}
}

func TestReplaceStanza_Idempotent(t *testing.T) {
	newStanza := []byte("Package: stratux\nStatus: install ok installed\nVersion: 1.9-old\n")
	once := ReplaceStanza([]byte(sampleStatus), "stratux", newStanza)
	twice := ReplaceStanza(once, "stratux", newStanza)
	if !bytes.Equal(once, twice) {
		t.Errorf("ReplaceStanza is not idempotent:\nonce=%q\ntwice=%q", once, twice)
	}
}

func TestReplaceStanza_EmptyStatus_AppendsSingleStanza(t *testing.T) {
	newStanza := []byte("Package: stratux\nStatus: install ok installed\nVersion: 2.0-pre5\n")
	result := ReplaceStanza([]byte(""), "stratux", newStanza)
	got, ok := ExtractStanza(result, "stratux")
	if !ok {
		t.Fatal("expected stanza in previously-empty status")
	}
	if !bytes.Contains(got, []byte("2.0-pre5")) {
		t.Errorf("wrong content: %q", got)
	}
}
