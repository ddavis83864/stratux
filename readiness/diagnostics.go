package readiness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// sensitiveKeyPattern matches settings-map keys that must never appear in
// a diagnostic bundle. It is deliberately broad (substring match, case
// insensitive) rather than an exact-name allowlist: a new settings field
// whose name happens to contain "password"/"key"/"token"/etc. is excluded
// by default, which is the safe failure direction for this check - a
// falsely-excluded harmless field only loses a little diagnostic detail;
// a falsely-included secret is a credential leak.
var sensitiveKeyPattern = regexp.MustCompile(`(?i)(password|passphrase|secret|token|credential|private[_-]?key|authorized[_-]?key|ssh)`)

// SanitizeSettings returns a copy of raw with every key matching
// sensitiveKeyPattern removed, at every nesting level (a settings map may
// contain nested objects/arrays, e.g. NetworkOutputs, WiFiClientNetworks).
// It performs no I/O and never mutates raw.
func SanitizeSettings(raw map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(raw))
	for k, v := range raw {
		if sensitiveKeyPattern.MatchString(k) {
			continue
		}
		out[k] = sanitizeValue(v)
	}
	return out
}

func sanitizeValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return SanitizeSettings(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, item := range t {
			out[i] = sanitizeValue(item)
		}
		return out
	default:
		return v
	}
}

// DiagnosticBundle is the sanitized snapshot written to
// /var/lib/stratux-data/diagnostics for support/troubleshooting. Every
// field here is safe to share: SanitizeSettings has already removed
// credentials, and RecentLogLines is expected to be pre-filtered by the
// caller for the same reason (log lines can themselves contain a
// passphrase a user pasted into a field, which SanitizeSettings cannot
// catch since it operates on structured settings, not arbitrary log
// text - see BuildDiagnosticBundle's doc for the caller's responsibility
// here).
type DiagnosticBundle struct {
	GeneratedAt       time.Time
	Version           string
	Commit            string
	Health            HealthReport
	SanitizedSettings map[string]interface{}
	RecentLogLines    []string
}

// maxDiagnosticLogLines bounds how much log text one bundle embeds, so a
// verbose recent session cannot make the bundle unreasonably large.
const maxDiagnosticLogLines = 500

// BuildDiagnosticBundle assembles a DiagnosticBundle. rawSettings should
// be the settings struct already round-tripped through
// encoding/json (map[string]interface{}) so SanitizeSettings can walk it;
// recentLogLines should already have had any obviously sensitive lines
// (e.g. ones containing "passphrase=") filtered by the caller, since log
// text is unstructured and this package cannot reliably distinguish a
// logged secret from ordinary text.
func BuildDiagnosticBundle(now time.Time, version, commit string, health HealthReport, rawSettings map[string]interface{}, recentLogLines []string) DiagnosticBundle {
	lines := recentLogLines
	if len(lines) > maxDiagnosticLogLines {
		lines = lines[len(lines)-maxDiagnosticLogLines:]
	}
	return DiagnosticBundle{
		GeneratedAt:       now,
		Version:           version,
		Commit:            commit,
		Health:            health,
		SanitizedSettings: SanitizeSettings(rawSettings),
		RecentLogLines:    lines,
	}
}

// diagnosticFilePrefix/Suffix identify bundle files for retention pruning
// without touching anything else that might live in the diagnostics
// directory.
const (
	diagnosticFilePrefix = "diagnostic-"
	diagnosticFileSuffix = ".json"
)

// WriteDiagnosticBundle marshals bundle to a timestamped JSON file in dir
// and prunes older bundle files beyond maxRetain. It returns the path
// written. A failure here (e.g. the persistent partition is full or
// unavailable) must never be allowed to disrupt ADS-B/GDL90 operation -
// this function only ever returns an error for the caller to log; it does
// not panic or otherwise affect any other subsystem.
func WriteDiagnosticBundle(dir string, bundle DiagnosticBundle, maxRetain int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("could not create diagnostics directory: %w", err)
	}
	name := fmt.Sprintf("%s%s%s", diagnosticFilePrefix, bundle.GeneratedAt.UTC().Format("20060102T150405Z"), diagnosticFileSuffix)
	path := filepath.Join(dir, name)

	data, err := json.MarshalIndent(&bundle, "", "  ")
	if err != nil {
		return "", fmt.Errorf("could not marshal diagnostic bundle: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("could not write diagnostic bundle: %w", err)
	}

	if err := pruneDiagnosticBundles(dir, maxRetain); err != nil {
		// The bundle itself was written successfully; a pruning failure
		// is worth returning so the caller can log it, but must not make
		// this call look like it failed to produce a bundle.
		return path, fmt.Errorf("bundle written but retention pruning failed: %w", err)
	}
	return path, nil
}

func pruneDiagnosticBundles(dir string, maxRetain int) error {
	if maxRetain <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, diagnosticFilePrefix) && strings.HasSuffix(n, diagnosticFileSuffix) {
			names = append(names, n)
		}
	}
	// Filenames embed a sortable UTC timestamp, so lexical sort is
	// chronological.
	sort.Strings(names)
	if len(names) <= maxRetain {
		return nil
	}
	for _, n := range names[:len(names)-maxRetain] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}
