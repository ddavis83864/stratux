/*
	ota.go: the Go-daemon half of the deterministic OTA update state
	machine (see the `ota` package for the pure decision logic, and
	debian/stratux-pre-start.sh for the bare-ext4 install half, which must
	run before this daemon exists on that boot).

	Responsibilities on this side only:
	  - accept and verify an uploaded .deb (GET/POST /updateUpload)
	  - stage it under the persistent data partition, record its hash and
	    the version/commit it contains
	  - request an overlay-disabled reboot using the proven-persistent
	    marker procedure (narrow remount-rw, write, sync, remount-ro)
	  - after returning from the round trip, verify the new build is
	    actually running before declaring success
	  - on failure, hand off to the bare-ext4 rollback path the same way
	    a normal install does (request disable, reboot) rather than try to
	    restore files itself while running under the live overlay
*/

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stratux/stratux/ota"
	"github.com/stratux/stratux/readiness"
)

// otaDir is the persistent directory holding all OTA state and staged
// packages - on the dedicated data partition, never on /boot/firmware
// (small, shared with critical boot files) and never anywhere under the
// protected overlay.
const otaDir = "/var/lib/stratux-data/updates"

func otaStagedDir() string { return filepath.Join(otaDir, "staged") }
func otaBackupDir() string { return filepath.Join(otaDir, "backup") }

// otaMaxRetainedPackages bounds how many staged/backup files accumulate
// under otaDir - cleanup after a successful update keeps this small.
const otaMaxRetainedPackages = 3

