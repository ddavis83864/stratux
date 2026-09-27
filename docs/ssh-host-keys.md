# SSH host key lifecycle

This document describes how this fork generates, preserves, and (does not) rotate SSH *host*
keys — the keys that establish a device's own identity to an SSH client (`ssh_host_rsa_key`,
`ssh_host_ecdsa_key`, `ssh_host_ed25519_key` under `/etc/ssh/`). It is not about SSH *user*
keys (`authorized_keys`, which grant client access) — those are a separate, unrelated
mechanism this document does not cover.

## The defect this fixes

`v2.0.0-rc2`'s image build generated a real SSH host key set once, at *image-build* time
(`ssh-keygen -A` inside `image_build/stage2/10-stratux/01-run.sh`), then disabled the systemd
unit (`regenerate_ssh_host_keys`) that would otherwise have generated a fresh set on each
device's own first boot. The practical effect: every device ever flashed from that exact
published image file shared byte-for-byte identical SSH host keys — confirmed by direct,
read-only inspection of the published `v2.0.0-rc2.img.xz` (loop-mounted, host key files
present and non-empty at image-build timestamps; `regenerate_ssh_host_keys.service` present
but disabled). Shared host keys defeat the entire point of host-key verification: an attacker
who obtains one device's private keys (e.g. by buying a retail unit, or from a leaked image)
can silently impersonate *every* device built from that image to any client that trusts it.

## Why the stock Raspberry Pi OS mechanism can't just be re-enabled

Simply undoing the `systemctl disable regenerate_ssh_host_keys` line does not work on this
image. This fork's root filesystem runs behind a protected, read-only overlay
(`image_build/stage2/10-stratux/files/init-overlay`; see [ota.md](ota.md) for the full
mechanics), whose writable upper layer is **tmpfs** and is discarded on every reboot. The
stock `regenerate_ssh_host_keys` script writes new host keys — and its own self-disable — via
the plain `/etc/...` path, which under this overlay lands only in that volatile tmpfs layer.
The result would not be "keys are regenerated once, safely" but "keys are regenerated *from
scratch, differently, on every single boot*" — worse than sharing one static key set, since it
also breaks every SSH client's own `known_hosts` trust on every reboot.

## The fix: `stratux-ssh-hostkeys.service`

- The image build no longer generates any SSH host key material at all (`ssh-keygen -A -v` was
  removed from `01-run.sh`). A clean-install image ships with **no** files under
  `/etc/ssh/ssh_host_*`.
