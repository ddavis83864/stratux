# OTA / Persistent-Storage Defect: Investigation and Corrective Design

## Status

**Confirmed root cause, hardware-validated on a live, real device. Corrective code proposed in this
document's own branch, NOT merged, NOT deployed.** The fail-fast validation this document proposes
(and includes as draft code) will, correctly, cause every OTA upload to be rejected on every
existing device until the underlying image-provisioning gap is also fixed - see "What this does
NOT fix" below. Do not treat this document's existence as evidence OTA is safe to use again.

## Why this exists

A routine OTA deployment attempt (of an unrelated, disabled-by-default optional feature branch)
silently failed: the daemon reported a clean state-machine transition (`staged` →
`disable_requested` → a reboot → `idle`), the device rebooted and came back running, and
`stratux-pre-start.sh`'s own log said only `Exited without updating anything...` - no error, no
corruption, but also no update. This document is the full investigation of why, performed
read-only against a live device (see "Evidence" below), followed by a corrective design.

## Root cause

**`/var/lib/stratux-data` (`main.PersistentDataPath`) is not a dedicated, provisioned persistent
partition anywhere in this project.** Confirmed by exhaustive repo search: `image_build/` (the
pi-gen image-provisioning pipeline) never references that path at all - no partition, no `fstab`
entry, no systemd `.mount` unit, no bind-mount setup. The only `fstab` entry the image build adds
is an *optional* `/dev/sda1 → /var/log` USB-stick mount
(`image_build/stage2/10-stratux/01-run.sh`).

Under **normal** operation (the protected, RAM-backed root overlay active - this project's
standard, always-on read-only-root architecture), `/var/lib/stratux-data` is simply an ordinary
directory reached through that overlay. Per overlayfs's own semantics, any **new** file written
there lands in the RAM-backed upper layer (`/overlay/rwdata`, a 250 MiB tmpfs), not on the real
disk - indistinguishable from genuine persistence until the system actually reboots without that
RAM content.

### The decisive evidence

`main/health.go`'s `ensurePersistentDataUUID` pins `PersistentDataUUID` (a one-time,
permanently-saved "is this genuinely persistent" fact) the first time it observes
`findmnt --target /var/lib/stratux-data` reporting `FSType == "ext4"` with a real UUID - the one
condition `readiness.DiscoverableMount` requires. Under the protected overlay, that target
resolves to the overlay itself (`FSType == "overlay"`), so this can never succeed. It succeeded,
for the very first time on this device, **during the abnormal bare-ext4 boot this incident itself
caused**:

| | `PersistentDataUUID` |
|---|---|
| Captured baseline, device running normally under the protected overlay, *before* the OTA attempt | `""` (empty - never pinned) |
| Read again, moments later, on the very next boot (the OTA's own overlay-disabled bare-ext4 boot) | `de7e0b63-eb97-4c37-a08f-b02e677092a9` (`mmcblk0p2`'s real UUID) |

This proves, directly and unambiguously, that under every normal boot before this incident,
`/var/lib/stratux-data` was never genuinely ext4-backed - only this one abnormal, OTA-triggered
bare-ext4 boot ever satisfied that check.

### The failure sequence

1. `POST /updateUpload` stages the `.deb` under `otaDir` (`/var/lib/stratux-data/updates/`) while
   the overlay is active - the write lands in RAM.
2. The daemon requests the overlay-disable marker (`ota.IsPersistent`/`ota.StatMount` correctly
   proves *that* specific file is genuinely persistent - this part of the mechanism, from the
   SSH-host-key-hardening work, functions exactly as designed) and reboots.
3. The device boots genuinely bare ext4 (confirmed live: `findmnt -T /` reported
   `SOURCE=/dev/mmcblk0p2 FSTYPE=ext4`, no overlay at all).
4. `debian/stratux-pre-start.sh` looks for `/var/lib/stratux-data/updates/state.json`. It is not
   there - it only ever existed in the previous boot's RAM. `[ -f "${OTA_STATE}" ]` is false, the
   entire OTA block is skipped, and the script falls through to
   `wLog "Exited without updating anything..."` - exactly what was observed.
5. The daemon's own OTA state (also written to the same never-actually-persistent location)
   independently resets to idle on its own next read, for the same reason.

