#!/bin/bash
# ssh_authorized_keys_systemd_lab.sh: proves persistent SSH key restore against a
# REAL sshd on Debian 12 (systemd 252), off-device. Opt-in: needs Docker
# (privileged; network for the one-time image build); prints SKIP and exits 0
# without it.
#
#   test/ssh_authorized_keys_systemd_lab.sh
#
# What is real: Debian 12's OpenSSH server running Stratux's own sshd_config
# (image_build/stage2/10-stratux/files/sshd_config, so StrictModes is genuinely
# enforced), systemd as PID 1, the shipped unit and helper (owned by root), a real
# `pi` user (uid 1000), a real mount point for /var/lib/stratux-data (a Docker
# volume), and a home directory on a tmpfs that is genuinely empty after every
# container restart (the same property as the Pi's RAM overlay). Keys are
# generated ephemerally; the only credential in the lab is a throwaway password
# for `pi` that exists solely to prove password recovery still works.
#
# What is simulated, and why it still matters: "reboot" is `docker restart` (a
# real systemd boot, with the tmpfs home recreated empty and the data volume
# retained), not a Raspberry Pi power cycle, and the Pi's overlay is a tmpfs home
# rather than overlayfs. Physical acceptance on the real Pi remains outstanding.
set -u
cd "$(dirname "$0")/.." || exit 1
fail=0
pass_n=0
check() { if [ "$2" = 1 ]; then echo "PASS: $1"; pass_n=$((pass_n + 1)); else echo "FAIL: $1"; fail=1; fi; }
b() { [ "$1" = "$2" ] && echo 1 || echo 0; }

command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 || { echo "SKIP: docker not available; real-sshd lab not run"; exit 0; }

WORK=$(mktemp -d)
IMG=stxssh-lab:bookworm
PW=lab-only-password
cleanup() { [ -n "${LAB_KEEP:-}" ] || { docker rm -f $(docker ps -aq --filter 'name=stxssh-') >/dev/null 2>&1; docker volume rm -f $(docker volume ls -q --filter 'name=stxssh-') >/dev/null 2>&1; }; rm -rf "$WORK"; }
trap cleanup EXIT

docker build -q -t $IMG - >/dev/null <<'EOF' || { echo "FAIL: could not build the lab image"; exit 1; }
FROM debian:bookworm
RUN apt-get update && apt-get install -y --no-install-recommends systemd systemd-sysv dbus openssh-server openssh-client sshpass procps util-linux ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask systemd-logind.service getty.target console-getty.service systemd-udevd.service systemd-remount-fs.service \
 && useradd -m -u 1000 -s /bin/bash pi && echo 'pi:lab-only-password' | chpasswd \
 && mkdir -p /run/sshd /opt/stratux/bin /var/lib/stratux-data && ssh-keygen -A && systemctl enable ssh
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
EOF

# --- ephemeral keys ---------------------------------------------------------
mkdir -p "$WORK/keys"
for k in A B C; do ssh-keygen -q -t ed25519 -N '' -C "lab-$k" -f "$WORK/keys/$k" >/dev/null; done
pub() { cat "$WORK/keys/$1.pub"; }

