#!/bin/bash
# epaper_packaging_test.sh: proves the e-paper systemd unit's durable
# activation is actually wired into packaging - a real hardware-
# validation finding, found TWICE:
#
#   1. A manual, post-boot `systemctl enable` was found to persist only
#      in the protected overlay's RAM-backed (tmpfs) upper layer and be
#      silently lost on the very next reboot.
#   2. The first fix for #1 placed `systemctl enable stratux_epaper` in
#      the SAME block as `systemctl start stratux_epaper`, after the
#      STRATUX_OTA_INSTALL early-exit - which meant an OTA-delivered
#      install of this newly-added service never actually reached that
#      line at all, leaving the unit permanently disabled/inactive on
#      any device that received it via OTA rather than a fresh image
#      build. Confirmed on real hardware: after a real OTA install,
#      `systemctl is-enabled stratux_epaper` reported "disabled".
#
# The fix: `systemctl enable` (never `start`) for stratux_epaper must run
# BEFORE the STRATUX_OTA_INSTALL check; `systemctl start` stays after it
# (needed only for the non-OTA path - the OTA path's own subsequent
# reboot starts an enabled unit through the ordinary boot sequence). This
# test proves that exact ordering exists in the actual packaging script,
# not merely in a doc comment - it is a direct regression test for
# finding #2 above.
set -u
cd "$(dirname "$0")/.."

fail=0
check() {
	local desc="$1" ok="$2"
	if [ "$ok" = "1" ]; then
		echo "PASS: $desc"
	else
		echo "FAIL: $desc"
		fail=1
	fi
}

POSTINST="debian/postinst.dpkg"
PRERM="debian/prerm.dpkg"
SERVICE="debian/stratux_epaper.service"

enable_line=$(grep -n '^\s*systemctl enable stratux_epaper\s*$' "$POSTINST" | head -1 | cut -d: -f1)
start_line=$(grep -n '^\s*systemctl start stratux_epaper\s*$' "$POSTINST" | head -1 | cut -d: -f1)
ota_check_line=$(grep -n 'STRATUX_OTA_INSTALL:-' "$POSTINST" | head -1 | cut -d: -f1)

check "postinst enables stratux_epaper" \
	"$([ -n "$enable_line" ] && echo 1 || echo 0)"
check "postinst starts stratux_epaper" \
	"$([ -n "$start_line" ] && echo 1 || echo 0)"
check "STRATUX_OTA_INSTALL check is present" \
	"$([ -n "$ota_check_line" ] && echo 1 || echo 0)"

# The actual regression test: enable must come BEFORE the OTA early-exit
# check, and start must come AFTER it - otherwise an OTA-delivered
# install never durably enables the unit at all (finding #2 above).
if [ -n "$enable_line" ] && [ -n "$ota_check_line" ] && [ -n "$start_line" ]; then
	check "systemctl enable stratux_epaper runs BEFORE the STRATUX_OTA_INSTALL early-exit (so an OTA-delivered install still durably enables it)" \
		"$([ "$enable_line" -lt "$ota_check_line" ] && echo 1 || echo 0)"
	check "systemctl start stratux_epaper runs AFTER the STRATUX_OTA_INSTALL early-exit (never invoked during an OTA install, per the documented recursion hazard)" \
		"$([ "$start_line" -gt "$ota_check_line" ] && echo 1 || echo 0)"
else
	echo "FAIL: could not locate all three anchor lines to check ordering"
	fail=1
fi

# stratux_fancontrol gets the identical treatment, in the same edit - not
# scope creep, the same restructured conditional block covers both.
fc_enable_line=$(grep -n '^\s*systemctl enable stratux_fancontrol\s*$' "$POSTINST" | head -1 | cut -d: -f1)
fc_start_line=$(grep -n '^\s*systemctl start stratux_fancontrol\s*$' "$POSTINST" | head -1 | cut -d: -f1)
if [ -n "$fc_enable_line" ] && [ -n "$ota_check_line" ] && [ -n "$fc_start_line" ]; then
	check "systemctl enable stratux_fancontrol ALSO runs before the OTA early-exit" \
		"$([ "$fc_enable_line" -lt "$ota_check_line" ] && echo 1 || echo 0)"
	check "systemctl start stratux_fancontrol stays after the OTA early-exit" \
		"$([ "$fc_start_line" -gt "$ota_check_line" ] && echo 1 || echo 0)"
else
	echo "FAIL: could not locate stratux_fancontrol anchor lines"
	fail=1
fi

check "prerm stops stratux_epaper" \
	"$(grep -q 'systemctl stop stratux_epaper' "$PRERM" && echo 1 || echo 0)"
check "stratux_epaper.service unit file is present for packaging" \
	"$([ -f "$SERVICE" ] && echo 1 || echo 0)"