No corruption occurred. No package was partially installed. The failure is silent-but-safe, which
is exactly why it went unnoticed until this investigation.

### A second, independent, confirmed defect: `overlayctl status` is unreliable

While investigating, `overlayctl status` was found to report `overlay is active` on a boot that
`findmnt`/`/proc/self/mountinfo` unambiguously proved was genuinely bare ext4. Its own
`overlay_is_active()` only checks whether a directory (`/overlay/robase/overlay`) exists - which,
once created by any past overlay-active session, persists on the real disk forever, including
through a later boot that is genuinely disabled. `debian/stratux-pre-start.sh` already implements
this correctly (`[ "$(findmnt -n -o FSTYPE /)" = "overlay" ]`); `overlayctl` never matched it. This
is fixed in this branch (see "Corrective changes" below) - a pure, no-regression correctness fix,
since the old check was already always right in the case it correctly detected, and only ever
wrong in the false-positive case this incident found.

### The live incident's own resolution (already completed, separately from this document)

The investigation itself left the device with its overlay genuinely disabled (a real, live
consequence of step 2/3 above with nothing to consume the reboot cleanly). Under a narrowly-scoped
owner authorization, this was recovered: `stratux.service` stopped cleanly, `overlayctl enable`
run (confirmed from source to target the correct, single authoritative marker file - see "Marker
path identity" below - before use), marker verified absent, one controlled reboot, and the
restored state fully verified via `findmnt`/`/proc/self/mountinfo` (never `overlayctl status`,
known unreliable): genuine `overlay` root, read-only lower (`/overlay/robase`), RAM-backed upper
(`/overlay/rwdata`, tmpfs), marker absent, OTA idle, build unchanged, no failed units, no
throttling/undervoltage, every other health component `READY`. Full before/after evidence
preserved separately (boot IDs, `findmnt`, `mountinfo`, marker `stat`, OTA status, settings
snapshots) - available on request, not included verbatim in this document.

One residual, expected, **left deliberately untouched** artifact: `PersistentDataUUID` remains
pinned to `de7e0b63-...` from the abnormal boot. Because a genuine, protected-overlay boot now
correctly reports an *empty* live UUID at that path (proving the overlay does **not** pass through
the lower filesystem's UUID - an earlier hypothesis explored and disproven during this
investigation), `readiness.CertifyPersistentStorage`'s mismatch check now correctly fires,
reporting `Storage: NOT_READY` and dragging `Overall` down - worse than this device's own
pre-incident baseline (`DEGRADED`, for an unrelated, pre-existing `StorageLifecycle` scan-error
condition). This is preserved as evidence, not corrected here, per explicit instruction; it is
listed below as part of the corrective scope.

### Marker path identity (resolved)

Two spellings appear across the codebase: `/overlay/disable` and `/overlay/robase/overlay/disable`.
`stat` proved these are **the same file** (identical device `179,2`, identical inode `22284`) -
`/overlay/robase` is a bind-mount of `/`, so both paths name the same real inode on
`/dev/mmcblk0p2`. `init-overlay` (next-boot check, before any layering exists) reads the bare path;
`overlayctl` (a running userspace context, after layering) reads the `/overlay/robase/...`
spelling. No ambiguity, no divergent behavior - confirmed both from source and by direct `stat`
evidence on the live device.

## What this does NOT fix

The corrective code in this branch adds a **fail-fast validation** (below) - it makes the failure
loud, immediate, and honest instead of silent. It does **not** make OTA updates work again on any
existing device, including this one: since no device running the published image has a genuinely
persistent `/var/lib/stratux-data`, this validation will (correctly) reject every OTA upload until
the image-provisioning gap itself is fixed. That is deliberate - the alternative (leaving the
silent failure in place) is worse. **Do not retry OTA deployment of any feature branch until the
image-provisioning fix below is designed, implemented, and validated separately.**

## Affected scope beyond OTA (explicitly unverified - not claimed as broken)

Every subsystem that writes to `/var/lib/stratux-data` under normal operation makes the same
persistence assumption this incident disproved for the OTA path specifically: calibration
profiles (`main/calprofilesapi.go`), recordings/exports (`main/recordingapi.go`), diagnostics
bundles (`main/diagnosticsapi.go`), power-session state (`main/powerapi.go`), storage-lifecycle
inventory (`storagelifecycle/namespace.go`), Configuration Backup, Wi-Fi admin settings, alert
settings, and Traffic CPA settings. **Whether an ordinary (non-OTA) reboot actually loses this
data has not been tested** - the files observed on the live device during this investigation were
all traceable to writes made during the abnormal bare-ext4 boot (genuinely landing on real disk,
consistent with the root cause) or to genuinely older data whose provenance was not independently
re-verified. This is flagged as a required, separate, careful test - not asserted as a confirmed
second defect - precisely because overclaiming it would be as unsafe as denying it.

## Release implications

This affects every device running the published image, not just this bench unit - the gap is in
image provisioning, not anything specific to this card. Any release/OTA-readiness claim depending
on the OTA mechanism actually completing a real install should be treated as unverified until the
image-provisioning fix lands and is hardware-validated end-to-end (a real staged upload surviving
the disable-reboot-install-reboot cycle on a freshly-flashed image).

## Corrective changes in this branch

1. **`main/ota.go`: `validateStagingPersistence()`**, called at the very start of
   `handleOTAUploadRequest`, before a single byte of the upload body is read. Rejects the upload
   with a clear, bounded, honest `503` error instead of silently accepting a doomed staging write.
   Two layered checks, each using the tool actually suited to it:
   1. `PersistentDataPath` itself must be a genuine, dedicated, non-volatile mount of the exact
      expected filesystem type - `ota.IsDedicatedPersistentMount` (new), which checks whether
      `findmnt`'s own resolved mount *target* for the path equals the path itself. An earlier
      version of this guard instead compared device numbers against `/overlay/robase` (the same
      check already used for the overlay-disable marker) - **that would have incorrectly rejected
      the one currently-known-correct hardware layout**, a dedicated partition with an entirely
      different device number than root's own. Caught and fixed during this workstream's own
      review before merge, not after a second incident.
   2. `otaDir` (a subdirectory of `PersistentDataPath`, never separately mounted itself) must
      share that already-proven device - `ota.IsPersistent`, the right tool for exactly this
      "has a subdirectory been shadowed by something else stacked on top" question, unchanged
      from its existing use for the marker.
2. **`readiness.DiscoverableMount`/`main/autorecordrun.go`'s `autoRecordMountReady`**: both
   one-time/repeated "is the real persistent partition here yet" checks previously trusted
   `FSType == "ext4"` alone. Both now also require `Target == path` (via the same new
   `readiness.MountInfo.Target` field `IsDedicatedPersistentMount` uses) - closing the exact
   mechanism that mis-pinned `PersistentDataUUID` during the incident's own abnormal boot (see
   "PersistentDataUUID: full incident trace" below) at its actual source, not only at the OTA
   staging point.