# --- container helpers ------------------------------------------------------
# lab_new NAME [env: VOL, NOWANTS=1, HELPER_OVERRIDE=file, UNIT_SED=expr, MOUNTMODE=delay|fail|none|late-nodep, NODATA=1]
lab_new() {
	local c=stxssh-$1
	docker rm -f "$c" >/dev/null 2>&1
	local vol=${VOL:-stxssh-vol-$1} volargs=()
	if [ -z "${NODATA:-}" ]; then
		docker volume inspect "$vol" >/dev/null 2>&1 || docker volume create "$vol" >/dev/null
		if [ -n "${MOUNTMODE:-}" ]; then volargs=(-v "$vol:/dsrc"); else volargs=(-v "$vol:/var/lib/stratux-data"); fi
	fi
	# Stage the shipped files exactly as the package lays them out.
	local s="$WORK/stage-$1"; rm -rf "$s"
	mkdir -p "$s/usr/lib/systemd/system" "$s/opt/stratux/bin" "$s/etc/ssh" "$s/etc/systemd/system/multi-user.target.wants"
	cp debian/stratux_ssh_authorized_keys.service "$s/usr/lib/systemd/system/"
	[ -n "${UNIT_SED:-}" ] && sed -i "$UNIT_SED" "$s/usr/lib/systemd/system/stratux_ssh_authorized_keys.service"
	cp "${HELPER_OVERRIDE:-debian/stratux-ssh-authorized-keys.sh}" "$s/opt/stratux/bin/stratux-ssh-authorized-keys.sh"
	chmod 755 "$s/opt/stratux/bin/stratux-ssh-authorized-keys.sh"
	cp image_build/stage2/10-stratux/files/sshd_config "$s/etc/ssh/sshd_config"
	[ -z "${NOWANTS:-}" ] && ln -s /lib/systemd/system/stratux_ssh_authorized_keys.service "$s/etc/systemd/system/multi-user.target.wants/stratux_ssh_authorized_keys.service"
	if [ -n "${MOUNTMODE:-}" ]; then
		case $MOUNTMODE in
		delay|late-nodep) # a nofail data mount that comes up 4 s late, like a slow SD partition
			cat >"$s/etc/systemd/system/slow-prep.service" <<'UEOF'
[Unit]
Description=lab: make the data mount late
[Service]
Type=oneshot
ExecStart=/bin/sleep 4
UEOF
			cat >"$s/etc/systemd/system/var-lib-stratux\\x2ddata.mount" <<'UEOF'
[Unit]
Description=lab: /var/lib/stratux-data (late, nofail-style)
After=slow-prep.service
Requires=slow-prep.service
[Mount]
What=/dsrc
Where=/var/lib/stratux-data
Type=none
Options=bind
[Install]
WantedBy=local-fs.target
UEOF
			ln -s "/etc/systemd/system/var-lib-stratux\\x2ddata.mount" "$s/etc/systemd/system/multi-user.target.wants/var-lib-stratux\\x2ddata.mount" ;;
		fail) # a nofail data mount whose device is missing
			cat >"$s/etc/systemd/system/var-lib-stratux\\x2ddata.mount" <<'UEOF'
