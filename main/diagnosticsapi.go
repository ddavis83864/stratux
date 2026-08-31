/*
diagnosticsapi.go: wires the existing, unit-tested readiness.diagnostics
package into the running daemon via a minimal, additive HTTP API.

Endpoints (all new, none replace or rename an existing one):

	POST /generateDiagnostics          - generate a new sanitized bundle
	GET  /getDiagnostics                - list available bundles with metadata
	GET  /downloadDiagnostics?name=...  - download one sanitized bundle

Bundles are written only under diagnosticsDir (on the persistent data
partition, never the temporary root overlay), with server-generated
filenames (readiness.WriteDiagnosticBundle already timestamps them) -
the client never supplies a path, only ever selects an existing name
returned by /getDiagnostics. /downloadDiagnostics validates that name
against a fresh directory listing before ever building a path from it,
which is what actually prevents path traversal here - not string
scrubbing on the input.
*/
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/stratux/stratux/readiness"
)

const (
	diagnosticsDir         = PersistentDataPath + "/diagnostics"
	diagnosticsMaxRetain   = 10
	diagnosticsMaxLogLines = 500
	// diagnosticsMaxLogBytes bounds how much of the on-disk log file is
	// ever read for one bundle request, regardless of how large the file
	// on disk has grown - an "oversized log input" must degrade to a
	// truncated tail, never an unbounded read.
	diagnosticsMaxLogBytes = 8 << 20 // 8 MiB
)

// diagnosticsMu serializes bundle generation so concurrent /generateDiagnostics
// requests cannot interleave writes or race on retention pruning.
var diagnosticsMu sync.Mutex

// sensitiveLogLinePattern drops whole log lines that plausibly contain a
// credential, rather than trying to redact in place - the diagnostics
// bundle's log excerpt is for troubleshooting shape/timing/errors, not a
// byte-exact record, so dropping a suspect line costs little and is the
// safe failure direction.
var sensitiveLogLinePattern = regexp.MustCompile(`(?i)(passphrase|password|secret|token|credential|private[_-]?key|authorization:\s*bearer|authorization:\s*basic|ssh-rsa|ssh-ed25519|BEGIN [A-Z ]*PRIVATE KEY)`)

// diagnosticBundleNamePattern is the exact shape readiness.WriteDiagnosticBundle
// produces - used only to reject obviously-malformed names fast, before the
// real safety check (exact match against a fresh directory listing).
var diagnosticBundleNamePattern = regexp.MustCompile(`^diagnostic-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z\.json$`)