3. **`image_build/stage2/10-stratux/files/overlayctl`: fixed `overlay_is_active()`** to query the
   live mount type (`findmnt -n -o FSTYPE /`) instead of a leftover directory's mere existence -
   matching `debian/stratux-pre-start.sh`'s own already-correct implementation.

## PersistentDataUUID: full incident trace (Workstream A4)

- **Where it is stored**: `globalSettings.PersistentDataUUID`, persisted via `saveSettings()` into
  `/boot/firmware/stratux.conf` - a real, separate VFAT partition (`/dev/mmcblk0p1`), genuinely
  durable across every reboot this investigation observed, unlike anything under
  `/var/lib/stratux-data` itself.
- **Is the pinned value the root filesystem's own UUID?** Yes, exactly: `de7e0b63-eb97-4c37-a08f-b02e677092a9`
  is `/dev/mmcblk0p2`'s own `blkid`-reported UUID (confirmed directly) - the same single ext4
  partition that backs both `/overlay/robase` and, during the abnormal boot, bare root itself.
- **How it behaves after returning to protected-overlay operation**: `findmnt --target
  /var/lib/stratux-data` now correctly reports an *empty* live UUID (proving overlayfs does not
  pass the lower filesystem's UUID through to an ordinary path reached only via the overlay - a
  hypothesis this investigation explored and disproved). The stored `de7e0b63-...` no longer
  matches, so `readiness.EvaluateStorage`'s existing, correct mismatch handling now fires:
  `Storage: NOT_READY`, dragging `Overall` down from the pre-incident `DEGRADED` baseline.