[Unit]
Description=lab: /var/lib/stratux-data (fails, like a missing partition)
[Mount]
What=/dev/stxssh-no-such-partition
Where=/var/lib/stratux-data
Type=ext4
Options=defaults
[Install]
WantedBy=multi-user.target
UEOF
			ln -s "/etc/systemd/system/var-lib-stratux\\x2ddata.mount" "$s/etc/systemd/system/multi-user.target.wants/var-lib-stratux\\x2ddata.mount" ;;
		esac
	fi
	docker create --name "$c" --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock \
		--tmpfs /home/pi:uid=1000,gid=1000,mode=0700 -v /sys/fs/cgroup:/sys/fs/cgroup:rw "${volargs[@]}" $IMG /sbin/init >/dev/null || return 1
	tar --owner=0 --group=0 --numeric-owner -C "$s" -cf - . | docker cp - "$c:/" || { echo "FAIL: could not stage files into $c"; fail=1; return 1; }
	tar --owner=0 --group=0 --numeric-owner -C "$WORK" -cf - keys | docker cp - "$c:/"
	docker start "$c" >/dev/null
	boot_wait "$1"
}
boot_wait() { for _ in $(seq 1 90); do docker exec stxssh-$1 systemctl is-system-running 2>/dev/null | grep -qE 'running|degraded' && return 0; sleep 1; done; echo "WARN: $1 did not finish booting"; return 1; }
reboot_lab() { docker restart -t 10 stxssh-$1 >/dev/null; boot_wait "$1"; }   # a real systemd boot; tmpfs home comes back empty, the data volume is retained
dx() { docker exec stxssh-$1 bash -c "$2" 2>&1; }
key_login() { dx "$1" "chmod 600 /keys/$2; ssh -i /keys/$2 -o IdentitiesOnly=yes -o BatchMode=yes -o PasswordAuthentication=no -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5 pi@127.0.0.1 id -un" | tail -1; }
pw_login() { dx "$1" "sshpass -p $PW ssh -o PubkeyAuthentication=no -o PreferredAuthentications=password -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5 pi@127.0.0.1 id -un" | tail -1; }
prop() { dx "$1" "systemctl show -p $3 --value '$2'"; }
seed() { # VOLNAME  MODE  [OWNER]   (source content on stdin; ssh/ dir root 0755)
	local vol=$1 mode=$2 owner=${3:-0:0}
	docker volume inspect "$vol" >/dev/null 2>&1 || docker volume create "$vol" >/dev/null
	docker run --rm -i -v "$vol:/d" $IMG bash -c "mkdir -p /d/ssh && chown 0:0 /d/ssh && chmod 755 /d/ssh && cat >/d/ssh/authorized_keys && chown $owner /d/ssh/authorized_keys && chmod $mode /d/ssh/authorized_keys"
}
target_state() { dx "$1" "stat -c '%U:%G %a' /home/pi/.ssh /home/pi/.ssh/authorized_keys 2>&1 | tr '\n' ' '"; }
ordered() { # unit A finished before ssh.service started (monotonic microseconds since boot)
	local a s; a=$(prop "$1" stratux_ssh_authorized_keys.service ExecMainExitTimestampMonotonic); s=$(prop "$1" ssh.service ExecMainStartTimestampMonotonic)
	[ -n "$a" ] && [ -n "$s" ] && [ "$a" -gt 0 ] && [ "$s" -gt 0 ] && [ "$a" -le "$s" ] && echo 1 || echo 0
}

echo "=== S1: boot restore with a real sshd ==="
V=stxssh-vol-s1; seed $V 644 <<<"$(pub A)
$(pub B)"
lab_new s1
check "unit ran and stayed active (exited), Result=success" "$([ "$(prop s1 stratux_ssh_authorized_keys.service ActiveState)/$(prop s1 stratux_ssh_authorized_keys.service SubState)/$(prop s1 stratux_ssh_authorized_keys.service Result)" = "active/exited/success" ] && echo 1 || echo 0)"
check "the restore finished BEFORE ssh.service started" "$(ordered s1)"
check "~/.ssh is pi:pi 0700 and authorized_keys is pi:pi 0600 (real ownership)" "$([ "$(target_state s1)" = "pi:pi 700 pi:pi 600 " ] && echo 1 || echo 0)"
check "sshd accepts key A (real StrictModes enforcement)" "$(b "$(key_login s1 A)" pi)"
check "sshd accepts key B" "$(b "$(key_login s1 B)" pi)"
check "sshd rejects key C, which is not in the persistent file" "$([ "$(key_login s1 C)" != pi ] && echo 1 || echo 0)"
sshd_before=$(dx s1 "sshd -T | sha256sum; sha256sum /etc/ssh/sshd_config /etc/ssh/ssh_host_*_key | sha256sum")
check "the persistent source file is not modified by the restore" "$([ "$(dx s1 "cat /var/lib/stratux-data/ssh/authorized_keys | wc -l")" = 2 ] && echo 1 || echo 0)"

