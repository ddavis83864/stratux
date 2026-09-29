#!/bin/bash
# pr51_field_preflight_capture.sh: captures the "Before deployment" evidence
# baseline for PR #51's field acceptance kit (docs/pr51-field-acceptance-kit.md,
# section "Before deployment", checks B3-B11) in one pass.
#
# READ-ONLY. This script never uploads a package, never calls /updateUpload,
# /setSettings, /setFISBCacheSettings, /restart, /reboot, /shutdown, or any
# other mutating endpoint, and never writes to the device over SSH - only
# GET requests and read-only SSH commands (cat, find, du, df, systemctl
# status/is-active, dpkg --audit). Safe to run repeatedly.
#
# Deliberately does NOT: upload/build the candidate, trigger OTA, or perform
# any destructive/disruptive action - those are separate, explicit,
# owner-attended steps in the kit, not something an automated script should
# ever do unattended.
#
# Usage:
#   EV=~/acceptance-evidence/stratux-pr51-field-$(date -u +%F) \
#     test/pr51_field_preflight_capture.sh [device-host]
#
# device-host defaults to 192.168.10.1. Requires: curl, ssh access to
# pi@<device-host> (see docs/ssh-authorized-keys.md), python3.
set -u

HOST="${1:-192.168.10.1}"
EV="${EV:-$HOME/acceptance-evidence/stratux-pr51-field-$(date -u +%F)}"
mkdir -p -m 700 "$EV"/pre
echo "Evidence directory: $EV/pre"

fail=0
get() { # $1 = path, $2 = output file
	if ! curl -s -m 6 "http://$HOST$1" -o "$EV/pre/$2"; then
		echo "FAIL: GET $1 (device unreachable? try: nmcli con up Stratux)" >&2
		fail=1
	fi
}

echo "== B3: device build =="
get /getStatus getStatus.json
python3 -c "import json; print('Build:', json.load(open('$EV/pre/getStatus.json')).get('Build'))" 2>/dev/null

echo "== B6: settings (local evidence only - never posted anywhere) =="
get /getSettings getSettings.json

echo "== B7: OTA state (must be idle before proceeding) =="
get /getOTAStatus getOTAStatus.json
stage=$(python3 -c "import json; print(json.load(open('$EV/pre/getOTAStatus.json')).get('Stage','?'))" 2>/dev/null)
echo "OTA stage: $stage"
if [ "$stage" != "idle" ]; then
	echo "FAIL: OTA stage is '$stage', not idle - do not proceed with a deployment" >&2
	fail=1
fi

echo "== B8: FIS-B cache status =="
get /getFISBCacheStatus getFISBCacheStatus.json

echo "== B9: FIS-B cache inventory =="
get /getFISBCacheInventory getFISBCacheInventory.json

echo "== B4/B5/B10/B11: boot ID, service health, persisted cache files, free space (read-only SSH) =="
ssh -o ConnectTimeout=6 -o BatchMode=yes "pi@$HOST" '
echo "== boot_id =="; cat /proc/sys/kernel/random/boot_id
echo "== failed units =="; systemctl --failed --no-legend
echo "== dpkg audit =="; sudo dpkg --audit && echo AUDIT_OK
echo "== stratux/epaper units =="; systemctl is-active stratux stratux_epaper stratux_epaper_splash stratux_epaper_shutdown 2>&1
echo "== fisb cache file count =="; sudo find /var/lib/stratux-data/fisb-weather-cache -maxdepth 1 -type f 2>/dev/null | wc -l
echo "== fisb cache file listing =="; sudo find /var/lib/stratux-data/fisb-weather-cache -maxdepth 1 -type f 2>/dev/null | sort
echo "== fisb cache size =="; sudo du -sh /var/lib/stratux-data/fisb-weather-cache 2>&1
echo "== free space =="; df -h /var/lib/stratux-data /boot/firmware / 2>&1
' > "$EV/pre/ssh-health.txt" 2>&1 || { echo "FAIL: SSH capture" >&2; fail=1; }
cat "$EV/pre/ssh-health.txt"

# Split the fisb cache file listing out on its own for B10's later diff.
awk '/== fisb cache file listing ==/{flag=1;next}/== fisb cache size ==/{flag=0}flag' "$EV/pre/ssh-health.txt" > "$EV/pre/fisb-cache-files.txt"
grep "boot_id" -A1 "$EV/pre/ssh-health.txt" | tail -1 > "$EV/pre/boot-id.txt"

echo
if [ "$fail" -eq 0 ]; then
	echo "All checks captured to $EV/pre - review docs/pr51-field-acceptance-kit.md before proceeding."
else
	echo "One or more checks FAILED - see above. Do not proceed to deployment until resolved." >&2
fi
exit "$fail"
