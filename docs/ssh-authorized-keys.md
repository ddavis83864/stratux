# Persistent SSH authorized keys

> **Status: physically validated on the real Stratux Pi** (one Raspberry Pi 4 Model B), after
> off-device validation (helper and packaging tests, real systemd parsing, a Debian 12 lab with a
> real `sshd`, and a package-lifecycle test). The package was installed through the supported OTA,
> a key provisioned on the data partition survived **one real reboot** with no manual re-adding,
> the restore was proven from the boot journal to finish before `ssh.service` started, and live
> revocation worked without restarting `sshd`. See the
> [physical acceptance record](#physical-acceptance-record). What was tested is stated there
> precisely; nothing beyond it is claimed.

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

- **Tested on one Pi, through one reboot.** See the acceptance record for exactly what was exercised.
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

## Physical acceptance record

Owner-authorized acceptance on the real device, using only the supported OTA path. The Pi's
wall clock is untrusted (no GPS fix, no time sync), so boots are identified by boot ID and events
by their relative order and the monotonic clock. Keys are identified by fingerprint only.

| Field | Value |
|---|---|
| Device | Raspberry Pi 4 Model B Rev 1.4, kernel `6.12.109+rpt-rpi-v8`, protected overlay root, data partition `/dev/mmcblk0p3` (ext4) mounted at `/var/lib/stratux-data` |
| Package | `stratux-2.0.0~rc2-arm64.deb`, 86,323,392 bytes, SHA-256 `e611a31a2baf2bc85be1bd7d0116a46c7c747966eaa6acb8cd4efd0baed0ffde`, built by CI on the PR head |
| Embedded commit | `4a03ec08ca56e054245d94c44978bd93cfcc3573`, `vcs.modified=false` (running `Build` equalled it after the OTA) |
| Result | **PASSED** |

**Deployment.** Upload via `POST /updateUpload` returned the exact expected version, commit and
SHA-256. OTA states observed: `idle` -> `staged` -> `disable_requested` -> API unreachable (the
OTA's own reboot into the bare root for the install) -> `idle`, in about two minutes, with no
manual intervention. The boot before the OTA was `b0d54bd4-0f18-4242-8722-d2bb056a699a` and the
boot after it `2ce2604d-ff56-414e-856c-d1a12c5649c7`; the OTA's intermediate boot(s) cannot be
observed from outside (the API and SSH are down) and were not individually recorded.

**Post-OTA gate (before any key was provisioned).** `dpkg --audit` clean, 0 failed units, overlay
active, no disable marker, OTA `idle`, e-paper `READY`, the other Stratux services healthy. The
new files were **`root:root`** on the device (`0755` helper, `0644` unit) although the package
ships them as the CI user id, i.e. the `postinst` ownership fix worked in a real OTA install, and
both were byte-identical to the approved package (unit `e2ffe18d...`, helper `c5100cf9...`).
`systemctl` showed the unit loaded, enabled and `active (exited)`, `Before=ssh.service`,
`After=var-lib-stratux\x2ddata.mount`, `TimeoutStartSec=20s`, `StartLimitIntervalSec=0`, no
`Wants=`/`BindsTo=`/`PartOf=`. With no persistent file the service was a clean no-op (journal:
`no persistent SSH key file ... nothing to restore`), and nothing was created. The effective
`sshd` policy (`StrictModes yes`, password and public-key authentication on, default
`AuthorizedKeysFile`, no `AuthorizedKeysCommand`), `sshd_config` and the three host-key hashes
were identical before and after.

**Provisioning and applying (no reboot).** Over a password login the persistent directory
(`root:root 0755`) and file (`root:root 0644`) were created as documented, holding the owner key
plus one ephemeral throwaway test key (private half only on the test host, shredded afterwards).
`systemctl restart stratux_ssh_authorized_keys` produced `~/.ssh` `pi:pi 0700` and
`authorized_keys` `pi:pi 0600`, byte-identical to the source, with only fingerprints in the
journal; `sshd` was not restarted (same main PID).

**Before the reboot (fresh connections, not an existing session).** A key-only login with each of
the two keys succeeded and the server journal recorded `Accepted publickey` with the matching
fingerprints; a password-only login (no key offered) succeeded; a key that was not provisioned was
refused.

**The one acceptance reboot.** Orderly, via Stratux's reboot endpoint. Pre-reboot boot
`2ce2604d-ff56-414e-856c-d1a12c5649c7`, post-reboot boot `169dbc46-6585-4c21-baf3-61187de6b749`.
The **first action after the Pi returned was a key-only login with the owner key, and it
succeeded on the first attempt**, before anything else connected and without any key being
re-added. The persistent source survived; the volatile target was recreated (its file birth time
is the helper's install time in the boot journal) and was byte-identical to the source.

**Runtime ordering, from the boot journal (Pi clock; relative order is what matters):**

```
05:05:50.430  Mounted var-lib-stratux-data.mount
05:05:51.120  Starting stratux_ssh_authorized_keys.service
05:05:51.984  helper: installed /home/pi/.ssh/authorized_keys from the persistent file (2 key(s))
05:05:52.000  Finished stratux_ssh_authorized_keys.service
05:05:52.022  Starting ssh.service                       (22 ms after the restore finished)
05:05:52.434  sshd: Server listening / Started ssh.service
05:06:07.573  first Accepted publickey (the login above)
```

The monotonic clock agrees: data mount active at 4.47 s, restore 5.16 s to 6.04 s, `ssh.service`
main process at 6.42 s.

**After the reboot.** Password-only login succeeded; `dpkg --audit` clean, 0 failed units, overlay
active, no disable marker, OTA `idle`, the new files still `root:root` with the approved hashes,
e-paper `READY` with 0 consecutive failures and 0 BUSY timeouts, and the `sshd` policy, `sshd_config`
and host keys unchanged. The 1090 ES and UAT radios reported `NOT_INSTALLED` for the first ~2-3
minutes on both boots observed in this test (the post-OTA boot and the post-reboot boot) and then
`READY`; they were `READY` by the end. Their hardware was enumerated and `Devices` matched the
pre-deployment value throughout.

**Live revocation (no reboot, no `sshd` restart).** With an independent recovery session open,
only the throwaway key was revoked: the persistent file was rewritten atomically without it and
only `stratux_ssh_authorized_keys` was restarted. A new connection with the revoked key was
refused (`Connection closed by authenticating user`), the owner key and the password still worked,
the target held exactly one key, `sshd`'s main PID and start timestamp were unchanged, the journal
had no `Stopped ssh.service`, and the recovery session stayed alive. (The owner key was never
removed, so the final state is simply: persistent owner-key access, proven by a fresh login after
the revocation.)

**Not exercised on hardware:** a rejected or unsafe key file on the real device (proven in the lab,
not repeated here to avoid risking access), more than one reboot, other Pi models, and a Pi without
a dedicated data partition. The device's clock was untrusted throughout.

**Observations, not caused by this feature.** A transient under-voltage flag was raised once,
early in the OTA boot (`throttled=0x50000`, not active afterwards and absent after the acceptance
reboot). The package installs its files owned by the CI user id and `/opt/stratux/bin` is owned by
`pi` on the device; this predates the feature and is a separate follow-up.

**Evidence.** Preserved on the data partition under `/var/lib/stratux-data/acceptance/ssh-authorized-keys/`
(existing shutdown-splash evidence untouched; its hashes were re-checked) with a byte-identical
host copy. It holds fingerprints only, with no key bodies, private-key text or credentials
(scanned). Manifest SHA-256 `3680bc09b3c2e42498e4d0ba577eb4073fb94a12e6b784fc94c930a67874c21b`:

```
33b91903d3ffcd3554290ca6c8b0e6257080a3cefb6d4a084fe7681710599e08  01-preboot-unit-and-sshd-journal.txt
0c9b46f0875f4fe3c458a9d67bba56b4a5714b7defc7ff61f7b9a49d35786fff  02-preboot-full-boot-journal.txt
1247cf48a9fb2ad7f1b42d39034bc0ee142d6949e38593019b659ec7e70cf5ee  10-apply-apply-result.txt
fc53aaa703297d9e18aaa2ae02e8406bb54b48374b8947bf413acf9760939dcb  10-post-shell-postota.txt
de29b51a62d5ccbfb6b96570b54da188d05a3fa386497c193432e5a3b2bda0e7  10-prereboot-gate.txt
91cd12f97271c8702fcfb355517d1ca00bf23a27c560283bae3c851331b57cd9  10-prereboot-login-proofs.txt
dde71f7205ba20dbfffbd2a3c0c89a0d093cb256b3bdcb9ce160dfdfe289acde  10-prereboot-sshd-journal.txt
f0ccc80e8f6d30b403780b29a56fa3d838bd07401b6dc4237c75683b895db5d2  10-pre-shell-preflight.txt
2116321a35566cbf31fa929b1d32398135021666124d7d22e97dc60128c26317  10-prov-provision-result.txt
fddbb57754c7f642d03a8cb87e8659d42eda3ea94a87a0077eb6f9ae49dfdd38  20-postreboot-full-boot-journal.txt
650ddf6c89a16b32dd51ea77fdd8042899b338938d3428bfe0fee98c83860c54  21-postreboot-unit-and-sshd-journal.txt
a31da01b6209c73849e291e8a1b7891bd0482ef7cd9a4c5c7da1b47f711198c9  30-postreboot-first-login-attempts.txt
b2d078359992fcb167771450398fbbb7a05423d8fe7388a2efe2d1a2c518e1ad  30-postreboot-health.txt
9e3de963a5fe8607b26df8b1038666b4991dc0830bd364c99ece6d205a3d95dc  30-postreboot-password-login.txt
86d2c231c4e79c7b523ca67594da0623d32819d68f120f7043a4450290a01bc8  30-postreboot-persistence-proof.txt
e36b165f9d305831917b3f9a03585acd8b4e58087cf1f8e582577d9bff4e525a  40-revocation-result.txt
0c8f544eed6bfb78cece1b5375caa6bf2a9b9b60b05a651626935ffb806efbb3  41-final-full-boot-journal.txt
```