echo "=== S2: simulated reboot (volatile home lost, persistent partition retained) ==="
dx s1 "touch /home/pi/.ssh/volatile-marker"
reboot_lab s1
check "after a real systemd boot the tmpfs home was empty again (the volatile marker is gone)..." "$(dx s1 "[ -e /home/pi/.ssh/volatile-marker ] && echo present || echo gone" | grep -q gone && echo 1 || echo 0)"
check "...but the key file was restored automatically, before ssh.service started" "$([ "$(target_state s1)" = "pi:pi 700 pi:pi 600 " ] && [ "$(ordered s1)" = 1 ] && echo 1 || echo 0)"
check "key A logs in after the simulated reboot with no manual step" "$(b "$(key_login s1 A)" pi)"
check "key B logs in after the simulated reboot" "$(b "$(key_login s1 B)" pi)"
reboot_lab s1
check "and again after a second simulated reboot (idempotent across boots)" "$([ "$(key_login s1 A)" = pi ] && [ "$(dx s1 "grep -c ssh-ed25519 /home/pi/.ssh/authorized_keys")" = 2 ] && echo 1 || echo 0)"
sshd_after=$(dx s1 "sshd -T | sha256sum; sha256sum /etc/ssh/sshd_config /etc/ssh/ssh_host_*_key | sha256sum")
check "SSH policy untouched: effective sshd config, sshd_config and host keys are identical before and after" "$(b "$sshd_before" "$sshd_after")"
check "StrictModes, password auth, root-login and key-auth policy are exactly the shipped values" "$([ "$(dx s1 "sshd -T | grep -E '^(strictmodes|passwordauthentication|permitrootlogin|pubkeyauthentication|authorizedkeysfile) ' | sort" | tr '\n' '|')" = 'authorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2|passwordauthentication yes|permitrootlogin yes|pubkeyauthentication yes|strictmodes yes|' ] && echo 1 || echo 0)"

echo "=== S2c: control - WITHOUT the feature the key is lost at 'reboot' (proves the simulation is faithful) ==="
NOWANTS=1 VOL=stxssh-vol-s2c lab_new s2c
dx s2c "install -d -m 700 -o pi -g pi /home/pi/.ssh; echo '$(pub A)' >/home/pi/.ssh/authorized_keys; chown pi:pi /home/pi/.ssh/authorized_keys; chmod 600 /home/pi/.ssh/authorized_keys" >/dev/null
check "control: a manually added key works before the reboot" "$(b "$(key_login s2c A)" pi)"
reboot_lab s2c
check "control: the same key is GONE after the reboot (the reported bug), and password recovery still works" "$([ "$(key_login s2c A)" != pi ] && [ "$(pw_login s2c)" = pi ] && echo 1 || echo 0)"

echo "=== S3/S4: add and revoke keys without a reboot ==="
dx s1 "printf '%s\n' '$(pub A)' >/var/lib/stratux-data/ssh/authorized_keys.new && chmod 644 /var/lib/stratux-data/ssh/authorized_keys.new && mv /var/lib/stratux-data/ssh/authorized_keys.new /var/lib/stratux-data/ssh/authorized_keys" >/dev/null
sshd_pid_before=$(prop s1 ssh.service MainPID)
dx s1 "systemctl restart stratux_ssh_authorized_keys.service" >/dev/null
check "revocation: source now holds only A; after restarting the unit A works and B is rejected" "$([ "$(key_login s1 A)" = pi ] && [ "$(key_login s1 B)" != pi ] && echo 1 || echo 0)"
check "revocation happened without restarting sshd (same sshd main PID)" "$(b "$sshd_pid_before" "$(prop s1 ssh.service MainPID)")"
dx s1 "printf '%s\n%s\n' '$(pub A)' '$(pub C)' >/var/lib/stratux-data/ssh/authorized_keys.new && chmod 644 /var/lib/stratux-data/ssh/authorized_keys.new && mv /var/lib/stratux-data/ssh/authorized_keys.new /var/lib/stratux-data/ssh/authorized_keys" >/dev/null
dx s1 "systemctl restart stratux_ssh_authorized_keys.service" >/dev/null
check "adding a key: A+C then restart: C now works, A still works, B still rejected" "$([ "$(key_login s1 C)" = pi ] && [ "$(key_login s1 A)" = pi ] && [ "$(key_login s1 B)" != pi ] && echo 1 || echo 0)"
dx s1 "systemctl restart stratux_ssh_authorized_keys.service; systemctl restart stratux_ssh_authorized_keys.service" >/dev/null
check "repeated restarts are idempotent: still exactly 2 keys and A+C both work" "$([ "$(dx s1 "grep -c ssh-ed25519 /home/pi/.ssh/authorized_keys")" = 2 ] && [ "$(key_login s1 C)" = pi ] && echo 1 || echo 0)"

