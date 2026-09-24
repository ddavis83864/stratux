# ARS e-paper shutdown splash

> **Status: physically validated on the real Stratux Pi** (owner-observed),
> package `stratux-2.0.0~rc2-arm64.deb` built from commit
> `616ed20ed9b95e9b2be92e1a2e00792b0abd8b89` and installed through the supported
> OTA mechanism. Poweroff draws the ARS splash, the image survives power removal,
> a reboot draws nothing, and the boot splash is unchanged; the shutdown/reboot
> journals prove the panel-ownership ordering. See the
> [acceptance record](#physical-acceptance-record). The acceptance was of the
> code as packaged; this documentation was updated afterwards.

On an **orderly power-off** (or halt) of the Raspberry Pi, the last image left
on the Waveshare **4.2" V2** e-paper panel is the approved ARS splash instead of
the operational status screen. E-paper is bistable, so the image stays on the
panel after the Pi has halted and power has been removed.

It is a separate feature from the validated [boot
splash](epaper-boot-splash.md) and reuses that feature's renderer, artwork,
timeouts and configuration gate unchanged. The approved artwork is frozen: the
shutdown path draws the **same embedded bitmap** (SHA-256
`51039f8a375a2ecc44ed25fe7b6f373b31e7695e7a854bd192d605695b5b73dc`,
400×300, 15,000 bytes) via the same `runSplash`; no separate shutdown artwork
exists.

## What the user sees

```
NORMAL BOOT (unchanged, validated)
  power applied -> ARS boot splash -> operational Stratux status display

ORDERLY POWER-OFF / HALT (new)
  operational status display
    -> stratux_epaper (operational renderer) stops, releases SPI/GPIO
    -> shutdown splash acquires the panel, draws the ARS splash (full refresh),
       panel sleeps, SPI/GPIO released
    -> Pi completes shutdown -> ARS image remains on the unpowered panel

NEXT POWER-ON
  retained ARS image -> boot splash (unchanged) -> operational display

REBOOT (deliberately no shutdown splash)
  operational display -> reboot -> boot splash -> operational display
```

Note the operational renderer's own, pre-existing stop behavior is unchanged: on
SIGTERM it first draws its fixed text screen ("Stratux is shut down. Safe to
remove power.") and puts the panel to sleep. On a power-off the shutdown splash
then replaces that text screen, so the panel shows, in order: status -> text
screen -> (blank flash from `Clear`) -> ARS splash. See
[Limitations](#limitations-and-risks).

## Architecture

| Item | Where |
|---|---|
| New unit | `debian/stratux_epaper_shutdown.service` |
| New renderer mode | `epaperd -splash-shutdown` (`epaper_main/splashshutdown.go`) |
| Reused, unchanged | `runSplash`/`renderSplash` (`splash.go`), the panel driver, the embedded bitmap, the boot splash's `decideBootSplash` config gate and `bootSplashTimeout` |
| Packaged into | `lib/systemd/system/` by the `Makefile` `dpkg` target |
| Enabled by | `debian/postinst.dpkg`: `systemctl enable stratux_epaper_shutdown`, inside the Pi guard, **before** the `STRATUX_OTA_INSTALL` early-exit |
| Armed (started) by | `debian/postinst.dpkg` on the **non-OTA** path only, before `stratux_epaper` is started; an OTA install is armed by its own reboot |
| Stopped by | `debian/prerm.dpkg`, after `stratux_epaper` |
| Validated units | `stratux_epaper.service` and `stratux_epaper_splash.service` are **byte-for-byte unchanged** |

The unit is `Type=oneshot`, `RemainAfterExit=yes`, with a no-op
`ExecStart=/bin/true` and the real work in
`ExecStop=/opt/stratux/bin/epaperd -splash-shutdown`. It is **active from boot
until shutdown**; systemd runs its `ExecStop` when the unit is stopped, which the
default dependencies (`Conflicts=shutdown.target`) do in every shutdown
transaction.

`-splash-shutdown` makes two decisions, in this order:

1. **Enabled?** The boot splash's exact decision (`decideBootSplash`):
   `EpaperEnabled` in `/boot/firmware/stratux.conf`, panel
   `waveshare-4.2in-v2`, rotation 0 or 180. Nothing is asked of systemd if this
   says no.
2. **Is this a power-off?** It asks systemd's job queue
   (`systemctl list-jobs`). It draws **only** if a `poweroff.target` or
   `halt.target` *start* job is queued and no `reboot.target` / `kexec.target`
   (or `soft-reboot.target`) start job is. Anything else - including any
   failure to find out - is a clean exit 0 that touches no hardware.

It then calls `runSplash` (Init -> Clear -> full refresh -> Sleep -> release),
with the ownership guard on.

## Why this systemd design is correct

### The two questions and the constraint that decides them

1. **How is exclusive panel ownership guaranteed** between the operational
   renderer, a possibly still-drawing boot splash, and the shutdown renderer?
2. **How is a poweroff told apart from a reboot** (and from an ordinary service
   stop)?

Both were answered by measurement on the target's systemd, not by reasoning
alone (Debian 12 Bookworm ships systemd **252**; measured with `debian:bookworm`
systemd 252.39 as PID 1 in a privileged container, journal read back from the
stopped container).

### Two designs were built and measured

**D1 - a start job pulled in by `poweroff.target`** (`WantedBy=poweroff.target
halt.target`, `DefaultDependencies=no`, `After=stratux_epaper.service`). This is
the obvious way to get "poweroff only" declaratively, because a reboot never
enqueues `poweroff.target`. It orders correctly against the operational
renderer, but **fails on filesystem availability**: systemd always orders a stop
job *before* the start job of any unit it has an ordering relation with
(`systemd.unit(5)`: "if one unit is shut down and the other is started up, the
shut down is ordered before the start-up"). So a start job in the shutdown
transaction can never be ordered *before* an unmount; it can only run after or
race it. Measured with a 12 s D1 unit:

```
D1 begin                mount=yes conf=present
   ... /boot/firmware unmounted ~4 s in, while D1 was still running ...
D1 end   (+12 s)        mount=no  conf=-          <-- stratux.conf gone
```

D1 also blocks `shutdown.target` for its whole run. A D1 that finishes within
about a second wins the race, but that is exactly the timing assumption this
feature must not rest on. **Rejected.**

**D2 - an `ExecStop` on a unit ordered `Before=` the renderers** (the chosen
design). At shutdown systemd stops units in the reverse of their start order, so
a unit that is `Before=stratux_epaper.service` is stopped *after* it. Because the
shutdown unit also has the default `After=sysinit.target basic.target` it is
stopped *before* `basic.target`, `local-fs.target` and the mount units, so
`/boot/firmware` is still mounted. Measured:

```
operational renderer  got-TERM ...... released     (3 s to let go)
shutdown ExecStop     .............. begins 0.03 s later   mount=yes conf=present
   ... only after it finishes: Stopped basic.target -> Stopped local-fs.target
       -> Unmounting boot-firmware.mount
```

While the shutdown unit's stop job is still running, `systemctl list-jobs` shows
`boot-firmware.mount stop waiting` queued behind it.

### `ExecStop` alone cannot tell poweroff from reboot

`ExecStop` runs on **every** stop of the unit and receives no indication of why.
So D2 needs a runtime discriminator. The shutdown transaction always contains a
**start job for exactly one of `poweroff.target` / `halt.target` /
`reboot.target` / `kexec.target`** for as long as units are being stopped, and
`systemctl list-jobs` (served by PID 1 throughout) reports it. This is systemd's
own state, not a timing heuristic:

| Operation (measured in the lab¹) | Queued shutdown start job | `-splash-shutdown` |
|---|---|---|
| `systemctl poweroff` | `poweroff.target` | **draws** |
| `systemctl halt` | `halt.target` | **draws** |
| `shutdown -h now` | `poweroff.target` | **draws** |
| `systemctl reboot` | `reboot.target` | skips |
| `shutdown -r now` | `reboot.target` | skips |
| poweroff then `reboot` both queued | both | skips (which wins is unclear; skipping is the harmless error) |
| `systemctl restart/stop stratux_epaper` | none - the unit is not involved at all | (not run) |
| package `prerm`-style stop of the renderers + `daemon-reload` | none - not involved | (not run) |
| `systemctl stop/restart stratux_epaper_shutdown` | none | skips ("not a system power-off") |
| `systemctl isolate rescue.target` | `rescue.target` (not a shutdown) | skips |

¹ Every row except "poweroff then reboot both queued" and `isolate
rescue.target` is exercised by the committed
`test/epaper_shutdown_systemd_lab.sh` with the real `epaperd`. Those two rows were
measured with the standalone probe used to develop the design; the "both
queued" case is also a unit test (`TestClassifyJobs`,
`TestRunSplashShutdown_NotAPoweroffDrawsNothing`). `kexec` is classified as a
reboot by the same code and tests but was not exercised in the lab (a container
cannot kexec).

`list-jobs` output for `poweroff`, `halt` and `reboot` was captured from the lab
and is committed as test fixtures (`epaper_main/testdata/list-jobs-*.txt`).
Stratux's own paths all use these: `POST /shutdown` and
`/confirmShutdown` run `systemctl poweroff`; `POST /reboot` runs
`systemctl reboot`; the OTA script (`debian/stratux-pre-start.sh`) runs the
plain `reboot` command, which is `systemctl reboot` under Debian's systemd-sysv.

### Both directions of the ordering

| | Declared | Start-up effect | Shutdown effect |
|---|---|---|---|
| Boot splash | `Before=stratux_epaper.service` (unchanged) | runs to completion, then the operational renderer starts | operational renderer stops, then the boot splash |
| Shutdown unit | `Before=stratux_epaper.service stratux_epaper_splash.service` | "started" (a no-op) just before both; adds no delay | stopped **after** both have fully stopped, before `/boot/firmware` is unmounted |

Both `Before=` names are required. Naming only `stratux_epaper.service` does
**not** order the shutdown unit against the boot splash, because the boot splash
and the shutdown unit are then both merely `Before=` the operational unit with
no relation to each other. Measured: a power-off requested while the boot splash
was still drawing (it takes time to release after SIGTERM):

```
with both Before= lines:   bootsplash released +5.01 s  ->  shutdownsplash acquired +5.03 s
control (only Before=E):   shutdownsplash acquired +2.02 s ... bootsplash released +5.02 s   (OVERLAP)
```

`test/epaper_shutdown_systemd_lab.sh` reproduces both, including the control that
proves the check can fail.

### The checklist from the design brief

| Question | Answer |
|---|---|
| Which units are active at runtime? | `stratux`, `stratux_epaper` (operational renderer), the boot splash (`active (exited)`), and the shutdown unit (`active (exited)`). |
| Which are pulled into poweroff? | All are stopped via the default `Conflicts=shutdown.target`; the shutdown unit is *stopped*, not started, by the transaction. |
| When does the operational renderer get SIGTERM, and when does it release? | Early in the stop phase (it only waits for units ordered after it). It draws its text screen, sleeps the panel, closes SPI/GPIO and exits; the stop job completes when the process has exited. |
| When does the shutdown renderer start? | When the shutdown unit's stop job runs, which systemd orders after the stop jobs of both renderers (and of the boot splash). |
| Can `shutdown.target` start a service after another stops? | Yes (D1), but that start job cannot be ordered before an unmount - measured. Not used. |
| `DefaultDependencies`? | Left at `yes` deliberately: it provides `After=basic.target` (stop before the unmount) and `Conflicts=shutdown.target` (included in every shutdown). A test pins it. |
| Late enough / filesystems available? | `/boot/firmware` (the config) is mounted while `ExecStop` runs and unmounted only afterwards - measured. `/opt/stratux/bin/epaperd` is on the root filesystem, which outlives all unit stops. The artwork is embedded in the binary, so no asset file is read. |
| Can package removal or a service restart show it? | No. Restarting `stratux_epaper` never involves this unit (no coupling in either direction). `prerm` does stop this unit, which runs `ExecStop`, but with no shutdown job queued it exits 0 without touching the panel (measured). |
| Would a reboot trigger it? | No: `reboot.target`/`kexec.target` start job => skip (measured). |
| Can a failed render delay shutdown? | Only by a bound: process deadline 45 s, then `TimeoutStopSec=60`, then systemd kills it and shutdown continues. |

## Hardware ownership guarantees

There is never simultaneous panel ownership. Three independent layers:

1. **Ordering (primary).** The shutdown unit's `ExecStop` starts only after
   systemd has finished stopping `stratux_epaper.service` (process exited, so
   the kernel has closed its SPI/GPIO handles) and
   `stratux_epaper_splash.service`. This holds even for a power-off requested
   while the boot splash is still drawing (measured above). No sleeps or polling.
2. **Sequential process lifecycle.** `-splash-shutdown` opens SPI/GPIO only
   inside `runSplash`, after every decision, and releases on every path
   (success, failure, timeout).
3. **Ownership guard (belt and braces).** `runSplash`'s status-file guard stays
   on: if the operational renderer's status file (in its `RuntimeDirectory`,
   removed when that unit stops) is fresh, the process refuses with exit 2
   without opening anything.

## Behavior by event

- **Orderly power-off / halt** (`systemctl poweroff|halt`, `shutdown -h`,
  Stratux's shutdown endpoints, the power key): draws once, sleeps the panel,
  releases SPI/GPIO; the Pi then completes shutdown.
- **Reboot** (`systemctl reboot`, `shutdown -r`, Stratux's reboot, OTA reboots;
  kexec is classified the same way): **no** shutdown splash refresh; the normal boot splash follows. (The
  operational renderer's own pre-existing stop text screen still appears.)
- **Ordinary service stop/restart** (`systemctl restart stratux_epaper`,
  settings change, OTA restart): the shutdown unit is not involved.
- **Stopping/restarting the shutdown unit itself**: `ExecStop` runs, finds no
  power-off, exits 0.
- **Package upgrade / removal**: `prerm` stops the shutdown unit (no draw);
  `postinst` re-arms it on the non-OTA path; the OTA path is armed by its own
  reboot. The unit file is replaced with the package. `daemon-reload` does not
  stop or run it.
- **OTA**: `systemctl enable` runs before the OTA early-exit, exactly like the
  other e-paper units, so an OTA-delivered install durably enables it (the
  overlay defect in `ota-persistent-storage-defect.md` is why); `start` runs only
  on the non-OTA path.
- **Abrupt power loss** (cable pulled, supply failure, kernel panic, hard
  reset, `systemctl poweroff --force`, `-ff`): the shutdown splash **cannot** run.
  Whatever image was already on the panel stays. This feature applies only to an
  orderly operating-system shutdown.

## Configuration

Uses `/boot/firmware/stratux.conf` exactly as the boot splash does; there is
**no new setting**.

| Settings | Shutdown splash |
|---|---|
| `EpaperEnabled` false / absent / no or unparseable config | none (logged, exit 0) |
| Enabled, `waveshare-4.2in-v2`, rotation 0 or 180, on power-off/halt | **ARS splash** |
| Enabled, 4.2" V2, but reboot / not a power-off / cannot tell | none (logged, exit 0) |
| Enabled, 3.7" or panel unset, 4.2" V2 rotation 90/270, invalid panel/rotation | none (logged, exit 0) |

## Failure and timeout policy

The shutdown splash is cosmetic; a display problem never stops the shutdown.

| Failure | Behavior | Unit result |
|---|---|---|
| Display absent / BUSY never idle | first BUSY wait times out (~10 s); hardware released | failed (exit 1); shutdown continues |
| GPIO/SPI unavailable | fails immediately, nothing opened | failed (exit 1) |
| SPI write / reset error | panel slept if initialized, hardware released | failed (exit 1) |
| Embedded asset invalid | detected before any hardware is opened | failed (exit 1) |
| Operational renderer appears to own the panel | refuses before opening anything | failed (exit 2) |
| `systemctl list-jobs` fails or hangs | skips, logged | success (exit 0) |
| Hang / slow panel | process deadline, then systemd backstop | failed or killed; shutdown continues |

| Bound | Value | Where |
|---|---|---|
| Process deadline (whole run, including the systemd query) | 45 s | `shutdownSplashTimeout` (equal to `bootSplashTimeout`; a test asserts it) |
| systemd query | 5 s | `jobQueryTimeout` |
| Per-BUSY wait | 10 s | existing driver default |
| systemd backstop | `TimeoutStopSec=60` | unit; a test requires it to exceed the process deadline so the process ends itself (panel asleep) before systemd must kill it |
| Arming | `TimeoutStartSec=10` | unit (`/bin/true`) |

On the real panel the boot splash was measured at about 4.2 s from start to
release, so the healthy added shutdown time is roughly that. The worst case is
`TimeoutStopSec` (60 s) and only then does systemd kill the process. A device
with `EpaperEnabled` true but no display attached adds about 10 s at shutdown (as
at boot), then continues.

## Limitations and risks

- **The "Safe to remove power" text is replaced.** The operational renderer's
  existing text screen appears first and is then overwritten by the ARS splash.
  The final image carries no text, so it no longer says "safe to remove power".
  The operational renderer was deliberately left unchanged (validated unit and
  a stated regression requirement); a future change could make it skip its text
  screen when a power-off is queued, saving one refresh. This is a product
  decision for the owner.
- **Extra refreshes at power-off.** Text screen (full), `Clear` (full), splash
  (full): three full refreshes and a blank flash, on top of normal wear
  considerations for e-paper. Reboots add none.
- **Relies on `systemctl list-jobs`.** It is a stable CLI over PID 1's job queue,
  fails safe (skip), and is exercised by fixtures from the real target systemd,
  but it is not a formally versioned API.
- **Two queued shutdown kinds** (poweroff then reboot) skip the splash, because
  which takes effect is uncertain.
- **Design measured first in a container, then confirmed on the Pi.** The
  ordering, mount and `list-jobs` reasoning was measured under real systemd 252
  in a container (stand-in `/boot/firmware`, no display), then confirmed on the
  real device by the [acceptance record](#physical-acceptance-record). Only one
  unit and one panel were tested; other Pi models and panel wirings are not.
- **On some Pi models `poweroff` halts without cutting power.** The panel image
  is retained either way.
- **Manual `systemctl disable` does not persist** on the protected overlay root
  (same as the other units). To stop it, disable the e-paper feature
  (`EpaperEnabled` false).
- **`-splash-shutdown` run by hand outside a shutdown draws nothing** (no
  power-off queued). To test the rendering itself use `epaperd -splash`
  ([procedure](epaper-boot-splash.md#physical-acceptance-gate)).

## Files

| File | Change |
|---|---|
| `debian/stratux_epaper_shutdown.service` | new unit |
| `epaper_main/splashshutdown.go` | `-splash-shutdown`: config gate, job-queue classification, calls `runSplash` |
| `epaper_main/main.go` | `-splash-shutdown` flag (refused in combination with `-splash`/`-splash-boot`); `-splash-config` help text |
| `Makefile` | packages the unit |
| `debian/postinst.dpkg`, `debian/prerm.dpkg` | enable / arm / stop the unit |
| `epaper_main/splashshutdown_test.go`, `epaper_main/shutdownunit_test.go`, `epaper_main/testdata/list-jobs-*.txt` | tests and real-systemd fixtures |
| `test/epaper_packaging_test.sh` | shutdown unit, scripts, `systemd-analyze verify` |
| `test/epaper_shutdown_systemd_lab.sh` | opt-in real-systemd container lab |
| `docs/epaper-shutdown-splash.md` (this file), `docs/README.md`, `docs/waveshare-epaper-display.md`, `docs/epaper-boot-splash.md` | documentation |

## Automated validation

```sh
go test ./epaper_main/...             # decision, renderer, unit structure, CLI wiring
bash test/epaper_packaging_test.sh    # postinst/prerm executed against stubs; systemd-analyze verify
bash test/epaper_shutdown_systemd_lab.sh   # opt-in: real systemd 252 in Docker (about 2 minutes)
```

Covered: configuration (enabled / disabled / supported and unsupported panel /
rotation 0, 180, 90 / missing / malformed / oversized); classification of real
`list-jobs` fixtures for poweroff, halt and reboot plus edge cases; the approved
asset checksum and validation; exactly one full refresh writing the exact bitmap
to both planes, ending in deep sleep and a release; 180 degrees; skip paths touch
no hardware; every failure class exits non-zero and releases; a stuck BUSY and a
hung systemd query are cut off by the deadline; a corrupt asset fails before
hardware; the ownership guard; CLI wiring in a real subprocess (`-splash-boot`
unchanged, `-splash-shutdown` isolated and refused in combination); unit
structure, `Before=` both renderers, no `After=`, no coupling, no shutdown-target
wiring, bounded timeouts, and a stop-order model; the existing units unchanged
and uncoupled; `postinst`/`prerm` behavior in OTA and normal modes. Each key
property was mutation-tested (dropping a `Before=`, `After=` instead of
`Before=`, `WantedBy=poweroff.target`, `RemainAfterExit=no`,
`DefaultDependencies=no`, reboot no longer winning, dropped ownership guard, ...
each makes a test fail).

## Physical acceptance procedure

The repeatable checklist (it was run once; see the
[record](#physical-acceptance-record)). Owner-run only. Install a real package build
through the normal path first (see
[step 0 of the boot-splash gate](epaper-boot-splash.md#cold-boot-acceptance-gate)):
web-UI OTA upload, never a hand copy (which would vanish with the overlay).
After the install reboots:

```sh
systemctl is-enabled stratux_epaper_shutdown          # enabled
systemctl is-active  stratux_epaper_shutdown          # active  (active (exited))
/opt/stratux/bin/epaperd -h 2>&1 | grep splash-shutdown   # flag present
grep -o '"EpaperEnabled": *[a-z]*' /boot/firmware/stratux.conf   # true
```

**Capturing journal evidence.** The image's journal is volatile
(`Storage=volatile`), so a shutdown's journal is lost at power-off, and a
userspace follower (`journalctl -f` writing to a file) was found **unreliable
during shutdown**: two attempts were cut off before the key lines. What worked
was making journald itself write to the data partition, temporarily and in RAM
only (no change under `/etc`, fstab, boot config or the repository):

```sh
D=/var/lib/stratux-data/acceptance/journal
sudo mkdir -p $D /var/log/journal /run/systemd/journald.conf.d
sudo mount --bind $D /var/log/journal
printf '[Journal]\nStorage=persistent\n' | sudo tee /run/systemd/journald.conf.d/acceptance.conf
sudo systemd-tmpfiles --create --prefix /var/log/journal
sudo systemctl restart systemd-journald && sudo journalctl --flush
# ...do the poweroff/reboot, boot again, then read the previous boot:
sudo journalctl --directory=$(ls -d $D/*/) -b <boot-id> -o short-precise --no-pager
```

The drop-in lives in `/run` and the bind mount in the overlay's RAM, so both
vanish at the next reboot (verify: `Storage=volatile` again, no bind mount);
delete the evidence files afterwards only if you no longer need them. Note that
the bind keeps `/var/lib/stratux-data` busy, so systemd logs a harmless
`Failed unmounting ... target is busy` for it at shutdown. Also note the SSH
key you add to `~/.ssh/authorized_keys` lives in the overlay's RAM layer and is
lost at every reboot: re-add it after each one.

**TEST A - boot regression.** Cold power-on. Expected: ARS boot splash, then the
operational display. `systemctl status stratux_epaper_splash` is `active
(exited)`, `status=0/SUCCESS`. Unchanged from the validated boot gate.

**TEST B - orderly power-off.** With the operational display showing, request
shutdown through Stratux's normal path. Expected on the panel: status -> the
"Stratux is shut down." text screen -> the ARS splash. Wait for the Pi's activity
to stop, then remove power. Expected: the **ARS image remains** on the unpowered
panel (check again after several minutes).

**TEST C - subsequent boot.** Restore power. Expected: the retained ARS image,
then the boot-splash sequence, then the operational display.

**TEST D - reboot.** From the operational display, request a reboot. Expected:
**no ARS refresh at shutdown** (the panel shows the operational renderer's text
screen, then the boot splash's ARS at boot, then the operational display), and
the shutdown unit's journal reads `shutdown splash skipped: the system is
rebooting (reboot.target); the boot splash follows`.

**TEST E - journal evidence for TEST B.** `journalctl -b -1 -o short-precise -u
stratux_epaper -u stratux_epaper_shutdown --no-pager` must show, in this order:

```
Stopping stratux_epaper.service ...                     (operational renderer told to stop)
Stopped  stratux_epaper.service ...                     (process exited => SPI/GPIO released)
Stopping stratux_epaper_shutdown.service ...
epaperd: shutdown splash: power-off in progress (poweroff.target); drawing the ARS splash
epaperd: initializing panel...
epaperd: clearing panel (full refresh)...
epaperd: drawing ARS splash (full refresh)...
epaperd: done: splash drawn, panel asleep
epaperd: released SPI/GPIO; this process no longer owns the panel
Stopped  stratux_epaper_shutdown.service ...            (shutdown continues)
```

The `Stopped stratux_epaper.service` line must precede the `power-off in
progress` line (the ownership proof). Optional negative checks: `sudo systemctl
restart stratux_epaper` must add no `epaperd: shutdown splash` lines; with
`EpaperEnabled` false, a power-off must log `shutdown splash skipped:
EpaperEnabled is false` and leave the operational renderer's text screen as the
final image. Anything unexpected (a splash on reboot, overlapping ownership,
shutdown noticeably delayed, an ARS splash that is blank or garbled): stop and
report before trusting the feature.

## Physical acceptance record

Owner-run on the real Stratux Raspberry Pi with the Waveshare 4.2" V2 panel
(`EpaperEnabled` true, `waveshare-4.2in-v2`, rotation 0), protected overlay root.
Physical observations are the owner's; the journals are the evidence for
ordering. The Pi's wall clock is untrusted (no GPS fix, unsynchronized), so
boots are identified by boot ID and events by their relative times.

| Field | Value |
|---|---|
| Package | `stratux-2.0.0~rc2-arm64.deb`, SHA-256 `996c5f31e14533e44675c502662b95d23f3949541e988dd1799c1d5f3693d8d0`, built by CI on the PR head |
| Embedded commit | `616ed20ed9b95e9b2be92e1a2e00792b0abd8b89` (running `Build` equalled it after the OTA) |
| Pre-OTA build | `026e69d1c7b69a1d79e386cc4f41bceb4817b01d` (the validated boot-splash build), boot `483d6227-6734-4ec2-bdbc-50b69148664c` |
| Install | supported OTA (`POST /updateUpload`); state `idle` -> `staged` -> `disable_requested` -> (device rebooted, API down during bare-ext4 install) -> `idle`; no manual intervention |
| Post-OTA state | overlay active, no disable marker, `dpkg --audit` clean, 0 failed units, all three e-paper units and `stratux` enabled and active, shutdown unit `active (exited)` with its `Before=` set, installed unit files and `epaperd` byte-identical to the audited package |
| Result | **PASSED** (all of A-E; see below for how E's evidence was completed) |

| Test | Observation | Journal evidence |
|---|---|---|
| **A** boot regression | Owner: after the cold boot, the retained ARS image, then the boot-splash refresh, then the operational display (the first post-OTA boot was not watched; the cold boot of test C is the observation) | boot `2942a838-e621-4256-8216-ae8d7c0f9c5a`: shutdown unit armed in ~74 ms, boot splash `initializing panel` -> `clearing` -> `drawing ARS splash` -> `done: splash drawn, panel asleep` -> `released SPI/GPIO` -> `Finished` (about 4.2 s), then `Started stratux_epaper.service` |
| **B** orderly poweroff | Owner: ARS splash appeared automatically, correct orientation, complete, no clipping or competing renderers; the image remained after power was removed; owner estimated under about 15 s overall; the intermediate text screen was seen and judged fine/useful | see test E |
| **C** cold boot | Owner: image intact while unpowered; then retained ARS image, boot-splash sequence, operational display. Boot ID changed `0b6e8d69-...` -> `2942a838-...` | as test A |
| **D** reboot | Owner: no extra ARS refresh before the reboot (observed twice: the reboot and its evidence re-run); boot splash then operational display | see test E |
| **E** journal / ownership | complete journals, below | below |

**Poweroff journal (test E, from the evidence re-run; boot `1741a13eca694c1f935908d1cd9290b6`):**

```
11:09.061  systemd-logind: The system will power off now!
11:09.162  Stopping stratux_epaper.service            (operational renderer told to stop)
11:10.965  Stopped  stratux_epaper.service            (exited => SPI/GPIO released; ~1.8 s incl. its text screen)
11:10.968  Stopped  stratux_epaper_splash.service
11:11.001  Stopping stratux_epaper_shutdown.service   (ExecStop starts)
11:11.069  epaperd: shutdown splash: power-off in progress (poweroff.target); drawing the ARS splash
11:11.072  epaperd: initializing panel...
11:11.157  epaperd: clearing panel (full refresh)...
11:12.915  epaperd: drawing ARS splash (full refresh)...
11:14.669  epaperd: done: splash drawn, panel asleep
11:14.670  epaperd: released SPI/GPIO; this process no longer owns the panel
11:14.673  Stopped  stratux_epaper_shutdown.service   (shutdown continues)
11:14.760  Unmounting boot-firmware.mount             (only after the shutdown unit stopped)
11:14.828  Reached target poweroff.target
11:15.175  systemd-journald: Journal stopped
```

Ownership is strictly sequential: the operational renderer was `Stopped` 0.1 s
before the shutdown renderer began, and there is no overlap. The renderer's own
draw took about 3.6 s; the whole poweroff took about 6.1 s from the request to
the end of the journal.

**Reboot journal (test E, from the evidence re-run; boot `a6c7ce25d80a4773b3c3fcdd2dbd118d`):**

```
10:35.923  systemd-logind: The system will reboot now!
10:36.038  Stopping stratux_epaper.service
10:37.819  Stopped  stratux_epaper.service
10:37.838  Stopping stratux_epaper_shutdown.service   (ExecStop runs)
10:37.872  epaperd: shutdown splash skipped: the system is rebooting (reboot.target); the boot splash follows
10:37.876  Stopped  stratux_epaper_shutdown.service
10:40.702  Unmounting boot-firmware.mount             (after the shutdown unit stopped)
10:40.792  Reached target reboot.target -> Shutting down
```

There are no drawing lines: the reboot classification won and no shutdown refresh
was made, matching the owner's observation.

**Evidence handling.** The first poweroff (test B) and first reboot (test D) were
captured by a userspace journal follower that died before the key lines
(recorded honestly rather than relied on); the journal proof above comes from one
additional reboot and one additional poweroff run with journald temporarily
persisting to the data partition, both authorized by the owner and behaving
identically to the first runs. The temporary configuration was verified gone
after each reboot. Evidence retained under `/var/lib/stratux-data/acceptance/`:
these files (SHA-256):

```
863d381950f0e2e12d8fab1744a64c54449dd1e9403cd15c10151ddf9c3203e3  journal/<machine-id>/system.journal
d630e7a38feff00c7e98877bd19da7409fa98e06b25a0ca2877a9931fefadb4e  journal/<machine-id>/user-1000.journal
1b1e545d1fd9f2ca62eed895b7e60498d32b33c9593beaf74ab60de9d70fc7bd  testB-poweroff-journal.log   (incomplete: follower died early)
5c0ad122df85978a67705a4ea70dc4109bc191536819e3150e031b69ce824d44  testD-reboot-journal.log     (incomplete: follower died early)
```

**Findings from the real device.**

- `/boot/firmware` is a standard fstab mount (`RequiredBy=local-fs.target`,
  `Before=umount.target local-fs.target`), so its unmount is ordered after the
  shutdown unit's stop, exactly as measured in the lab; on both the poweroff and
  the reboot it was unmounted after the shutdown unit stopped.
- `/var/lib/stratux-data` is a `nofail` mount ordered only `Before=stratux.service`
  and `umount.target`, so systemd tries to unmount it as soon as `stratux` stops,
  independently of the shutdown unit. The shutdown splash never uses it.
- Observed refresh sequence at poweroff: status display, the operational
  renderer's "Stratux is shut down. Safe to remove power." full refresh (about
  1.8 s to stop), then the shutdown splash `Clear` and full refresh (about 3.6 s):
  three full refreshes in total. The owner saw the text screen and found it
  acceptable and useful. Whether a future change should suppress or integrate it
  is a separate owner decision and was **not** changed here.

**Post-acceptance health (final boot `b0d54bd4-0f18-4242-8722-d2bb056a699a`).**
`Build` 616ed20e; OTA `idle`; overlay active, no disable marker; `dpkg --audit`
clean; 0 failed units; `stratux`, `stratux_epaper`, `stratux_epaper_splash`,
`stratux_epaper_shutdown`, `stratux_fancontrol` enabled and active; e-paper
`READY`/`RUNNING`, panel detected, 0 consecutive failures, 0 BUSY timeouts;
AHRS, baro, 1090 ES, UAT, GDL90, fan and storage `READY`; power `ok`
(`throttled=0x0`). The only degradations were environmental (indoors: GPS "no
satellite solution yet", so time unsynchronized).
