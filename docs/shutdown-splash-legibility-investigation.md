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
observed in `epaper_main`, unrelated to the shutdown splash:

```
--- FAIL: TestDashboardGoldenImages/06-fisb-stale
--- FAIL: TestDashboardGoldenImages/12-worst-case-text
epaper: recovered panic in dashboard refresh: runtime error: invalid memory address or nil pointer dereference
```

This is a genuine nil-pointer panic in the **dashboard** renderer (a different code path
from the shutdown-text/ARS-splash renderer this issue is about), caught by its own
`recover()` wrapper (so it cannot crash the service), for two specific fixture scenarios
("fisb-stale" and "worst-case-text"). This predates this investigation (zero code changes
were made in this worktree before running the tests) and is out of scope for the
shutdown-splash issue. Recorded here so it is not lost; a separate issue should track it if
the owner wants it investigated.

## Recommended next step (not performed here)

The only way to further narrow this is a fresh, owner-attended, closely observed real
shutdown - ideally recorded (slow-motion video, or the owner watching frame-by-frame rather
than glancing) - specifically to determine whether each of the three refresh stages is
individually clean (supporting the "normal multi-stage flash, misperceived" hypothesis) or
whether any single stage's *settled* end-state (not mid-flash) is genuinely garbled
(which would point to a real hardware or timing defect requiring further investigation).
This has not been performed as part of this investigation. Physical validation of this
issue therefore remains **NOT RUN**.