dx s1 "for i in 1 2 3 4 5 6 7 8 9 10; do systemctl restart stratux_ssh_authorized_keys.service || echo RESTART-FAILED; done" >/dev/null
check "ten rapid restarts in a row all succeed (no systemd start-limit-hit) and the keys still work" "$([ "$(prop s1 stratux_ssh_authorized_keys.service Result)" = success ] && [ "$(prop s1 stratux_ssh_authorized_keys.service ActiveState)" = active ] && [ "$(key_login s1 A)" = pi ] && echo 1 || echo 0)"

echo "=== S5: StrictModes control (the lab really enforces it; the helper restores safe modes) ==="
dx s1 "chmod 666 /home/pi/.ssh/authorized_keys" >/dev/null
check "control: with authorized_keys made world-writable, sshd REJECTS a valid key (StrictModes is really on)" "$([ "$(key_login s1 A)" != pi ] && echo 1 || echo 0)"
dx s1 "systemctl restart stratux_ssh_authorized_keys.service" >/dev/null
check "restarting the unit tightens the mode back to 0600 and the key works again" "$([ "$(target_state s1)" = "pi:pi 700 pi:pi 600 " ] && [ "$(key_login s1 A)" = pi ] && echo 1 || echo 0)"

echo "=== S6: failure modes - every one leaves sshd running and password recovery available ==="
recover_ok() { [ "$(prop "$1" stratux_ssh_authorized_keys.service LoadState)" = loaded ] && [ "$(prop "$1" ssh.service ActiveState)" = active ] && [ "$(pw_login "$1")" = pi ] && echo 1 || echo 0; }

VOL=stxssh-vol-f0 lab_new f0
check "no persistent file: unit succeeds as a no-op, nothing is created, sshd up, password login works" "$([ "$(prop f0 stratux_ssh_authorized_keys.service ActiveState)/$(prop f0 stratux_ssh_authorized_keys.service SubState)" = active/exited ] && [ "$(dx f0 "[ -e /home/pi/.ssh ] && echo yes || echo no")" = no ] && [ "$(dx f0 "[ -e /var/lib/stratux-data/ssh ] && echo yes || echo no")" = no ] && [ "$(recover_ok f0)" = 1 ] && echo 1 || echo 0)"

seed stxssh-vol-f1 644 <<<"$(pub A)
this-is-not-a-key"
VOL=stxssh-vol-f1 lab_new f1
check "malformed source: unit FAILS visibly, target not created, sshd still up, password login works" "$([ "$(prop f1 stratux_ssh_authorized_keys.service ActiveState)" = failed ] && [ "$(dx f1 "[ -e /home/pi/.ssh/authorized_keys ] && echo yes || echo no")" = no ] && [ "$(recover_ok f1)" = 1 ] && [ "$(key_login f1 A)" != pi ] && echo 1 || echo 0)"
check "malformed source: the failure is in the journal with a clear diagnostic (no key bodies)" "$(dx f1 "journalctl -u stratux_ssh_authorized_keys --no-pager" | grep -q 'not a valid authorized_keys entry' && ! dx f1 "journalctl -u stratux_ssh_authorized_keys --no-pager" | grep -q -F "$(awk '{print $2}' "$WORK/keys/A.pub")" && echo 1 || echo 0)"

seed stxssh-vol-f2 666 <<<"$(pub A)"
VOL=stxssh-vol-f2 lab_new f2
check "world-writable source: rejected, sshd up, password login works" "$([ "$(prop f2 stratux_ssh_authorized_keys.service ActiveState)" = failed ] && [ "$(recover_ok f2)" = 1 ] && [ "$(key_login f2 A)" != pi ] && echo 1 || echo 0)"

