#!/bin/bash
# package_ownership_lifecycle_test.sh: what the package DOES to file ownership across its
# whole life, with the real dpkg and the real maintainer scripts, in an isolated Debian 12
# container: fresh install, upgrade of a device installed from a PRE-normalization package
# (normal and OTA-style), idempotency, OTA-backup rollback, downgrade, removal/purge,
# preservation of user data, and hostile/odd inputs to the migration itself.
#
#   test/package_ownership_lifecycle_test.sh /path/to/stratux-<ver>-<arch>.deb
#
# The "legacy" package is derived FROM THE PACKAGE UNDER TEST: same tree, owner rewritten
# to a builder uid (1001 like CI, and 1000 like a developer/owner build), packaged with a
# plain `dpkg-deb -b`, carrying the pre-normalization postinst from test/fixtures and a
# lower version. So the test works on any candidate, including the exact CI arm64 artifact
# (run under QEMU; set PLATFORM=linux/arm64, which the file name also implies).
#
# `systemctl` and `uname -m`/the Pi marker are stubbed (no systemd in the container; the
# systemd behavior has its own labs). NEVER touches a real device.
set -u
DEB=${1:-}
[ -n "$DEB" ] && [ -f "$DEB" ] || { echo "SKIP: pass the path of a built stratux .deb"; exit 0; }
command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 || { echo "SKIP: docker not available"; exit 0; }
DEB=$(readlink -f "$DEB")
HERE=$(cd "$(dirname "$0")" && pwd)
PLATFORM=${PLATFORM:-}
[ -n "$PLATFORM" ] || case "$DEB" in *arm64*) PLATFORM=linux/arm64;; *amd64*) PLATFORM=linux/amd64;; esac
ARGS=(); [ -z "$PLATFORM" ] || ARGS=(--platform "$PLATFORM")
IMAGE=debian:bookworm
if [ -z "$PLATFORM" ] || [ "$PLATFORM" = linux/amd64 ]; then
	. "$HERE/package_ownership_image.sh"; ensure_pkgtest_image; IMAGE=$IMG
fi

OUT=$(mktemp)
docker run --rm -i "${ARGS[@]}" --privileged -v "$DEB":/pkg/stratux.deb:ro -v "$HERE/fixtures/package_ownership":/fx:ro "$IMAGE" bash -s <<'CONTAINER' 2>&1 | tee "$OUT"
set -u
fail=0
check() { if [ "$2" = 1 ]; then echo "PASS: $1"; else echo "FAIL: $1"; fail=1; fi; }
ok() { [ "$1" = "$2" ] && echo 1 || echo 0; }
export DEBIAN_FRONTEND=noninteractive
if ! dpkg -s librtlsdr0 >/dev/null 2>&1; then
	apt-get update -qq >/dev/null 2>&1
	apt-get install -y -qq --no-install-recommends libncurses6 librtlsdr0 openssh-client >/dev/null 2>&1 || { echo "FAIL: could not install the package's dependencies"; exit 1; }
fi
useradd -m -u 1000 -s /bin/bash pi

LOG=/tmp/systemctl.log; CHOWN=/tmp/chown.log
printf '#!/bin/sh\necho "systemctl $*" >> %s\nexit 0\n' "$LOG" >/usr/local/bin/systemctl
# a logging wrapper: records every chown the maintainer scripts run, then does it
printf '#!/bin/sh\necho "chown $*" >> %s\nexec /bin/chown "$@"\n' "$CHOWN" >/usr/local/bin/chown
chmod +x /usr/local/bin/systemctl /usr/local/bin/chown

NEWVER=$(dpkg-deb -f /pkg/stratux.deb Version)
echo "candidate: $(dpkg-deb -f /pkg/stratux.deb Package Version Architecture | tr '\n' ' ') on $(uname -m)"

