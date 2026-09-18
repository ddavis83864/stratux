# Persistent-Data Partition: Design, Implementation, and Migration Policy

## Status

**Design implemented and unit/loop-device tested. Not yet hardware-validated on any physical
device.** This closes the provisioning gap `docs/ota-persistent-storage-defect.md` documents:
`/var/lib/stratux-data` is now a genuinely dedicated, separately-mounted ext4 partition on any
newly-built, freshly-flashed image - never provisioned automatically on an already-deployed
device. See "Hardware-validation checklist" below for exactly what remains, and
`docs/ota-persistent-storage-defect.md` for the incident this exists to close.

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
3. This never touches an already-deployed device automatically - two independent, defense-in-depth
   safety checks: the marker itself (removed after first use, so a device that has already booted
   even once will never re-enter this code path for any reason), and (new in this design) an
   explicit partition-count check: if the card does not have *exactly* 2 partitions when this runs,
   the whole partition-table change is skipped entirely, logged, and the (now-unnecessary but
   harmless) marker is simply removed. This is what protects a device with an already-existing,
   different layout - most concretely, the operational device's own pre-existing dedicated third
   partition - from ever being touched by this logic, even in some future rebuild-and-reflash
   scenario the marker check alone might not anticipate.

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

The new partition is mounted via a plain `/etc/fstab` entry
(`LABEL=stratux-data /var/lib/stratux-data ext4 defaults,noatime,nofail 0 2`) - the same mechanism,
and the same `nofail` convention, this project already uses for the optional `/dev/sda1 → /var/log`
USB-stick mount. `nofail` means a missing/failed mount never blocks boot; ordinary systemd
early-boot ordering (`local-fs.target`, reached before `multi-user.target`, which
`stratux.service` is `WantedBy=`) mounts it before Stratux starts in the overwhelmingly common
case, without needing any explicit `Before=`/`After=` engineering.

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

