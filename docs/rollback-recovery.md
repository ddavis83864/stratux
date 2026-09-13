# Rollback and recovery

Three distinct situations, in increasing order of severity — see [ota.md](ota.md) for the
full technical account behind the first two.

## 1. An OTA update fails during install

Handled automatically. The install state machine backs up the previous package's file-level
and `dpkg` database state before installing, and rolls it back on any detected failure —
including a failure specific to the overlay-unlock/relock handoff itself. No action needed;
check `GET /getOTAStatus` afterward to confirm it settled back to `idle`.

## 2. OTA gets stuck in `failed` or `recovery_required`

As of `v2.0.0-rc1` (PR #22), a stuck `StageFailed` no longer retries forever — it backs off
over up to five attempts (~4m35s total) and then stops in a new terminal
`StageRecoveryExhausted` stage rather than looping indefinitely.

To recover: `POST /resetOTA`. It is guarded (refuses with `409` if an update is genuinely
still mid-flight), idempotent (a repeated call once idle is a no-op), and preserves the
pre-reset state to a timestamped file before clearing it — nothing about a prior failed
attempt is silently discarded, and it never deletes staged/backup artifacts. This was not
available before this release; on an older build, the only recovery was manually stopping
the service and removing `state.json` over SSH — do not do that on this release, use the
supported endpoint instead.

## 3. Total failure — the device won't boot, or a clean reinstall is needed

This is outside what OTA or `/resetOTA` can fix (a corrupted SD card, a failed OS-level
partition, or starting over intentionally). Recovery here means re-flashing:

1. Get the exact release image you want to run (this release's own `.img.xz` from its
   GitHub Release, verified per [artifact-verification.md](artifact-verification.md)), or
   your own previously-made backup of your own device.
2. Write it to the SD card with a standard imaging tool (`dd`, Raspberry Pi Imager,
   balenaEtcher, etc.) — see [clean-install-guide.md](clean-install-guide.md) for the exact
   verified procedure this project uses.
3. Boot from it. A clean-install image starts with default settings (no calibration
   profile, no recordings, default Wi-Fi) — restore your own configuration afterward via
   [Configuration Backup](configuration-backup-restore.md) if you have an export, and
   re-run AHRS calibration for your airframe.

**Making your own recovery backup is your responsibility and is not part of this release
process.** If you clone your own SD card as a personal backup before experimenting, store it
somewhere private — a full device image can contain your own recordings, Wi-Fi
configuration, and calibration data. Never attach a personal device backup to a public
report, issue, or release.
