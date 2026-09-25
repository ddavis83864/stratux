#!/bin/bash
# ssh_authorized_keys_package_lifecycle_test.sh: installs a BUILT stratux .deb into
# an isolated Debian 12 arm64 container and exercises the package lifecycle of the
# persistent-SSH-key feature: contents and modes, the postinst ownership fix, the
# real maintainer scripts (fresh install, OTA-style install, upgrade/reinstall,
# removal), a real run of the PACKAGED helper against a real mount point, and that
# the administrator's persistent key file survives removal.
#
#   test/ssh_authorized_keys_package_lifecycle_test.sh /path/to/stratux-<ver>-arm64.deb
#
# Opt-in: needs Docker with arm64 emulation (QEMU) and network for `apt`. Prints SKIP
# and exits 0 without a .deb or Docker. `systemctl` is stubbed (there is no systemd in
# the container) and every call is checked; the real systemd behavior is covered by
# test/ssh_authorized_keys_systemd_lab.sh. NEVER touches a real device.
set -u
DEB=${1:-}
[ -n "$DEB" ] && [ -f "$DEB" ] || { echo "SKIP: pass the path of a built stratux .deb"; exit 0; }
command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 || { echo "SKIP: docker not available"; exit 0; }
DEB=$(readlink -f "$DEB")

OUT=$(mktemp)
docker run --rm -i --platform linux/arm64 --privileged -v "$DEB":/pkg/stratux.deb:ro debian:bookworm bash -s <<'CONTAINER' 2>&1 | tee "$OUT"
set -u
fail=0
check() { if [ "$2" = 1 ]; then echo "PASS: $1"; else echo "FAIL: $1"; fail=1; fi; }
b() { [ "$1" = "$2" ] && echo 1 || echo 0; }
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null 2>&1
apt-get install -y -qq --no-install-recommends libncurses6 librtlsdr0 openssh-client >/dev/null 2>&1 || { echo "FAIL: could not install the package's dependencies"; exit 1; }

# A Pi-like environment: aarch64, the Pi marker file, a pi user, a stub systemctl.
useradd -m -u 1000 -s /bin/bash pi
mkdir -p /boot/firmware && : >/boot/firmware/config.txt
LOG=/tmp/systemctl.log
printf '#!/bin/sh\necho "systemctl $*" >> %s\nexit 0\n' "$LOG" >/usr/local/bin/systemctl && chmod +x /usr/local/bin/systemctl
check "environment is aarch64 (the package's real target architecture)" "$(b "$(uname -m)" aarch64)"
dpkg-deb -f /pkg/stratux.deb Package Version Architecture | tr '\n' ' '; echo

echo "=== package contents ==="
dpkg-deb -c /pkg/stratux.deb | grep -E 'stratux-ssh-authorized-keys.sh|stratux_ssh_authorized_keys.service' | awk '{print $1, $2, $NF}'
check "package ships the helper (executable) and the unit (0644)" "$(dpkg-deb -c /pkg/stratux.deb | grep -E '\./opt/stratux/bin/stratux-ssh-authorized-keys.sh$' | grep -q '^-rwxr-xr-x' && dpkg-deb -c /pkg/stratux.deb | grep -E '\./lib/systemd/system/stratux_ssh_authorized_keys.service$' | grep -q '^-rw-r--r--' && echo 1 || echo 0)"

echo "=== fresh install ==="
: >$LOG
dpkg -i /pkg/stratux.deb >/tmp/dpkg1.log 2>&1; rc=$?
check "dpkg -i succeeds" "$(b $rc 0)"
check "installed files present" "$([ -x /opt/stratux/bin/stratux-ssh-authorized-keys.sh ] && [ -f /lib/systemd/system/stratux_ssh_authorized_keys.service ] && echo 1 || echo 0)"
check "postinst made the helper and the unit root:root (whatever uid the package was built under)" "$([ "$(stat -c %U:%G /opt/stratux/bin/stratux-ssh-authorized-keys.sh)" = root:root ] && [ "$(stat -c %U:%G /lib/systemd/system/stratux_ssh_authorized_keys.service)" = root:root ] && [ "$(stat -c %a /opt/stratux/bin/stratux-ssh-authorized-keys.sh)" = 755 ] && echo 1 || echo 0)"
check "the enabled unit and helper paths agree with what the unit executes" "$(grep -q '^ExecStart=/opt/stratux/bin/stratux-ssh-authorized-keys.sh$' /lib/systemd/system/stratux_ssh_authorized_keys.service && echo 1 || echo 0)"
check "postinst enabled stratux_ssh_authorized_keys (once) and started it after enabling" "$([ "$(grep -c 'systemctl enable stratux_ssh_authorized_keys' $LOG)" = 1 ] && [ "$(grep -n 'systemctl enable stratux_ssh_authorized_keys' $LOG | cut -d: -f1)" -lt "$(grep -n 'systemctl start stratux_ssh_authorized_keys' $LOG | cut -d: -f1)" ] && echo 1 || echo 0)"
check "a fresh install creates no key file anywhere (nothing provisioned = normal no-op)" "$([ ! -e /var/lib/stratux-data/ssh ] && [ ! -e /home/pi/.ssh ] && echo 1 || echo 0)"
check "dpkg --audit is clean" "$([ -z "$(dpkg --audit)" ] && echo 1 || echo 0)"