| Namespace | Owner | Criticality / retention | Atomic writes? | Trigger | Behavior if mount unavailable |
|---|---|---|---|---|---|
| `calibration-profiles/` | `calprofile` package, `main/calprofilesapi.go` | Protected (never auto-evicted) - AHRS calibration | Yes (temp+rename, `atomicWriteJSON`) | Every boot (`EnsureMigrated`) + on-demand calibration/profile API calls | Self-heals dir via `MkdirAll`; a genuine write failure is logged, reported (`profilesInitError`), never blocks other subsystems |
| `recordings/` | `recording` package, `main/recordingapi.go`, `main/autorecordrecovery.go` | Important (never auto-evicted); potentially large (up to 400 MiB/session) | Metadata sidecar: yes (temp+rename). Sample data: **direct append** (appropriate for a growing log; a crash mid-append can leave a partial last line) | `/startRecording`, 1 Hz while active, boot-time crash recovery | `MkdirAll` + explicit free-space gate (503/507 below 100 MiB free); errors loudly (HTTP 500), never silently proceeds |
| `exports/` | `main/recordingapi.go` | Important (never auto-evicted) | Yes (temp+rename) | On-demand `/exportRecording` | `MkdirAll`; failure returns HTTP 500 with the error, never silent |
| `diagnostics/` | `readiness/diagnostics.go`, `main/diagnosticsapi.go` | Bounded (self-retains, hard cap of 10, oldest pruned) | Yes (temp+rename) | On-demand `/generateDiagnostics` | `MkdirAll`; failure returns HTTP 500 |
| `updates/` (OTA staged/backup/state) | `main/ota.go`, `ota/state.go`; `updates/backup/` populated by `debian/stratux-pre-start.sh` | Bounded (`otaMaxRetainedPackages = 3`); not registered in Storage Lifecycle (documented gap - "already has its own bounded retention") | State file and staged upload: yes (temp+rename). `/resetOTA`'s pre-reset backup copy: plain, non-atomic | On-demand `/updateUpload`, periodic OTA-decision tick, `/resetOTA` | **The one namespace with an explicit, hard, fail-closed gate**: `validateStagingPersistence()` rejects the entire upload (HTTP 503) up front if the mount is not proven a genuine dedicated ext4 filesystem - see `docs/ota-persistent-storage-defect.md`. Never silently proceeds onto a volatile mount. |
| `power-session.json` | `power/session.go`, `main/powerapi.go` | Not registered in Storage Lifecycle (documented gap - "single bare files at the partition root"); tiny | Yes (temp+rename) | Every boot, and immediately before a controlled shutdown/reboot | `MkdirAll`; failure only logged ("next boot's assessment will be conservative"), never fatal |
| `alert-settings.json` | `main/alertsettings.go` | Not registered in Storage Lifecycle; tiny | Yes (temp+fsync+rename - the canonical pattern most other settings files mirror) | On settings change, Config Backup/Restore | `MkdirAll` on write (errors surfaced, e.g. HTTP 500); missing/corrupt file on load degrades safely to defaults, never fatal |
| `autorecord-settings.json` | `main/autorecordsettings.go` | Not registered in Storage Lifecycle; tiny | Yes (temp+fsync+rename) | On settings change, boot, Config Backup/Restore | `MkdirAll` on write; load-side degrades safely to *disabled* defaults on any problem - deliberately, so a corrupt file can never silently enable automatic recording |
| `traffic-cpa-settings.json` | `main/trafficcpasettings.go` | Not registered in Storage Lifecycle; tiny | Yes (temp+fsync+rename) | On settings change, Config Backup/Restore | `MkdirAll` on write; load-side degrades safely to defaults |
| `wifi-admin-last-known-good.json` / `wifi-admin-pending-transaction.json` | `main/wifiadminsettings.go`, `wifiadmin` package | Not registered in Storage Lifecycle; tiny but high-criticality (network-lockout avoidance) | Yes, and the most thorough of any namespace: temp+fsync+rename **plus an explicit parent-directory fsync** afterward | Two-phase-commit Wi-Fi settings changes | `MkdirAll` on write; a missing file on load is treated as "nothing persisted yet" (not an error), a present-but-corrupt file IS reported as an error |
| Storage Lifecycle's own scan | `storagelifecycle` package, `main/storagelifecycleapi.go` | N/A - **read-only**, never writes | N/A | 60-second periodic scan | A scan failure (including the namespace root not existing yet) degrades to an empty inventory - "never panics or stops this loop" |
| Configuration Backup | `main/configbackupapi.go`, `configbackup` package | N/A - **no filesystem calls of its own** | N/A (delegates entirely to the namespace-specific save functions above, inheriting each one's own atomicity) | Restore/rollback | Inherits whatever the target namespace's own save function does |
| Readiness write-test marker | `readiness.WriteTest`/`CertifyPersistentStorage` | N/A - transient probe, not real data | Create+immediately-remove | Every 5s health tick, and on every recording free-space check | Guarded: only attempted when already `Mounted && !ReadOnly` - never attempted at all against an absent/read-only mount, degrading to a `NOT_READY` health report instead |

Every namespace above independently reaches for `MkdirAll`/temp-file-and-rename rather than sharing one library function (only `atomicWriteJSON`, reused by `wifiadminsettings.go` and `calprofile/store.go`, is shared by more than one caller) - a pre-existing, minor inconsistency this document does not attempt to consolidate (out of scope for this specific fix; noted for a possible future cleanup). Every namespace's read-side degrades safely (defaults, empty inventory, or an honest error) rather than panicking or corrupting data when the mount is missing - **the one namespace that fails a client-visible operation outright (HTTP 503) rather than degrading is OTA staging**, which is exactly the fail-fast behavior `docs/ota-persistent-storage-defect.md` added.

**No claim is made here that any of these namespaces loses data on an *ordinary* (non-OTA) reboot**
beyond what is stated as directly observed. This audit describes what the code does and assumes
about the mount's availability; it is not itself proof of behavior under conditions this
investigation did not directly exercise (an ordinary, unforced reboot of a device that has never
had a genuine dedicated partition) - see `docs/ota-persistent-storage-defect.md`'s own identical
disclosure for why that distinction matters and stays open pending the dedicated hardware test
Workstream D's persistence matrix performs.

## Tests

- **`test/persistent_data_partition_test.sh`** (new): runs the real, unmodified
  `/sbin/provision-data-partition` script against real, disposable loop-mounted disk images
  (`losetup`/`parted`/`mkfs.ext4`) - not a reimplementation of its logic. Covers: a ≥16 GiB card
  gets a correctly-sized (±8 MiB of the 8192 MiB cap), correctly-labeled, correctly-typed data
  partition; running it again against an already-3-partition card is a safe no-op (idempotence);
  a pre-existing, unrelated 3-partition layout (simulating the operational device's own layout) is
  never modified; a <16 GiB card falls back to whole-card root growth with no data partition
  created. **Found and fixed a real bug during this empirical validation**: the original
  implementation misparsed `lsblk`'s default tree-drawing output (`└─loop3p3` instead of
  `loop3p3`) when locating the newly-created partition device - caught only because the test uses
  a real device, not a mock, and now fixed (`lsblk -l`, the flat-list form). Requires
  `CAP_SYS_ADMIN` (confirmed available via passwordless `sudo` on the development machine used to
  run and validate this test; skips itself cleanly, rather than failing, when unavailable - e.g. a
  restricted container). Every loop device and temp file is cleaned up via a trap, including on
  failure.
- **`readiness/storage_test.go`**: new `IsDedicatedMount` unit tests (dedicated mount accepted,
  ancestor-covered path rejected, volatile filesystem type rejected even at its own target).
- `sh -n`/`dash -n` clean on `init-overlay` and `provision-data-partition`.
- Full `go build`/`go vet`/`gofmt`/`go test -race` clean on `ota`, `readiness`, `main` (see the PR
  description for exact commands/results).

## Hardware-validation checklist

Not yet performed - this is Workstream D, gated behind your explicit, per-device authorization.
See the separate validation-procedure proposal for the exact steps.

## Rollback

If a device provisioned by this design needs to be rolled back: the data partition
(`LABEL=stratux-data`) and its contents are untouched by rolling back the `stratux` package itself
(rollback only reinstalls prior application files, never touches the partition table). To fully
undo the partition-level change would require reimaging with a pre-this-fix image - the same
"default remediation is reimage" policy as migration, in reverse.
