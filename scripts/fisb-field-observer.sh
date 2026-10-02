#!/usr/bin/env bash
# fisb-field-observer.sh - read-only, low-rate telemetry for a FIS-B field session.
#
# Polls ONLY existing, read-only HTTP endpoints and appends one timestamped JSON
# line per sample. Changes nothing on the device. Added after the 2026-10-01
# field session, where several ForeFlight states (No Towers / Marginal, old radar),
# a SYSTEM_CAUTION alert and a bursty GDL90 weather uplink could be explained only
# partly because the relevant state was not captured at the time:
#   /getClients        per-client SleepFlag, LastPingResponse, LastPongResponse,
#                      LastUnreachable  (the GDL90 client sleep/throttle inputs)
#   /getAlerts         active alerts (what SYSTEM_CAUTION is)
#   /getPreflightReport which preflight items drive an overall CAUTION
#   /getStatus         UAT/product counters; /getTowers tower msgs per minute
#   /getFISBRecorderStatus (only on builds that have it) live recorder counters
#   /getFISBCacheStatus cache state and counters
# Note the stratuxClock-based timestamps in /getClients are monotonic since boot, not UTC.
#
# Usage: fisb-field-observer.sh <outfile.jsonl> [host] [interval_seconds]   (Ctrl-C to stop)
# Annotate app switches by hand with:  echo "$(date -u +%FT%TZ) ForeFlight -> Safari" >> notes.txt
set -u
OUT="${1:?usage: $0 outfile.jsonl [host] [interval_seconds]}"; HOST="${2:-192.168.10.1}"; IV="${3:-15}"
EPS="getClients getAlerts getPreflightReport getStatus getTowers getFISBRecorderStatus getFISBCacheStatus"
while :; do
  python3 - "$OUT" "$HOST" $EPS <<'P'
import json,sys,time,datetime,urllib.request
out,host,*eps=sys.argv[1:]
rec={"utc":datetime.datetime.now(datetime.timezone.utc).isoformat()}
for ep in eps:
    try:
        with urllib.request.urlopen(f"http://{host}/{ep}",timeout=6) as r: rec[ep]=json.loads(r.read().decode())
    except Exception as e: rec[ep]={"error":str(e)[:120]}
open(out,"a").write(json.dumps(rec,separators=(",",":"))+"\n")
P
  sleep "$IV"
done
