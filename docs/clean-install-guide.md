# Clean install from the public image

The public `stratux-<version>.img.xz` is built entirely fresh via `pi-gen` (the official
Raspberry Pi OS image builder, used as a git submodule — see
[release-process.md](release-process.md)) plus this project's own `image_build/stage2`
customization layer. It is **never** derived from any personal/private device backup — see
this release's own notes for the exact sanitization checks performed against it before
publication.

## What you need

- A microSD card (check this release's notes for the minimum size).
- A way to write an image: `dd`, [Raspberry Pi
  Imager](https://www.raspberrypi.com/software/), balenaEtcher, or equivalent.
- The exact `.img.xz` and `SHA256SUMS` from this release's GitHub Release — verify both per
  [artifact-verification.md](artifact-verification.md) before writing.

## Writing the image

**This erases the entire target card. Double- and triple-check the target device path —
there is no undo.**

```sh
xz -d -k stratux-<version>.img.xz          # -k keeps the compressed original
# Identify your card FIRST - lsblk, diskutil list, or your OS's own disk utility.
# NEVER guess the device path.
sudo dd if=stratux-<version>.img of=/dev/sdX bs=4M status=progress conv=fsync
sync
```

Or use Raspberry Pi Imager / balenaEtcher's own "write a custom image" option with the
decompressed (or, if supported, the `.xz` directly) file.

## First boot

- Wi-Fi: `Stratux`, open (no password), channel 1, `192.168.10.1` — the project's
  long-standing default, unless this release's notes say otherwise.
- No active AHRS calibration profile, no recordings, no diagnostics, no cached weather, no
  OTA residue — a genuinely clean starting state.
- SSH is enabled with the standard `pi-gen` default credentials (see
  [known-limitations.md](known-limitations.md)) — change this before trusting the device on
  any network you don't control.
- The persistent-data partition initializes on first boot; give it a minute before expecting
  recordings/exports to be available.

## After first boot

1. Confirm the version/commit via the dashboard or `GET /getStatus` matches what you wrote.
2. Set your own Wi-Fi SSID/security through the **Wi-Fi Admin** page (never the legacy
   `/setSettings` path, which no longer accepts Wi-Fi fields as of this release — see
   [wifi-administration-hardening.md](wifi-administration-hardening.md)).
3. Run AHRS calibration for your specific airframe/mounting before trusting attitude output.
4. If you have a Configuration Backup export from a prior device, restore it now (radio
   enablement, alert thresholds, calibration profiles — never credentials, which
   Configuration Backup never touches).

## Recovering your own device from your own backup

This guide covers the **public** image only. If you are restoring your own personal device
from your own previously-made backup image, that is a separate, private workflow — see
[rollback-recovery.md](rollback-recovery.md)'s "total failure" section. Never use a personal
device backup as a substitute for the public image, and never publish or attach a personal
device backup anywhere public.