// handleOTAUploadRequest replaces the previous handleUpdatePostRequest for
// the package-update path: it stages the upload under the persistent data
// partition (not /boot/firmware), verifies it is a well-formed .deb,
// records its hash and contained version/commit, and then drives the
// first OTA transition (staged -> disable_requested) itself, exactly as
// ota.Decide would direct, before rebooting.
func handleOTAUploadRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)

	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, fmt.Sprintf("update failed: %s", err), http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(otaStagedDir(), 0o755); err != nil {
		http.Error(w, fmt.Sprintf("update failed: could not create staging directory: %s", err), http.StatusInternalServerError)
		return
	}

	var stagedPath string
	for {
		part, err := reader.NextPart()
		if err != nil {
			http.Error(w, fmt.Sprintf("update failed: %s", err), http.StatusBadRequest)
			return
		}
		if part == nil {
			http.Error(w, "update failed: no update_file part found", http.StatusBadRequest)
			return
		}
		if part.FormName() != "update_file" {
			continue
		}
		stagedPath = filepath.Join(otaStagedDir(), filepath.Base(part.FileName()))
		tmpPath := stagedPath + ".uploading"
		fi, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			http.Error(w, fmt.Sprintf("update failed: %s", err), http.StatusInternalServerError)
			return
		}
		_, copyErr := io.Copy(fi, part)
		fi.Close()
		if copyErr != nil {
			os.Remove(tmpPath)
			http.Error(w, fmt.Sprintf("update failed: %s", copyErr), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(tmpPath, stagedPath); err != nil {
			http.Error(w, fmt.Sprintf("update failed: %s", err), http.StatusInternalServerError)
			return
		}
		break
	}

	sha256Hex, err := ota.HashFile(stagedPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("update failed: could not hash staged package: %s", err), http.StatusInternalServerError)
		return
	}
	version, commit, err := inspectDebPackage(stagedPath)
	if err != nil {
		os.Remove(stagedPath)
		http.Error(w, fmt.Sprintf("update failed: not a usable stratux package: %s", err), http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	state := ota.NewState(stagedPath, sha256Hex, version, commit, now)
	if err := ota.SaveState(otaDir, state, now); err != nil {
		http.Error(w, fmt.Sprintf("update failed: could not persist OTA state: %s", err), http.StatusInternalServerError)
		return
	}
	log.Printf("OTA: staged %s (version=%s commit=%s sha256=%s)\n", stagedPath, version, commit, sha256Hex)

	if err := otaAdvance(); err != nil {
		log.Printf("OTA: could not advance past staging: %s\n", err)
		http.Error(w, fmt.Sprintf("update staged but could not proceed: %s", err), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "{\"staged\":true,\"version\":%q,\"commit\":%q,\"sha256\":%q}\n", version, commit, sha256Hex)
}

// inspectDebPackage extracts the Version control field and the embedded
// git commit (from the packaged stratuxrun binary) of a staged .deb,
// without installing it.
func inspectDebPackage(path string) (version, commit string, err error) {
	out, err := exec.Command("dpkg-deb", "-f", path, "Version").Output()
	if err != nil {
		return "", "", fmt.Errorf("dpkg-deb -f: %w", err)
	}
	version = strings.TrimSpace(string(out))
	if version == "" {
		return "", "", fmt.Errorf("package has no Version field")
	}

	tmpDir, err := os.MkdirTemp("", "ota-inspect-*")
	if err != nil {
		return "", "", fmt.Errorf("could not create inspection temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if out, err := exec.Command("dpkg-deb", "-x", path, tmpDir).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("dpkg-deb -x: %w: %s", err, out)
	}
	binPath := filepath.Join(tmpDir, "opt", "stratux", "bin", "stratuxrun")
	data, err := os.ReadFile(binPath)
	if err != nil {
		return "", "", fmt.Errorf("could not read packaged binary: %w", err)
	}
	commit = findEmbeddedCommit(data)
	if commit == "" {
		return "", "", fmt.Errorf("could not find an embedded commit hash in the packaged binary")
	}
	return version, commit, nil
}

// findEmbeddedCommit scans data for a 40-character lowercase-hex string
// (the format `-ldflags -X main.stratuxBuild=...` embeds, see Makefile),
// bounded on both sides by non-hex bytes so it does not match inside a
// longer run of hex-looking bytes.
func findEmbeddedCommit(data []byte) string {
	isHex := func(b byte) bool {
		return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
	}
	n := len(data)
	for i := 0; i+40 <= n; i++ {
		if i > 0 && isHex(data[i-1]) {
			continue
		}
		if i+40 < n && isHex(data[i+40]) {
			continue
		}
		candidate := data[i : i+40]
		allHex := true
		for _, b := range candidate {
			if !isHex(b) {
				allHex = false
				break
			}
		}
		if allHex {
			return string(candidate)
		}
	}
	return ""
}

// otaOverlayRobaseDisableMarker is the proven-persistent marker path -
// see ota/mount.go's package documentation for the hardware evidence.
// /overlay/robase/overlay/disable shares its device number with the real
// mounted ext4 lower root; the lookalike /overlay/pivot/overlay/disable
// is a tmpfs shadow mounted there by init-overlay's own choreography and
// does not survive a reboot.
const otaOverlayRobaseDisableMarker = "/overlay/robase/overlay/disable"

// requestOverlayDisable performs the narrow remount-rw/write/sync/relock
// sequence proven on hardware to persistently request a bare-ext4 boot.
// It first proves the marker path is genuinely persistent (matching the
// device number of /overlay/robase itself) rather than assuming it -
// the same device-identity check ota.IsPersistent encodes.
func requestOverlayDisable() error {
	reference, err := ota.StatMount("/overlay/robase")
	if err != nil {
		return fmt.Errorf("could not stat /overlay/robase: %w", err)
	}
	if out, err := exec.Command("/sbin/overlayctl", "unlock").CombinedOutput(); err != nil {
		return fmt.Errorf("overlayctl unlock: %w: %s", err, out)
	}
	candidate, err := ota.StatMount(filepath.Dir(otaOverlayRobaseDisableMarker))
	if err != nil {
		exec.Command("/sbin/overlayctl", "lock").Run()
		return fmt.Errorf("could not stat marker directory: %w", err)
	}
	if ok, reason := ota.IsPersistent(candidate, reference); !ok {
		exec.Command("/sbin/overlayctl", "lock").Run()
		return fmt.Errorf("refusing to write the overlay-disable marker: %s", reason)
	}
	if err := os.WriteFile(otaOverlayRobaseDisableMarker, []byte("1\n"), 0o644); err != nil {
		exec.Command("/sbin/overlayctl", "lock").Run()
		return fmt.Errorf("could not write disable marker: %w", err)
	}
	syscall.Sync()
	if out, err := exec.Command("/sbin/overlayctl", "lock").CombinedOutput(); err != nil {
		return fmt.Errorf("overlayctl lock: %w: %s", err, out)
	}
	return nil
}

// otaAdvance loads the current OTA state, decides the next action via
// ota.Decide, and performs any action that belongs on this (overlay-
// active) side of the sequence. It is idempotent and safe to call
// repeatedly - most calls will find nothing to do (ActionNone,
// ActionAwaitReboot, ActionAwaitRebootToOverlay all take no action here).
func otaAdvance() error {
	state, err := ota.LoadState(otaDir)
	if err != nil {
		return err
	}
	if state.Stage == ota.StageIdle {
		return nil
	}

	root, err := readiness.FindMount("/")
	if err != nil {
		return fmt.Errorf("could not determine root filesystem type: %w", err)
	}
	signals := ota.RealSignals{RootFSType: root.FSType}
	if state.PackagePath != "" {
		if _, statErr := os.Stat(state.PackagePath); statErr == nil {
			signals.PackageFileExists = true
			if h, hashErr := ota.HashFile(state.PackagePath); hashErr == nil {
				signals.ComputedSHA256 = h
			}
		}
	}
	signals.RunningCommit = globalStatus.Build

	decision := ota.Decide(state, signals)
	now := time.Now().UTC()

	switch decision.Action {
	case ota.ActionNone, ota.ActionAwaitReboot, ota.ActionAwaitRebootToOverlay:
		// Nothing for the daemon to do; either idle/terminal, or waiting
		// on a reboot that has not happened yet.
		return nil

	case ota.ActionRequestDisable:
		if err := requestOverlayDisable(); err != nil {
			state.Stage = ota.StageFailed
			state.LastError = err.Error()
			ota.SaveState(otaDir, state, now)
			return err
		}
		state.Stage = ota.StageDisableRequested
		if err := ota.SaveState(otaDir, state, now); err != nil {
			return err
		}
		log.Printf("OTA: overlay-disable requested; rebooting to bare root\n")
		go delayReboot()
		return nil

	case ota.ActionVerify:
		if signals.RunningCommit == state.ExpectedCommit {
			state.Stage = ota.StageComplete
			ota.SaveState(otaDir, state, now)
			log.Printf("OTA: update to commit %s verified complete\n", state.ExpectedCommit)
			otaCleanup(state)
			return nil
		}
		state.Stage = ota.StageFailed
		state.LastError = decision.Reason
		ota.SaveState(otaDir, state, now)
		log.Printf("OTA: verification failed (%s); requesting rollback\n", decision.Reason)
		if err := requestOverlayDisable(); err != nil {
			return err
		}
		go delayReboot()
		return nil

	case ota.ActionFail, ota.ActionRollback:
		// A failure was recorded (by this daemon or by the bare-ext4
		// install stage) while we are back under the overlay. Hand off
		// to the same disable/reboot path the install stage itself
		// uses; debian/stratux-pre-start.sh's "failed" handling
		// restores the pre-install backup and re-enables the overlay.
		state.Stage = ota.StageFailed
		state.LastError = decision.Reason
		ota.SaveState(otaDir, state, now)
		log.Printf("OTA: %s; requesting rollback boot\n", decision.Reason)
		if err := requestOverlayDisable(); err != nil {
			return err
		}
		go delayReboot()
		return nil

	default:
		// ActionInstall / ActionRequestEnable / ActionComplete are either
		// bare-ext4-only steps this daemon never runs under (Install,
		// RequestEnable) or already handled above (Complete never
		// reaches here as a fresh decision because StageComplete is
		// terminal in Decide). Log and take no action rather than guess.
		log.Printf("OTA: no daemon-side handling for action %s (%s)\n", decision.Action, decision.Reason)
		return nil
	}
}

// otaCleanup removes staged packages and backups beyond
// otaMaxRetainedPackages after a confirmed-successful update, and clears
// the state file, returning to idle.
func otaCleanup(state ota.State) {
	for _, dir := range []string{otaStagedDir(), otaBackupDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		if len(entries) <= otaMaxRetainedPackages {
			continue
		}
		// Directory entries from os.ReadDir are sorted by name; staged
		// package/backup filenames both embed a sortable timestamp or
		// are the single current package, so oldest-first pruning here
		// is safe.
		for _, e := range entries[:len(entries)-otaMaxRetainedPackages] {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	if err := ota.ClearState(otaDir); err != nil {
		log.Printf("OTA: cleanup could not clear state: %s\n", err)
	}
}

// handleOTAStatusRequest serves GET /getOTAStatus: the current OTA update
// state as JSON, for diagnostics. Intentionally separate from
// /getHealth - this is operational state for an in-progress update, not a
// component health record.
func handleOTAStatusRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	data, err := otaStateJSON()
	if err != nil {
		http.Error(w, fmt.Sprintf("could not read OTA state: %s", err), http.StatusInternalServerError)
		return
	}
	w.Write(data)
}

// otaStateJSON serves the current OTA state as JSON for diagnostics -
// intentionally not part of the stable /getHealth schema, since this is
// operational state for an in-progress update, not a component health
// record.
func otaStateJSON() ([]byte, error) {
	state, err := ota.LoadState(otaDir)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(&state, "", "  ")
}
