# Issue #49 investigation: OTA overlay-disable-marker race

> **Status: root cause identified by code analysis (not reproduced on real
> hardware); a narrow, safe fix implemented and tested.** This document is
> a diagnostic record, not a physical acceptance report. See
> [issue #49](https://github.com/ddavis83864/stratux/issues/49) for the
> original observation.

## What was observed (issue #49, 2026-09-28)

During PR #48's physical acceptance session, the first `POST /updateUpload`
attempt against the real device rolled back with:

```
LastError: "could not write disable marker: open /overlay/robase/overlay/disable: read-only file system"
```

- The device rebooted mid-attempt and rolled back cleanly - no corruption,
  no failed units, `dpkg --audit` clean afterward.
- A manual `sudo /sbin/overlayctl unlock` on the running device immediately
  afterward succeeded, remounted `/overlay/robase` read-write, and a test
  write succeeded without issue.
- An unmodified retry of the exact same `/updateUpload` request succeeded
  cleanly end to end.

## What is confirmed, and by what evidence

**Confirmed by direct code reading (this session), not by reproducing the
race on hardware:**

`/sbin/overlayctl unlock`/`lock` toggles a single, process-global,
filesystem-wide mount state (`/overlay/robase`'s read-write/read-only
mode). This process has (at least) three independent call sites that each
assume they alone are toggling it for the duration of their own critical
section, with **no mutual exclusion between them**:

| Call site | File | What it does between unlock and lock |
|---|---|---|
| `requestOverlayDisable` | `main/ota.go:295` | stats the marker directory, writes the persistent overlay-disable marker, `sync()`s |
| `(realWifiExecutor).Apply` | `main/wifiadminexecutor.go:421` | writes Wi-Fi admin config files to the persistent partition |
| `applyNetworkSettings`'s inner closure | `main/networksettings.go:186` | writes dnsmasq/interfaces/wpa_supplicant templates to the persistent partition |

Confirmed via `grep -rln overlayctl main/*.go`: these are the only three
call sites (plus the shared `overlayctl()` helper itself,
`main/gen_gdl90.go:1570`, which `requestOverlayDisable` does not even use -
it shells out directly). Confirmed via direct reading of each call site
(quoted above): none of the three previously held any mutex, semaphore, or
other coordination device around their own unlock/work/lock sequence.

**The failure signature matches exactly what this race predicts.**
`requestOverlayDisable`'s own `overlayctl unlock` call succeeded (its error
path, `"overlayctl unlock: %w"`, is not what issue #49 shows); the
subsequent `os.WriteFile` is what failed with `EROFS`. The only way an
`unlock` can succeed and a write moments later can see the filesystem
read-only again is if something else re-locked it in between. A concurrent
`Apply()` or `applyNetworkSettings()` invocation - unlock (no-op, already
unlocked) → its own work → `lock` - happening to finish and re-lock in
that exact window would produce precisely this symptom.

## What remains unconfirmed

**Whether this specific race is what actually happened in the one observed
instance.** No log evidence (journal, application log) from that session
captures a concurrent Wi-Fi admin or network-settings apply actually
running at the same moment. The hypothesis is fully consistent with the
observed symptom and with the code's own structure, but causation for that
one historical event is not proven - only that a real, exploitable race
exists in the code as written, independent of whether it explains that
particular incident.

**Whether the race is easily reproducible.** Reproducing it would require
triggering an OTA update and a Wi-Fi admin/network-settings apply at
almost exactly the same moment on real hardware - not attempted in this
task (no OTA/device manipulation is authorized here), and inherently
timing-sensitive even if attempted.

## Why this is a specific defect, not a guessed timing fix

This is not a "wait longer" or "retry N times" patch papering over
unpredictable timing. It is a standard, well-understood concurrency
defect - unsynchronized access to shared mutable state from multiple
goroutines/call sites in the same process - with a standard, well-understood
fix: mutual exclusion around each critical section. The codebase already
uses exactly this pattern elsewhere for comparable shared state
(`systemErrsMutex`, `msgLogMutex`, `ADSBTowerMutex`, all in
`main/gen_gdl90.go`). The fix here follows the same established convention.

## The fix

A single package-level `sync.Mutex` (`overlayCtlMu`, `main/gen_gdl90.go`,
next to the existing `overlayctl()` helper and its doc comment) is now
acquired for the full duration of each of the three critical sections -
from before the first `overlayctl("unlock")` (or, for
`requestOverlayDisable`, the direct `exec.Command` equivalent) to after the
final `overlayctl("lock")` - via `Lock()`/`defer Unlock()`, so every exit
path (including early returns and, for `networksettings.go`'s inner
closure, a future panic) still releases it.

**Diff scope:** 4 files, 49 insertions, 6 deletions (`git diff --stat`):
- `main/gen_gdl90.go`: adds `overlayCtlMu` and its doc comment. No other
  change to this file.
- `main/ota.go`: two lines added to `requestOverlayDisable` (acquire before
  the first `overlayctl unlock`, `defer` the release). No other change.
- `main/wifiadminexecutor.go`: two lines added to `Apply` (same pattern).
  No other change.
- `main/networksettings.go`: the existing unlock/writes/lock block wrapped
  in an immediately-invoked function literal that acquires the mutex
  first; the four `writeTemplate` calls and their arguments are otherwise
  byte-identical to before.

No behavior changes for the non-concurrent case: a single caller still
does exactly what it did before, just now also holding a mutex that only
another *concurrent* overlayctl caller could ever contend for.

## Testing

**What was tested:** `main/overlayctl_test.go`
(`TestOverlayCtlMu_SerializesConcurrentCriticalSections`) directly proves
`overlayCtlMu` correctly serializes many concurrent goroutines each
simulating one caller's full critical section (enter, do simulated work,
leave), using an atomic compare-and-swap on a shared flag to detect any
overlap - the exact mechanism the fix relies on. 16 goroutines × 500
iterations, PASS.

