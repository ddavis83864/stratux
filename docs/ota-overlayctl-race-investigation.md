# Issue #49 investigation: OTA overlay-disable-marker race

> **Status: a real, code-evidenced in-process race identified and fixed
> (not reproduced on real hardware); this fix is NOT a complete
> system-wide fix.** A second, independent class of risk - a genuinely
> separate OS process (`debian/stratux-pre-start.sh`, run by systemd as
> `stratux.service`'s `ExecStartPre`) with its own five unlock/lock
> critical sections, entirely outside this process's mutex - is
> identified below and left unfixed, deliberately, pending hardware-
> validated testing this session cannot safely perform. See
> ["Scope limitation"](#scope-limitation-this-fix-is-not-system-wide)
> below before relying on this document to mean the race is closed. This
> document is a diagnostic record, not a physical acceptance report. See
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

## Scope limitation: this fix is not system-wide

**A Go `sync.Mutex` only coordinates goroutines within one process.**
`overlayCtlMu` (this fix) does nothing at all for a concurrent caller
that is a *different OS process*. Reading `image_build/stage2/10-stratux/files/overlayctl`
(the shell script both Go and shell callers invoke) confirms `lock`/`unlock`
do exactly one thing: `mount -o remount,ro|rw "$overlay_base"` - a raw
kernel mount-table operation, identical regardless of which process or
language issued it. The kernel has no concept of "this remount belongs to
this mutex's critical section"; two processes calling `mount -o remount`
on the same mountpoint race exactly the same way two unsynchronized
goroutines did before this fix, and nothing in this fix changes that.

**A confirmed, separate OS process also toggles this same state:**
`debian/stratux-pre-start.sh` - run by systemd as `stratux.service`'s
`ExecStartPre=-/opt/stratux/bin/stratux-pre-start.sh` (`debian/stratux.service:6`)
- contains **five of its own** `overlayctl unlock`/`lock` (or the
equivalent `disable`/`enable`, which the shell script's own `case`
statement shows do the identical unlock-write-lock dance internally)
critical sections, gated on: a pending legacy script-based update (lines
40-51, 70), the OTA state machine's own overlay-enable request during a
normal advance (lines 274-277) and its failure/rollback path (lines
420-423, 508-511), and first-boot overlay re-enablement (line 475). None
of these coordinate with `overlayCtlMu`, or with each other beyond their
own script's sequential control flow - because they cannot: they are a
different process, with its own separate memory space, started and ended
by systemd independently of the Go daemon's own lifetime.

**A concrete (not merely hypothetical) path connecting them was found by
code inspection**, though not observed to have actually occurred:

1. `applyNetworkSettings` (`main/networksettings.go`) dispatches its own
   `overlayCtlMu`-protected critical section inside a closure run via
   `go f()` when a settings change needs a network reconfiguration -
   **asynchronously**; the HTTP handler that triggered it
   (`/setSettings`, `main/managementinterface.go:671`) returns
   immediately, without waiting for `f()` to reach, let alone finish,
   its own unlock/lock window (which starts only after `f()`'s own
   `time.Sleep(time.Second)` and an `ifdown wlan0`).
2. A separate endpoint, `/restart` (`handleRestartRequest` ->
   `doRestartApp`, `main/managementinterface.go`), runs
   `systemctl restart stratux` - unconditionally, with no check for or
   coordination with any in-flight `overlayCtlMu` holder.
3. If a client calls `/restart` while `applyNetworkSettings`'s
   asynchronous goroutine is still inside its own critical section (or
   about to enter it), `systemctl restart` SIGTERMs the running daemon.
   Nothing in this codebase makes that goroutine's `overlayCtlMu`
   acquisition survive, block, or delay the restart - Go's default
   `os/signal` handling for an unhandled SIGTERM is immediate process
   termination, mid-goroutine, wherever it happens to be.
4. systemd then runs `ExecStartPre` (`stratux-pre-start.sh`) for the new
   instance - a **freshly started, separate process** - which, if an OTA
   is also mid-flight at that moment (`OTA_STATE` file present), reaches
   its own unlock/lock critical sections (above) with **no way to know**
   whether the overlay was left mid-transition by the just-killed
   process.

This is not proof the historical incident happened this way - no log
evidence ties a `/restart` call, or any other trigger of
`stratux-pre-start.sh`, to the timestamp of the one observed failure. It
is proof that **the class of risk this fix addresses is not fully closed
by an in-process mutex alone**, via a mechanism this session found by
reading the code, not by speculation about arbitrary timing.

**Why this was not also fixed in this session.** A complete fix would
need a cross-process lock (e.g. `flock(2)` on a well-known file both the
Go binary and the shell script acquire around their respective critical
sections) covering `stratux-pre-start.sh` as well as the three Go call
sites. That script runs at early boot, before most of the system this
project depends on is necessarily in a known state, has its own
documented history of subtle defects (see its own comment on
`overlay_is_active`'s prior false-positive, `docs/ota-persistent-storage-defect.md`),
and - critically - **cannot be exercised by any test this session can run
without real hardware**: unlike the Go mutex (provably tested by
`main/overlayctl_test.go`, architecture-independent), a change to this
script's own control flow would ship entirely unverified. Per this task's
own instruction to leave a speculative change out when the evidence does
not support a *safe* fix, that risk was judged to outweigh closing a gap
that is itself unconfirmed to have ever been exercised. The narrower,
well-tested in-process fix is judged safe to keep; the broader,
untestable cross-process fix is not attempted here.

**Recommended follow-up** (not implemented): a `flock`-based lock file
(e.g. `/run/lock/stratux-overlayctl.lock`, a location available very
early in boot on a tmpfs regardless of overlay state), acquired by
`overlayctl unlock`/`lock` themselves (or by every caller of them,
Go and shell alike) around each critical section - closing the gap at its
single narrowest point rather than requiring every future caller to
remember to coordinate. This needs hardware-validated testing before
being considered for the same reason it was not attempted now.

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

**Invariant, deadlock, and failure-state review** (this session, code
reading, not a hardware test):

- **The exact invariant protected**: at most one of this process's three
  critical sections is ever between its own `overlayctl unlock` and its
  own `overlayctl lock` at a time. This is precisely, and only, the
  invariant needed to prevent the in-process interleaving described
  above; it says nothing about `stratux-pre-start.sh` (see "Scope
  limitation").
- **All three in-process paths use the same lock**: confirmed by `grep -n
  overlayCtlMu main/*.go` (excluding the test file's own independent
  usage) - one declaration plus exactly one `Lock()`/`defer Unlock()`
  pair in each of `main/ota.go`, `main/wifiadminexecutor.go`,
  `main/networksettings.go`, no other acquisition anywhere in the
  package.
- **No re-entrant deadlock risk**: none of the three critical sections
  calls a function that itself acquires `overlayCtlMu` - confirmed by
  reading every function each critical section calls
  (`ota.StatMount`, `restoreDualTargets`, `(realAPReloader).Reload`,
  `writeTemplate`); none references `overlayctl` or `overlayCtlMu`. A Go
  `sync.Mutex` is not reentrant, so this matters: a nested acquisition
  from the same goroutine would deadlock instantly. There is none.
- **No new hang source, and no unintended overlay state on failure**: the
  underlying `mount -o remount` syscall these all eventually call has no
  Go-level timeout either before or after this fix - a genuinely wedged
  block device could still hang a caller indefinitely, exactly as before.
  What changes is that a hang now also blocks the *other* two call sites
  from proceeding (previously they could race ahead independently) -
  but the kernel already serializes concurrent remounts of the same
  mountpoint at the VFS layer regardless of any userspace mutex, so this
  is not a materially new failure mode, only a userspace ordering
  guarantee added on top of an already-serialized kernel operation. Every
  exit path (error or success) still reaches its own `overlayctl lock`
  call (or the deferred one) before the function returns and before
  `overlayCtlMu.Unlock()` runs - confirmed for all three sites by the
  diff itself: no early return added or changed that skips a lock call.

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

**Issue #49: findings updated, not closed, and its scope widened by this
session's review.** Three explanations for the historical incident are now
on record, classified by evidentiary strength, none proven:

1. **A demonstrated overlapping execution path**: none. No evidence (this
   session or the original) shows two callers actually running at the
   same moment.
2. **A possible path supported by code**, of which two independent
   instances are now identified:
   - Two of the three in-process Go call sites (`requestOverlayDisable`,
     `Apply`, `applyNetworkSettings`) running concurrently - the
     original hypothesis, and what `overlayCtlMu` fixes.
   - A Go call site racing `stratux-pre-start.sh` as a separate process
     - concretely reachable via `applyNetworkSettings`'s async goroutine
     plus a `/restart` call, per the "Scope limitation" section above -
     and **not fixed by this session's change**.
3. **An unproven explanation for the single observed incident**: both of
   the above remain exactly that - plausible, code-supported, not shown
   to be what actually happened.

**The process-local mutex is a correct, tested fix for its own stated
invariant** (no two of this process's own three call sites are ever
inside their overlay critical section at the same time) **and is not a
complete system-wide fix** - it does not, and structurally cannot,
coordinate with `stratux-pre-start.sh` running as a separate process. See
"Scope limitation" above for why that gap was documented rather than
closed in this session, and the recommended `flock`-based follow-up.

**Validation gate before this can be considered resolved (not yet met):**
either (a) direct evidence - a daemon log line or journal entry -
correlating a concurrent overlay-touching caller (Go-side or
`stratux-pre-start.sh`) with a future recurrence of the "read-only file
system" symptom, confirming (or ruling out) one of the two mechanisms
above; or (b) the cross-process gap is also closed (the recommended
`flock` follow-up, hardware-tested) and a subsequent OTA field session
completes without recurrence. Neither has happened. Issue #49 stays open
until one does.

**Does this change PR #51's acceptance procedure?** No new procedural
step is required, and this finding does not change that answer. The
acceptance kit's existing guidance - if a first OTA attempt reports this
specific error and rolls back safely, wait briefly and retry once before
treating it as a real problem - remains valid regardless of which
mechanism (if either) is the real cause, since the original rollback
behavior was already safe in the one observed instance. Nothing in this
session's widened scope finding makes that guidance wrong or requires
adding a new field-run precaution; it only clarifies that PR #52 should
not be described as having closed the underlying risk.

**Next diagnostic step, if this recurs:** capture the daemon's own log
around the failure (not just `getOTAStatus`) to check for a concurrent
`wifiadmin: applying settings` or network-settings-apply log line, **and**
the systemd journal around the same window for a `stratux.service`
restart/`ExecStartPre` invocation - the two pieces of direct evidence
this investigation could not obtain without real hardware, corresponding
to the two mechanisms above.
