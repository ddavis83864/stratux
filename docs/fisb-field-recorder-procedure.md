# FIS-B field recorder: field kit procedure

> Complements, does not replace, `docs/pr51-field-acceptance-kit.md` and
> `docs/pr51-field-results-template.md` (PR #51's own field kit). This
> procedure is specific to the field-recording facility on
> `feature/fisb-field-recorder` (see `docs/fisb-field-recorder-design.md`).
> It assumes the same ARS-Macbook / `pi@192.168.10.1` SSH access, Wi-Fi
> route, and evidence-handling conventions PR #51's kit already
> established and rehearsed - not re-derived here.
>
> **Before this procedure is run in the field, the instrumented build it
> depends on must first go through the deployment/scope-gate owner review**
> (exact artifact + SHA-256 + source revision + expected device changes +
> rollback package + capture resource bounds) and be explicitly installed
> on the grounded device by the owner. This document does not authorize
> that installation; it is the procedure to run once it has separately
> been approved and completed.
>
> **Rehearsal status (as of this revision):** every command below has
> been exercised against the real instrumented daemon in an isolated
> bench environment on ARS01 (not the grounded device, not the MacBook -
> see the final report's bench-session evidence). The MacBook/device
> portion specifically (SSH, Wi-Fi routing, physical hardware) has **not**
> been rehearsed in this pass - ARS01 had no live network path to
> ARS-Macbook or the grounded device available. `fisb-recording-tool` was
> cross-compiled for macOS (`GOOS=darwin GOARCH=arm64`) and confirmed to
> build cleanly, but not run there. That remains an open gate before a
> field trip - see the final report's ready/not-ready determination.

## What this adds to the existing kit

PR #51's kit already gets you to the device with SSH/package/network
access rehearsed. This procedure adds: turning the recorder on before
entering reception range, confirming it actually captured something,
turning it off cleanly, pulling the bundle back to the MacBook, and
running it through `fisb-recording-tool validate`/`replay`/`compare`
*before* the trip is called successful.

## Prerequisites (verify once per trip, before leaving)

1. The device is running a build with `FISBRecordingEnabled` support -
   confirm via:
   ```
   curl -s http://192.168.10.1/getSettings | python3 -c 'import json,sys; d=json.load(sys.stdin); print("FISBRecordingEnabled" in d)'
   ```
   `True` means this build has the setting. `False`/`KeyError` means the
   device is still running a build from before this branch was installed
   - stop, this is not the instrumented build, do not proceed.
2. `fisb-recording-tool` is built and present on the MacBook:
   ```
   go build -o ~/stratux-field-acceptance/kit/fisb-recording-tool ./cmd/fisb-recording-tool/
   ```
   (run from a checkout of this branch; the binary is pure Go, no cgo, so
   it builds natively on the Mac with no cross-toolchain).
3. SSH reaches the device: `ssh pi@192.168.10.1 true`.

## Start (before entering 978 MHz reception range)

```
ssh pi@192.168.10.1 '
  curl -s -X POST http://localhost/setSettings \
    -H "Content-Type: application/json" \
    -d "{\"FISBRecordingEnabled\": true}" \
  && sleep 2 \
  && curl -s http://localhost/getSettings | grep -o "\"FISBRecordingEnabled\":[a-z]*"
'
```
Expect `"FISBRecordingEnabled":true` echoed back. `fisbRecorderWatchdog`
(main/fisbrecorderwiring.go) polls this setting once a second and starts
the session; allow up to ~2s for the start log line, visible via:
```
ssh pi@192.168.10.1 'sudo journalctl -u stratux -n 20 --no-pager | grep fisbRecorder'
```
Expect a `field-recording session <YYYYMMDD-HHMMSS> started` line. Note
the session ID - it names the bundle directory you'll pull back later.

**If this line does not appear within 10 seconds**, do not proceed into
the field window on the assumption recording is running - stop and
diagnose (check `FISBRecordingEnabled` really is `true`, check disk space
under `/var/lib/stratux-data`, check the service is actually running the
instrumented build per Prerequisite 1).

## Monitor (periodically during the field window)

```
ssh pi@192.168.10.1 'ls -la /var/lib/stratux-data/fisb-recordings/<session-id>/'
```
Growing `frames.jsonl.gz`/`gdl90.jsonl.gz`/`snapshots.jsonl.gz` file sizes
across successive checks is the simplest live evidence that capture is
progressing. There is no separate live counter API yet - this direct
file-size check is deliberately the same low-tech method the design
favors over adding a new HTTP endpoint mid-trip.

## Stop (at the end of the field window, or at the first bounded-stop
condition)

```
ssh pi@192.168.10.1 '
  curl -s -X POST http://localhost/setSettings \
    -H "Content-Type: application/json" \
    -d "{\"FISBRecordingEnabled\": false}" \
  && sleep 2 \
  && sudo journalctl -u stratux -n 20 --no-pager | grep fisbRecorder
'
```
Expect a `field-recording session <id> stopped (stopReason=requested,
frames=<N>, dropped=<D>)` line. `stopReason` other than `requested` (e.g.
`disk_full`, `max_duration`) means the session ended on its own bound, not
your stop request - the bundle is still usable (`Validate` will mark it
`partial`), but note which bound was hit in the results template.

**If reception never produces a tower or useful products** (frames=0 or
very low), do not extend the window indefinitely or manufacture activity
- stop on schedule, preserve the partial bundle exactly as below, and
report the missing gates honestly in the results template.

## Verify and preserve (before leaving the site, or immediately on return)

```
SESSION=<session-id-from-the-start-step>
mkdir -p ~/stratux-field-acceptance/evidence/fisb-recorder/$SESSION
scp -r pi@192.168.10.1:/var/lib/stratux-data/fisb-recordings/$SESSION \
  ~/stratux-field-acceptance/evidence/fisb-recorder/

cd ~/stratux-field-acceptance/evidence/fisb-recorder/$SESSION
sha256sum -c <(python3 -c "
import json
m = json.load(open('manifest.json'))
for f in m['files']:
    print(f['sha256'] + '  ' + f['name'])
")
```
Every file must report `OK`. A `FAILED` here means the copy itself was
corrupted in transit - re-copy before doing anything else; do not attempt
to validate or replay a bundle that failed its own hash check on arrival.

Then, on the MacBook (or ARS01 once copied further):
```
fisb-recording-tool validate ~/stratux-field-acceptance/evidence/fisb-recorder/$SESSION
```
Exit code 0 and `"classification": "valid"` is the clean case. `"partial"`
still exit-codes non-zero but is a usable bundle with caveats printed in
`warnings` - read them, note them in the results template, do not discard
the bundle. `"unusable"` means stop: do not claim this session as field
evidence; report exactly what `errors` says failed.

## Recovery from an interrupted capture (rehearse this on the bench before
travel)

