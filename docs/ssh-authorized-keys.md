# Persistent SSH authorized keys

> **Status: implemented and validated off-device only** (unit and helper tests, real
> systemd parsing, and a Debian 12 lab with a real `sshd`). **It has not been installed
> on a Stratux Pi and has not been physically validated.** Physical acceptance (an OTA
> install and a real reboot) is a separate, owner-controlled step.

## Why `~/.ssh/authorized_keys` disappears at reboot

Stratux runs from a protected, read-only root. `/sbin/init-overlay` mounts the real
ext4 root read-only as the lower layer and puts a **RAM `tmpfs`** (`size=250m`) on top
as the writable layer, so that a power cut can never corrupt the system. Everything
written under `/` - including `/home/pi/.ssh/authorized_keys` - lands in that tmpfs and
is discarded at the next boot. On a live device the key exists only at
`/overlay/rwdata/data/home/pi/.ssh/authorized_keys`, and the ext4 lower layer has no
`.ssh` directory at all. Nothing is deleting the key; this is the overlay working as
designed. The login is `pi` (uid 1000, home `/home/pi`), and `sshd` runs with
`StrictModes yes`, so the permissions were never the problem.

## What this feature does

The administrator's public keys are kept on the dedicated persistent data partition, and
a small boot-time service copies them back into the volatile home before `sshd` starts.

| | Path | Notes |
|---|---|---|
| **Persistent source** (authoritative) | `/var/lib/stratux-data/ssh/authorized_keys` | Owned by root. Survives reboot, OTA, package upgrade and removal. |
| **Volatile target** | `/home/pi/.ssh/authorized_keys` | Recreated at every boot. `~/.ssh` is `pi:pi 0700`, the file is `pi:pi 0600`. |
| Service | `stratux_ssh_authorized_keys.service` | `Type=oneshot`, `RemainAfterExit=yes`. |
| Helper | `/opt/stratux/bin/stratux-ssh-authorized-keys.sh` | Does the validation and the atomic install. |

The file uses ordinary OpenSSH `authorized_keys` syntax: one key per line, comments (`#`)
and blank lines, and optional leading options (`from="..."`, `command="..."`,
`restrict`, `cert-authority`, ...). It is copied byte for byte.

## Provisioning a key

Do this once per device, over a password login or any existing session. First confirm the
data partition is really mounted; if it is not, anything you write would be in RAM and
would not persist (the helper will refuse to use it):

```sh
mountpoint /var/lib/stratux-data          # must say: ... is a mountpoint
```

Then, using **your own public key** (never a private key; a placeholder is shown):

```sh
# from your workstation: copy the public key over
scp ~/.ssh/id_ed25519.pub pi@<stratux-address>:/tmp/admin-key.pub

# on the Stratux
sudo install -d -m 0755 -o root -g root /var/lib/stratux-data/ssh
sudo sh -c 'cat /tmp/admin-key.pub >> /var/lib/stratux-data/ssh/authorized_keys'
sudo chown root:root /var/lib/stratux-data/ssh/authorized_keys
sudo chmod 0644 /var/lib/stratux-data/ssh/authorized_keys
rm /tmp/admin-key.pub
sudo systemctl restart stratux_ssh_authorized_keys.service      # apply now, no reboot
journalctl -u stratux_ssh_authorized_keys --no-pager -n 20      # shows the fingerprint(s) installed
```

The file's contents look like this (a fake, clearly-marked example):

```text
# administrator keys
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLEEXAMPLEEXAMPLEEXAMPLEEXAMPLEEXAMPLE0000 admin@example
from="192.168.10.0/24" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLEEXAMPLEEXAMPLEEXAMPLEEXAMPLEEXAMPLE1111 laptop@example
```

## Applying changes, and revoking a key, without a reboot

Edit the persistent file, then re-run the service:

```sh
sudo systemctl restart stratux_ssh_authorized_keys.service
```