- **Does it cause false readiness/storage/health results?** Yes, in both directions at different
  times: falsely `READY` before the incident (no UUID pinned yet, so the mismatch check was simply
  never engaged - the actual, original defect this whole document is about), and correctly
  `NOT_READY` now (an honest report of a real problem, not a new bug - the alternative, silently
  clearing or ignoring the mismatch, would recreate the false-`READY` condition by a different
  path).
- **Is it safe to clear automatically?** Not attempted here, and not something this Workstream A
  fix does. Clearing it would let a *future*, still-nonexistent dedicated mount be discovered fresh
  - reasonable in principle - but doing so automatically, today, on this device, without the
  provisioning fix that would ever let a real discovery succeed correctly, would only reproduce the
  identical false-`READY` masking this document exists to end, the moment any future abnormal
  bare-ext4 boot recurs for any reason (this device's own history shows that can happen). The
  now-hardened `DiscoverableMount`/`autoRecordMountReady` checks make a *repeat* mis-pin
  structurally impossible (Target must equal the path, which an abnormal bare-ext4 boot can never
  satisfy for an unprovisioned path) - but the *existing*, already-pinned wrong value on this one
  device is a separate, already-done fact, not something this code change can retroactively
  invalidate.
- **Correct behavior**: leave it exactly as the abnormal boot left it - preserved evidence, and a
  live, honest signal (`Storage: NOT_READY`) that nothing has silently glossed over the fact this
  device still has no genuinely persistent data partition - until the Workstream B provisioning
  fix gives it a real one to discover. This is exactly what was done, under explicit instruction,
  during the incident's own separately-authorized recovery, and is unchanged by this PR.
- **Regression test**: `readiness.TestDiscoverableMount_RejectsAbnormalBareBootFalsePositive`
  (`readiness/storage_test.go`) directly reproduces the incident's own signals (FSType `ext4`, the
  real, valid-looking UUID, everything else structurally perfect) with the one fact that actually
  mattered - `Target == "/"`, not the requested path - and proves the hardened check rejects it.

## Proposed corrective design NOT included as code in this branch (needs its own dedicated effort)

- **Image provisioning**: `/var/lib/stratux-data` should be a genuinely dedicated, separately
  mounted filesystem (a distinct partition with its own `fstab`/systemd `.mount` entry, or an
  explicit early-boot bind-mount of a lower-root subtree performed *before* the overlay is
  constructed) so it is real, persistent storage under normal protected-overlay operation, not
  something that only happens to work during an abnormal bare-ext4 boot. This requires
  partition-table changes to the published image and a migration story for already-flashed cards
  (this exact card's own 29 GB single ext4 partition would need to be resized/split, or the fix
  could instead carve out a bind-mount source directory on the existing partition - either choice
  has real trade-offs affecting every existing installation and deserves its own focused design,
  implementation, and hardware validation before being proposed for merge). Not attempted here:
  authoring partition/format logic without a real opportunity to validate it end-to-end would be
  worse than leaving it as a documented, scoped recommendation.
- **`readiness.CertifyPersistentStorage`**: consider whether the one-time discovery
  (`ensurePersistentDataUUID`) should itself require proof of genuine persistence (the same
  `ota.IsPersistent` check) rather than accepting any `FSType == "ext4"` observation at face value
  - this incident's own abnormal boot shows that condition alone is not sufficient evidence of
  durable persistence across the *normal* operating mode.
- **The already-incorrectly-pinned `PersistentDataUUID`** on this specific device: needs an
  explicit, separate decision (clear it back to empty so the daemon can attempt a legitimate future
  discovery once the provisioning fix lands, or replace it with whatever the corrected
  provisioning produces) - deliberately not touched by this document or its branch.
- **The broader persistence exposure** listed above: a dedicated, careful test plan (a genuine,
  intentional full power-cycle reboot with explicit before/after checks on calibration-profile,
  recording, and diagnostics survival) before any claim is made either way.

## Tests