- A new systemd oneshot unit, `stratux-ssh-hostkeys.service`
  (`image_build/stage2/10-stratux/files/stratux-ssh-hostkeys.service`, script
  `.../files/stratux-generate-ssh-hostkeys`), runs on every boot with `Before=ssh.service
  sshd.service` — sshd is never started before this unit's start job has completed.
  - If no complete host key set exists yet at the persistent location, it unlocks the overlay
    (`overlayctl unlock`), runs `ssh-keygen -A -v -f /overlay/robase` — writing the generated
    keys directly into the real ext4 lower layer at `/overlay/robase/etc/ssh/...`, the same
    persistence path this codebase already uses for every other `/etc` change that must
    survive a reboot (see `main/networksettings.go`, `stratux-pre-start.sh`'s timezone fix) —
    then relocks the overlay (`overlayctl lock`).
  - If a complete host key set already exists at that persistent location, the script exits
    immediately without ever unlocking the overlay. This check is a plain file-existence test
    performed before any write, so an ordinary boot on an already-initialized device never
    remounts the protected filesystem read-write at all.
  - The actual cryptographic generation is unmodified, standard `ssh-keygen -A`: per its own
    documented behavior it fills in only whichever default key types are missing and never
    touches one that already exists. This makes the script inherently safe to re-run after an
    interrupted or partial prior attempt (power loss, killed process mid-generation) — the
    next boot's run simply completes whatever is missing, with no custom recovery logic
    needed.
  - This script and unit ship **only** as part of the image build
    (`image_build/stage2/10-stratux/`) — not in the `.deb` package. An already-configured
    device upgrading via OTA never installs or runs any of this, so the OTA path cannot be a
    vector for touching an existing device's host keys, by construction.

## Clean install

A device flashed from a corrected image and booted for the first time:

1. Boots with no `/etc/ssh/ssh_host_*` files at all.
2. `stratux-ssh-hostkeys.service` runs before `ssh.service`, generates a full key set unique
   to that boot, and persists it through the overlay's lower layer.
3. `ssh.service` starts only after that ordering point and finds real, complete host keys —
   sshd's own inherent requirement for at least one host key means if generation had failed
   entirely, sshd would fail to start too, surfacing honestly in `systemctl --failed` rather
   than silently accepting connections with a false sense of identity.
4. On every subsequent boot, the pre-check finds the existing key set and does nothing further
   — the overlay is never unlocked again for this purpose, and the keys are never replaced.
5. A second, independently flashed device from the exact same image produces different key
   fingerprints on its own first boot, because each device performs its own independent
   `ssh-keygen -A` invocation — there is no shared build-time seed anymore.

## OTA upgrade of an already-configured device

An existing, already-initialized device's SSH host keys are never touched by an OTA (`.deb`)
upgrade:

- `stratux-ssh-hostkeys.service` and its script are not part of the `.deb` package at all —
  OTA-installing a new `stratux` version does not add, remove, enable, or run anything related
  to host keys.
- Nothing in the OTA install/rollback state machine (`debian/stratux-pre-start.sh`, `ota/`)
  reads or writes `/etc/ssh/ssh_host_*` or `/overlay/robase/etc/ssh/*` in any code path.
- A rollback to a prior version, for the same reason, does not rotate host keys either.

This applies whether the device was originally flashed from an image built before or after
this fix — an existing device already has *some* host key set (unique, if flashed from a
corrected image; shared, if flashed from `v2.0.0-rc2` or earlier), and OTA upgrading it never
regenerates, replaces, or deletes that set either way.

### If you already have a `v2.0.0-rc2`-flashed device and want unique keys now

This fix does not retroactively rotate any existing device's keys — doing so automatically,
without the owner's own action, is deliberately out of scope (see "Not implemented" below). If
your device was flashed from `v2.0.0-rc2` or earlier and you want to stop sharing host keys
with every other device from that image, you can regenerate them yourself over SSH:

```sh
sudo /sbin/overlayctl unlock
sudo rm -f /etc/ssh/ssh_host_*
sudo ssh-keygen -A -f /overlay/robase
sudo sync
sudo /sbin/overlayctl lock
sudo reboot
```

This will invalidate any SSH client's existing `known_hosts` entry for the device (expected —
delete the old entry and re-verify the new fingerprint on next connect, the same as any other
legitimate host-key change).

## Recovery, reflash, and cloning — stated honestly

- **A fresh reflash from a corrected published image** gets a new, independent host key set on
  its own next first boot, exactly like any other clean install.
- **Restoring a full raw private card image** (e.g. `stratux-original-*.img`, a byte-for-byte
  backup of a specific, already-initialized card) intentionally restores *that exact image's*
  full identity, including its SSH host keys. This is correct, expected behavior for a
  restore, not a defect — but it means two cards, each independently restored from the same
  raw backup image, will again share identical host keys with each other, for the same reason
  the original defect existed in the first place. If you restore a raw backup onto more than
  one card, treat those cards the same way you would two devices flashed from the same
  unfixed public image.
- **Cloning an already-configured card** (e.g. `dd`-copying one working card to another) also
  clones its full host identity, including SSH host keys, for the same reason. Two clones of
  the same source card must not be run simultaneously on a network where an SSH client could
  connect to either — they are indistinguishable by host key. If you intentionally maintain a
  cloned spare, regenerate its host keys (see the manual procedure above) before ever running
  it alongside the original.

## A defect physical validation found (and fixed) before this shipped

Automated tests alone did not prove this design worked: `scripts/test-ssh-hostkeys.sh` stubs
`overlayctl` out entirely, so it only ever proved the generator script's own control flow was
correct, never that the real `overlayctl unlock`/`lock` mechanism actually succeeds on a real
device. Booting the corrected image on real hardware found that it didn't, every time:

