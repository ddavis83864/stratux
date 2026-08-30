package ota

import "testing"

func TestParseDpkgStatus_HealthyInstall(t *testing.T) {
	out := "Package: stratux\nStatus: install ok installed\nVersion: 2.0-pre5\n"
	got := ParseDpkgStatus(out)
	if !got.Healthy() {
		t.Errorf("expected Healthy()==true for %+v", got)
	}
	if got.Broken() {
		t.Errorf("a healthy install must not be Broken()")
	}
	if got.Version != "2.0-pre5" {
		t.Errorf("Version = %q, want 2.0-pre5", got.Version)
	}
}

func TestParseDpkgStatus_HalfInstalled(t *testing.T) {
	out := "Package: stratux\nStatus: half-installed\n"
	got := ParseDpkgStatus(out)
	if got.Healthy() {
		t.Error("half-installed must not be Healthy()")
	}
	if !got.Broken() {
		t.Error("half-installed must be Broken()")
	}
}

func TestParseDpkgStatus_NotInstalledAtAll(t *testing.T) {
	// A package dpkg has never heard of: no Status line at all.
	got := ParseDpkgStatus("")
	if got.Healthy() || got.Broken() {
		t.Errorf("a completely unknown package should be neither Healthy nor Broken, got %+v", got)
	}
}

func TestQueryDpkgStatus_UnknownPackage(t *testing.T) {
	// Exercises the real dpkg binary if present; if dpkg itself is
	// unavailable in the test environment, skip rather than fail.
	status, err := QueryDpkgStatus("this-package-does-not-exist-in-ota-tests")
	if err != nil {
		t.Skipf("dpkg not usable in this environment: %v", err)
	}
	if status.Healthy() {
		t.Error("a nonexistent package must not report Healthy")
	}
}

func TestClassifyDpkgFailure_NoSpace(t *testing.T) {
	out := "dpkg: error processing archive /tmp/stratux.deb (--install):\n unable to make backup link of './opt/stratux/mapdata/osm.mbtiles' before installing new version: No space left on device"
	if got := ClassifyDpkgFailure(out); got != FailureNoSpace {
		t.Errorf("ClassifyDpkgFailure = %q, want %q", got, FailureNoSpace)
	}
}

func TestClassifyDpkgFailure_Dependency(t *testing.T) {
	out := "dpkg: dependency problems prevent configuration of stratux:\n stratux depends on libncurses6"
	if got := ClassifyDpkgFailure(out); got != FailureDependency {
		t.Errorf("ClassifyDpkgFailure = %q, want %q", got, FailureDependency)
	}
}

func TestClassifyDpkgFailure_Unknown(t *testing.T) {
	if got := ClassifyDpkgFailure("some entirely unrelated error text"); got != FailureUnknown {
		t.Errorf("ClassifyDpkgFailure = %q, want %q", got, FailureUnknown)
	}
}
