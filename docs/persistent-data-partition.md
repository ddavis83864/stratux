# Persistent-Data Partition: Design, Implementation, and Migration Policy

## Status

**Design implemented, reviewed, and corrected. Unit/loop-device/static gates green. Not yet
hardware-validated on any physical device.** This closes the provisioning gap
`docs/ota-persistent-storage-defect.md` documents: `/var/lib/stratux-data` is now a genuinely
dedicated, separately-mounted ext4 partition on any newly-built, freshly-flashed image - never
provisioned automatically on an already-deployed device. See "Hardware-validation checklist" below
for exactly what remains, and `docs/ota-persistent-storage-defect.md` for the incident this exists
to close.

A review pass identified five gaps in the design as first implemented, all closed in this revision:
deterministic mount ordering (B3), a writer-behavior guard shared by every persistence namespace
(B5), dynamic mount-state test coverage (Tests), strengthened provisioning safety checks (B2), and
first-boot durability/idempotency proof (B2). Each is called out inline below, and summarized in
"Review-response changes" at the end of this document.

## B1: Current image and boot lifecycle (as traced from source)

1. **Image creation** (`image_build/build.sh`): builds the `stratux` `.deb` first, then drives
   `pi-gen` (a vendored submodule, stages 3-5 skipped - only stage2 is customized) to produce a
   Raspberry Pi OS Lite-based image. pi-gen's own stock output is a 2-partition layout: a small
   FAT32 boot partition and an ext4 root partition sized only to fit the content actually built
   into it (a few GB) - never pre-grown to any particular card size, since the actual card size
   is unknown at build time.
2. `image_build/stage2/10-stratux/01-run.sh` (build-time, inside the pi-gen chroot):
   - Disables pi-gen/Raspberry Pi OS's own stock first-boot resize service
     (`systemctl disable resize2fs_once`) - **Stratux has never used the stock growth mechanism**;
     this was already true before this change.
   - Installs the stratux `.deb` (`02-run.sh`, actually a separate stage script) is not this one;
     `01-run.sh` itself installs `overlayctl`/`init-overlay` (see below) to `/sbin/`, runs
     `overlayctl install` (rewrites `/boot/firmware/cmdline.txt` to
     `... ro init=/sbin/init-overlay`), and - the fact this whole design builds on -
     **unconditionally `touch`es `/var/grow_root_part`** in every fresh image. This one file is
     the entire signal that drives first-boot partition work; nothing else creates or checks it.
3. **Image flash**: the owner writes the built image to an SD card via the standard, unmodified
   `.img` flashing process (Raspberry Pi Imager, `dd`, etc.) - out of this project's own control
   and unchanged by this work.
4. **First boot**: the kernel boots with `init=/sbin/init-overlay` (from the modified
   `cmdline.txt`). `init-overlay` checks `/var/grow_root_part` **before** anything overlay-related
   happens (`/` is still the plain, freshly-flashed root partition at this point) - present on
   every genuinely fresh image, so this branch always runs on a true first boot.