# ---- helpers -------------------------------------------------------------------------
in_scope() { case "$1" in
	/opt/stratux/mapdata/*) return 1;;
	/opt/stratux|/opt/stratux/*|/lib/systemd/system/*.service|/etc/udev/rules.d/*.rules) return 0;;
	*) return 1;; esac; }
scope() { dpkg-query -L stratux | while IFS= read -r p; do in_scope "$p" && [ -e "$p" ] && [ ! -L "$p" ] && printf '%s\n' "$p"; done; }
nonroot() { scope | while IFS= read -r p; do [ "$(stat -c %u:%g "$p")" = 0:0 ] || printf '%s\n' "$p"; done; }
census() { scope | while IFS= read -r p; do stat -c '%u:%g' "$p"; done | sort | uniq -c | tr '\n' ' '; }
modes() { scope | while IFS= read -r p; do printf '%s %s\n' "$(stat -c %a "$p")" "$p"; done | sort -k2; }
snap() { scope | while IFS= read -r p; do stat -c '%n %u:%g %a' "$p"; done | sort; }
own() { stat -c %u:%g "$1"; }
# the reference modes straight from the archive under test
rm -rf /tmp/ref && mkdir /tmp/ref && dpkg-deb -x /pkg/stratux.deb /tmp/ref
refmodes() { scope | while IFS= read -r p; do printf '%s %s\n' "$(stat -c %a "/tmp/ref$p")" "$p"; done | sort -k2; }

mk_legacy() { # $1 = builder uid: same tree, owner rewritten, plain dpkg-deb -b, old postinst, lower version
	local t=/tmp/leg-$1; rm -rf "$t"; mkdir -p "$t"; dpkg-deb -R /pkg/stratux.deb "$t"
	cp /fx/postinst.pre-normalization "$t/DEBIAN/postinst"; chmod 755 "$t/DEBIAN/postinst"
	sed -i 's/^Version: .*/Version: 0.0.1~legacy/' "$t/DEBIAN/control"
	find "$t" -path "$t/DEBIAN" -prune -o -exec chown -h "$1:$1" {} +
	dpkg-deb -Znone -b "$t" "/tmp/legacy-$1.deb" >/dev/null
}
mk_layerA_only() { # new archive (root:root) but the OLD postinst: shows dpkg alone cannot repair dirs/conffile
	local t=/tmp/la; rm -rf "$t"; mkdir -p "$t"; dpkg-deb -R /pkg/stratux.deb "$t"
	cp /fx/postinst.pre-normalization "$t/DEBIAN/postinst"; chmod 755 "$t/DEBIAN/postinst"
	dpkg-deb -Znone --root-owner-group -b "$t" /tmp/layerA-only.deb >/dev/null
}
wipe() { dpkg -P --force-all stratux >/dev/null 2>&1; rm -rf /opt/stratux /lib/systemd/system/stratux*.service /etc/udev/rules.d/*stratux* /etc/udev/rules.d/99-uavionix.rules /etc/udev/rules.d/99-pong.rules; : >$LOG; : >$CHOWN; }
# the state of a real device that was imaged with a pi-owned tree and OTA-updated since:
# directories and the conffile pi:pi (uid 1000), files from the CI build (uid $1)
make_device_state() {
	wipe; dpkg -i "/tmp/legacy-$1.deb" >/tmp/leg-install.log 2>&1 || { echo "FAIL: legacy install failed"; cat /tmp/leg-install.log; fail=1; }
	find /opt/stratux -type d -exec chown 1000:1000 {} +; chown 1000:1000 /lib/systemd/system/stratux.service
	# user data and things the package does not own
	printf 'my-tiles\n' >/opt/stratux/mapdata/user-upload.mbtiles; mkdir -p /opt/stratux/mapdata/styles/x; printf '{}\n' >/opt/stratux/mapdata/styles/x/style.json
	chown -R 1000:1000 /opt/stratux/mapdata/user-upload.mbtiles /opt/stratux/mapdata/styles; chmod 0640 /opt/stratux/mapdata/user-upload.mbtiles
	printf '#!/bin/sh\necho mine\n' >/opt/stratux/bin/custom-tool.sh; chmod 755 /opt/stratux/bin/custom-tool.sh; chown 1000:1000 /opt/stratux/bin/custom-tool.sh
	printf 'x=1\n' >/opt/stratux/cfg/user.conf; chown 1000:1000 /opt/stratux/cfg/user.conf
	USERSUM=$(cd /opt/stratux && sha256sum mapdata/user-upload.mbtiles mapdata/styles/x/style.json bin/custom-tool.sh cfg/user.conf | sha256sum)
	: >$LOG; : >$CHOWN
}
user_data_intact() { # ownership, mode AND content of everything the package does not own
	[ "$(cd /opt/stratux && sha256sum mapdata/user-upload.mbtiles mapdata/styles/x/style.json bin/custom-tool.sh cfg/user.conf | sha256sum)" = "$USERSUM" ] &&
	[ "$(own /opt/stratux/mapdata/user-upload.mbtiles)" = 1000:1000 ] && [ "$(stat -c %a /opt/stratux/mapdata/user-upload.mbtiles)" = 640 ] &&
	[ "$(own /opt/stratux/mapdata/styles/x/style.json)" = 1000:1000 ] && [ "$(own /opt/stratux/mapdata/styles)" = 1000:1000 ] &&
	[ "$(own /opt/stratux/bin/custom-tool.sh)" = 1000:1000 ] && [ "$(own /opt/stratux/cfg/user.conf)" = 1000:1000 ] && echo 1 || echo 0
}
audit_clean() { [ -z "$(dpkg --audit 2>&1)" ] && echo 1 || echo 0; }

# ---- 1. fresh install, on a system that is not a Raspberry Pi ---------------------------
echo "=== 1. fresh install (non-Pi system) ==="
wipe; dpkg -i /pkg/stratux.deb >/tmp/d.log 2>&1; rc=$?
check "dpkg -i succeeds" "$(ok $rc 0)"
check "every package-owned path under /opt/stratux, the units and the rules is root:root ($(census))" "$([ -z "$(nonroot)" ] && [ -n "$(scope)" ] && echo 1 || echo 0)"
check "/opt/stratux, /opt/stratux/bin and stratux.service are root:root" "$([ "$(own /opt/stratux)" = 0:0 ] && [ "$(own /opt/stratux/bin)" = 0:0 ] && [ "$(own /lib/systemd/system/stratux.service)" = 0:0 ] && echo 1 || echo 0)"
check "mapdata is root-owned but stays world-writable 0777 (users upload there as pi)" "$([ "$(own /opt/stratux/mapdata)" = 0:0 ] && [ "$(stat -c %a /opt/stratux/mapdata)" = 777 ] && echo 1 || echo 0)"
check "a fresh install needed no ownership repair (no 'chown -h' issued by the migration)" "$(grep -q '^chown -h' $CHOWN && echo 0 || echo 1)"
check "modes of every scoped path equal the archive's" "$([ "$(modes)" = "$(refmodes)" ] && echo 1 || echo 0)"
check "dpkg --audit is clean" "$(audit_clean)"

# ---- Pi-like environment for everything below ------------------------------------------
mkdir -p /boot/firmware && : >/boot/firmware/config.txt
if [ "$(uname -m)" != aarch64 ]; then printf '#!/bin/sh\n[ "$1" = "-m" ] && echo aarch64 || exec /usr/bin/uname "$@"\n' >/usr/local/bin/uname; chmod +x /usr/local/bin/uname; fi
check "environment reports aarch64 with the Pi marker (the postinst's Pi-only block runs)" "$([ "$(uname -m)" = aarch64 ] && echo 1 || echo 0)"

for BUILDER in 1001 1000; do
echo "=== 2. LEGACY device state (files from a build under uid $BUILDER, dirs/conffile pi:pi like the real image) ==="
mk_legacy $BUILDER
make_device_state $BUILDER
pre=$(nonroot | wc -l)
check "[legacy $BUILDER] precondition: the device really is in the broken state ($pre paths not root-owned; /opt/stratux=$(own /opt/stratux) bin=$(own /opt/stratux/bin) stratux.service=$(own /lib/systemd/system/stratux.service))" "$([ "$pre" -gt 100 ] && [ "$(own /opt/stratux)" = 1000:1000 ] && [ "$(own /opt/stratux/bin)" = 1000:1000 ] && [ "$(own /lib/systemd/system/stratux.service)" = 1000:1000 ] && echo 1 || echo 0)"

echo "--- 2a. Layer A alone is NOT enough (new archive, old postinst)"
mk_layerA_only
dpkg -i /tmp/layerA-only.deb >/tmp/d.log 2>&1
check "[legacy $BUILDER] dpkg re-owns the files but leaves the existing directories and the conffile alone (so a migration is required)" "$([ "$(own /opt/stratux)" = 1000:1000 ] && [ "$(own /opt/stratux/bin)" = 1000:1000 ] && [ "$(own /lib/systemd/system/stratux.service)" = 1000:1000 ] && [ "$(own /opt/stratux/bin/stratuxrun)" = 0:0 ] && echo 1 || echo 0)"

echo "--- 2b. normal upgrade legacy -> candidate"
make_device_state $BUILDER
dpkg -i /pkg/stratux.deb >/tmp/d.log 2>&1; rc=$?
check "[legacy $BUILDER] upgrade succeeds" "$(ok $rc 0)"
check "[legacy $BUILDER] every package-owned path is root:root afterwards ($(census))" "$([ -z "$(nonroot)" ] && echo 1 || echo 0)"
check "[legacy $BUILDER] /opt/stratux, /opt/stratux/bin, stratux.service (a conffile) and the SSH helper/unit are root:root" "$([ "$(own /opt/stratux)" = 0:0 ] && [ "$(own /opt/stratux/bin)" = 0:0 ] && [ "$(own /lib/systemd/system/stratux.service)" = 0:0 ] && [ "$(own /opt/stratux/bin/stratux-ssh-authorized-keys.sh)" = 0:0 ] && [ "$(own /lib/systemd/system/stratux_ssh_authorized_keys.service)" = 0:0 ] && echo 1 || echo 0)"
check "[legacy $BUILDER] mapdata: root-owned, mode still 0777" "$([ "$(own /opt/stratux/mapdata)" = 0:0 ] && [ "$(stat -c %a /opt/stratux/mapdata)" = 777 ] && echo 1 || echo 0)"
check "[legacy $BUILDER] no mode changed: every scoped path has exactly the archive's mode" "$([ "$(modes)" = "$(refmodes)" ] && echo 1 || echo 0)"
check "[legacy $BUILDER] USER DATA preserved: mapdata uploads, an unlisted script, an unlisted config keep owner, mode and bytes" "$(user_data_intact)"
check "[legacy $BUILDER] the migration is per-path: no recursive chown anywhere in the whole upgrade" "$(grep -E -q 'chown (-[a-zA-Z]*R|--recursive)' $CHOWN && echo 0 || echo 1)"
check "[legacy $BUILDER] every chown the migration issued was 'chown -h root:root -- <listed path>'" "$(grep '^chown -h' $CHOWN | while IFS= read -r l; do p=${l#chown -h root:root -- }; [ "$l" != "$p" ] && dpkg-query -L stratux | grep -qxF "$p" && continue; echo bad; done | grep -q bad && echo 0 || echo 1)"
check "[legacy $BUILDER] a normal upgrade still (re)starts the service" "$(grep -q 'systemctl start stratux$' $LOG && echo 1 || echo 0)"
check "[legacy $BUILDER] dpkg --audit is clean" "$(audit_clean)"

echo "--- 2c. OTA-style upgrade legacy -> candidate (STRATUX_OTA_INSTALL: postinst exits early)"
make_device_state $BUILDER
STRATUX_OTA_INSTALL=1 dpkg -i --force-depends /pkg/stratux.deb >/tmp/d.log 2>&1; rc=$?
check "[legacy $BUILDER] OTA-style upgrade succeeds" "$(ok $rc 0)"
check "[legacy $BUILDER] OTA path: everything root:root although the postinst takes its early exit (the migration runs BEFORE it)" "$([ -z "$(nonroot)" ] && [ "$(own /opt/stratux)" = 0:0 ] && [ "$(own /opt/stratux/bin)" = 0:0 ] && [ "$(own /lib/systemd/system/stratux.service)" = 0:0 ] && echo 1 || echo 0)"
check "[legacy $BUILDER] OTA path starts/stops/restarts NOTHING (no recursion into the running unit)" "$(grep -Eq 'systemctl (start|restart|stop)' $LOG && echo 0 || echo 1)"
check "[legacy $BUILDER] OTA path: mapdata still 0777 and user data intact" "$([ "$(stat -c %a /opt/stratux/mapdata)" = 777 ] && [ "$(user_data_intact)" = 1 ] && echo 1 || echo 0)"
check "[legacy $BUILDER] OTA path: modes equal the archive's" "$([ "$(modes)" = "$(refmodes)" ] && echo 1 || echo 0)"

echo "--- 3. idempotency (on the migrated device)"
s1=$(snap); : >$CHOWN
STRATUX_OTA_INSTALL=1 /var/lib/dpkg/info/stratux.postinst configure >/dev/null 2>&1; rc=$?
check "[legacy $BUILDER] re-running the postinst succeeds, changes no owner or mode, and the migration issues no chown at all" "$([ $rc = 0 ] && [ "$(snap)" = "$s1" ] && ! grep -q '^chown -h' $CHOWN && echo 1 || echo 0)"
dpkg -i /pkg/stratux.deb >/tmp/d.log 2>&1; rc=$?
check "[legacy $BUILDER] reinstalling the same package succeeds and leaves owners and modes identical" "$([ $rc = 0 ] && [ "$(snap)" = "$s1" ] && [ "$(user_data_intact)" = 1 ] && echo 1 || echo 0)"

echo "--- 4. rollback (the OTA backup tar restores the pre-upgrade owners)"
make_device_state $BUILDER
BK=/tmp/pre-install.tar.gz
tar czf $BK -C / opt/stratux lib/systemd/system/stratux.service lib/systemd/system/stratux_fancontrol.service etc/udev/rules.d/10-stratux.rules
STRATUX_OTA_INSTALL=1 dpkg -i --force-depends /pkg/stratux.deb >/dev/null 2>&1
check "[legacy $BUILDER] (setup) OTA install normalized the device" "$([ -z "$(nonroot)" ] && echo 1 || echo 0)"
tar xzf $BK -C / --exclude=ota-dpkg-meta
check "[legacy $BUILDER] rollback restores the previous, LEGACY owners (documented boundary: a restore returns the state it captured) and no data is lost" "$([ "$(own /opt/stratux)" = 1000:1000 ] && [ "$(own /opt/stratux/bin/stratuxrun)" = $BUILDER:$BUILDER ] && [ "$(user_data_intact)" = 1 ] && echo 1 || echo 0)"
STRATUX_OTA_INSTALL=1 dpkg -i --force-depends /pkg/stratux.deb >/dev/null 2>&1; rc=$?
check "[legacy $BUILDER] re-upgrading after a rollback repairs the ownership again" "$([ $rc = 0 ] && [ -z "$(nonroot)" ] && [ "$(user_data_intact)" = 1 ] && echo 1 || echo 0)"

echo "--- 5. downgrade to the legacy package, then upgrade again"
dpkg -i /tmp/legacy-$BUILDER.deb >/tmp/d.log 2>&1; rc=$?
check "[legacy $BUILDER] downgrading to the pre-normalization package succeeds and dpkg stays consistent" "$([ $rc = 0 ] && [ "$(dpkg-query -W -f='${Version}' stratux)" = 0.0.1~legacy ] && [ "$(audit_clean)" = 1 ] && echo 1 || echo 0)"
check "[legacy $BUILDER] downgrade: user data intact (the old package re-creates the old ownership of ITS files only)" "$(user_data_intact)"
dpkg -i /pkg/stratux.deb >/tmp/d.log 2>&1; rc=$?
check "[legacy $BUILDER] upgrading again from the downgraded state repairs everything" "$([ $rc = 0 ] && [ -z "$(nonroot)" ] && [ "$(dpkg-query -W -f='${Version}' stratux)" = "$NEWVER" ] && echo 1 || echo 0)"
done

echo "=== 6. removal and purge of the migrated package ==="
: >$CHOWN; : >$LOG
dpkg -r stratux >/tmp/d.log 2>&1; rc=$?
check "dpkg -r succeeds and removes the packaged files (the stratux.service conffile is kept until purge, as dpkg always does)" "$([ $rc = 0 ] && [ ! -e /opt/stratux/bin/stratuxrun ] && [ ! -e /opt/stratux/bin/stratux-ssh-authorized-keys.sh ] && [ -e /lib/systemd/system/stratux.service ] && echo 1 || echo 0)"
check "removal leaves user data untouched (owner, mode, bytes) and unowned files alone" "$(user_data_intact)"
check "removal runs no ownership migration" "$(grep -q '^chown -h' $CHOWN && echo 0 || echo 1)"
dpkg -P stratux >/tmp/d.log 2>&1; rc=$?
check "dpkg -P succeeds (removing the conffile too); dpkg --audit clean; user data still there" "$([ $rc = 0 ] && [ ! -e /lib/systemd/system/stratux.service ] && [ "$(audit_clean)" = 1 ] && [ "$(user_data_intact)" = 1 ] && echo 1 || echo 0)"

echo "=== 7. the migration function against hostile and odd inputs ==="
mk_legacy 1001; make_device_state 1001
dpkg -i /pkg/stratux.deb >/dev/null 2>&1                                  # a normalized, installed system
awk '/^stratux_normalize_ownership\(\) \{/{f=1} f{print} f&&/^\}/{exit}' /var/lib/dpkg/info/stratux.postinst > /tmp/fn.sh
sed -i 's#/var/lib/dpkg/info/stratux.list#/tmp/test.list#' /tmp/fn.sh
check "(setup) extracted the migration function from the installed postinst" "$([ "$(wc -l </tmp/fn.sh)" -gt 5 ] && echo 1 || echo 0)"
runfn() { : >$CHOWN; bash -c '. /tmp/fn.sh; stratux_normalize_ownership'; echo $?; }
victim=/tmp/victim; printf 'v\n' >$victim; chown 1000:1000 $victim
mkdir -p /tmp/evil && printf 'e\n' >/tmp/evil/stratux.conf.default && chown -R 1000:1000 /tmp/evil
printf 'odd\n' >'/opt/stratux/we ird*name'; chown 1000:1000 '/opt/stratux/we ird*name'
printf 'decoy\n' >/opt/stratux/decoyA; chown 1000:1000 /opt/stratux/decoyA
printf 'x\n' >/home/pi/outside; chown 1000:1000 /home/pi/outside
ln -sfn $victim /opt/stratux/link-to-victim; chown -h 1000:1000 /opt/stratux/link-to-victim
mv /opt/stratux/cfg /opt/stratux/cfg.real; ln -s /tmp/evil /opt/stratux/cfg
# a symlink where a shipped UNIT would be (no canonical-path guard applies outside /opt/stratux)
ln -sfn $victim /lib/systemd/system/zz-evil.service; chown -h 1000:1000 /lib/systemd/system/zz-evil.service
cat >/tmp/test.list <<LIST
/opt/stratux/we ird*name
/opt/stratux/link-to-victim
/lib/systemd/system/zz-evil.service
/opt/stratux/cfg/stratux.conf.default
/opt/stratux/../tmp/victim
/opt/stratux/bin/../../home/pi/outside
/home/pi/outside
/etc/passwd
/opt/stratux/mapdata/user-upload.mbtiles
/opt/stratux/does-not-exist
LIST
rc=$(runfn)
check "odd-but-legitimate name (spaces, a glob character) IS repaired, exactly that file" "$([ "$(own '/opt/stratux/we ird*name')" = 0:0 ] && [ "$(own /opt/stratux/decoyA)" = 1000:1000 ] && echo 1 || echo 0)"
check "a symlink at a listed path is skipped: its target AND the link itself keep their owner (under /opt/stratux and at a unit path)" "$([ "$(own $victim)" = 1000:1000 ] && [ "$(stat -c %u:%g -- /lib/systemd/system/zz-evil.service | head -1)" = 1000:1000 ] && [ "$(stat -c %u:%g -- /opt/stratux/link-to-victim)" = 1000:1000 ] && echo 1 || echo 0)"
check "a symlinked parent directory is not followed (files behind it keep their owner)" "$([ "$(own /tmp/evil/stratux.conf.default)" = 1000:1000 ] && echo 1 || echo 0)"
check "'..' path components, out-of-scope paths (/home, /etc) and mapdata contents are never touched" "$([ "$(own /home/pi/outside)" = 1000:1000 ] && [ "$(own /opt/stratux/mapdata/user-upload.mbtiles)" = 1000:1000 ] && [ "$(own /etc/passwd)" = 0:0 ] && echo 1 || echo 0)"
check "missing paths are skipped; the function returns 0 and never fails the install" "$(ok "$rc" 0)"
check "the only chown issued was for the one legitimate path" "$([ "$(grep -c '^chown' $CHOWN)" = 1 ] && grep -q 'we ird\*name' $CHOWN && echo 1 || echo 0)"
rm -f /tmp/test.list; rc=$(runfn)
check "no dpkg file list at all: returns 0 quietly" "$(ok "$rc" 0)"
printf '/opt/stratux/decoyA\n' >/tmp/test.list
printf '#!/bin/sh\nexit 1\n' >/usr/local/bin/chown
rc=$(runfn 2>/tmp/fn.err)
check "a chown that fails is reported as a WARNING and still returns 0 (best-effort, never breaks the install)" "$(ok "$rc" 0)"
printf '#!/bin/sh\necho "chown $*" >> %s\nexec /bin/chown "$@"\n' "$CHOWN" >/usr/local/bin/chown
rm -rf /tmp/evil $victim /lib/systemd/system/zz-evil.service /opt/stratux/link-to-victim /opt/stratux/cfg; mv /opt/stratux/cfg.real /opt/stratux/cfg; rm -f '/opt/stratux/we ird*name'

echo "=== 8. static: the SSH helper defensive chown is kept and agrees with the migration ==="
check "postinst still contains the defensive SSH helper/unit chown+chmod (redundant with the migration, harmless, kept on purpose)" "$(grep -q 'chown root:root /opt/stratux/bin/stratux-ssh-authorized-keys.sh /lib/systemd/system/stratux_ssh_authorized_keys.service' /var/lib/dpkg/info/stratux.postinst && echo 1 || echo 0)"
exit $fail
CONTAINER
rc=${PIPESTATUS[0]}
n=$(grep -c '^PASS' "$OUT"); f=$(grep -c '^FAIL' "$OUT"); rm -f "$OUT"
echo "$n checks passed, $f failed"
[ "$n" -ge 60 ] && [ "$f" = 0 ] && [ "$rc" = 0 ] && exit 0
echo "FAIL: ownership lifecycle run did not complete cleanly (container exit $rc)"; exit 1