The persistent file is authoritative **when it exists and is valid**: the volatile file is
replaced with exactly its contents. To revoke a key, delete its line and restart the
service; the key stops working immediately, without restarting `sshd` or rebooting.

- To revoke **every** key on purpose, leave a comment line such as `# no keys` in the file.
  A **zero-byte** file is deliberately ignored with a warning, because that is almost
  always a truncated write rather than an intention to lock everyone out, and ignoring it
  keeps the keys that are currently active.
- **Removing the persistent file altogether does not revoke anything.** With no source
  file the service does nothing at all (see below). To revoke while the device is running,
  use the comment-line method; any key that was only in RAM disappears at the next reboot
  anyway.

## Behavior in each situation

| Situation | What happens |
|---|---|
| **No persistent file** (a device where nothing was ever provisioned) | A normal no-op, exit 0. Nothing is created, no error, and any keys already in `~/.ssh` are left alone. The device behaves exactly as it did before this feature. |
| **Valid file** | Validated, installed atomically, `sshd` accepts the keys. |
| **Unsafe file** (not root-owned, group- or world-writable, a symlink, not a regular file, more than one hard link, in an unsafe directory) | Rejected as a whole. The current `~/.ssh/authorized_keys` is left untouched, a clear error is logged, and the unit shows **failed** (visible in `systemctl --failed` and the Stratux health page). `sshd` still starts. |
| **Malformed entry** (or private-key material, control characters, more than 256 entries or 64 KiB) | Rejected as a whole in the same way; the error names the line number, never the key. |
| **Data partition not mounted, missing, or its mount fails** | The service is skipped by its `ConditionPathIsMountPoint` (and the helper independently refuses a same-named directory in the RAM root), so a directory that merely *looks* like the persistent copy is never trusted. `sshd` starts normally. |
| **Data partition mounts late** | The service is ordered `After=` the data mount, so it waits for it, and `Before=ssh.service`, so keys are in place before any login. |
| **Helper hangs** | Bounded by `TimeoutStartSec=20`; it is killed, the unit fails, and `sshd` starts. The helper also enforces a 10 s validation budget of its own. |

Failure always fails toward **recoverability**: the service has no `Requires=`, `Wants=` or
similar relationship with `ssh.service` in either direction, so a problem here can never
stop `sshd`. Password login (the shipped default) remains available to recover.

## Security model

- The source must be **owned by root** and **not writable by group or other**, and so must
  its `ssh/` directory. Otherwise a non-root user could choose who is allowed to log in.
- The file is **opened once** and every check (regular file, owner, mode, link count) is
  made on the open file descriptor and the content is read from that same descriptor, so
  the file that was checked is the file that is installed. Symbolic links are never followed.
- Entries are validated with `ssh-keygen -l`, which understands the full `authorized_keys`
  syntax including leading options (it does not validate option **names**; `sshd` itself
  skips an entry with bad options when someone tries to log in). Public keys are never
  printed to the journal, only fingerprints. A file containing private-key material is
  rejected.
- The install is **atomic**: a private temporary file with the final owner and mode is
  written in `~/.ssh` and renamed into place, so a failure at any step leaves the previous
  file intact.
- This feature **does not change any SSH policy**: `sshd_config`, `StrictModes`, host keys,
  root login, password authentication and `AuthorizedKeysFile` are untouched. There is
  intentionally **no API or web-UI way to add a key**: the Stratux API has no authentication
  and the Wi-Fi network is open, so such a channel would give anyone in range shell access.
- No key of any kind is committed to the repository. Tests generate ephemeral keys.

## Packaging, OTA, upgrade, removal and reflash

- The unit and helper ship in the normal Stratux `.deb`. `postinst` enables the unit
  **before** its `STRATUX_OTA_INSTALL` early-exit, exactly like the e-paper units, so an
  OTA-delivered install enables it; on the non-OTA path it is also started. The OTA
  mechanism itself is unchanged.
