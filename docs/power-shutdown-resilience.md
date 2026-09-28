# Power and Controlled-Shutdown Resilience

> **Status: built and tested, not yet deployed.** This feature shipped on its own draft
> pull request, built and verified into an ARM64 `.deb`, but deliberately not installed on
> any device. The hardware-validation checklist at the bottom of this document is prepared
> but not executed - see that section for why, and what a future, owner-authorized mission
> needs to do before this is deployed.

## What this is, and what it is not

Stratux typically runs from an ordinary USB power bank or an aircraft's USB power source.
That hardware path gives the Raspberry Pi's own firmware exactly one signal about power
health - the `get_throttled` register - and nothing else.

This feature adds three things on top of that one real signal:

1. A power-health model (`power` package) that reports what the Pi firmware can actually
   measure - under-voltage and thermal/frequency throttling, both right now and at any point
   since boot - debounced against single noisy readings, and paired with explicit,
   always-visible text about what this hardware *cannot* measure.
2. A manual, confirmed controlled-shutdown flow (two explicit steps) and controlled-restart
   flow (one explicit confirmation), so an operator can power the device off or reboot it
   cleanly (flushing any active recording first) without pulling power or SSHing in - the
   single supported place in the Web UI for either action.
3. A previous-session marker: a small, conservatively-worded note about whether the last
   session recorded a clean shutdown or reboot before this boot started.

### Explicit non-goals

None of the following are implemented, and none are planned as part of this feature:

- **Battery percentage or state-of-charge estimation.** Nothing in this signal path reports
  one, on any hardware this feature has been built or tested against.
- **Automatic or unattended shutdown.** Every shutdown this feature can perform requires two
  separate, explicit human actions (`POST /requestShutdown` then `POST /confirmShutdown`
  with the token the first call returned). There is no code path that shuts the device down
  on its own initiative in response to a power reading.
- **GPIO reservation for a power controller.**
- **UPS HAT or smart-battery integration of any kind.**

A future revision could add real capability here (e.g. a HAT that reports true
state-of-charge) without breaking anything documented below - the power-health response's
`hasTrustedBatterySignal`/`hasRuntimeEstimate` fields exist precisely so a client can tell
"no such signal exists" (today, always `false`) apart from "a signal exists and reports
normal," which this feature must never claim on the hardware it is built and tested for.

## The `get_throttled` register

This feature reuses `readiness.ThrottleStatus` (see `readiness/vcgencmd.go`), the same
parsing of `vcgencmd get_throttled` the Readiness dashboard's basic "throttled" indicator
already uses, so there is exactly one implementation of this parsing in the codebase.

| Bit | Meaning | Exposed as |
|---|---|---|
| 0 | Under-voltage detected right now | `undervoltageNow` |
| 2 | Currently throttled | `throttledNow` |
| 16 | Under-voltage has occurred since boot | `undervoltageOccurred` |
| 18 | Throttling has occurred since boot | `throttledOccurred` |

`power.Evaluate` classifies one reading into a `Severity` (`ok`/`warning`/`critical`):
current under-voltage is `critical`; current throttling, or either condition having
occurred earlier this boot, is `warning`; otherwise `ok`. `power.Monitor` debounces this
against `RequiredConsecutive` consecutive samples (main/ uses 3, one per
`healthUpdateInterval` health tick) before its reported reading actually changes, so a
single noisy sample cannot flip the dashboard back and forth.

## The controlled-shutdown flow

`power.Manager` (`power/shutdown.go`) implements the state machine:

```
IDLE -> CONFIRMATION_REQUIRED -> SHUTDOWN_REQUESTED -> FLUSHING -> READY_TO_POWER_OFF -> COMMAND_ISSUED
                                                                                       -> FAILED (from any of the three middle stages)
```

- **`POST /requestShutdown`** (step one) runs every precondition (see below) and, if they
  all pass, issues a short-lived (120s), single-use, boot-session-bound confirmation token
  and moves to `CONFIRMATION_REQUIRED`. It never mutates any system state.