- **`ota.IsDedicatedPersistentMount`** (`ota/mount_test.go`): dedicated ext4 mount accepted,
  a path only covered by an ancestor mount rejected (the exact incident shape), volatile
  filesystem type rejected even at its own dedicated target, and - the specific bug this
  workstream's own review caught before merge - device number proven irrelevant to the result
  (a same-device bind-mount and a different-device dedicated partition both accepted equally).
- **`ota.StatMount`**: existing real-path and nonexistent-path tests unchanged; added a symlink
  test proving a symlink cannot be used to substitute a volatile destination (`stat`/`findmnt`
  both resolve the real target, not the symlink's own containing directory).
- **`readiness.DiscoverableMount`**: existing tests extended with `Target`; new
  `TestDiscoverableMount_RejectsAbnormalBareBootFalsePositive` reproduces the incident's own exact
  signals and proves the hardened check rejects it.
- **`main` package, `handleOTAUploadRequest`** (`main/otaupload_test.go`, new file): an ordinary
  (non-mounted) directory rejected end-to-end over real HTTP, a missing persistent-data path
  rejected, rejection leaves no staged file and no OTA state mutation (never creates the overlay-
  disable marker or requests a reboot), the rejection error is bounded (no stack trace, no
  unexpected leakage), two concurrent uploads each reject independently (the guard holds no shared
  mutable state to race on), and the pre-existing malformed/empty-multipart-body handling is
  confirmed unchanged.
- **Explicit test-coverage boundary, disclosed rather than overstated**: a full end-to-end
  acceptance test against a *real* mounted filesystem (proving the upload succeeds when
  `otaPersistentDataRoot` is a genuine dedicated ext4 mount) requires `CAP_SYS_ADMIN` to construct
  a real mount - confirmed absent in this test sandbox by direct probe (a plain self bind-mount
  fails with "operation not permitted"). That acceptance path's own *decision logic* is fully
  covered by `IsDedicatedPersistentMount`'s pure unit tests above; the real-mount, end-to-end
  version of that same test is deferred to the loop-device-backed CI tests planned for the
  persistent-storage partition work (Workstream C), which can construct one.
- **`image_build/.../overlayctl`** (`test/overlayctl_test.sh`, new): runs the real script end to
  end against a fake `findmnt` stub on `PATH` - root reported as `overlay` (active), root reported
  as `ext4` (inactive - the exact case the old check got wrong), the same `ext4` case with a stale
  leftover directory present (proving directory existence is no longer consulted at all), a
  failing `findmnt` degrading safely to "inactive" rather than crashing or false-reporting
  "active", and confirms `status` performs no mutation. Shell syntax also verified with `sh -n`.
- Full `go build`/`go vet`/`gofmt`/`go test -race` run clean on `ota`, `readiness`, `main` after
  this change (see PR description for the exact commands and results).

## Safest recovery path (for any device found in this same stuck state)

1. Confirm via `findmnt -T /`/`/proc/self/mountinfo` (not `overlayctl status`) that root is
   genuinely bare ext4.
2. Confirm no OTA/recording/config-restore operation is active.
3. Stop `stratux.service` cleanly.
4. Confirm the marker file's identity (`stat`/`readlink -f` both `/overlay/disable` and
   `/overlay/robase/overlay/disable`) before touching anything.
5. `overlayctl enable`, verify the marker is absent at both paths, `sync`.
6. One controlled reboot.
7. Verify via `findmnt`/`mountinfo` (never `overlayctl status`) that root is genuinely `overlay`
   again, the lower layer is read-only, the upper layer is RAM-backed tmpfs, the marker remains
   absent, OTA is idle, the build is unchanged, and no failed units/throttling/undervoltage appear.
8. Leave `PersistentDataUUID` untouched pending the separate corrective decision above, unless it
   is actively preventing normal startup (it is not, in the case investigated here - only
   `Storage`/`Overall` health reporting is affected).

## Explicitly out of scope for this document and its branch

- No changes to PR #30 (Waveshare e-paper display), PR #15 (FIS-B weather cache), or PR #27 (SSH
  host-key hardening).
- No OTA retry, no manual package installation, no partition changes, no media formatting.
- No merge of this branch without a separate, explicit owner decision - and even then, only after
  the image-provisioning fix is designed, implemented, and independently hardware-validated;
  merging the fail-fast check alone, without that, would correctly but unhelpfully block all OTA
  use until the real fix follows.