**What was not tested, and why:** the real `requestOverlayDisable`,
`Apply`, and `applyNetworkSettings` functions end-to-end under real
concurrency. Each does real filesystem/exec work (`/overlay/robase`,
`/sbin/overlayctl`) that does not exist off-device; exercising them safely
requires the actual device, which this task's own constraints exclude
(no OTA, no device mutation). The mutex-serialization test above proves
the mechanism; each of the three call sites' own correct use of it was
verified by code review (the diff above), not by an integration test.

**Environment note:** the race detector (`go test -race`) could not run
for the cgo-linked `main` package in this session's Docker/QEMU
arm64 cross-build environment (`FATAL: ThreadSanitizer: unsupported VMA
range` - a QEMU user-mode emulation limitation unrelated to this change).
The concurrency test above does not depend on `-race`; its own
atomic-compare-and-swap-based overlap detection is architecture-independent
and does not need it. `go test ./main/... ./ota/...` (no `-race`): all
existing tests plus the new one pass, 18.4s total, no regressions. `go
vet ./main/...`: the same two pre-existing `main/datalog.go` findings this
project already carries on unmodified `master` (confirmed by direct
comparison); nothing new.

## Disposition

**Issue #49: findings updated, not closed.** The structural race is
confirmed and fixed in code (branch `fix/ota-overlayctl-race`, kept
separate from PR #51); the one historical incident's exact cause remains
unconfirmed (plausible, not proven). The fix has not been physically
validated - that requires a real OTA/Wi-Fi-settings concurrency scenario
on hardware, not attempted here.

**Does this change PR #51's acceptance procedure?** No new procedural step
is required. The acceptance kit's existing guidance - if a first OTA
attempt reports this specific error and rolls back safely, wait briefly
and retry once before treating it as a real problem - remains valid
regardless of whether this fix is present, since the original rollback
behavior was already safe. If this fix is deployed to the test device in a
future session, it should reduce or eliminate recurrences, but the kit
does not assume that outcome.

**Next diagnostic step, if this recurs:** capture the daemon's own log
around the failure (not just `getOTAStatus`) to check for a concurrent
`wifiadmin: applying settings` or network-settings-apply log line at the
same timestamp - the one piece of direct evidence this investigation
could not obtain without real hardware.
