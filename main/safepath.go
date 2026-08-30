/*
safepath.go: the one safety primitive diagnosticsapi.go and
recordingapi.go both rely on for turning a client-supplied name into a
filesystem path. The actual defense against path traversal is not
pattern-matching the input (a regex can always be gotten wrong) - it is
that a name is only ever used to build a path if it exactly matches an
entry from a *fresh* directory listing taken at request time. A name of
"../../etc/passwd" or "foo/../../bar" simply never equals any real
directory entry, regardless of what characters it contains.
*/
package main

import (
	"os"
	"path/filepath"
	"regexp"
)

// resolveNameInDir returns the full path for name if and only if name
// exactly matches a non-directory entry in dir that also matches pattern.
// The pattern check is a cheap fast-reject for obviously-wrong input; the
// directory-listing match is what actually matters for safety.
func resolveNameInDir(dir, name string, pattern *regexp.Regexp) (string, bool) {
	if name == "" || !pattern.MatchString(name) {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() && e.Name() == name {
			return filepath.Join(dir, name), true
		}
	}
	return "", false
}

// resolveSubdirInDir is resolveNameInDir's counterpart for names that must
// resolve to a subdirectory (a recording session), not a file.
func resolveSubdirInDir(dir, name string, pattern *regexp.Regexp) (string, bool) {
	if name == "" || !pattern.MatchString(name) {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() == name {
			return filepath.Join(dir, name), true
		}
	}
	return "", false
}
