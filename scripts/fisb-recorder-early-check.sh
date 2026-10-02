#!/usr/bin/env bash
# fisb-recorder-early-check.sh - prove, within minutes, that the FIS-B field
# recorder is actually admitting raw UAT frames while live UAT is arriving.
#
# Why: on 2026-10-01 a 52-minute field session ran with live UAT reception
# but recorded ZERO raw frames, because the production low-power UAT radio
# bypassed the recorder hook. The gzip output sits at 0 bytes while buffered,
# so file size cannot reveal this; the recorder's own counter can.
#
# Receiver-agnostic: uses only /getStatus UAT_messages_total (which counts
# frames from every UAT receiver) and /getFISBRecorderStatus framesAccepted.
# It does not depend on any RTL-SDR/assignment state.
#
# Usage: fisb-recorder-early-check.sh [host[:port]] [window_seconds] [min_uat_msgs]
#   defaults: 192.168.10.1, 240, 50
# Run AFTER enabling FISBRecordingEnabled and before settling into the window.
#
# Exit codes / verdicts (printed as the last line):
#   0 PASS_EARLY               framesAccepted > 0 while UAT is arriving
#   2 RECORDER_PATH_FAILURE    >= min_uat_msgs UAT messages arrived, framesAccepted == 0
#   3 UNSUPPORTED_BUILD        build has no /getFISBRecorderStatus - cannot check live
#   4 INCONCLUSIVE_NO_RF       window ended with fewer than min_uat_msgs UAT messages
#   5 RECORDER_NOT_ACTIVE      recorder is not recording (setting off / failed start)
#   6 DEVICE_UNREACHABLE
set -u
DEV="${1:-192.168.10.1}"; WINDOW="${2:-240}"; MINUAT="${3:-50}"
BASE="http://$DEV"; T0=$(date +%s)

fetch() { curl -s -m 8 -w '\n%{http_code}' "$BASE/$1" 2>/dev/null; }
jget()  { python3 -c 'import json,sys
d=json.load(sys.stdin); v=d.get(sys.argv[1]); print("" if v is None else v)' "$1"; }

ST=$(fetch getStatus); code=${ST##*$'\n'}
[ "$code" = 200 ] || { echo "cannot read /getStatus (HTTP $code)"; echo "VERDICT DEVICE_UNREACHABLE"; exit 6; }
BASE_UAT=$(printf '%s' "${ST%$'\n'*}" | jget UAT_messages_total)

while :; do
  R=$(fetch getFISBRecorderStatus); rcode=${R##*$'\n'}; rbody=${R%$'\n'*}
  if [ "$rcode" = 404 ]; then
    echo "this build has no /getFISBRecorderStatus"; echo "VERDICT UNSUPPORTED_BUILD"; exit 3
  fi
  [ "$rcode" = 200 ] || { echo "recorder status HTTP $rcode"; echo "VERDICT DEVICE_UNREACHABLE"; exit 6; }
  ST=$(fetch getStatus); sbody=${ST%$'\n'*}
  UAT=$(printf '%s' "$sbody" | jget UAT_messages_total)
  ACTIVE=$(printf '%s' "$rbody" | jget active)
  FRAMES=$(printf '%s' "$rbody" | jget framesAccepted)
  DROPS=$(printf '%s' "$rbody" | jget droppedFrames)
  ELAPSED=$(( $(date +%s) - T0 ))
  DELTA=$(( UAT - BASE_UAT ))
  echo "t+${ELAPSED}s uat_delta=$DELTA recorder_active=$ACTIVE framesAccepted=$FRAMES droppedFrames=$DROPS"

  if [ "$ACTIVE" != "True" ] && [ "$ACTIVE" != "true" ]; then
    echo "VERDICT RECORDER_NOT_ACTIVE"; exit 5
  fi
  if [ "${FRAMES:-0}" -gt 0 ]; then
    echo "VERDICT PASS_EARLY"; exit 0
  fi
  if [ "$DELTA" -ge "$MINUAT" ]; then
    echo "RAW RECORDER FRAME COUNT IS 0 WHILE $DELTA UAT MESSAGES ARRIVED"
    echo "VERDICT RECORDER_PATH_FAILURE"; exit 2
  fi
  if [ "$ELAPSED" -ge "$WINDOW" ]; then
    echo "only $DELTA UAT messages in ${ELAPSED}s (< $MINUAT): no RF to judge by"
    echo "VERDICT INCONCLUSIVE_NO_RF"; exit 4
  fi
  sleep 10
done
