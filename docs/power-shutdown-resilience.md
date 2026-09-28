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

**Update (Web UI power-control consolidation):** the pre-existing, single-call `POST
/shutdown` and `POST /reboot` (`handleShutdownRequest`/`handleRebootRequest` in
`main/managementinterface.go`) used to be completely unaffected by this feature - deliberately
left alone, on the theory that any existing client might depend on their exact original
behavior. That changed: the Settings page's own standalone Reboot/Shutdown buttons (the only
UI callers of those two endpoints for an operator-initiated action) have been removed in favor
of this page, and both endpoints now run through the *exact same* `otaNotBusyPrecondition`/
`configBackupNotBusyPrecondition`/`gracefulShutdown`/`markSessionClosed` functions this
confirmed flow uses - a stale browser tab, a script, or any other caller of the legacy
endpoints can no longer bypass the OTA/restore-busy check or skip the recording-flush/SDR-
shutdown sequence the confirmed flow already enforced. They remain reachable (not removed) for
compatibility - in particular, `POST /reboot` is still called directly by the Settings page's
own "a setting you just changed requires a reboot" prompt, which has no confirmation-token
flow of its own and does not need one (the setting has already been explicitly changed by the
operator). Neither legacy endpoint gained the two-step/token-based interactive confirmation -
they have no confirmation UI of their own to gate - only the same *safety* checks the confirmed
flow already had.

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
| POST | `/shutdown` | Legacy, single-call, no confirmation UI of its own - kept for compatibility, now precondition-checked and flushed identically to the confirmed flow (see "The controlled-shutdown flow" above) |
| POST | `/reboot` | Legacy, single-call - still used by the Settings page's "reboot required by a setting change" prompt; same precondition/flush treatment as `/shutdown` |

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

The legacy `/shutdown` and `/reboot` endpoints have no injectable executor (they call
`exec.Command` directly, unlike the confirmed flows), so `main/managementinterface_test.go`
can only exercise their new *blocked* path automatically (a real OTA-busy state written via
`ota.SaveState`, and a real configuration-restore-busy state) - proving the precondition gate
itself works without ever risking a real `systemctl` invocation from a test run. Their
unblocked/success path is not, and cannot safely be, covered by an automated test; that
remains a manual, physical-device step (see the checklist below).

## Hardware-validation checklist (prepared, not executed)

This feature has not been deployed to any device, and this checklist has not been run. It
is a starting point for the future, owner-authorized hardware-validation mission this
feature's own introducing PR explicitly reserves that work for - not a substitute for it.

1. Deploy via the established OTA state machine; confirm the new boot ID, installed commit,
   and that `/getPowerHealth`/`/getShutdownStatus` respond.
2. With the device powered normally, confirm `throttled=0x0` and the dashboard shows `ok`.
3. Using a known-weak USB power source or cable, induce a real under-voltage condition and
   confirm bit 0 sets, the dashboard reaches `critical` within `RequiredConsecutive` samples,
   and it clears back to `ok` after the condition is removed and stays cleared per this
   feature's "occurred" bits resetting only on the next boot (they do not self-clear - by
   design, matching the firmware's own semantics).
4. Perform a real controlled shutdown through the two-step dashboard UI while an active
   recording is running; confirm the recording's metadata shows `complete: true` and the
   device actually powers off (physical confirmation, not just the HTTP response).
5. Power the device back on and confirm `/getPowerHealth`'s previous-session fields show
   `previousSessionEndedCleanly: true` for that boot.
6. Deliberately cut power without using the shutdown flow (a real, physical yank - the
   scenario this whole feature exists to report on honestly); power back on and confirm
   `previousSessionEndedCleanly: false` with the conservative note text, and confirm the
   Preflight page shows the new "Previous session" check as `CAUTION`, never blocking.
7. Start `/requestShutdown`, then start an OTA update before confirming; confirm
   `/confirmShutdown` is rejected with `409` and nothing happens.
8. Confirm a confirmation token cannot be replayed after a daemon restart (kill and restart
   the `stratux` service between request and confirm).
9. Perform a real controlled restart through the Power page's single-confirmation Restart
   flow while an active recording is running; confirm the recording's metadata shows
   `complete: true`, the device actually reboots (physical confirmation), and it rejoins the
   Wi-Fi AP/Web UI on its own afterward.
10. Start `/requestReboot`, then start an OTA update before confirming; confirm
    `/confirmReboot` is rejected with `409` and nothing happens - the reboot-flow equivalent of
    check 7.
11. Confirm the Settings page no longer shows standalone Reboot/Shutdown buttons, that its
    "Power page" link works, and that changing a setting that requires a reboot (e.g.
    Persistent Logging) still shows its own "reboot required" prompt and reboots successfully
    via `postReboot()`/`POST /reboot`.
12. With a real OTA update or configuration restore in progress, confirm both legacy endpoints
    (`POST /shutdown`, `POST /reboot`) refuse the action (`409`) rather than proceeding - the
    physical-device counterpart to the automated blocked-path tests in
    `main/managementinterface_test.go`. This is the one behavior this feature's own earlier
    draft explicitly left unverified for `/shutdown` (see "The controlled-shutdown flow"
    above's "Update" note) - now that it is no longer left unaffected, it needs confirming.
13. With neither an OTA update nor a configuration restore in progress, confirm `POST
    /shutdown` and `POST /reboot` still work end-to-end (they are kept reachable for
    compatibility, not merely gated) - a real device power-off and a real device reboot,
    physically confirmed, each via a direct call to the endpoint rather than the Power page's
    UI.