5. **Root partition growth and data-partition creation** (this document's own change): now
   delegated to a new, separately-testable script, `/sbin/provision-data-partition` (own file,
   installed by `01-run.sh`) - see "B2: Partition design" below for exactly what it does. Runs
   entirely before any overlay construction; `init-overlay` itself only mounts `/proc`/`/sys`,
   calls the script, applies its own follow-up steps (root filesystem resize, `/etc/fstab`
   entry + mount for the new partition if one was created), removes the marker, and reboots.
6. **First-boot reboot**: `reboot -f` (immediate, no clean shutdown sequence needed - nothing of
   value has started yet) once the marker is removed.
7. **Overlay initialization** (second boot onward, marker no longer present): `init-overlay`
   proceeds directly to its normal overlay-construction logic (unchanged by this work) -
   `/overlay/robase` (read-only bind-mount of the real root), `/overlay/rwdata` (a 250 MiB tmpfs
   upper layer), combined via `mount -t overlay`, then `pivot_root`.
8. **Local-filesystem mounting**: standard `/etc/fstab` processing (via systemd's own
   fstab-to-`.mount`-unit generator, part of ordinary early boot, `local-fs.target`) - this is
   where the new `LABEL=stratux-data` entry (written by `init-overlay` immediately after
   `provision-data-partition` succeeds) gets mounted, in **both** the normal (overlay-active) boot
   path and any future overlay-disabled (bare-ext4, OTA-install) boot: `/etc/fstab` is read from
   whatever is currently `/` either way, and a plain fstab entry naming a partition by `LABEL=`
   works identically regardless of whether `/` itself is an overlay or bare ext4 - the data
   partition is a distinct block device from root either way.
9. **Stratux startup** (`stratux.service`, `ExecStartPre=-/opt/stratux/bin/stratux-pre-start.sh`
   then `ExecStart=/opt/stratux/bin/stratuxrun`): unchanged by this work. Deliberately **no** hard
   `RequiresMountsFor=`/`After=` dependency was added on the data-partition mount - see "B3: Mount
   and service ordering" below for why that would be the wrong design for this specific service.
10. **OTA overlay-disabled boot** (`debian/stratux-pre-start.sh`, unchanged by this document -
    already hardened by `docs/ota-persistent-storage-defect.md`'s own PR): reads
    `/var/lib/stratux-data/updates/state.json` from whatever is genuinely mounted there at that
    point - now genuinely the dedicated partition, on a device this design has actually
    provisioned, rather than an ordinary directory on the overlay's own RAM-backed upper layer.

## B2: Partition design

### Chosen approach and rationale

None of the three approaches the mission brief lists applies unmodified, because the *existing*
mechanism already does something none of them assume: it grows root to fill the entire card on
first boot, using entirely custom (not pi-gen-stock) logic already fully owned by this project's
own `init-overlay` script. The chosen design is a direct, minimal extension of that existing,
already-proven mechanism - closest in spirit to option 3 ("a fixed root partition plus data
partition sized by the image builder"), except the *sizing* decision happens at **first boot**
(when the real card's actual capacity is known), not baked into the image at build time (when it
is not):

1. `/sbin/provision-data-partition` (new file, factored out of `init-overlay` specifically so it
   can be tested directly against a real loop-mounted disk image - see "Tests" below) grows root
   (partition 2) from its shipped, minimal size to a **bounded cap**, then creates partition 3 in
   the remaining space, formats it ext4, and labels it `stratux-data`.
2. This never shrinks anything - partition 2 only ever grows, monotonically, exactly once (the
   `/var/grow_root_part` marker, consumed on first use, structurally prevents this logic from
   ever running a second time on the same device).
3. This never touches an already-deployed device automatically. The marker itself (removed by
   `init-overlay`, unconditionally, after this script returns - so a device that has already booted
   even once will never re-enter this code path for any reason) is the first line of defense, but a
   review correctly did not accept it alone as sufficient proof of the expected stock layout, since
   partition *count* by itself cannot distinguish "the stock 2-partition layout" from "some other
   2-partition layout that happens to share the same count." `provision-data-partition` now proves
   four independent facts before ever touching the partition table, refusing (identically, via
   `reject_unexpected_layout`, leaving the table completely untouched) if any fails:
   1. `ROOT_DEV` is a whole-disk block device (`lsblk -dno TYPE` reports `disk` or, for the test
      harness's own loop-device stand-in, `loop`) - never a partition or something else entirely.
   2. The card has *exactly* 2 partitions (or, see below, exactly 3 in one specific, provably-safe
      state) - the original check, kept.
   3. Partition 2 of `ROOT_DEV` is genuinely `ROOT_PART_DEV` - the exact device `init-overlay`
      identified via `findmnt / -no source` - proof this is not some mismatched pairing.
   4. Partition 1 is a FAT/vfat filesystem - the expected boot partition type.
   This is what protects a device with an already-existing, different layout - most concretely, the
   operational device's own pre-existing dedicated third partition - from ever being touched by this
   logic, even in some future rebuild-and-reflash scenario the marker check alone might not
   anticipate.
4. **Idempotency after interruption at any material step (review requirement).** The original
   design's "any `PART_COUNT != 2` → skip forever, treat as foreign layout" rule had a real gap: the
   one-time retry marker is removed by `init-overlay` *regardless* of what this script reports, so a
   power loss or crash between creating partition 3 (`parted mkpart`) and formatting it (`mkfs.ext4`)
   would strand that partition forever as an unformatted, unusable "foreign layout" - there is no
   second opportunity to complete it. The script now inspects partition 3's *actual, observable
   state* whenever the count is exactly 3, rather than assuming what a count of 3 means:
   - **No filesystem at all** (`lsblk -no FSTYPE` empty): this can only be this script's own
     interrupted `mkpart` - nothing else creates an unformatted partition and leaves it that way.
     Safe, and necessary, to complete: `mkfs.ext4 -F -L stratux-data` runs now, on this one
     remaining chance, and reports `RESULT=provisioned` exactly as a from-scratch run would.
   - **`ext4` filesystem labeled `stratux-data`**: already fully provisioned by a previous run (the
     marker survived a reboot between `mkfs` completing and `init-overlay` removing it, or this
     script was invoked again by mistake). Reports the new `RESULT=already-provisioned` and does
     nothing further - critically, **never reformats**, which would destroy whatever has already
     been written there since.
   - **Anything else** (a real, different filesystem/label): genuinely a foreign layout already in
     active use - `reject_unexpected_layout`, untouched, exactly as the original design intended.
   Every step that mutates the partition table or a filesystem (`parted resizepart`, `parted mkpart`,
   `mkfs.ext4`) is followed by an explicit `sync` before the script's next decision point, so a crash
   immediately after any one of them leaves the *filesystem-level* state (not just the in-memory
   kernel state) durably reflecting what just happened - the resumption logic above depends on this:
   without it, a crash right after `mkfs.ext4` returns but before its writes reach disk could still
   present as "no filesystem" on the next boot, and the (already-safe, since it would just re-run
   `mkfs.ext4` on what is genuinely still-unformatted media) resumption path is only as trustworthy
   as this durability guarantee.
5. **A real, reproduced race between `partprobe` and reading the result back (review-adjacent
   hardening, found during this round's own test development, not the original review findings.
   themselves).** `partprobe` updates the kernel's partition table synchronously, but the new
   partition's device node and udev-probed `FSTYPE`/`LABEL` properties are refreshed asynchronously,
   via udev processing the "change" uevent `partprobe` triggers. Immediately querying `lsblk` after
   `partprobe`, with nothing forcing that processing to complete first, is a real race - most likely
   to actually bite on a slow/loaded first boot. `settle_udev` (`udevadm settle --timeout=5`,
   falling back to a fixed `sleep 1` if `udevadm` is unavailable) now runs after every `partprobe`
   call, and defensively before the script's own very first read of partition state too.

### Minimum supported card size

**16 GiB.** Below this, `provision-data-partition` falls back to the previous, simpler behavior
(grow root to fill the whole card, exactly as before this change) and creates no dedicated data
partition at all - a small card is left with an honest `NOT_INSTALLED`/`NOT_READY` persistent-data
state (via the already-hardened `readiness`/`ota`/`autorecord` checks) rather than a data partition
too small to be practically useful. Rationale for the 16 GiB threshold: the bounded root cap alone
(8 GiB, see below) plus the existing FAT32 boot partition (512 MiB) already consumes ~8.5 GiB;
16 GiB leaves a data partition of a few usable GiB even in the worst case, while smaller,
commonly-available card sizes (8 GiB and below) are left exactly as capable as they are today
(whole-card root, no data partition, no regression).

**This must never be mistaken for "this device works exactly as it always did" (review
requirement): unsupported cards are reported clearly, not silently.** `provision-data-partition`
writes a plain-text explanation directly onto the bare root filesystem at
`/etc/stratux-persistence-unsupported` (deliberately *not* under `/var/lib/stratux-data` itself -
that path is exactly what is unavailable in this branch) stating the measured card capacity and
that OTA and durable recording/calibration/diagnostics persistence are not supported on this card.
`readiness.UnsupportedPersistenceReason()` reads this file, and `main/health.go`'s `updateHealth()`
substitutes its content directly into `Storage`'s own health `Reason` field whenever the storage
state is not `Ready` - so a below-minimum card's `/getHealth` response names the specific,
measured reason (too small) rather than leaving an operator to infer it from a generic "not
mounted" message that reads identically to a card that is simply broken or not yet booted.

### Bounded root cap

**8192 MiB (8 GiB).** The current, fully-updated install footprint (confirmed on the live bench
device, per `docs/ota-persistent-storage-defect.md`'s own evidence) is ~1.5 GB used of the prior
whole-card root. 8 GiB leaves roughly 5x headroom for the OS, Stratux itself, OTA's own retained
backups (bounded to `otaMaxRetainedPackages = 3` staged/backup files, `main/ota.go`), logs, and
several future package-update cycles, without consuming space disproportionate to what an ext4
root genuinely needs on this class of embedded appliance.

### Supported card sizes

Works across any card size, with two regimes: **≥ 16 GiB** gets a real dedicated data partition
(size = total card capacity − ~8.5 GiB for boot+root, growing with the card - a 32 GB card gets a
substantially larger data partition than a 16 GB one, with no further code change needed); **< 16
GiB** gets the previous whole-card-root behavior, unchanged, with persistence-dependent features
(OTA, recording, calibration-profile durability, etc.) correctly and honestly reporting themselves
unavailable rather than silently degrading.

### Stable filesystem label

`stratux-data` (exactly as specified), assigned via `mkfs.ext4 -F -L stratux-data`. Mounted in
`/etc/fstab` by `LABEL=stratux-data`, never by a raw device path (`/dev/mmcblk0p3` is not
guaranteed to be the same physical partition number on every possible card/reader combination, and
is explicitly the pattern the mission brief itself warns against relying on alone).

### Filesystem choice

ext4 - already what every other partition on this image uses (root, and boot's own FAT32 aside),
already what `PersistentDataFSType` (`main/health.go`) already hardcodes and every readiness/OTA
check already expects, and already what the operational device's own existing third partition
uses. No evidence surfaced during this investigation favoring any alternative.

## B3: Mount and service ordering

The new partition is mounted via a plain `/etc/fstab` entry:

```
LABEL=stratux-data  /var/lib/stratux-data  ext4  defaults,noatime,nofail,x-systemd.device-timeout=5s,x-systemd.before=stratux.service  0  2
```

`nofail` (unchanged from the original design, the same convention this project already uses for the
optional `/dev/sda1 → /var/log` USB-stick mount) means a missing/failed mount never blocks boot or
takes down `stratux.service` - this partition is optional persistence, never a prerequisite for core
ADS-B/GPS/GDL90 function. **Ordering is required; successful mounting is not** (review requirement,
now made explicit rather than incidental):

- `x-systemd.device-timeout=5s` bounds how long systemd will wait for the underlying block device to
  appear before treating the mount attempt as failed - generous enough to absorb ordinary onboard-SD
  enumeration delay without meaningfully delaying boot in the worst case (a missing/dead partition).
- `x-systemd.before=stratux.service` is pure *ordering* - deliberately not `RequiresMountsFor=` (which
  would add a hard `Requires=` dependency, exactly the "optional feature blocks core function" failure
  mode this design has repeatedly guarded against elsewhere). systemd auto-derives the symmetric
  `After=` relationship from this `Before=`, and - the property this closes a real gap with - an
  ordering relationship waits for the *mount's own start job to fully resolve, success or failure*,
  not merely to begin.

**Proof this closes the shadow-mount race, not just delays it:** without this ordering, nothing
guarantees the mount *attempt* has fully resolved (successfully or not) before `stratux.service`
(and its `ExecStartPre=-/opt/stratux/bin/stratux-pre-start.sh`) is allowed to start its own jobs. If
a namespace writer created a file at `/var/lib/stratux-data/<path>` while that directory was still
the RAM-backed overlay's own upper layer, and the real partition then mounted moments later, the
kernel's mount operation *transparently replaces what is visible at that path* - the newly-mounted
filesystem shadows whatever was written into the overlay directory underneath it, invisibly and
permanently (the file still physically exists in the overlay's tmpfs, but nothing can reach it once
something else is mounted on top, and it is lost entirely at the next reboot when the overlay itself
is torn down). `x-systemd.before=stratux.service` eliminates this by construction: systemd will not
start `stratux.service`'s own unit (and therefore nothing that unit spawns can attempt any write)
until the `stratux-data` mount's start job has already completed - one way or the other - for this
boot. There is no window in which "the mount is still pending" and "a writer is already running"
overlap, because the ordering dependency is on the *job*, not on the mount succeeding. Genuinely
concurrent, orthogonal safety: the per-namespace write guards in B5 below independently refuse to
write into a not-yet-genuinely-mounted directory even if this ordering were somehow bypassed (a
manual `systemctl start stratux.service` before the mount unit runs, for instance) - the ordering
fix and the write guards close the same race from two different layers, deliberately redundant.

**Deliberately no `RequiresMountsFor=/var/lib/stratux-data` (or any other hard dependency) was
added to `stratux.service` itself.** `stratux.service` is the core ADS-B/GPS/GDL90 daemon - making
its own startup depend on an entirely optional persistence partition would be exactly the kind of
"optional feature blocks core function" failure this whole mission has repeatedly guarded against
elsewhere (the e-paper display, the OTA guard, the readiness rollup's own exclusion of
`NOT_INSTALLED` components). Instead, the mount's presence or absence is handled entirely by the
already-existing, already-hardened per-feature checks:

- `main/autorecordrun.go`'s `autoRecordAwaitMountAndReload` already retries for the mount to
  become ready for a bounded window after startup (pre-existing behavior, unchanged) - this
  absorbs the ordinary, brief race between "systemd is still mounting fstab entries" and
  "stratuxrun's own `initAutoRecord` already ran its one-time synchronous settings load."
- `readiness.CertifyPersistentStorage`/`DiscoverableMount`, `ota.validateStagingPersistence`, and
  `autoRecordMountReady` (all using the one canonical `readiness.IsDedicatedMount` - see
  "Persistent-mount validation" below) each independently, honestly report "not available" for
  their own feature if the mount genuinely never materializes, without taking down anything else.

This satisfies "service dependencies do not deadlock" and "failure does not trigger an
uncontrolled reboot loop" by construction: nothing in this design can fail in a way that blocks or
restarts boot - a missing mount is always, only, a per-feature degraded/unavailable report.

### Persistent-mount validation: one reusable, authoritative function

`readiness.IsDedicatedMount(info MountInfo, path string) (bool, string)` (new) is now the single
implementation three previously-separate, independently-evolved checks all delegate to:
`readiness.DiscoverableMount` (the one-time `PersistentDataUUID` pin),
`ota.IsDedicatedPersistentMount` (the OTA staging guard, a thin wrapper kept for this package's own
naming), and `main/autorecordrun.go`'s `autoRecordMountReady`. It proves `path` is `findmnt`'s own
resolved mount target (not merely an ordinary directory reached through a covering ancestor mount)
and that its filesystem type is not one of the volatile types (`overlay`, `tmpfs`, etc.) - device
number is deliberately never part of this check, since both a dedicated partition (a different
device than root) and a hypothetical future bind-mount-based provisioning (sharing root's own
device) must be accepted equally.

## B4: Existing installations and migration policy

| Class | Example | This design's effect |
|---|---|---|
| **1. Already has a valid dedicated data partition** | The operational Stratux, `/dev/mmcblk0p3` | **Unaffected.** The partition-count safety check (`PART_COUNT != 2` → skip entirely) means this automated logic never runs on such a device at all, regardless of how or when that third partition was created. No destructive migration, no re-check, no interference. |
| **2. Standard clean-image device lacking a dedicated data partition** | Any device already deployed from a pre-this-fix image (root already grown to 100%, `/var/grow_root_part` already consumed) | **Cannot be retrofitted automatically**, by explicit mission constraint ("do not automatically shrink root or create a new partition on a deployed device") and by this design's own structure (the marker that drives this logic is already gone on such a device - there is no remaining signal to safely resume from). OTA staging already fails honestly for these devices (`docs/ota-persistent-storage-defect.md`'s own fix). **Default remediation: reimage** a spare/new card with a build of this fix and restore supported configuration/data (see below) - never an automated in-place repartition of a running, deployed device. |
| **3. Unexpected/custom layout** | Any card with a partition count other than exactly 2 that isn't class 1 | Same protection as class 1 - the count check does not distinguish *why* the count is not 2, only that it is not, and refuses to guess. |
| **4. Incorrectly pinned `PersistentDataUUID`** | This mission's own bench device, per the incident report | A **settings-level**, not partition-level, problem - independent of whatever the actual partition table looks like. `PersistentDataUUID` is not currently exposed as an editable `/setSettings` field (confirmed: absent from `settingsFieldTypes`), so no in-place "clear it via the dashboard" path exists today, and none is added by this change (adding one is a separately-scoped decision, not bundled into this fix). The value lives in `/boot/firmware/stratux.conf`, which a **reimage naturally resets** (a freshly-flashed boot partition ships without a prior `PersistentDataUUID` value), letting a genuine future discovery succeed correctly once the reimaged device also has a real dedicated partition (class 1, achieved via this same design's own first-boot logic on the new image). This is the same default remediation as class 2, and resolves both problems (missing partition, wrong pinned UUID) in one step. |

**Any future in-place migration** (repartitioning an already-deployed, already-running device
without reimaging) is explicitly out of scope for this document and this branch, per the mission's
own instruction - it would require its own separately designed, separately authorized project,
almost certainly involving destructive operations (shrinking or moving an in-use root filesystem)
this document's own constraints forbid attempting here.

### Configuration Backup/Restore and migration

The existing Configuration Backup/Restore feature (`docs/configuration-backup-restore.md`) already
exports the *configuration* categories it was designed for (radio enablement, alert settings,
calibration profiles, etc. - see that document's own allowlist) and can be used, unchanged, to
carry configuration forward onto a reimaged card. It explicitly does **not** include recordings,
diagnostics bundles, or raw OTA state - these are large, bulky, or inherently
device/session-specific data, and remain outside Configuration Backup's own scope both before and
after this change. A reimage-based migration therefore preserves configuration cleanly but does
**not**, by itself, carry forward recordings/exports/diagnostics already on the old card - an
owner who needs those must copy them off (e.g. via the existing download endpoints) before
reimaging, exactly as would already be true today for any other reason to reimage a card.

## B5: Broader persistence namespace audit

See the table below - every production reader/writer under `/var/lib/stratux-data` this
investigation found, its ownership, criticality, retention, atomicity, and behavior when the
mount is unavailable.

**Review finding, now closed: 9 of these namespaces used to just call `os.MkdirAll` and write,
without first proving the mount was genuine - meaning each would have silently written into the
RAM-backed overlay directory if the real partition failed to mount.** Only OTA staging had an
explicit fail-fast gate (`validateStagingPersistence`). Every writer below now calls
`readiness.EnsurePersistentDir` - the single, canonical "is this genuinely a dedicated, writable,
non-volatile mount" check (built on the same `IsDedicatedMount` primitive `DiscoverableMount` and
OTA's own guard already shared) - immediately before its own `MkdirAll`/write, and refuses with a
clear error if it is not. Three packages outside `main` (`calprofile`, `recording`, `power`) and
`readiness` itself (for `WriteDiagnosticBundle`) expose this as an **injectable guard function**
(`SetPersistenceGuard`/`SetDiagnosticsPersistenceGuard`), defaulting to a no-op so each package's
own unit tests - which write to a plain `t.TempDir()` never intended to be a real dedicated mount -
are unaffected; `main`'s own `wireProductionPersistenceGuards()` (called once from `main()`, after
`readSettings()`, before any subsystem below it could write) switches every one of these to the
real check for an actual running daemon. This is "refuses persistence" for every one of the 9 -
none of them queue writes elsewhere or otherwise proceed without writing, so no namespace needed a
different resolution than a clean refusal.

| Namespace | Owner | Criticality / retention | Atomic writes? | Trigger | Behavior if mount unavailable |
|---|---|---|---|---|---|
| `calibration-profiles/` | `calprofile` package, `main/calprofilesapi.go` | Protected (never auto-evicted) - AHRS calibration | Yes (temp+rename, `atomicWriteJSON`) | Every boot (`EnsureMigrated`) + on-demand calibration/profile API calls | **Refuses** via `EnsurePersistentDir` before `MkdirAll`; a genuine failure is logged, reported (`profilesInitError`), never blocks other subsystems |
| `recordings/` | `recording` package, `main/recordingapi.go`, `main/autorecordrecovery.go` | Important (never auto-evicted); potentially large (up to 400 MiB/session) | Metadata sidecar: yes (temp+rename). Sample data: **direct append** (appropriate for a growing log; a crash mid-append can leave a partial last line) | `/startRecording`, 1 Hz while active, boot-time crash recovery | **Refuses** at `NewStore` (a recording cannot even start) and again at every file rotation (catches the mount disappearing mid-session, not just at startup - see B3's ordering fix and the dynamic-mount-state tests below); explicit free-space gate (503/507 below 100 MiB free) unchanged; errors loudly (HTTP 500), never silently proceeds |
| `exports/` | `main/recordingapi.go` | Important (never auto-evicted) | Yes (temp+rename) | On-demand `/exportRecording` | **Refuses** via `EnsurePersistentDir` before `MkdirAll`; failure returns HTTP 500 with the error, never silent |
| `diagnostics/` | `readiness/diagnostics.go`, `main/diagnosticsapi.go` | Bounded (self-retains, hard cap of 10, oldest pruned) | Yes (temp+rename) | On-demand `/generateDiagnostics` | **Refuses** via `EnsurePersistentDir` before `MkdirAll`; failure returns HTTP 500 |
| `updates/` (OTA staged/backup/state) | `main/ota.go`, `ota/state.go`; `updates/backup/` populated by `debian/stratux-pre-start.sh` | Bounded (`otaMaxRetainedPackages = 3`); not registered in Storage Lifecycle (documented gap - "already has its own bounded retention") | State file and staged upload: yes (temp+rename). `/resetOTA`'s pre-reset backup copy: plain, non-atomic | On-demand `/updateUpload`, periodic OTA-decision tick, `/resetOTA` | Unchanged by this revision - already the one namespace with an explicit, hard, fail-closed gate: `validateStagingPersistence()` rejects the entire upload (HTTP 503) up front if the mount is not proven a genuine dedicated ext4 filesystem - see `docs/ota-persistent-storage-defect.md`. Never silently proceeds onto a volatile mount. |
| `power-session.json` | `power/session.go`, `main/powerapi.go` | Not registered in Storage Lifecycle (documented gap - "single bare files at the partition root"); tiny | Yes (temp+rename) | Every boot, and immediately before a controlled shutdown/reboot | **Refuses** via `EnsurePersistentDir` before `MkdirAll`; failure only logged ("next boot's assessment will be conservative"), never fatal |
| `alert-settings.json` | `main/alertsettings.go` | Not registered in Storage Lifecycle; tiny | Yes (temp+fsync+rename - the canonical pattern most other settings files mirror) | On settings change, Config Backup/Restore | **Refuses** via the shared guard before `MkdirAll` (errors surfaced, e.g. HTTP 500); missing/corrupt file on load degrades safely to defaults, never fatal |
| `autorecord-settings.json` | `main/autorecordsettings.go` | Not registered in Storage Lifecycle; tiny | Yes (temp+fsync+rename) | On settings change, boot, Config Backup/Restore | **Refuses** via the shared guard before `MkdirAll`; load-side degrades safely to *disabled* defaults on any problem - deliberately, so a corrupt file can never silently enable automatic recording |
| `traffic-cpa-settings.json` | `main/trafficcpasettings.go` | Not registered in Storage Lifecycle; tiny | Yes (temp+fsync+rename) | On settings change, Config Backup/Restore | **Refuses** via the shared guard before `MkdirAll`; load-side degrades safely to defaults |
| `wifi-admin-last-known-good.json` / `wifi-admin-pending-transaction.json` | `main/wifiadminsettings.go`, `wifiadmin` package | Not registered in Storage Lifecycle; tiny but high-criticality (network-lockout avoidance) | Yes, and the most thorough of any namespace: temp+fsync+rename **plus an explicit parent-directory fsync** afterward | Two-phase-commit Wi-Fi settings changes | **Refuses** via the shared guard before `MkdirAll`; a missing file on load is treated as "nothing persisted yet" (not an error), a present-but-corrupt file IS reported as an error |
| Storage Lifecycle's own scan | `storagelifecycle` package, `main/storagelifecycleapi.go` | N/A - **read-only**, never writes | N/A | 60-second periodic scan | Unchanged: N/A, read-only. A scan failure (including the namespace root not existing yet) degrades to an empty inventory - "never panics or stops this loop" |
| Configuration Backup | `main/configbackupapi.go`, `configbackup` package | N/A - **no filesystem calls of its own** | N/A (delegates entirely to the namespace-specific save functions above, inheriting each one's own atomicity) | Restore/rollback | Unchanged: inherits whatever the target namespace's own (now-guarded) save function does |
| Readiness write-test marker | `readiness.WriteTest`/`CertifyPersistentStorage` | N/A - transient probe, not real data | Create+immediately-remove | Every 5s health tick, and on every recording free-space check | Unchanged - already correctly guarded: only attempted when already `Mounted && !ReadOnly`, never attempted at all against an absent/read-only mount |

Every guarded namespace above still reaches for its own `MkdirAll`/temp-file-and-rename rather than
sharing one full write-path library function (`atomicWriteJSON`, reused by `wifiadminsettings.go`
and `calprofile/store.go`, is the closest thing to a shared implementation, and now also calls the
shared guard) - a pre-existing, minor inconsistency this revision does not attempt to fully
consolidate beyond the one thing the review specifically required (the guard itself being shared).
Every namespace's read-side degrades safely (defaults, empty inventory, or an honest error) rather
than panicking or corrupting data when the mount is missing - **the one namespace that fails a
client-visible operation outright (HTTP 503) rather than degrading is OTA staging**, which is
exactly the fail-fast behavior `docs/ota-persistent-storage-defect.md` added.

**No claim is made here that any of these namespaces loses data on an *ordinary* (non-OTA) reboot**
beyond what is stated as directly observed. This audit describes what the code does and assumes
about the mount's availability; it is not itself proof of behavior under conditions this
investigation did not directly exercise (an ordinary, unforced reboot of a device that has never
had a genuine dedicated partition) - see `docs/ota-persistent-storage-defect.md`'s own identical
disclosure for why that distinction matters and stays open pending the dedicated hardware test
Workstream D's persistence matrix performs.

## Tests

- **`test/persistent_data_partition_test.sh`**: runs the real, unmodified
  `/sbin/provision-data-partition` script against real, disposable loop-mounted disk images
  (`losetup`/`parted`/`mkfs.ext4`) - not a reimplementation of its logic. 7 cases, 22 checks, all
  green across repeated consecutive runs:
  1. a ≥16 GiB card gets a correctly-sized (±8 MiB of the 8192 MiB cap), correctly-labeled,
     correctly-typed data partition;
  2. running it again against an already-3-partition (fully provisioned) card reports
     `RESULT=already-provisioned` and leaves the table untouched (idempotence);
  3. a pre-existing, genuinely foreign 3-partition layout (formatted with a different
     filesystem/label, simulating the operational device's own already-in-use third partition) is
     never modified;
  4. a <16 GiB card falls back to whole-card root growth, creates no data partition, and writes the
     clear unsupported-persistence marker (see "Minimum supported card size" above);
  5. **(new)** interrupted after the root resize but before partition 3 is created - resumes and
     completes correctly, still yielding exactly 3 partitions;
  6. **(new)** interrupted after partition 3 is created but before it is formatted - the one
     scenario the original design's idempotency gap (see B2 above) would have stranded forever;
     now resumes and formats it correctly;
  7. **(new)** a repeat invocation on an already-fully-provisioned card reports
     `RESULT=already-provisioned` and - proven directly, not just asserted - **never reformats**: a
     sentinel file written to the data partition between the two invocations is confirmed to
     survive.

  **Found and fixed two real bugs during this round's own empirical validation** (both caught only
  because these tests use real devices, not mocks):
  - A genuine race between `partprobe` and `lsblk`/`blkid` reading back the result - closed by
    `settle_udev` (see B2 above). Confirmed via direct, isolated reproduction that this specific
    race was *not* what caused Case 6's own flakiness during development (see the next bullet) -
    it is real and worth the fix regardless, just not the cause of that particular symptom.
  - The test harness's own `make_test_image` helper reused the same on-disk image file path across
    every case without clearing prior content: `dd if=/dev/zero of=file bs=1M seek=N count=0`
    (a well-known idiom for creating a sparse file of a given size) is a no-op against a *pre-existing*
    file of the same or larger size - it only extends via `seek`, it never truncates or zeroes
    existing content, since `count=0` means zero write operations happen at all. Two cases using
    the same image size and root cap therefore put partition 3 at the identical byte offset, and a
    later case's "freshly unformatted" partition could land squarely on top of an earlier case's
    real, still-intact `stratux-data` filesystem - genuinely still on disk, not a caching artifact.
    Fixed by `rm -f` before every `dd` in `make_test_image`, restoring the sparse-file idiom's
    actual intended behavior.
  - (Previously fixed, kept for context) The original implementation misparsed `lsblk`'s default
    tree-drawing output (`└─loop3p3` instead of `loop3p3`) when locating the newly-created
    partition device - fixed with `lsblk -l`, the flat-list form.

  Requires `CAP_SYS_ADMIN` (confirmed available via passwordless `sudo` on the development machine
  used to run and validate this test; skips itself cleanly, rather than failing, when unavailable -
  e.g. a restricted container). Every loop device and temp file is cleaned up via a trap, including
  on failure.
- **`readiness/storage_test.go`**: `IsDedicatedMount` unit tests (dedicated mount accepted,
  ancestor-covered path rejected, volatile filesystem type rejected even at its own target), plus
  **(new)** `EnsurePersistentDir` tests against a real (non-synthetic) `t.TempDir()` - proven to
  refuse a plain temp directory (never its own dedicated mount) and to fail closed for a path
  `findmnt` cannot resolve at all.
- **(new) Persistence-guard tests**, one pair (default-allows / refuses-when-unsafe) per package
  that gained the shared write guard: `calprofile/store_test.go`, `recording/store_test.go`,
  `power/session_test.go`, `readiness/diagnostics_test.go`. Each proves both that the default
  no-op guard never blocks a write (so every pre-existing test in that package is unaffected) and
  that a guard reporting "unsafe" is actually consulted and blocks the write **before any file is
  created** - not merely that an error is returned.
- **(new) `main/persistenceguards_test.go`**: proves `wireProductionPersistenceGuards()` actually
  switches every namespace's guard from its default no-op to the real
  `readiness.EnsurePersistentDir(PersistentDataPath)` check (exercising `calprofile`/`recording`/
  `power` directly, not just this package's own copy of the check), and covers the mission's four
  named dynamic mount-state scenarios end to end against a real write path
  (`recording.NewStore`/`Append`): **failed mount** (every attempt refused, never succeeds),
  **delayed mount / mount appearing after service startup** (refused before the mount becomes
  ready, succeeds on a fresh attempt once it does - the two scenarios are indistinguishable from a
  writer's own point of view), and **mount disappearing during operation** (a session that started
  successfully while the mount was present is refused at its next file rotation once the mount
  vanishes, never continuing to write into whatever now backs that path).
- `sh -n`/`dash -n` clean on `init-overlay` and `provision-data-partition`.
- Full `go build`/`go vet`/`gofmt` clean, and `go test -count=1` green, on `ota`, `readiness`,
  `main`, `calprofile`, `recording`, `power` (see the PR description for exact commands/results).
  `go test -race` on the same packages surfaced one pre-existing, unrelated flaky data race in
  `main/autorecordrun_test.go` (`TestAutoRecordAwaitMountAndReload_ReloadsOnceMountBecomesReady`) -
  confirmed present, identically, on the pre-review-fix commit this branch is based on, so it
  predates and is unrelated to this revision's own changes; left unfixed as out of this specific
  review's scope, disclosed here rather than silently ignored.

## Image artifact

A complete clean-install `.img.xz` was built from this branch's own head using the trusted native
build path (`image_build/build.sh`, which clones this repository's own committed history - not the
working tree - so the build reflects a real commit, not uncommitted local state) and independently
verified: partition table, filesystems, labels, boot scripts (`cmdline.txt`'s `init=/sbin/init-overlay`),
`/etc/fstab`, `provision-data-partition`/`init-overlay`/`overlayctl` byte-for-byte against this
branch's own source, absence of private SSH host keys, settings sanitization, the embedded commit,
size, and SHA-256. See the PR description for the exact identity and evidence of that build.

## Hardware-validation checklist

Not yet performed - this is Workstream D, gated behind your explicit, per-device authorization.
See the separate validation-procedure proposal for the exact steps.

## Rollback

If a device provisioned by this design needs to be rolled back: the data partition
(`LABEL=stratux-data`) and its contents are untouched by rolling back the `stratux` package itself
(rollback only reinstalls prior application files, never touches the partition table). To fully
undo the partition-level change would require reimaging with a pre-this-fix image - the same
"default remediation is reimage" policy as migration, in reverse.