- **`POST /confirmShutdown`** (step two), given that exact token back, re-checks
  preconditions (state may have changed since the preview - e.g. an OTA update could have
  started), then runs the flush step (`gracefulShutdown()` - the same function
  `main/gen_gdl90.go`'s `signalWatcher` already calls on `SIGTERM`, so an active recording is
  stopped and finalized exactly the way a normal daemon stop already handles it), then
  `syscall.Sync()`. Only after the HTTP response describing success has been written and
  flushed to the client does the handler call `IssuePowerOff()`, which runs
  `systemctl poweroff` - see `power.Manager.Confirm`'s doc comment for exactly why this
  ordering is enforced by the API surface (a separate method call) rather than left as a
  comment.

**Preconditions** (checked at both steps): an OTA update in progress
(`ota.LoadState(otaDir)`, the same check `main/configbackupapi.go`'s restore-apply path
already uses) or a configuration restore in progress (`configBackupState`) each block the
request with `409 Conflict` and mutate nothing.

**Token binding**: a confirmation token is invalid if reused, expired, or presented to a
daemon process other than the one that issued it (a restart invalidates every outstanding
token, even an unexpired one) - the same shape as `configbackup.ConfirmationToken`, applied
here to a simpler binding (no uploaded-document checksum, since a shutdown request carries
no content of its own).

**Concurrency**: `power.Manager` holds one internal mutex; a token can only ever be consumed
once even under concurrent `confirmShutdown` calls (`power.TestManager_ConcurrentConfirmOnlyOneWins`
exercises this directly with 20 concurrent goroutines).

A parallel `Manager` instance (`rebootManager`) drives an identical flow for a controlled
restart, differing only in the UI's confirmation count (one explicit confirmation instead of
shutdown's two) and the final command (`IssueReboot` instead of `IssuePowerOff`) - see
"Restart" below.

The pre-existing, single-call `POST /shutdown` and `POST /reboot` predate this feature and are
now fully retired (return `410 Gone`, perform no action) rather than merely precondition-gated -
see "Legacy endpoint retirement" below for the full trace of why an intermediate, gated-but-
unconfirmed version of those two endpoints was itself still a genuine confirmation bypass.

## Legacy endpoint retirement

An earlier revision of this feature left `POST /shutdown` and `POST /reboot`
(`handleShutdownRequest`/`handleRebootRequest` in `main/managementinterface.go`) reachable "for
compatibility," gated only by the same `otaNotBusyPrecondition`/`configBackupNotBusyPrecondition`
checks the confirmed flows use. A review correctly identified that this was still a genuine
confirmation bypass: neither an OTA-busy check nor a config-restore-busy check is a substitute
for a server-issued, single-use confirmation token. A single POST to either endpoint - forged,
replayed, or sent by a stale browser tab that still had the old page loaded - would shut the
device down or reboot it immediately, with no record that an operator had actually confirmed
anything. Gating an action on unrelated system state is not the same thing as confirming that a
human meant to trigger it.

**The trace, exhaustively, by direct code inspection (not assumed):**

- `POST /shutdown`: **zero** remaining callers anywhere in this repository. Its one-time UI
  caller (Settings' standalone Shutdown button, and the `postShutdown()` function behind it) was
  already removed by the earlier Web UI consolidation. `URL_SHUTDOWN` (`web/js/main.js`) had
  become an unused constant.
- `POST /reboot`: **exactly one** remaining caller - the Settings page's "a setting you just
  changed requires a reboot" prompt (`modalRebootRequired` in `web/plates/settings.html`,
  formerly calling `postReboot()` in `web/plates/js/settings.js`). That caller had no
  server-issued confirmation of its own: a client-side modal is UI state, not server state: it
  proves nothing to the server about whether a human actually clicked it, versus a request
  replayed or forged after the fact.