// recentSanitizedLogLines returns up to maxLines of the tail of path, with
// any line matching sensitiveLogLinePattern dropped. Missing file, unreadable
// file, or any other error returns (nil, false) rather than failing the
// caller - a diagnostic bundle missing its log excerpt is still useful and
// must be reported as a partial success, never turned into no bundle at all.
func recentSanitizedLogLines(path string, maxLines int) ([]string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	if info, statErr := f.Stat(); statErr == nil && info.Size() > diagnosticsMaxLogBytes {
		// Seek to the last diagnosticsMaxLogBytes rather than scanning
		// (and discarding) an unbounded amount of file we don't need -
		// this is the actual protection against "oversized log input".
		if _, seekErr := f.Seek(-diagnosticsMaxLogBytes, 2); seekErr == nil {
			// Discard the first (likely partial) line after the seek.
			bufio.NewReader(f).ReadString('\n')
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if sensitiveLogLinePattern.MatchString(line) {
			continue
		}
		lines = append(lines, line)
		if len(lines) > maxLines {
			lines = lines[1:]
		}
	}
	return lines, true
}

// diagnosticBundleInfo is what /getDiagnostics reports per bundle - just
// enough to pick one to download, never the bundle's own (sanitized, but
// still not public-by-default) content.
type diagnosticBundleInfo struct {
	Name        string    `json:"name"`
	SizeBytes   int64     `json:"sizeBytes"`
	GeneratedAt time.Time `json:"generatedAt"`
}

// listDiagnosticBundles returns bundle metadata, newest first. It never
// returns an error for "directory does not exist yet" - that's simply zero
// bundles, not a failure.
func listDiagnosticBundles() ([]diagnosticBundleInfo, error) {
	entries, err := os.ReadDir(diagnosticsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var bundles []diagnosticBundleInfo
	for _, e := range entries {
		if e.IsDir() || !diagnosticBundleNamePattern.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // a bundle that vanished between ReadDir and Stat is simply omitted
		}
		bundles = append(bundles, diagnosticBundleInfo{
			Name:        e.Name(),
			SizeBytes:   info.Size(),
			GeneratedAt: info.ModTime().UTC(),
		})
	}
	sort.Slice(bundles, func(i, j int) bool { return bundles[i].Name > bundles[j].Name })
	return bundles, nil
}

// handleGenerateDiagnosticsRequest serves POST /generateDiagnostics: builds
// and writes one new sanitized bundle, returning its metadata.
func handleGenerateDiagnosticsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	diagnosticsMu.Lock()
	defer diagnosticsMu.Unlock()

	now := time.Now().UTC()

	globalHealthMutex.Lock()
	health := globalHealth
	globalHealthMutex.Unlock()

	// Round-trip settings through JSON so SanitizeSettings can walk it as
	// a generic map - it must operate structurally, not on the Go type,
	// so a future settings field is safe-by-default even if this file is
	// never updated to know about it.
	var rawSettings map[string]interface{}
	if settingsJSON, err := json.Marshal(&globalSettings); err == nil {
		json.Unmarshal(settingsJSON, &rawSettings) //nolint:errcheck - best-effort; nil map is fine, SanitizeSettings handles it
	}

	logLines, logOK := recentSanitizedLogLines(logDir+"stratux.log", diagnosticsMaxLogLines)

	bundle := readiness.BuildDiagnosticBundle(now, stratuxVersion, stratuxBuild, health, rawSettings, logLines)
	path, err := readiness.WriteDiagnosticBundle(diagnosticsDir, bundle, diagnosticsMaxRetain)
	if err != nil && path == "" {
		log.Printf("diagnostics: generation failed: %s\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	resp := map[string]interface{}{
		"success":     true,
		"generatedAt": now,
	}
	if info, statErr := os.Stat(path); statErr == nil {
		resp["name"] = filepath.Base(path)
		resp["sizeBytes"] = info.Size()
	}
	if !logOK {
		resp["partial"] = true
		resp["warning"] = "recent log excerpt unavailable (log file missing or unreadable) - bundle generated without it"
	}
	if err != nil {
		// Bundle itself succeeded (path != "" above); this is a retention
		// pruning failure, worth surfacing but not a generation failure.
		resp["partial"] = true
		resp["warning"] = err.Error()
	}
	log.Printf("diagnostics: generated bundle %s\n", path)
	json.NewEncoder(w).Encode(resp)
}

// handleListDiagnosticsRequest serves GET /getDiagnostics.
func handleListDiagnosticsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	bundles, err := listDiagnosticBundles()
	if err != nil {
		http.Error(w, fmt.Sprintf("could not list diagnostic bundles: %s", err), http.StatusInternalServerError)
		return
	}
	if bundles == nil {
		bundles = []diagnosticBundleInfo{}
	}
	json.NewEncoder(w).Encode(bundles)
}

// handleDownloadDiagnosticsRequest serves GET /downloadDiagnostics?name=...
// The requested name is never used to build a path unless it exactly
// matches an entry from a fresh directory listing taken at request time -
// this, not pattern-matching the input, is what actually rules out path
// traversal (a name of "../../etc/passwd" simply never matches a listed
// entry, regardless of what characters it contains).
func handleDownloadDiagnosticsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	requested := r.URL.Query().Get("name")
	path, ok := resolveNameInDir(diagnosticsDir, requested, diagnosticBundleNamePattern)
	if !ok {
		http.Error(w, "diagnostic bundle not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", requested))
	http.ServeFile(w, r, path)
}
