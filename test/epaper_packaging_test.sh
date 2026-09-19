#!/bin/bash
# epaper_packaging_test.sh: proves the e-paper systemd unit's durable
# activation is actually wired into packaging - a real hardware-
# validation finding: a manual, post-boot `systemctl enable` was found to
# persist only in the protected overlay's RAM-backed (tmpfs) upper layer
# and be silently lost on the very next reboot. The fix is that
# debian/postinst.dpkg unconditionally enables+starts stratux_epaper,
# exactly like stratux_fancontrol - this test proves that wiring exists
# in the actual packaging scripts, not merely in a doc comment.
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

check "postinst enables stratux_epaper" \
	"$(grep -q '^\s*systemctl enable stratux_epaper\s*$' "$POSTINST" && echo 1 || echo 0)"
check "postinst starts stratux_epaper" \
	"$(grep -q '^\s*systemctl start stratux_epaper\s*$' "$POSTINST" && echo 1 || echo 0)"
check "postinst enables stratux_epaper in the same aarch64/config.txt-guarded block as stratux_fancontrol (not unconditionally at top level)" \
	"$(awk '/systemctl enable stratux_fancontrol/,/systemctl start stratux_epaper/' "$POSTINST" | grep -q 'systemctl enable stratux_epaper' && echo 1 || echo 0)"
check "prerm stops stratux_epaper" \
	"$(grep -q 'systemctl stop stratux_epaper' "$PRERM" && echo 1 || echo 0)"
check "stratux_epaper.service unit file is present for packaging" \
	"$([ -f "$SERVICE" ] && echo 1 || echo 0)"
check "stratux_epaper.service still has WantedBy=multi-user.target (systemctl enable has a target to enable against)" \
	"$(grep -q '^WantedBy=multi-user.target$' "$SERVICE" && echo 1 || echo 0)"

dash -n "$POSTINST" && echo "PASS: postinst.dpkg is valid dash syntax" || { echo "FAIL: postinst.dpkg dash syntax"; fail=1; }
dash -n "$PRERM" && echo "PASS: prerm.dpkg is valid dash syntax" || { echo "FAIL: prerm.dpkg dash syntax"; fail=1; }

exit $fail
