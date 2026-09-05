package ota

import (
	"os/exec"
	"strings"
)

// DpkgStatus is the subset of `dpkg -s <package>` output this package
// needs to judge install health.
type DpkgStatus struct {
	Status  string // the raw "Status:" line value, e.g. "install ok installed"
	Version string
}

// Healthy reports whether status represents a fully, successfully
// installed package - dpkg's own definition of "nothing left to do".
func (d DpkgStatus) Healthy() bool {
	return d.Status == "install ok installed"
}

// Broken reports whether status represents a package dpkg left in an
// inconsistent, interrupted state (e.g. a crash or power loss partway
// through unpacking or configuring) - one that a plain "install ok
// installed" check would miss, but that must never be treated as "good
// enough" or silently retried without rolling back first.
func (d DpkgStatus) Broken() bool {
	switch d.Status {
	case "install ok installed", "":
		return false
	default:
		// Any other status ("half-installed", "unpacked",
		// "half-configured", "config-files", etc.) is dpkg's own signal
		// that this package did not reach a clean installed state.
		return true
	}
}

// ParseDpkgStatus parses `dpkg -s` output. It performs no I/O and is
// exercised directly by tests against real captured dpkg output.
func ParseDpkgStatus(output string) DpkgStatus {
	var d DpkgStatus
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "Status:"):
			d.Status = strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
		case strings.HasPrefix(line, "Version:"):
			d.Version = strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
		}
	}
	return d
}

// QueryDpkgStatus runs `dpkg -s <package>` and parses its output. A
// package dpkg has never heard of is reported as a zero-value DpkgStatus
// (Healthy()==false, Broken()==false) with no error - "not installed at
// all" is a normal, expected state for this query, not a failure to run
// dpkg.
func QueryDpkgStatus(pkg string) (DpkgStatus, error) {
	out, err := exec.Command("dpkg", "-s", pkg).CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			// dpkg -s exits non-zero (and prints "not installed") for a
			// package it has no record of - a normal, not-yet-installed
			// state, not an execution failure.
			return DpkgStatus{}, nil
		}
		return DpkgStatus{}, err
	}
	return ParseDpkgStatus(string(out)), nil
}

// FailureClass categorizes a dpkg failure for logging/decision purposes.
type FailureClass string

const (
	FailureUnknown     FailureClass = "unknown"
	FailureNoSpace     FailureClass = "no_space"
	FailureDependency  FailureClass = "dependency"
	FailureScriptError FailureClass = "maintainer_script"
)

// ClassifyDpkgFailure inspects dpkg's own error output text to categorize
// why an install attempt failed. This is the automated check for the
// exact failure mode ("No space left on device") that the first live
// deployment attempt hit when dpkg was accidentally run against the
// 250 MiB overlay instead of the real ext4 partition - this package's
// disable/enable sequencing exists specifically to prevent that scenario,
// but the classifier still exists so a genuine out-of-space condition
// (e.g. the bare ext4 partition itself is unexpectedly full) is reported
// distinctly from other install failures rather than lumped together.
func ClassifyDpkgFailure(output string) FailureClass {
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "no space left on device"):
		return FailureNoSpace
	case strings.Contains(lower, "dependency problems") || strings.Contains(lower, "depends on"):
		return FailureDependency
	case strings.Contains(lower, "subprocess") && strings.Contains(lower, "script"):
		return FailureScriptError
	default:
		return FailureUnknown
	}
}
