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

dash -n "$POSTINST" && echo "PASS: postinst.dpkg is valid dash syntax" || { echo "FAIL: postinst.dpkg dash syntax"; fail=1; }
dash -n "$PRERM" && echo "PASS: prerm.dpkg is valid dash syntax" || { echo "FAIL: prerm.dpkg dash syntax"; fail=1; }

exit $fail
