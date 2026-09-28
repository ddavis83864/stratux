# Shutdown splash legibility investigation (issue #43)

## What was reported

During PR #42's owner-attended physical shutdown acceptance (2026-09-28, grounded test
Stratux, 192.168.10.1), the owner directly observed the shutdown e-paper splash rendering
garbled and illegible ("genuinely garbled, not legible as any message") - while the shutdown
*mechanism* itself worked correctly (device halted, owner-confirmed). See issue #43 and
`/home/ddavis/acceptance-evidence/stratux-power-consolidation-pr42-20260928/` for the full
evidence.

## Scope of this investigation

Code-only, from a fresh worktree/branch (`fix/shutdown-splash-legibility`) off the
newly-merged `master` (PR #42's merge commit `aac2f0cca990c2e1d1a706faad8544a6bb686a32`),
separate from the power-consolidation branch and from any FIS-B work. This document does
not claim the splash is fixed, because no defect has been corrected yet - see "Status" below
for what has now actually been confirmed.

## Status (updated after five owner-attended, video/trace-instrumented physical tests, two fix attempts)

**UNRESOLVED.** A real defect is confirmed (the shutdown text screen never appears legibly),
and two fix attempts have both been physically tested and both shown, via three independent,
convergent, quantitative frame-difference analyses, **not to work**. See "Two fix attempts,
both confirmed not working" below for the full account. No further physical testing has been
performed as of this update; do not merge or deploy any of this branch's fix code as a
working solution. Issue #43 stays open.

## Two fix attempts, both confirmed not working (2026-09-28, later in the day)

**Attempt 1** (commit `aed71566`): a 3-second pause between the operational renderer's text
draw and the shutdown-splash unit's own `Clear()`, cancellable by context
(`select { case <-time.After(pause): case <-ctx.Done(): }`) on the theory that a bounded,
cosmetic wait should never be why systemd has to SIGKILL this unit. Physically tested with an
owner-recorded, video-timestamped real shutdown (`IMG_4749.MOV`). Quantitative
frame-difference analysis (mean absolute pixel difference per 0.125s frame, not eyeballed)
showed the panel changing continuously for ~3.1s total with only a ~0.35-0.4s static gap in
the middle - indistinguishable from no pause at all. Hypothesis: systemd's own very-late
shutdown signal sweep reached this process during the wait and fired `ctx.Done()` almost
immediately.

**Attempt 2** (commit `c5d83e25`): the pause made unconditional (`time.Sleep(pause)`, not
cancelled by `ctx.Done()`) - directly testing the attempt-1 hypothesis. Physically tested
**twice**:
- Video-recorded real shutdown (`IMG_4755.MOV`): the exact same pattern - ~3.25s total flash,
  ~0.25-0.375s gap.
- A combined trace+pause instrumented build (temporary, never committed - added a
  synchronous, fsync'd stage logger to `main.go`/`splash.go`/`splashshutdown.go`, removed
  again afterward), tested with a real shutdown that was simultaneously video-recorded
  (`IMG_4759.MOV`) and traced. The trace confirms the operational renderer's own draw still
  succeeds cleanly (`driver.Update()` returns no error, ~1.8s, matching the validated
  non-shutdown baseline exactly) - but the trace from the *separate* shutdown-splash process
  cuts off immediately after it logs its own entry, before logging `classifyJobs`,
  `beforePause`, or anything else. Working theory (unconfirmed): `/var/lib/stratux-data`
  (where the trace file lives) may become unwritable very late in the real shutdown sequence,
  silently dropping subsequent writes - independent of whether the pause itself ran. The
  video from this same test shows the identical ~3.1s/~0.3s pattern a **third** time.

**Three independent, quantitative video analyses, across both fix attempts, converge on the
same negative result every time.** The visible transition is consistently too short (~3.1s
total) to contain both a working 3-second pause and the two remaining full refreshes
(~3.5s combined) - regardless of what the (separately inconclusive) trace shows. This is
strong enough, convergent evidence to conclude neither fix attempt visibly works, without
needing to fully resolve why.

Recommended next steps (none performed):
1. Relocate diagnostic trace output to a filesystem confirmed to stay mounted through the
   entire shutdown sequence (e.g. `/boot/firmware`, which this unit's own ordering comments
   already establish stays mounted through this unit's stop) instead of
   `/var/lib/stratux-data`, for a trace that does not cut off early.
2. Seriously consider that the pause code may not be reached at all in the real
   `-splash-shutdown` `ExecStop` execution path, despite passing every unit test - a
   systemd/`ExecStop`-specific interaction that hardware-free unit tests cannot exercise.
3. Consider a same-process mechanism instead: have the *operational renderer itself* hold its
   already-successful draw for the pause duration before releasing the panel, rather than
   relying on a *separate* process (the shutdown-splash service) to wait before taking over -
   removing the cross-process handoff from the timing-critical path entirely.

The rest of this document (below) is the original investigation that established the defect
and the first "just normal flashing" hypothesis (since superseded) - kept for the full
record.

## Decisive video evidence (2026-09-28, owner-attended, recorded)

The owner recorded a real, confirmed shutdown continuously on video from before the final
confirmation through the panel settling (`IMG_4741.MOV`, 123.85s, sha256
`94c9aad594ce40872ac79f61e4616833ce76ff1bcda08b3541918723012d5a57`, preserved at
`/home/ddavis/acceptance-evidence/issue-43-shutdown-splash-video-20260928/`). Extracting
frames at 2s, 0.25s, and finally 0.125s (8 fps, the video's practical limit near its native
~24 fps) resolution across the entire transition shows, consistently at every resolution:

1. The live operational dashboard persists unchanged, exactly as it was showing before
   shutdown was confirmed.
2. Directly from that dashboard frame, a single continuous LUT-driven full-refresh flash
   sequence begins (~2 seconds of flashing/inverting frames, including a transient
   inverted/ghost glimpse of the ARS logo partway through - normal for this controller).
3. The flash settles directly into the clean ARS splash bitmap, which then remains
   unchanged for the rest of the recording.

**At no point, at any sampling resolution, does a frame showing "Stratux is shut down." or
"Safe to remove power." appear.** The owner's own initial verbal recollection ("stage 2 was
clean and legible") was a good-faith but mistaken recollection under the speed of watching
this live - the video is the higher-fidelity record here, and it shows the text screen
simply never being drawn, not being drawn-and-then-hard-to-read.

This also better explains the *original* garbled-splash complaint than the earlier "just
normal flashing" hypothesis: with the intended intermediate "Safe to remove power" resting
frame entirely absent, a live viewer sees the dashboard jump straight into a single
~2-second flash-then-splash transition with no clean pause to read anything in between -
plausibly reading as "garbled" or "mixed with the existing dashboard" in real time, exactly
as originally reported.

**Independently reproduced.** The owner also recorded the second, journal-instrumented
shutdown below (`IMG_4743.MOV`, 143.12s, sha256
`db29052e873b6d9029face4a154d0e4eb7d74352c92532c404795504ca7f83d2`, same evidence
directory). The same frame-by-frame analysis (2s, then 0.125s resolution across the
transition) shows the identical pattern: dashboard persists unchanged, then one continuous
flash sequence directly into the settled ARS splash - no shutdown-text frame at any
resolution, this time either. **This is now confirmed reproducible across two independent
real shutdowns, not a one-off.** That reproducibility argues somewhat against the
nil-driver race-window hypothesis below (which would be expected to be intermittent,
dependent on unlucky settings-poll timing) and somewhat for the blocking-loop-timing
hypothesis or an as-yet-unidentified deterministic cause - but this is a weak inference from
absence of intermittency, not confirmation; the two hypotheses are still both open.

## Journal capture attempt (inconclusive)

To determine *why* the text screen is skipped - whether `stratux_epaper.service`'s
`shutdown()` closure never runs, runs but hits an error, or is killed before finishing -
persistent journal logging was set up before a second real shutdown, using the same
technique validated in an earlier, unrelated e-paper acceptance session: a `Storage=persistent`
drop-in under `/run/systemd/journald.conf.d/`, `/var/lib/stratux-data/acceptance/journal`
bind-mounted onto `/var/log/journal`, then `systemctl restart systemd-journald`. Confirmed
active immediately afterward (`journalctl --list-boots` correctly showed the live boot).

After the shutdown and the owner's power restore, `journalctl --directory=/var/lib/stratux-data/acceptance/journal
--list-boots` shows **no entries at all from that boot** - only stale data from an earlier,
unrelated session (2026-09-19). The persistent-storage migration was active but evidently
never flushed to the real (ext4-backed) disk before power was physically cut - this device's
poweroff is fast enough, and/or journald's own flush timing late enough in the shutdown
transaction, that this capture technique did not work for the operational renderer's own
shutdown-time behavior this time. (`stratux_epaper.service` also sets `StandardOutput=null`,
so even a successful capture would only have shown systemd's own PID-1-level unit start/stop
timing for it, not `epaperd`'s own log lines - a real limit of this technique for this
specific unit, worth noting for any future attempt.) Not repeated a third time this session,
per "do not repeatedly cycle the device without a reason" - two real shutdowns is enough
disruption for one session without a clearer plan for a third.

## What actually draws during a real shutdown

Two entirely separate processes, in this order, each independently opening and releasing
the panel:

1. **The operational renderer** (`epaperd`, `stratux_epaper.service`), on SIGTERM: draws
   `epaper.ShutdownLines()` ("Stratux is shut down." / "Safe to remove power.") plus
   `epaper.DisclaimerLine`, as a **full refresh**, then calls `Sleep()` and releases
   SPI/GPIO (`epaper_main/main.go`'s `shutdown()` closure).
2. **The shutdown-splash unit** (`stratux_epaper_shutdown.service`'s `ExecStop`, running
   `epaperd -splash-shutdown`): asks systemd's job queue whether a `poweroff.target`/
   `halt.target` job is queued (`classifyJobs` in `epaper_main/splashshutdown.go`); if so,
   it runs the *same* boot-splash renderer (`runSplash`) - Init, Clear (its own full
   refresh), draw the ARS bitmap (another full refresh), Sleep, release.

So a real orderly shutdown involves **three full refreshes in sequence**: whatever the
dashboard was last showing, then the shutdown text screen, then a clear, then the ARS
splash bitmap. This is a pre-existing, already-documented, previously-accepted
characteristic - not something PR #42 introduced (PR #42 touches none of this code; see
below).

## Sequencing: verified correct, not a race

The two units' ordering is deliberately, and already, hardware-validated
(`debian/stratux_epaper_shutdown.service`'s own extensive comments cite a specific,
previously-measured race on real hardware - "a power-off requested while the boot splash
was still drawing overlapped its panel ownership" - and its fix, a second `Before=` line):

- `stratux_epaper_shutdown.service` is `Before=` both `stratux_epaper.service` and
  `stratux_epaper_splash.service`. Because systemd stops units in the *reverse* of their
  start order, this unit's `ExecStop` (the splash-shutdown draw) only runs **after** both
  of the others have fully stopped and released the panel - not concurrently with either.
- Both are `Type=oneshot`, so systemd does not consider a unit's own start/stop "done"
  until its process has actually exited - there is no polling or sleep-based coordination
  to race.

Nothing in this ordering changed in PR #42, and PR #42 does not touch any file under
`epaper/` or `epaper_main/`, `debian/stratux_epaper*.service`, or `debian/postinst*`.

## Does PR #42's own code affect this timing at all?

No plausible mechanism was found. Before PR #42, `handleShutdownRequest` (the legacy
`/shutdown` endpoint) called `exec.Command("systemctl", "poweroff").Run()` directly. After
PR #42, the confirmed flow's `realPowerExecutor.PowerOff()` calls the exact same
`exec.Command("systemctl", "poweroff").Run()`. The OS-level shutdown transaction systemd
runs - and therefore every unit's stop ordering, including both e-paper units - is
identical either way; PR #42 only changed *which Go code issues* that already-identical
command, and added the equivalent, identically-implemented `Reboot()` path. There is no
timing difference for systemd to observe.

## Is the rendered bitmap itself ever "mixed" with old content?

No, by construction. `epaper_main/render.go`'s `Render()` - the function used for
`ShutdownLines()` (and every other text screen) - starts every call by filling the entire
logical canvas white (`draw.Draw(img, img.Bounds(), image.White, ...)`) before drawing any
text. There is no code path where a previous frame's pixels leak into a newly rendered
bitmap; existing tests (`TestRender_NeverDrawsPastPanelBounds`,
`TestRender_NeverPanicsOnRealOverviewPageContentForBothPanels`) already cover this render
path. Both the shutdown-text screen and the ARS splash bitmap are always freshly generated,
correctly sized (`TestRender_OutputSizeMatchesStrideForBothPanels` covers both panels),
never truncated (`DisclaimerLine`, 47 characters, comfortably fits the 400px-wide 4.2in V2
panel's line width at `basicfont.Face7x13`'s 7px/char).

If "mixed with the existing dashboard" was genuinely observed on the physical panel, the
cause is therefore not a software-generated bitmap defect - it would have to be either a
physical/electrical effect during the refresh itself, or a perceptual one (see below).

## Superseded: the earlier "just normal flashing" hypothesis

This section is kept for the record but is **no longer the leading explanation** - see
"Decisive video evidence" above. It previously read: E-paper full refreshes on this
controller are inherently a multi-stage, visibly-flashing process; three of these happen
back-to-back in a real shutdown, a sequence already known and previously accepted by the
owner; the most evidence-supported explanation was that this known multi-stage transition,
individually correct at each stage, was being perceived as "garbled" in real time. Frame-by-
frame video review now shows this was wrong in one specific, important way: it is not three
full refreshes with each settled frame legible - the *middle* stage (the shutdown text
screen) does not draw at all, leaving only two refreshes (dashboard -> flash -> splash) with
no legible intermediate frame. The "normal flashing" characteristic itself is real and still
not the defect; the *missing stage* is.

## Two hypotheses for the missing text screen (neither confirmed)

Both are grounded in the actual code (`epaper_main/main.go`'s `run()`/`shutdown()`), not
speculation, but neither has server-side log confirmation (see "Journal capture attempt"):

1. **The main loop doesn't reach `ctx.Done()` in time.** `run()`'s `select` only re-checks
   `ctx.Done()` at the top of each loop iteration; if a dashboard refresh cycle (network
   poll + render + panel write) is in progress when SIGTERM arrives, `shutdown()` isn't
   called until that iteration finishes. If systemd's stop budget for this unit (or the
   overall shutdown transaction) runs out first, `stratux_epaper.service` could be
   SIGKILLed - skipping `shutdown()` entirely - before an in-progress cycle finishes.
   `stratux_epaper.service`'s own unit file sets no explicit `TimeoutStopSec` (so it uses
   systemd's default, typically generous), which argues against this being the whole story,
   but the *overall* shutdown transaction's own budget wasn't checked here.
2. **`driver` is nil at the exact moment SIGTERM arrives.** `shutdown()`'s closure only
   draws `if driver != nil`; a settings-triggered re-initialization window
   (`driver.Sleep(); closeBusFn(); driver, bus = nil, nil`) briefly nils it out. If SIGTERM
   lands in that window, `shutdown()` silently does nothing visible - consistent with what
   the video shows (the dashboard's last complete frame simply persists, since e-paper
   retains its image with no further writes, right up until the *separate*
   `stratux_epaper_shutdown.service` process starts its own, independent `Init()`+`Clear()`).
   No settings change was made during either test this session, so this specific trigger for
   the nil window is not confirmed to have actually happened - only that the code path exists
   and would produce exactly this symptom if it did.

Distinguishing these (or finding a third cause) needs either a successful journal capture of
a future real shutdown, or temporary, reverted diagnostic logging added to `shutdown()`
itself for one instrumented test run - neither has been done.

## What was NOT found

No code fix has been implemented. A real, confirmed defect exists (the missing text screen),
but its precise internal cause is not yet established, and per the task's own instruction,
**no speculative fix is being made** for an unconfirmed root cause - guessing between the
two hypotheses above and "fixing" the wrong one would risk regressing already
hardware-validated code (the boot splash, which shares the same `runSplash`/`Render`
machinery and rendered cleanly in every test this session, on the very device this issue was
reported on) without actually closing the gap.

## Incidental, unrelated finding (not in scope, not fixed here)

While running the epaper package tests as baseline due diligence (`go test ./epaper/...
./epaper_main/...`, zero code changes in this worktree), two pre-existing failures were
observed in `epaper_main`:

```
--- FAIL: TestDashboardGoldenImages/06-fisb-stale
--- FAIL: TestDashboardGoldenImages/12-worst-case-text
epaper: recovered panic in dashboard refresh: runtime error: invalid memory address or nil pointer dereference
```

**Correction (this section originally mischaracterized this finding):** the panic log line
above is *not* the cause of these two failures. It comes from two separate, deliberate,
passing tests (`TestRefreshPanicIsContained`, `TestRefreshOnce_PanicIsContained`) that
exist specifically to verify the panic-recovery wrapper works - they happened to log near
the golden-image failures in non-verbose output, and an earlier pass through this doc wrongly
correlated the two. Re-running `-v` makes the actual source of each line unambiguous.

The two golden-image failures have their own, separate, now-understood root cause: commit
`1f19d281` regenerated goldens for 3 of the 5 fixtures its own fault-icon-styling change
affected, missing `06-fisb-stale` and `12-worst-case-text`. Confirmed via pixel diff (1-2
pixels out of 120,000, consistent with the known icon-outline change) and visual inspection
(both renders are clean and legible). Fixed separately, out of scope for this issue: see
issue #45 and PR #46 (`fix/dashboard-golden-images`, its own branch/worktree, deliberately
kept apart from this shutdown-splash work).

## Stage map for physical re-observation

For a decisive video-recorded test, this is the exact expected sequence of what should
appear on the panel from the final shutdown confirmation to the image no longer changing,
and which process/unit owns each stage:

| # | Stage | Owning process / unit | What should be visible | Trigger |
|---|---|---|---|---|
| 1 | Dashboard's last live frame | `epaperd`, `stratux_epaper.service` (operational renderer) | Whatever the dashboard was already showing at the moment of confirmation - unchanged, no new draw yet | N/A - this is simply the panel's state before SIGTERM arrives |
| 2 | Shutdown text screen | Same process, its `shutdown()` closure, on SIGTERM | A **full refresh** (brief flash/clear cycle) settling to: "Stratux is shut down." / "Safe to remove power." / the aviation disclaimer line, on a plain white background, top-left-aligned text | `systemctl poweroff` reaching this service's SIGTERM |
| 3 | Process exit, panel released | Same process | No visual change during this step - `Sleep()` (deep sleep, retains RAM) then SPI/GPIO release; the settled stage-2 image should persist unchanged on the panel (e-paper retains its image with no power) | Immediately after stage 2's `Update()` call returns |
| 4 | `stratux_epaper_shutdown.service` starts, clears | `epaperd -splash-shutdown` (separate process, its own `Init()`+`Clear()`) | A **second full refresh** (another flash/clear cycle) to a blank white panel | systemd's `ExecStop` for this unit, guaranteed (by `Before=` ordering) to run only after stage 3 has fully finished |
| 5 | ARS splash drawn | Same process, `runSplash` | A **third full refresh** settling to the ARS boot-splash artwork (same bitmap/renderer as the boot splash) | Immediately follows stage 4's clear, same process |
| 6 | Final state | Same process | `Sleep()` + SPI/GPIO release; the settled stage-5 ARS splash image persists unchanged - this is the "image stops changing" end state to record | End of the `-splash-shutdown` run |

**This stage map describes the intended design** (used to plan the decisive video test
above). The actual video evidence shows stages 2 and 3 do not happen - the panel goes
directly from stage 1 to stage 4. The map remains useful as a reference for what *should*
happen once a fix is made, and for framing any future diagnostic logging.

## Recommended next steps (not performed here)

1. **Root-cause the missing draw.** Either a successful persistent-journal capture of a
   future real shutdown (the attempt this session did not persist any data - see above), or
   temporary, explicitly-reverted diagnostic logging inside `shutdown()` itself
   (e.g., logging whether `driver` was nil, and how long the preceding loop iteration took)
   for one instrumented owner-attended test, to distinguish the two hypotheses above (or
   find a third).
2. **Implement the smallest fix once the cause is known**, preserving the boot splash,
   dashboard, "Safe to remove power" *intent*, unit ordering, and reboot-skips-splash
   behavior exactly. Add a regression test for whatever the actual defect turns out to be
   (e.g., if it's the nil-driver race, a test that `shutdown()` still draws using the
   *previous* driver/config if a reinit was in flight; if it's the blocking-loop timing, a
   test bounding how long a single loop iteration can run before yielding to `ctx.Done()`).
3. **Physically re-verify with another video-recorded real shutdown**, checked frame-by-
   frame against the stage map, before considering issue #43 closed.

None of this has been performed. Issue #43 remains open, with a now-confirmed (not
hypothesized) defect. No fix PR has been opened yet.
