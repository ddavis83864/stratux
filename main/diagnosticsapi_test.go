package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempLog(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.log")
	content := ""
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecentSanitizedLogLines_MissingFileDegradesGracefully(t *testing.T) {
	lines, ok := recentSanitizedLogLines("/nonexistent/path/stratux.log", 100)
	if ok {
		t.Error("a missing log file must report ok=false, not fabricate success")
	}
	if lines != nil {
		t.Error("a missing log file must return no lines")
	}
}

func TestRecentSanitizedLogLines_BoundedToMaxLines(t *testing.T) {
	var raw []string
	for i := 0; i < 1000; i++ {
		raw = append(raw, "ordinary log line")
	}
	path := writeTempLog(t, raw)
	lines, ok := recentSanitizedLogLines(path, 50)
	if !ok {
		t.Fatal("expected success reading a normal file")
	}
	if len(lines) != 50 {
		t.Errorf("got %d lines, want exactly 50 (the tail)", len(lines))
	}
}

func TestRecentSanitizedLogLines_DropsCredentialLookingLines(t *testing.T) {
	raw := []string{
		"ordinary line one",
		"WiFiPassphrase=SuperSecret123",
		"another ordinary line",
		`Authorization: Bearer abcdef123456`,
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAB really-a-key",
		"-----BEGIN RSA PRIVATE KEY-----",
		"final ordinary line",
	}
	path := writeTempLog(t, raw)
	lines, ok := recentSanitizedLogLines(path, 100)
	if !ok {
		t.Fatal("expected success")
	}
	joined := strings.Join(lines, "\n")
	for _, secret := range []string{"SuperSecret123", "Bearer abcdef123456", "AAAAB3NzaC1yc2E", "BEGIN RSA PRIVATE KEY"} {
		if strings.Contains(joined, secret) {
			t.Errorf("sanitized output still contains credential-looking text: %q", secret)
		}
	}
	if !strings.Contains(joined, "ordinary line one") || !strings.Contains(joined, "final ordinary line") {
		t.Error("ordinary lines must be preserved, not over-filtered")
	}
}

func TestRecentSanitizedLogLines_OversizedInputDoesNotPanicOrHang(t *testing.T) {
	// Build a file well beyond diagnosticsMaxLogBytes and confirm the
	// function returns quickly with a bounded result rather than reading
	// (or panicking on) the whole thing.
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("x", 1000) + "\n"
	target := diagnosticsMaxLogBytes * 2
	written := 0
	for written < target {
		n, werr := f.WriteString(chunk)
		if werr != nil {
			t.Fatal(werr)
		}
		written += n
	}
	f.Close()

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("recentSanitizedLogLines panicked on oversized input: %v", rec)
		}
	}()
	lines, ok := recentSanitizedLogLines(path, diagnosticsMaxLogLines)
	if !ok {
		t.Fatal("expected success reading an oversized (but otherwise valid) file")
	}
	if len(lines) > diagnosticsMaxLogLines {
		t.Errorf("got %d lines, want at most %d", len(lines), diagnosticsMaxLogLines)
	}
}

func TestRecentSanitizedLogLines_EmptyFile(t *testing.T) {
	path := writeTempLog(t, nil)
	lines, ok := recentSanitizedLogLines(path, 10)
	if !ok {
		t.Fatal("expected success reading an empty file")
	}
	if len(lines) != 0 {
		t.Errorf("expected zero lines from an empty file, got %d", len(lines))
	}
}

func TestDiagnosticBundleNamePattern_RejectsPathTraversalShapes(t *testing.T) {
	for _, bad := range []string{
		"../../etc/passwd",
		"diagnostic-20260101T000000000Z.json/../../etc/passwd",
		"diagnostic-invalid.json",
		"",
		"diagnostic-20260101T000000000Z.txt",
	} {
		if diagnosticBundleNamePattern.MatchString(bad) {
			t.Errorf("pattern incorrectly matched malformed/traversal name: %q", bad)
		}
	}
	if !diagnosticBundleNamePattern.MatchString("diagnostic-20260101T000000000Z.json") {
		t.Error("pattern should match a well-formed bundle name")
	}
}