check "stratux_epaper.service still has WantedBy=multi-user.target (systemctl enable has a target to enable against)" \
	"$(grep -q '^WantedBy=multi-user.target$' "$SERVICE" && echo 1 || echo 0)"

# --- ARS boot splash unit (stratux_epaper_splash) -------------------------
# Same durable-enable discipline as stratux_epaper above, plus: it is never
# started by the package (it runs at boot, before stratux_epaper), and the
# package stops it before replacing/removing itself.
SPLASH_SERVICE="debian/stratux_epaper_splash.service"
sp_enable_line=$(grep -n '^\s*systemctl enable stratux_epaper_splash\s*$' "$POSTINST" | head -1 | cut -d: -f1)
if [ -n "$sp_enable_line" ] && [ -n "$ota_check_line" ]; then
	check "systemctl enable stratux_epaper_splash runs BEFORE the STRATUX_OTA_INSTALL early-exit (an OTA-delivered install still durably enables it)" \
		"$([ "$sp_enable_line" -lt "$ota_check_line" ] && echo 1 || echo 0)"
else
	echo "FAIL: could not locate the stratux_epaper_splash enable line"
	fail=1
fi
check "postinst never starts/restarts stratux_epaper_splash (it runs at boot only; also never invoked during an OTA install)" \
	"$(grep -Eq 'systemctl (start|restart|reload-or-restart|try-restart) +stratux_epaper_splash|systemctl enable +(--now|-\-now).*stratux_epaper_splash' "$POSTINST" && echo 0 || echo 1)"
check "postinst enables the splash inside the same Raspberry Pi/aarch64 guard as stratux_epaper" \
	"$(awk '/if +\[ "\$arch" == "aarch64" \]; then/{a=1} a&&/systemctl enable stratux_epaper_splash/{f=1} /^fi$/{a=0} END{print f?1:0}' "$POSTINST")"
check "prerm stops stratux_epaper_splash" \
	"$(grep -q 'systemctl stop stratux_epaper_splash' "$PRERM" && echo 1 || echo 0)"
sp_stop=$(grep -n 'systemctl stop stratux_epaper_splash' "$PRERM" | head -1 | cut -d: -f1)
ep_stop=$(grep -n 'systemctl stop stratux_epaper 2' "$PRERM" | head -1 | cut -d: -f1)
check "prerm stops the splash BEFORE the operational renderer (never both owning the panel while the package is replaced)" \
	"$([ -n "$sp_stop" ] && [ -n "$ep_stop" ] && [ "$sp_stop" -lt "$ep_stop" ] && echo 1 || echo 0)"
check "stratux_epaper_splash.service unit file is present for packaging" \
	"$([ -f "$SPLASH_SERVICE" ] && echo 1 || echo 0)"
check "stratux_epaper_splash.service has WantedBy=multi-user.target" \
	"$(grep -q '^WantedBy=multi-user.target$' "$SPLASH_SERVICE" && echo 1 || echo 0)"
check "stratux_epaper_splash.service is ordered Before=stratux_epaper.service" \
	"$(grep -q '^Before=stratux_epaper.service$' "$SPLASH_SERVICE" && echo 1 || echo 0)"
check "stratux_epaper_splash.service is Type=oneshot" \
	"$(grep -q '^Type=oneshot$' "$SPLASH_SERVICE" && echo 1 || echo 0)"
check "the Makefile packages stratux_epaper_splash.service into lib/systemd/system" \
	"$(grep -q 'cp debian/stratux_epaper_splash.service \$(DEBPKG_BASE)/lib/systemd/system' Makefile && echo 1 || echo 0)"
check "stratux_epaper.service (validated operational unit) does not mention the splash" \
	"$(grep -v '^\s*#' "$SERVICE" | grep -qi splash && echo 0 || echo 1)"

# Real-systemd parse of the new unit, when systemd-analyze is available:
# verifies every directive is recognized and ordering resolves. Skipped
# (not failed) on hosts without it.
if command -v systemd-analyze >/dev/null 2>&1; then
	tmp=$(mktemp -d)
	mkdir -p "$tmp/etc/systemd/system" "$tmp/opt/stratux/bin" "$tmp/usr/lib/systemd/system"
	cp -a /usr/lib/systemd/system/. "$tmp/usr/lib/systemd/system/" 2>/dev/null
	cp debian/stratux_epaper.service debian/stratux_epaper_splash.service "$tmp/etc/systemd/system/"
	printf '#!/bin/sh\nexit 0\n' > "$tmp/opt/stratux/bin/epaperd"; chmod +x "$tmp/opt/stratux/bin/epaperd"
	cp "$tmp/opt/stratux/bin/epaperd" "$tmp/opt/stratux/bin/stratuxrun"
	cp debian/stratux.service "$tmp/etc/systemd/system/"
	out=$(systemd-analyze verify --root="$tmp" stratux_epaper_splash.service stratux_epaper.service 2>&1)
	rc=$?
	# stratux.service is pulled in by stratux_epaper's Wants=; its killall
	# ExecStopPost is absent from the scratch root, which is not this unit's problem.
	out=$(printf '%s\n' "$out" | grep -v 'killall')
	check "systemd-analyze verify accepts stratux_epaper_splash.service and stratux_epaper.service (no unknown directives, ordering resolves)" \
		"$([ -z "$out" ] && echo 1 || echo 0)"
	[ -n "$out" ] && printf '%s\n' "$out"
	rm -rf "$tmp"