- `postinst` also sets the installed helper and unit to `root:root`. The package is built
  under a CI user id, and the helper runs as root at boot and decides who may log in.
  (The `/opt/stratux/bin` directory itself is owned by `pi` on the current image; that is a
  pre-existing property of the packaging, is not changed here, and is worth a separate
  review.)
- **OTA and upgrade:** the persistent file is on the data partition, which OTA never
  touches, so keys survive. `prerm` only disables the unit on a real *removal*, never on
  an upgrade.
- **Removal:** the unit, helper and enablement are removed. The administrator's
  `/var/lib/stratux-data/ssh/authorized_keys` is **kept**: it is persistent administrator
  data, and package removal does not destroy that. A copy already restored into RAM stays
  until the next reboot.
- **Reflash / factory reset:** a freshly flashed image creates a new data partition, so the
  keys are gone and must be provisioned again. Configuration backups never contain SSH
  material.
- **Devices without a dedicated data partition** (older installs; see
  `persistent-data-partition.md`) get no persistence from this feature; the helper does
  nothing there.

## Recovery and rollback

- **Locked out or a key rejected:** log in with the password, then
  `journalctl -u stratux_ssh_authorized_keys` tells you why (unsafe permissions, a
  malformed line, partition not mounted).
- **Stop using the feature:** `sudo systemctl disable --now stratux_ssh_authorized_keys`
  and, if you wish, remove `/var/lib/stratux-data/ssh/authorized_keys`. Note that
  `systemctl disable` made by hand persists only in the overlay's RAM layer on this image
  (see `ota-persistent-storage-defect.md`); removing the persistent file is the reliable
  way to turn the feature off.
- **Back out the whole change:** install the previous package via OTA; the data-partition
  file is left in place and is simply not used.

## Known limitations

- **Not physically validated.** Only off-device validation exists (see below).
- Option names inside an entry are not validated (see above).
- A shell helper cannot fully eliminate races on the *target* directory; the service runs
  before `sshd` and before anyone can log in at boot, and the residual window on a manual
  restart is only relevant to a user who already has `sudo`.
- A rejected key file makes the unit **failed**, which the Stratux health page reports as
  a failed service; that is intentional visibility.
- Password login is enabled by default (see `known-limitations.md`); this feature does not
  change that.

## Validation

Off-device only, all runnable from the repository:

```sh
bash test/ssh_authorized_keys_test.sh            # helper behavior, unprivileged, ephemeral keys
bash test/ssh_authorized_keys_packaging_test.sh  # unit structure, maintainer scripts, systemd-analyze verify
bash test/ssh_authorized_keys_systemd_lab.sh     # opt-in: Debian 12 + real sshd + real systemd in Docker
bash test/epaper_packaging_test.sh               # existing test, extended for the new enable line
```

The lab uses Stratux's own `sshd_config` (so `StrictModes` is genuinely enforced), a real
`pi` user, a real mount point for the data partition, and a tmpfs home. It simulates a
reboot with a real systemd restart in which the tmpfs home comes back empty and the data
volume persists, and includes controls proving that the test can fail (the key is lost
without the feature; the key is not restored without the `After=` ordering edge).
It does **not** reproduce the Pi's overlayfs, its SD-card mount timing, or a real power
cycle.

## Physical acceptance still required

1. Install the package on the Pi **only through the supported OTA**, and confirm
   `systemctl is-enabled stratux_ssh_authorized_keys` and `systemctl cat` show the unit.
2. Provision a key on the data partition as above and restart the service; confirm a
   key login works.
3. **Reboot once with a second session kept open**, then confirm the key login works
   without re-adding anything, and that `journalctl -b -u stratux_ssh_authorized_keys`
   shows the restore finishing before `ssh.service` started.
4. Confirm password login still works, and that revoking the key (edit, restart the
   service) stops it working without a reboot.
