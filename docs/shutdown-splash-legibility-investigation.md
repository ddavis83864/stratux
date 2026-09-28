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
separate from the power-consolidation branch and from any FIS-B work. **No physical
reproduction was attempted** - a real shutdown test needs the owner beside the grounded
device to restore power, and that was not re-established for this investigation. This
document is the analysis and software-side due diligence; it does not claim the splash is
fixed, because no defect was confirmed, let alone corrected.

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

## Most likely explanation, given the evidence

E-paper full refreshes on this controller (SSD1677-family) are inherently a multi-stage,
visibly-flashing LUT-driven process (invert/clear cycles as part of ghosting mitigation),
not an instantaneous swap. Three of these happen back-to-back in a real shutdown (dashboard
-> shutdown text -> clear+splash), a sequence that was already known and previously accepted
by the owner as a UX characteristic (not treated as a defect) before this session. The most
evidence-supported explanation is that what was observed is this known multi-stage
transition - individually correct frames, each briefly visible mid-flash - being perceived
as "garbled" when watched in real time, rather than a data-corruption bug. This is a
hypothesis, not a confirmed root cause: it cannot be fully distinguished from a genuine
transient hardware/connection issue without a slow-motion recording or a careful,
frame-by-frame re-observation of a real shutdown, which requires the owner physically
present with the device - not available for this investigation.

## What was NOT found

No code or configuration defect was identified in the shutdown-splash rendering or
sequencing path. Per the task's own instruction, **no speculative fix is being made** for an
unconfirmed root cause - that would risk regressing already hardware-validated code (the
boot splash, which shares the same `runSplash`/`Render` machinery and rendered cleanly both
in Session 1 (journal) and Session 2 (direct owner observation), on the very device this
issue was reported on).

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

What would distinguish a real defect from the "normal multi-stage flash, misperceived"
hypothesis: **each stage's *settled* end-state** (2's text screen, 5's ARS splash) should
be individually sharp and legible once its own full-refresh flash finishes - stage 2's text
must read cleanly as "Stratux is shut down. / Safe to remove power." before stage 4 clears
it, and stage 5's ARS splash must be the clean, already-validated boot-splash artwork. A
genuine defect would show as one of those *settled* frames itself being garbled, not just
the transient flash *between* frames (which is a normal, expected part of every full
refresh on this controller, previously accepted by the owner). Recording continuously from
before the final confirmation through stage 6 lets each settled frame be checked frame-by-
frame afterward, rather than relying on what registers to the eye in real time.

## Recommended next step (not performed here)

The only way to further narrow this is a fresh, owner-attended, video-recorded real
shutdown, checked against the stage map above - specifically to determine whether each
settled frame (stage 2's text screen, stage 5's ARS splash) is individually clean, or
whether one of them is genuinely garbled at its own settled end-state, not just mid-flash.
This has not been performed as part of this investigation. Physical validation of this
issue therefore remains **NOT RUN**.