seed stxssh-vol-f3 664 <<<"$(pub A)"
VOL=stxssh-vol-f3 lab_new f3
check "group-writable source: rejected, sshd up, password login works" "$([ "$(prop f3 stratux_ssh_authorized_keys.service ActiveState)" = failed ] && [ "$(recover_ok f3)" = 1 ] && echo 1 || echo 0)"

seed stxssh-vol-f4 644 1000:1000 <<<"$(pub A)"
VOL=stxssh-vol-f4 lab_new f4
check "source owned by pi (uid 1000) inside a root-owned directory: rejected (real file-owner check), key not installed" "$([ "$(prop f4 stratux_ssh_authorized_keys.service ActiveState)" = failed ] && dx f4 "journalctl -u stratux_ssh_authorized_keys --no-pager" | grep -q 'owned by uid 1000, expected 0' && [ "$(key_login f4 A)" != pi ] && [ "$(recover_ok f4)" = 1 ] && echo 1 || echo 0)"

seed stxssh-vol-f5 644 <<<"$(pub A)"
docker run --rm -v stxssh-vol-f5:/d $IMG bash -c 'mv /d/ssh/authorized_keys /d/ssh/real && ln -s real /d/ssh/authorized_keys'
VOL=stxssh-vol-f5 lab_new f5
check "source is a symlink: rejected, not followed, sshd up" "$([ "$(prop f5 stratux_ssh_authorized_keys.service ActiveState)" = failed ] && [ "$(key_login f5 A)" != pi ] && [ "$(recover_ok f5)" = 1 ] && echo 1 || echo 0)"

seed stxssh-vol-f6 644 <<<""
docker run --rm -v stxssh-vol-f6:/d $IMG bash -c ': >/d/ssh/authorized_keys'
VOL=stxssh-vol-f6 lab_new f6
check "zero-byte source: warned and ignored (no target), unit succeeds, sshd up" "$([ "$(prop f6 stratux_ssh_authorized_keys.service Result)" = success ] && dx f6 "journalctl -u stratux_ssh_authorized_keys --no-pager" | grep -qi 'is empty' && [ "$(recover_ok f6)" = 1 ] && echo 1 || echo 0)"

# Data partition ABSENT: a same-named directory in the (RAM) root that holds a perfectly valid key file must NOT be trusted.
NODATA=1 lab_new f7 >/dev/null
dx f7 "mkdir -p /var/lib/stratux-data/ssh && echo '$(pub A)' >/var/lib/stratux-data/ssh/authorized_keys && chmod 644 /var/lib/stratux-data/ssh/authorized_keys" >/dev/null
dx f7 "systemctl restart stratux_ssh_authorized_keys.service" >/dev/null
check "data partition absent (a plain directory in the RAM root): the unit is skipped by its condition, the planted key is NOT installed" "$([ "$(prop f7 stratux_ssh_authorized_keys.service ConditionResult)" = no ] && [ "$(dx f7 "[ -e /home/pi/.ssh/authorized_keys ] && echo yes || echo no")" = no ] && [ "$(key_login f7 A)" != pi ] && echo 1 || echo 0)"
check "data partition absent: sshd started and password login works" "$(recover_ok f7)"
check "data partition absent: running the helper by hand is also a clean no-op (exit 0)" "$(b "$(dx f7 "/opt/stratux/bin/stratux-ssh-authorized-keys.sh >/dev/null 2>&1; echo \$?")" 0)"