echo "=== the PACKAGED helper, run for real ==="
mkdir -p /var/lib/stratux-data && mount -t tmpfs tmpfs /var/lib/stratux-data      # a real mount point
ssh-keygen -q -t ed25519 -N '' -C lifecycle-A -f /tmp/A; ssh-keygen -q -t ed25519 -N '' -C lifecycle-B -f /tmp/B
install -d -m 0755 -o root -g root /var/lib/stratux-data/ssh
cat /tmp/A.pub /tmp/B.pub >/var/lib/stratux-data/ssh/authorized_keys; chown root:root /var/lib/stratux-data/ssh/authorized_keys; chmod 644 /var/lib/stratux-data/ssh/authorized_keys
/opt/stratux/bin/stratux-ssh-authorized-keys.sh >/tmp/helper.out 2>&1; rc=$?
check "packaged helper installs the keys: exit 0, pi:pi 0600, ~/.ssh pi:pi 0700, contents identical" "$([ $rc = 0 ] && [ "$(stat -c '%U:%G %a' /home/pi/.ssh/authorized_keys)" = 'pi:pi 600' ] && [ "$(stat -c '%U:%G %a' /home/pi/.ssh)" = 'pi:pi 700' ] && cmp -s /var/lib/stratux-data/ssh/authorized_keys /home/pi/.ssh/authorized_keys && echo 1 || echo 0)"
printf 'not-a-key\n' >>/var/lib/stratux-data/ssh/authorized_keys
cp /home/pi/.ssh/authorized_keys /tmp/before
/opt/stratux/bin/stratux-ssh-authorized-keys.sh >/dev/null 2>&1; rc=$?
check "packaged helper rejects a malformed source, non-zero, and leaves the target byte-identical" "$([ $rc != 0 ] && cmp -s /tmp/before /home/pi/.ssh/authorized_keys && echo 1 || echo 0)"
head -n 2 /var/lib/stratux-data/ssh/authorized_keys > /tmp/good; cat /tmp/good >/var/lib/stratux-data/ssh/authorized_keys

echo "=== OTA-style install (STRATUX_OTA_INSTALL set): enables, never starts ==="
: >$LOG
STRATUX_OTA_INSTALL=1 dpkg -i /pkg/stratux.deb >/tmp/dpkg2.log 2>&1; rc=$?
check "reinstall over the existing install succeeds (this is also the upgrade path)" "$(b $rc 0)"
check "OTA-mode scripts: prerm and postinst touched no start/stop, and did not disable the unit" "$(grep -Eq 'systemctl (start|restart|stop)' $LOG && echo 0 || echo 1)"
check "OTA-mode postinst still enabled it (an OTA-delivered install must enable it)" "$(grep -q 'systemctl enable stratux_ssh_authorized_keys' $LOG && echo 1 || echo 0)"
check "the persistent keys and the restored keys survived the upgrade" "$([ "$(wc -l </var/lib/stratux-data/ssh/authorized_keys)" = 2 ] && [ -s /home/pi/.ssh/authorized_keys ] && echo 1 || echo 0)"

echo "=== normal upgrade/reinstall: prerm must NOT disable ==="
: >$LOG
dpkg -i /pkg/stratux.deb >/tmp/dpkg3.log 2>&1
check "on upgrade the unit is never disabled by prerm (postinst re-enables it)" "$(grep -q 'systemctl disable stratux_ssh_authorized_keys' $LOG && echo 0 || echo 1)"
check "and it is enabled again afterwards" "$(grep -q 'systemctl enable stratux_ssh_authorized_keys' $LOG && echo 1 || echo 0)"

echo "=== removal ==="
: >$LOG
dpkg -r stratux >/tmp/dpkg4.log 2>&1; rc=$?
check "dpkg -r succeeds" "$(b $rc 0)"
check "prerm remove disabled the unit" "$(grep -q 'systemctl disable stratux_ssh_authorized_keys' $LOG && echo 1 || echo 0)"
check "package-owned files are gone (unit, helper)" "$([ ! -e /lib/systemd/system/stratux_ssh_authorized_keys.service ] && [ ! -e /opt/stratux/bin/stratux-ssh-authorized-keys.sh ] && echo 1 || echo 0)"
check "the administrator's PERSISTENT key file survived removal (untouched, still 2 keys)" "$([ "$(wc -l </var/lib/stratux-data/ssh/authorized_keys)" = 2 ] && [ "$(stat -c '%U %a' /var/lib/stratux-data/ssh/authorized_keys)" = 'root 644' ] && echo 1 || echo 0)"
check "the already-restored volatile copy is left alone by removal (it disappears with the next reboot)" "$([ -s /home/pi/.ssh/authorized_keys ] && echo 1 || echo 0)"
exit $fail
CONTAINER
rc=${PIPESTATUS[0]}
# A silent container (no script received, image pull failure, ...) must never look like a pass.
n=$(grep -c '^PASS' "$OUT"); f=$(grep -c '^FAIL' "$OUT"); rm -f "$OUT"
echo "$n checks passed, $f failed"
[ "$n" -ge 20 ] && [ "$f" = 0 ] && [ "$rc" = 0 ] && exit 0
echo "FAIL: lifecycle run did not complete cleanly (container exit $rc)"; exit 1
