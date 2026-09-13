# Upgrading

This fork upgrades exclusively through its own OTA mechanism — see
[ota.md](ota.md) for the full state machine, overlay-protection design, and recovery
behavior. This page is the short, release-specific "how do I actually upgrade" version.

## From an existing install of this fork

1. Download the exact tagged `.deb` for the release you want (never a workflow-artifact
   build meant for testing) and verify it — see
   [artifact-verification.md](artifact-verification.md).
2. On the dashboard, use the update page to upload the `.deb` (`POST /updateUpload`) — this
   is the *only* supported path. Never `dpkg -i` manually on a live device: it bypasses the
   overlay-unlock/relock sequence and the resumable install state machine `ota.md` describes.
3. The device stages, verifies, unlocks the overlay, installs, and reboots on its own — this
   normally takes two reboot cycles. Do not power-cycle it manually during this window.
4. After it comes back, confirm on the dashboard (or `GET /getStatus`) that the running
   version/commit match what you uploaded.

## From the pre-fork upstream baseline

Same mechanism — the OTA endpoint accepts any well-formed Stratux `.deb`, including one
built from this fork, regardless of what was previously installed. There is no
special "migration" step: this fork's additional settings/state files are created on first
use with safe defaults, and nothing it adds touches or requires a specific prior version to
already be present.

## What is preserved across an upgrade

- Persistent data partition (recordings, exports).
- Last-known-good Wi-Fi configuration (`wifiadmin`'s own store — see
  [wifi-administration-hardening.md](wifi-administration-hardening.md)).
- Calibration profiles, alert settings, Automatic Flight Recording settings.
- Everything Configuration Backup covers is untouched by an OTA upgrade; OTA and
  Configuration Backup are independent mechanisms.

## What is not carried forward automatically

- Nothing in this release requires a schema migration for existing settings — every new
  settings section added since `v2.0-pre5` defaults safely on a device that has never seen
  it before (see each feature's own doc for its specific default).

## Verifying after upgrade

At minimum, confirm: `GET /getStatus` reports the expected version and commit; `GET
/getOTAStatus` reports `idle` with no error; `GET /getWifiAdminStatus` shows your prior
Wi-Fi configuration unchanged; the dashboard's Readiness/Preflight page reports the expected
subsystem health. See this release's own notes (`docs/releases/<version>.md`) for the exact
acceptance checks performed on the release engineer's own hardware.

## If the upgrade fails

See [rollback-recovery.md](rollback-recovery.md).