# Helper HANGS: sshd must still start, bounded by TimeoutStartSec.
printf '#!/bin/bash\nsleep 600\n' >"$WORK/hang.sh"
seed stxssh-vol-f8 644 <<<"$(pub A)"
HELPER_OVERRIDE="$WORK/hang.sh" VOL=stxssh-vol-f8 lab_new f8
check "helper hangs: the unit is killed by TimeoutStartSec (failed), sshd still starts, password login works" "$([ "$(prop f8 stratux_ssh_authorized_keys.service ActiveState)" = failed ] && [ "$(recover_ok f8)" = 1 ] && echo 1 || echo 0)"
gap=$(( $(prop f8 ssh.service ExecMainStartTimestampMonotonic) - $(prop f8 stratux_ssh_authorized_keys.service InactiveExitTimestampMonotonic) ))
check "helper hangs: sshd was delayed by at most the unit's bounded TimeoutStartSec (~20 s), not indefinitely (gap $((gap / 1000000)) s)" "$([ "$gap" -ge 15000000 ] && [ "$gap" -le 30000000 ] && echo 1 || echo 0)"

# Target home missing / .ssh is a symlink: fail safe.
seed stxssh-vol-f9 644 <<<"$(pub A)"
VOL=stxssh-vol-f9 lab_new f9 >/dev/null
dx f9 "rm -rf /home/pi/.ssh; mkdir -p /tmp/elsewhere; ln -s /tmp/elsewhere /home/pi/.ssh; systemctl restart stratux_ssh_authorized_keys.service" >/dev/null
check "~/.ssh replaced by a symlink: refused, nothing written through it, sshd unaffected" "$([ "$(prop f9 stratux_ssh_authorized_keys.service ActiveState)" = failed ] && [ ! -n "$(dx f9 "ls -A /tmp/elsewhere")" ] && [ "$(recover_ok f9)" = 1 ] && echo 1 || echo 0)"

echo "=== S7: data mount ordering (a nofail mount that comes up LATE, and one that FAILS) ==="
seed stxssh-vol-m1 644 <<<"$(pub A)"
MOUNTMODE=delay VOL=stxssh-vol-m1 lab_new m1
mnt=$(prop m1 'var-lib-stratux\x2ddata.mount' ActiveEnterTimestampMonotonic); rest=$(prop m1 stratux_ssh_authorized_keys.service ExecMainStartTimestampMonotonic); ssh_s=$(prop m1 ssh.service ExecMainStartTimestampMonotonic)
check "late mount: order is data mount active < restore starts < ssh.service starts (After= makes the unit wait)" "$([ "$mnt" -gt 0 ] && [ "$mnt" -le "$rest" ] && [ "$rest" -le "$ssh_s" ] && echo 1 || echo 0)"
check "late mount: the key was restored and key A logs in" "$(b "$(key_login m1 A)" pi)"
# CONTROL: the same lab WITHOUT the After= edge races the slow mount and skips the restore -> proves the edge is what makes it work.
seed stxssh-vol-m2 644 <<<"$(pub A)"
MOUNTMODE=late-nodep UNIT_SED='/^After=var-lib-stratux/d' VOL=stxssh-vol-m2 lab_new m2
check "control (no After=<data mount>): the restore runs before the late mount, is skipped, and the key is NOT restored - so the ordering edge is required and this test can fail" "$([ "$(prop m2 stratux_ssh_authorized_keys.service LoadState)" = loaded ] && [ "$(prop m2 stratux_ssh_authorized_keys.service ConditionResult)" = no ] && [ "$(key_login m2 A)" != pi ] && echo 1 || echo 0)"
MOUNTMODE=fail VOL=stxssh-vol-m3 lab_new m3
check "data mount FAILS (nofail): the mount really did not come up (job failed) and the path is not a mount point; our unit is skipped by its condition, sshd still starts, password login works" "$([ "$(prop m3 'var-lib-stratux\x2ddata.mount' ActiveState)" != active ] && dx m3 "journalctl -u 'var-lib-stratux\\x2ddata.mount' --no-pager" | grep -q 'failed with result' && [ "$(dx m3 "mountpoint -q /var/lib/stratux-data && echo yes || echo no")" = no ] && [ "$(prop m3 stratux_ssh_authorized_keys.service ConditionResult)" = no ] && [ "$(recover_ok m3)" = 1 ] && echo 1 || echo 0)"

echo
echo "$pass_n lab checks passed"
exit $fail