else
	echo "SKIP: systemd-analyze not available; real-systemd unit parse not run"
fi

# --- Execute the real maintainer scripts against stubs ---------------------
# Runs debian/postinst.dpkg and prerm.dpkg (with the Pi marker file path
# redirected into a scratch dir and systemctl/uname stubbed) and asserts the
# exact systemctl calls, in both OTA and normal-install modes.
run_script() { # $1=script $2=logfile $3=extra env (e.g. STRATUX_OTA_INSTALL=1 or "")
	local script="$1" log="$2" extra="$3" sb
	sb=$(mktemp -d)
	mkdir -p "$sb/bin" "$sb/boot"
	: > "$sb/boot/config.txt"
	printf '#!/bin/sh\necho "systemctl $*" >> "%s"\nexit 0\n' "$log" > "$sb/bin/systemctl"
	printf '#!/bin/sh\n[ "$1" = "-m" ] && echo aarch64 || echo Linux\n' > "$sb/bin/uname"
	chmod +x "$sb/bin/systemctl" "$sb/bin/uname"
	sed "s#/boot/firmware/config.txt#$sb/boot/config.txt#g" "$script" > "$sb/script.sh"
	: > "$log"
	env $extra PATH="$sb/bin:$PATH" bash "$sb/script.sh" >/dev/null 2>&1
	rm -rf "$sb"
}
LOGDIR=$(mktemp -d)

run_script "$POSTINST" "$LOGDIR/post_ota.log" "STRATUX_OTA_INSTALL=1"
check "postinst (OTA install) enables the splash and starts NOTHING" \
	"$(grep -qx 'systemctl enable stratux_epaper_splash' "$LOGDIR/post_ota.log" && ! grep -Eq 'systemctl (start|restart)' "$LOGDIR/post_ota.log" && echo 1 || echo 0)"

run_script "$POSTINST" "$LOGDIR/post_norm.log" "STRATUX_UNUSED=1"
check "postinst (normal install) enables the splash" \
	"$(grep -qx 'systemctl enable stratux_epaper_splash' "$LOGDIR/post_norm.log" && echo 1 || echo 0)"
check "postinst (normal install) still starts stratux_epaper, but never the splash" \
	"$(grep -qx 'systemctl start stratux_epaper' "$LOGDIR/post_norm.log" && ! grep -q 'start stratux_epaper_splash' "$LOGDIR/post_norm.log" && echo 1 || echo 0)"
check "postinst enable order is unchanged for the existing units (fancontrol, then epaper, then splash)" \
	"$(grep '^systemctl enable stratux_' "$LOGDIR/post_norm.log" | tr '\n' ' ' | grep -q '^systemctl enable stratux_fancontrol systemctl enable stratux_epaper systemctl enable stratux_epaper_splash $' && echo 1 || echo 0)"

run_script "$PRERM" "$LOGDIR/prerm.log" "STRATUX_UNUSED=1"
check "prerm (normal removal/upgrade) stops splash then operational renderer, in that order" \
	"$(grep -n 'stop stratux_epaper' "$LOGDIR/prerm.log" | sed 's/^[0-9]*://' | tr '\n' '|' | grep -q '^systemctl stop stratux_epaper_splash|systemctl stop stratux_epaper|$' && echo 1 || echo 0)"
run_script "$PRERM" "$LOGDIR/prerm_ota.log" "STRATUX_OTA_INSTALL=1"
check "prerm (OTA install) touches no units" \
	"$([ ! -s "$LOGDIR/prerm_ota.log" ] && echo 1 || echo 0)"
rm -rf "$LOGDIR"

dash -n "$POSTINST" && echo "PASS: postinst.dpkg is valid dash syntax" || { echo "FAIL: postinst.dpkg dash syntax"; fail=1; }
dash -n "$PRERM" && echo "PASS: prerm.dpkg is valid dash syntax" || { echo "FAIL: prerm.dpkg dash syntax"; fail=1; }

exit $fail