- The OTA install state machine (`main/ota.go`) calls `delayReboot()`/`doReboot()` directly, in
  two places (after requesting overlay-disable during a normal install, and during automatic
  rollback recovery) - **never through the HTTP `/reboot` endpoint**. This is a legitimate,
  narrowly-scoped, already-authorized internal caller: the reboot it performs is one step of an
  update the operator already explicitly uploaded and the device already hash-verified (or that
  same update's own automatic rollback), not a second, separate action needing its own
  confirmation. It was never reachable through the browser-facing endpoint in the first place,
  and nothing about this retirement changes it.
- No script, cron job, systemd unit, or other non-interactive integration anywhere in this
  repository calls either HTTP endpoint (`debian/stratux-pre-start.sh`, the one place a reboot is
  triggered outside the Go binary, runs the bare shell `reboot` command directly - never HTTP).

**Disposition:**

- `handleShutdownRequest` and `handleRebootRequest` now perform **no action at all**, under any
  precondition state, for any HTTP method: both unconditionally return `410 Gone` with a JSON
  body pointing to the confirmed flow (`POST /requestShutdown`+`/confirmShutdown`, or
  `POST /requestReboot`+`/confirmReboot`). They are kept registered (not deleted) so an old
  client gets an explicit, actionable explanation instead of an ambiguous bare 404.
  `main/managementinterface_test.go`'s `TestHandle{Shutdown,Reboot}Request_NeverActs_
  RegardlessOfPreconditionState` prove this response is identical whether OTA/config-restore is
  busy or idle - there is no remaining conditional path in either handler that could ever reach a
  real `systemctl` command.
- The Settings "reboot required" prompt now goes through the confirmed flow directly:
  `SettingsCtrl.confirmRequiredReboot()` (`web/plates/js/settings.js`) calls
  `POST /requestReboot`, then `POST /confirmReboot` with the returned token, as a direct result of
  the modal's own "Reboot" click - the same server-issued, short-lived, single-use,
  boot-session-bound token the Power page's Restart action uses, not a second, separate
  confirmation step. The modal now shows a busy state and surfaces a blocked precondition as an
  error instead of silently doing nothing (its previous behavior on a rejected request).
  `doReboot()`/`delayReboot()` are untouched and keep serving the OTA state machine exactly as
  before.

## The previous-session marker

`power.SessionMarker` (`power/session.go`) is a small JSON file
(`/var/lib/stratux-data/power-session.json`) main/ writes atomically (temp file + rename,
matching `readiness.WriteDiagnosticBundle`'s established pattern):

- At startup (`initPower`), main/ reads whatever marker the *previous* boot left behind,
  evaluates it, and only then overwrites it with a fresh
  `{sessionId: <this boot>, closedCleanly: false}` record for the current boot.
- Immediately before `doReboot()` issues `systemctl reboot`, and immediately after the
  confirmed-shutdown flow reaches `COMMAND_ISSUED` (before `IssuePowerOff` runs), main/
  rewrites the marker with `closedCleanly: true` for the current session.

`sessionId` is Linux's own per-boot random id
(`/proc/sys/kernel/random/boot_id`) when it can be read (stable across a daemon restart
within the same boot, so a crashed-and-restarted `stratux` process is never confused with an
actual power cycle), falling back to the daemon's own per-process random session id
(`preflightSessionID`) wherever that file cannot be read (a non-Linux dev build, or a
container/chroot without `/proc` mounted).

### Why the wording is deliberately hedged

`power.EvaluatePreviousSession`'s conclusion never claims "power loss," "crash," or any
other specific diagnosis. The only fact this mechanism can actually establish is narrower:
whether main/'s own shutdown/reboot code path ran and recorded `closedCleanly` before this
boot started. A `false` result can also mean a bug, a `kill -9`, a watchdog reset, or simply
that this feature was added after the previous boot already started - conflating any of
those with "the battery died" would be a claim this feature has no way to verify, so it does
not make one. The exact displayed text (and the test that enforces it,
`power.TestEvaluatePreviousSession_NotClosedCleanly`) is:

> the previous session did not record a clean shutdown or reboot before this boot - this can
> happen after a power interruption, a hard reset, a crash, or simply because this record
> only started being kept in a newer version; it is not a confirmed diagnosis of power loss

## Dashboard

The Power page (`web/plates/power.html` / `web/plates/js/power.js`, linked from the sidebar)
is the single supported place in the Web UI to restart or shut down the device. It shows the
current power-health reading (with the same capability-honesty notes as the API), the
previous-session assessment, and two action panels:

- **Restart** - one explicit confirmation: a "Restart" button that immediately requests a
  token (surfacing a blocked precondition, if any, before showing anything else to confirm),
  then a warning panel with a "Confirm Restart" button and a "Cancel" button.
- **Shutdown** - the original two-step flow: a "Prepare Shutdown" button, then an explicit "I
  understand this will power off the device now" checkbox that must be checked before "Confirm
  Shutdown" becomes clickable, plus a "Cancel" button.

Both "Cancel" actions simply abandon an issued-but-unconfirmed token (nothing to undo
server-side, since the request step never mutates anything). Restart deliberately keeps a
lighter confirmation than Shutdown - a reboot is self-recovering and far less consequential
than a poweroff - while still running through the identical server-side precondition/flush
machinery. The Settings page's former standalone Reboot/Shutdown buttons were removed in favor
of this page; Settings now links here instead.

## API reference

| Method | Path | Purpose |
|---|---|---|
| GET | `/getPowerHealth` | Debounced power-health reading + previous-session assessment |
| GET | `/getShutdownStatus` | Current controlled-shutdown stage |
| POST | `/requestShutdown` | Step 1: preconditions + issue a confirmation token |
| POST | `/confirmShutdown` | Step 2: consume the token, flush, sync, power off |
| GET | `/getRebootStatus` | Current controlled-reboot stage |
| POST | `/requestReboot` | Preconditions + issue a confirmation token |
| POST | `/confirmReboot` | Consume the token, flush, sync, reboot |
| POST | `/shutdown` | **Retired** - always `410 Gone`, no action, no remaining caller (see "Legacy endpoint retirement" above) |
| POST | `/reboot` | **Retired** - always `410 Gone`, no action, no remaining caller; the Settings "reboot required" prompt now uses `/requestReboot`+`/confirmReboot` directly |

`/getPowerHealth` never reports a battery percentage, a runtime estimate, or anything about
an outstanding shutdown/reboot token. `/confirmShutdown`/`/confirmReboot` and the diagnostics/
recording summaries below likewise never include a token value itself once issued.

## Readiness / Preflight / Diagnostics / Recording integration

- **Readiness**: unchanged - the existing `System` → `Throttled`/`UndervoltageDetected`
  booleans (`readiness.BuildSystemHealth`) are left exactly as they were before this
  feature; this feature's richer view is a separate, additive API rather than a change to
  that already-tested surface.
- **Preflight**: a new, always-non-blocking "Power" → "Previous session" check
  (`preflight.powerSessionChecks`) reports `NOT_APPLICABLE` when no previous-session record
  exists yet, `READY` when it recorded a clean close, and `CAUTION` (never `NOT_READY`) with
  the same hedged note otherwise - an ambiguous signal must never fail preflight on its own.
- **Diagnostics**: `readiness.DiagnosticBundle.PowerSummary` (opaque `interface{}`, following
  the same pattern as `AlertingSummary`/`ConfigBackupSummary`) carries severity, the four
  throttle booleans, the previous-session assessment, and the current shutdown stage - never
  an outstanding token.
- **Recording metadata**: `recording.SessionSnapshot`'s new `Power*`/`PreviousSession*`
  fields (schema version bumped 3 → 4, purely additive) capture this same small summary once
  at recording start.

## Tests

Every test in `power/*_test.go` and `main/powerapi_test.go` uses a fake `power.Executor` -
none of them ever calls `syscall.Sync()` or `systemctl poweroff`/`reboot`. Coverage includes:
bit-parsing edge cases (reused from `readiness/vcgencmd_test.go`), the debounce policy
(requires-N-consecutive-samples, streak resets on disagreement, recovers after N good
samples), the full state-machine lifecycle for both actions (success; missing/wrong/reused/
expired/wrong-boot-session token; precondition failure at request time and again at confirm
time; flush failure; sync failure; concurrent confirm attempts; the shutdown and reboot
Managers' tokens never cross-accept each other's), the session-marker's atomic read/write and
conservative-wording assertions, and the HTTP layer (every endpoint's method guard,
malformed/missing-field bodies, and the end-to-end request → confirm → marked-clean flow).

The legacy `/shutdown` and `/reboot` endpoints are retired - they no longer call
`exec.Command` at all, under any state. `main/managementinterface_test.go`'s
`TestHandle{Shutdown,Reboot}Request_NeverActs_RegardlessOfPreconditionState` prove the `410`
response is identical whether a real OTA-busy state (written via `ota.SaveState`) or a real
configuration-restore-busy state is active versus idle, and
`TestHandleShutdownAndRebootRequest_AnyMethodNeverActs` proves it for every HTTP method - there
is no remaining conditional path in either handler that could ever reach a real `systemctl`
command, so this is a full, not merely a blocked-path, proof for these two routes. The confirmed
flows' own success paths (both shutdown and reboot) are exercised end-to-end against a fake
`power.Executor` in `main/powerapi_test.go` - real hardware commands are never invoked by any
automated test; that remains a manual, physical-device step (see the checklist below).

## Hardware-validation checklist

Partially executed on the grounded test Stratux (192.168.10.1) on 2026-09-28, at commit
`60a0bb7e` - see `/home/ddavis/acceptance-evidence/stratux-power-consolidation-pr42-20260928/`
for the full evidence and gate-by-gate table. A real controlled *shutdown* was deliberately
**not** attempted (it requires the owner physically present to restore power afterward, which
was not confirmed that session), so items 4-6 remain open, and a few precondition-busy checks
that would require manufacturing risky on-disk OTA state on a shared device were also left open
(items 7, 10, and the busy-case half of 12). This is a starting point for the still-outstanding
owner-authorized hardware-validation work, not a substitute for it.

1. **DONE (2026-09-28).** Deployed via the established OTA state machine; new boot ID,
   installed commit, and `/getPowerHealth`/`/getShutdownStatus` all confirmed.
2. **DONE (2026-09-28).** `throttled=0x0` and `/getPowerHealth` severity `ok` confirmed with
   the device powered normally.
3. **Open.** Requires a known-weak USB power source/cable; not attempted.
4. **Open.** No real controlled shutdown was attempted this session (see above).
5. **Open** (for the shutdown path specifically - not attempted). The equivalent check for a
   controlled *restart* (`previousSessionEndedCleanly: true` on the next boot) **is DONE**,
   confirmed twice.
6. **Open.** A physical power yank is out of scope for a routine acceptance session; not
   attempted.
7. **Open.** Requires an OTA update genuinely in progress at the moment of `/confirmShutdown`;
   not forced this session (see reasoning above).
8. **Open.** Not attempted this session.
9. **DONE (2026-09-28).** A real controlled restart was performed through the Power page's
   single-confirmation Restart flow while a recording (`rec-20260928T153758Z`) was active;
   its metadata shows `complete: true`, the device's boot ID changed (genuine reboot, not just
   an HTTP 200), and it rejoined the Wi-Fi AP/Web UI on its own afterward.
10. **Open.** Requires an OTA update genuinely in progress at the moment of `/confirmReboot`;
    not forced this session (see reasoning above).
11. **DONE (2026-09-28), except the mid-OTA sub-case.** The Settings page was confirmed to
    show no standalone Reboot/Shutdown buttons and a working "Power page" link. Toggling
    Persistent Logging showed the "reboot required" prompt; clicking its "Reboot" button
    physically rebooted the device via the confirmed flow (`confirmRequiredReboot()` →
    `POST /requestReboot` → `POST /confirmReboot`) - done twice, once to apply the setting and
    once to restore it to its original value, each a genuine reboot (new boot ID, setting
    change verified via `/getSettings`). The prompt's blocked-precondition-error-while-mid-OTA
    sub-case was **not** forced live this session; the equivalent path against a synthetic
    unreachable backend was previously verified via headless Chrome (see PR #42's description).
12. **DONE (2026-09-28) for the idle case; open for the busy case.** With the device idle, a
    direct `POST /shutdown` and a direct `POST /reboot` both returned `410 Gone` and the device
    neither powered off nor rebooted (boot ID unchanged) - the physical-device counterpart to
    `main/managementinterface_test.go`'s `TestHandle{Shutdown,Reboot}Request_NeverActs_
    RegardlessOfPreconditionState` for the idle branch. This confirms the confirmation-bypass a
    review correctly identified in this feature's own earlier draft is closed for the case that
    matters most in practice (an idle device). The busy-case (mid-OTA/mid-restore) was not
    forced live this session; it remains proven only by the automated test above.
