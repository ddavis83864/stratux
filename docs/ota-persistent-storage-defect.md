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
   `handleOTAUploadRequest`, before a single byte of the upload body is read. Reuses the existing,
   already hardware-proven `ota.IsPersistent`/`ota.StatMount` device-identity check (previously
   applied only to the overlay-disable marker) against `otaDir`. Rejects the upload with a clear,
   honest `503` error instead of silently accepting a doomed staging write.
2. **`image_build/stage2/10-stratux/files/overlayctl`: fixed `overlay_is_active()`** to query the
   live mount type (`findmnt -n -o FSTYPE /`) instead of a leftover directory's mere existence -
   matching `debian/stratux-pre-start.sh`'s own already-correct implementation.

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

- `ota` package: existing `ota.IsPersistent`/`ota.StatMount` tests (`ota/mount_test.go`) already
  cover the pure logic this fix reuses - unchanged, still passing.
- `validateStagingPersistence` itself is real syscall-touching glue (like its sibling
  `requestOverlayDisable`, which has never had its own direct unit test in this codebase) - its
  correctness rests on the already-tested pure `ota.IsPersistent` plus the hardware validation
  performed live during this investigation (see above), matching this codebase's existing
  convention for this exact class of function.
- `image_build/.../overlayctl`: shell syntax verified (`sh -n`); the corrected `overlay_is_active`
  was validated conceptually against the same live evidence that exposed the original bug (a
  genuine bare-ext4 boot now correctly reports "not active" using the new check, whereas the old
  check would still have reported "active").
- Full `go build`/`go vet`/`gofmt`/`go test -race` run clean on `ota`, `main` after this change
  (see PR description for the exact commands and results).

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