`stratux-ssh-hostkeys.service` failed on first boot with `mount: /overlay/robase: mount point
is busy` the moment it tried to relock the overlay after generating keys — key generation
itself had already succeeded (keys existed at both `/overlay/robase/etc/ssh/...` and the live
`/etc/ssh/...` view, confirmed by matching device/inode numbers and content hashes, proving the
overlay's live/durable path relationship itself works exactly as designed), but the unit still
ended up `failed` because the final `overlayctl lock` call errored out.

Root cause: `/overlay/robase` is a bind mount of the same block device that `init-overlay`'s own
`pivot_root` leaves separately mounted at `/overlay/pivot` (the relocated old root). `overlayctl`
locked/unlocked it with a plain `mount -o remount,ro|rw` — but per `mount(8)`, changing a bind
mount's own flags requires including `bind` in the remount, otherwise the kernel treats the
request as targeting the whole shared superblock. Going to read-write plain-remounts the whole
superblock (works fine, and must stay this way — a bind remount cannot escalate a mount that
inherited a locked-read-only flag from a still-read-only source, which matters on a device's
very first boot). Going back to read-only that same way conflicts with `/overlay/pivot` still
holding a live reference to that superblock, and fails `EBUSY`. Fixed by using a bind remount
specifically for the read-only direction (`overlayctl`'s `lock`, and `enable`/`disable`'s own
internal re-lock step) while leaving the read-write direction a plain remount. Confirmed on real
hardware: first boot completes, keys generate and persist, and a subsequent reboot leaves them
unchanged.

This fix lives in `overlayctl` itself (used by every other caller of `overlayctl unlock`/`lock`
in this codebase, not just this feature) — see `scripts/test-overlayctl-remount.sh` for the
regression coverage.

## A second physical finding, and a correction to the original diagnosis

Repeating physical validation with the overlay fix in place surfaced a second issue: on the very
first bare-metal boot of a genuinely fresh clean-install card, `ssh.service` sometimes did not
become reachable for many minutes, while every other subsystem (the dashboard, DNS, GDL90/traffic
ports) came up normally and stayed healthy the whole time.

The first hypothesis was entropy starvation: `ssh-keygen -A`'s underlying `getrandom()` call
blocking until the kernel's CRNG has enough real entropy to seed itself, a known characteristic of
embedded ARM boards very early in their first boot. This project's base image ships `rng-tools`
support but doesn't install it by default, so `rng-tools5` was installed and enabled
(`image_build/stage2/10-stratux/01-run.sh`), and `stratux-ssh-hostkeys.service` was given an
explicit `After=` ordering on the entropy daemon so `ssh-keygen -A` could never race its startup.

**Re-testing on real hardware with both of those fixes in place found the exact same delay,
unchanged.** That result disproves entropy starvation as the explanation - if it were the cause,
guaranteeing the entropy daemon starts first would have measurably helped. Capturing
`systemd-analyze critical-chain`, monotonic `journalctl` timestamps, and the state of
`/var/grow_root_part` and the root partition's own size on a boot immediately following the delay
found the real cause: this project's pre-existing, unrelated first-boot behavior
(`image_build/stage2/10-stratux/files/init-overlay`) grows the root partition to fill the card and
then unconditionally `reboot -f`s, every time `/var/grow_root_part` is still present - which it
always is on a device's true first boot, regardless of anything this fix changes. A `resize2fs` of
a large partition on a real SD card is genuinely slow, and the delay observed was that first-boot
grow-and-reboot cycle running its course, not `stratux-ssh-hostkeys.service` hanging. A boot
captured immediately afterward, with `/var/grow_root_part` already gone and the root partition
already at its full grown size, completed in 9.4 seconds total -
`stratux-ssh-hostkeys.service` included, ordered correctly after `rngd.service`, `ssh.service`
listening within the same second, zero failed units.

This means the delay is **not a defect in this fix** and is **out of this PR's own scope** to
correct - it is a pre-existing characteristic of `init-overlay`'s own partition-growth mechanism,
present before this fix existed and unrelated to SSH host keys. The `rng-tools5` install and the
`After=` ordering are kept anyway: guaranteeing hardware entropy is available before this security-
sensitive key generation runs is independently good practice, verified as harmless (CI green,
9.4-second clean boot with both in place), even though neither was the actual fix for the delay
originally observed. A device's first-ever boot after flashing a clean-install image should be
expected to take longer than any subsequent boot, for reasons that have nothing to do with SSH.

## Not implemented (deliberately, this release)

- **No automatic key-rotation dashboard control.** Regenerating host keys on an existing,
  already-initialized device is a manual, owner-initiated action (see above) — not a settings
  toggle or API endpoint. Automating this safely (especially interaction with SSH clients'
  cached `known_hosts` state, and making sure it can never accidentally run on every boot) is
  a larger design question left out of this fix's narrow scope.
- **No retroactive fix for already-deployed `v2.0.0-rc2` devices.** This fix changes what a
  *newly built* image ships; it does not reach out and correct a device that was already
  flashed from an older image. See the manual procedure above if you want to fix an existing
  device yourself.