Simulate an interruption: start a session, record a few frames, then kill
the daemon (`sudo systemctl restart stratux`) instead of stopping
recording cleanly. On restart, `initFISBRecorder()` checks the persisted
setting: if it was still `true` at the moment of the kill, a **new**
session starts (a new session ID, new directory) - the interrupted
session's own files stay on disk exactly as they were at the moment of
the kill, with **no manifest.json** (the manifest is written only at a
clean `Stop()`).

To recover what's usable from an interrupted session:
```
ssh pi@192.168.10.1 'ls /var/lib/stratux-data/fisb-recordings/'
```
Identify the interrupted session's directory (the one with data files but
no `manifest.json`). Copy it back the same way as above. `fisb-recording-
tool validate` on a directory with no `manifest.json` reports `unusable`
with an explicit "cannot read manifest.json" error - this is correct and
expected, not a bug: an interrupted session was never finalized, so its
own record/GDL90/snapshot counts and file hashes were never computed or
written. The raw `frames.jsonl.gz` etc. may still be partially readable
gzip streams (gzip's own format allows reading whatever was flushed before
the kill), but treat any such recovery as best-effort forensics, not a
bundle that passes this facility's own completeness checks - never
represent it as a `valid` or `partial` result Validate itself produced.

## Offline replay and comparison (after preserving the bundle, before
declaring the trip's evidence complete)

```
fisb-recording-tool replay -speed=-1 ~/stratux-field-acceptance/evidence/fisb-recorder/$SESSION > replay-log.txt
```
This proves the recorded frames are readable and well-formed (each line
is one frame's seq/elapsed/length) - it does **not** re-run them through
the real parser/cache/GDL90 path, since this CLI tool has no cgo
dependency and cannot call `main`'s own `handleUatMessage`.

For a true replay-through-the-real-pipeline comparison, run the actual
instrumented `stratuxrun` binary itself in `-fisbReplay` mode (added
after this document's first draft - see
docs/fisb-field-recorder-design.md's Phase 2 notes and the bench session
evidence in the final report for a worked example):
```
stratuxrun -fisbReplay ~/stratux-field-acceptance/evidence/fisb-recorder/$SESSION -fisbReplaySpeed 1.0
```
This replays the bundle's `frames.jsonl.gz` through `handleUatMessage` -
the exact same function live reception calls - reproducing tower
identity, FIS-B cache admission, and GDL90 relay via the real production
code. `-fisbReplaySpeed` follows the same convention as the CLI tool's
`-speed`: `1.0` = original recorded pacing, `>1` = faster, `<=0` = as
fast as possible. If `FISBRecordingEnabled` is already `true` (in that
instance's own persisted settings) when the replay finishes, the daemon
closes and hashes a second, replay-derived bundle automatically - compare
it against the original with:
```
fisb-recording-tool compare <original-session-dir> <replay-derived-session-dir>
```
Frame bytes must match exactly, in order - `-fisbReplaySpeed` only
changes pacing, never content. GDL90/snapshot **counts** will generally
differ between the two bundles (the daemon keeps sending its own
periodic heartbeat/traffic/status output for as long as each session
runs, so a longer-duration live session accumulates more of that
background traffic than a short replay of the same handful of frames) -
that is a real, expected, clock-dependent difference, not a defect; it
does not need to be explained away to accept the bundle, only understood
when reading the comparison.

## Field acceptance criteria for this recorder specifically

In addition to PR #51's own L1-L7/F1-F3/B-series gates, a field trip's
recording is acceptance-worthy only if **all** of the following hold -
missing any one means reporting exactly which gate is missing, not
calling the dataset complete:

- [ ] Build verified (Prerequisite 1) and recording confirmed started
      (the `started` log line) before entering reception range.
- [ ] At least one real decoded UAT frame captured, and at least one real
      tower-identification event derivable from it (a non-empty
      `frames.jsonl.gz`, confirmed via `validate`'s `countedFrames`).
- [ ] No unexplained capture drops (`manifest.droppedFrames`/
      `droppedGdl90`/`droppedSnapshots` are all 0, or any non-zero value
      is explained by an identified bound, e.g. a brief queue-depth spike
      during a burst - not silently accepted).
- [ ] At least one text weather product actually received; NEXRAD only if
      actually received (never claimed if the session captured none -
      report its absence as a gate not met, not worked around).
- [ ] A closed, hashed bundle, copied to the MacBook, independently
      re-validated there (the `sha256sum -c` step above, then `validate`).
- [ ] A successful offline replay of the bundle's frames (the `replay`
      step above completing without error).
