# FIS-B field recorder: field kit procedure

> Complements, does not replace, `docs/pr51-field-acceptance-kit.md` and
> `docs/pr51-field-results-template.md` (PR #51's own field kit). This
> procedure is specific to the field-recording facility on
> `feature/fisb-field-recorder` (see `docs/fisb-field-recorder-design.md`).
> It assumes the same ARS-Macbook / `pi@192.168.10.1` SSH access, Wi-Fi
> route, and evidence-handling conventions PR #51's kit already
> established and rehearsed - not re-derived here.
>
> **Any future re-install of an updated candidate (a new fix, a new
> commit) still needs its own owner review before installation** - exact
> artifact + SHA-256 + source revision + expected device changes +
> rollback package + capture resource bounds - even though the specific
> candidate this revision documents has already gone through that and
> been installed; this document itself does not re-authorize a
> *different* build.
>
> **ARS-Macbook is an Intel MacBookPro8,2 running Ubuntu 24.04 - `linux/amd64`,
> NOT macOS.** Despite its name, it never runs a `darwin` binary. Build
> `fisb-recording-tool` for `GOOS=linux GOARCH=amd64` (native if built
> directly on the laptop itself, or cross-compiled from ARS01 with those
> two env vars set - never `GOOS=darwin`, which an earlier revision of
> this document incorrectly suggested and which would simply fail to
> execute there).
>
> **Rehearsal status (as of this revision): full rehearsal completed on
> the actual ARS-Macbook and the actual grounded test device**, not just
> on ARS01 - see the final report for the complete evidence trail. This
> included: `fisb-recording-tool` (`validate`/`replay`/`compare`/
> `compare-weather`) run for real on the MacBook against a real
> daemon-produced bundle; an owner-authorized OTA install of the PR #54
> candidate onto the grounded device (build confirmed matching, cache
> recovery proven across the reboot with real, trusted-GNSS-time
> persisted products); a real, bounded, at-home start->monitor->stop
> sequence (frame count zero; GDL90/snapshot traffic captured normally.
> **Correction, 2026-10-01:** the original text here attributed the zero
> frames to "no 978 MHz reception at that location/time". That was never
> established. The production receiver is the external low-power UAT radio,
> and until the fix described under "2026-10-01 field failure" below the
> recorder could not see that radio's frames at all, so zero frames was the
> expected result whether or not RF was present); the bundle
> copied off the device, hash-verified on both the MacBook and the
> device; and `validate` run on the copy. That real session is *how* a
> genuine record-accounting race in `Stop()` was found and fixed (see
> `fisbrecorder/recorder.go`'s own commit history) - this procedure's own
> exercise of the real workflow is what caught it, not a synthetic test.
> A second, real rehearsal after the fix (session `20260930-175710`, 3,476
> GDL90 records) confirmed the fix directly: `countedGdl90` exactly
> matched `manifest.gdl90Count` with no accounting-mismatch warning,
> versus the pre-fix session's own re-validated `manifest claims 82559
> gdl90 records, file contains 82557`. `validate`'s classification was
> still `partial` on this second session too, because zero frames were
> captured (cause: the low-power-UAT recorder bypass, see the 2026-10-01
> correction above - not established as "no RF at home"), not because of the
> accounting defect. Do not read "partial" here as the
> fix having failed; read the counted-vs-manifest numbers directly. This
> is still **not** field acceptance: no real 978 MHz tower or FIS-B
> product was received in this rehearsal, and no ForeFlight
> observation was made - see the final report's explicit separation of
> synthetic bench proof, grounded-device rehearsal, and real field
> RF/ForeFlight acceptance.

## 2026-10-01 field failure and remediation

**Field result: `FISB_FIELD_RECORDING_ACCEPTANCE_FAIL`.** Session
`20261001-225508` (22:55:08-23:47:12 UTC, 52m04s, build `ea1c3e43`) ran at a
site with strong live 978 MHz FIS-B (12,404 UAT messages, 561 METAR, 445 TAF,
4,129 NEXRAD, 615 NOTAM, one tower) yet recorded 262,266 GDL90 records, 1,872
snapshots and **zero raw frames** (`frames.jsonl.gz` was a valid, empty
28-byte gzip stream). The session is preserved read-only and hash-verified as
permanent negative evidence; it is not a regression corpus.

**Root cause (confirmed from source and runtime).** The installed receiver is
the external low-power UAT radio (`UATRadio_connected=true`,
`UAT_Detected=false`, `UAT_AssignmentSource=external`; no RTL-SDR on 978). Its
path was `radioSerialPortReader` -> `processRadioMessage`
(`main/lowpower_uat.go`) -> `parseInput` -> `relayMessage`, which never called
`handleUatMessage`, the only place `fisbRecorder.RecordFrame` is hooked. GDL90
and snapshots attach downstream of / independent of any receiver, so they
worked. The recorder's own tests and rehearsals only ever exercised the
RTL-SDR/`godump978`, `-uatin`, `-replay` and `-fisbReplay` paths.

**Remediation.** `processRadioMessage` now passes its post-FEC string to
`handleUatMessage`. Reed-Solomon failures and unhandled sizes still never reach
it. See `main/lowpower_uat.go`, `main/lowpower_uat_recorder_test.go`.

Ingestion paths before/after (from source):

| Input path | via `handleUatMessage` (before -> after) | `RecordFrame` (before -> after) |
|---|---|---|
| RTL-SDR / `godump978` (`uatReader`) | yes -> yes | yes -> yes |
| External low-power UAT radio | **no -> yes** | **no -> yes** |
| `-uatin` | yes -> yes | yes -> yes |
| `-replay` / `-uatlog` | yes -> yes | yes -> yes |
| `-fisbReplay` | yes -> yes | yes -> yes |
| Trace replay `CONTEXT_GODUMP978` | yes -> yes | yes -> yes |
| Trace replay `CONTEXT_LOWPOWERUAT` | no -> yes (re-enters `processRadioMessage`) | no -> yes |
| uAvionix Ping (`main/ping.go`) | no -> no | no -> no |
| uAvionix Pong (`main/pong.go`) | no -> no | no -> no |

**Known remaining gap (not the installed receiver, not fixed here):** the Ping
and Pong receivers also call `parseInput` directly and are still not recorded.
`TestUATIngestion_NoNewDirectParseInputCallers` lists them as explicit,
documented exceptions so a *new* bypass fails the build; closing these two is
follow-up work.

**Frame representation.** `godump978` yields `+hex;rs=N;ss=N;`; the low-power
radio yields `+hex;ss=N;` (no `rs=`; `ss` is the signed radio RSSI). Both are
post-FEC and both are handed to `parseInput` unchanged, so replay (which never
re-applies FEC) is byte-equivalent to live processing for either receiver.
Observation, deliberately not changed: `parseInput` reads signal strength from
the third `;`-separated field, so the low-power string's `ss=` is not parsed
into the signal-strength statistic (pre-existing behavior, unchanged).

**Early detection (new).** File size cannot reveal this failure (gzip output
buffers and can sit at 0 bytes mid-session). `GET /getFISBRecorderStatus`
exposes the live counters, and `scripts/fisb-recorder-early-check.sh` compares
`UAT_messages_total` (all receivers) with `framesAccepted` within minutes. Run
it right after enabling recording; see "Early check" under Start. Requires a
build that includes the endpoint (the field-failure build `ea1c3e43` does not).

**Cache counters seen in that session (documented, no code change made).**
`droppedWrites` rose 1,098 -> 1,935 early and then stayed at 1,935 for the final
eight 5-minute samples; `pressureRejected` was 5 and `expiredOnArrival` was 4.
From `main/fisbcacherun.go` and `main/fisbcachereserve.go`: `droppedWrites`
counts cache-admission offers refused because 256 distinct product keys were
already queued or in flight (`fisbCachePendingCapacity`; the offer is dropped,
not retried, and a rebroadcast re-offers it) - it is **not** lost RF (the frame
was already counted and decoded) and **not** a failed disk write.
`pressureRejected` counts arrivals refused while storage-lifecycle pressure was
HIGH, CRITICAL or UNKNOWN. `expiredOnArrival` counts products whose own source
time was already past expiry when received. The plateau is consistent with a
one-time burst of many distinct products (e.g. NEXRAD tiles) outrunning the
single admission worker after boot; *why* the jump fell inside the recording
window was not established. These counters are in-memory, so they reset at
daemon restart. No cache behavior was changed; any sizing question is separate
follow-up work.

**Qualification level: `LAB_VALIDATED_FIELD_RETEST_REQUIRED`.** Source and bench
tests do not close the live-RF gate. Another bounded recording through the
actual external low-power UAT radio is still required.

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
2. `fisb-recording-tool` is built and present on the MacBook - built
   directly there (Ubuntu 24.04, `linux/amd64`, so an ordinary native
   build, no cross-toolchain or `GOOS`/`GOARCH` override needed on the
   laptop itself):
   ```
   go build -o ~/stratux-field-acceptance/kit/fisb-recording-tool ./cmd/fisb-recording-tool/
   ```
   or cross-compiled from ARS01 and transferred over:
   ```
   GOOS=linux GOARCH=amd64 go build -o fisb-recording-tool ./cmd/fisb-recording-tool/
   scp fisb-recording-tool ARS-Macbook:~/stratux-field-acceptance/kit/
   ssh ARS-Macbook 'chmod +x ~/stratux-field-acceptance/kit/fisb-recording-tool'
   ```
   Either way, confirm the SHA-256 matches on both ends before trusting
   it:
   ```
   sha256sum fisb-recording-tool                                    # on ARS01
   ssh ARS-Macbook 'sha256sum ~/stratux-field-acceptance/kit/fisb-recording-tool'
   ```
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
the session; allow up to ~2s for the start log line. **The daemon does not
log to the journal** (`debian/stratux.service` sets `StandardOutput=null`; the
daemon writes `/var/log/stratux.log`), so an earlier revision of this kit that
grepped `journalctl -u stratux` could never find it - that, not a missing
message, is why the 2026-10-01 session showed no `started` line. Use:
```
ssh pi@192.168.10.1 'sudo grep fisbRecorder /var/log/stratux.log | tail -5'
```
Expect a `field-recording session <YYYYMMDD-HHMMSS> started` line. Note
the session ID - it names the bundle directory you'll pull back later.
(Device-side confirmation of that file's contents on the failed session's
device is still pending; the path above is established from source.)

### Early check (do this before leaving the recorder unattended)

Within the first few minutes, from the MacBook:
```
scripts/fisb-recorder-early-check.sh 192.168.10.1 240 50
```
It polls `/getStatus` and `/getFISBRecorderStatus` over HTTP only and prints
one verdict (exit code in parentheses): `PASS_EARLY` (0, frames are being
admitted), **`RECORDER_PATH_FAILURE` (2, live UAT is arriving but
`framesAccepted` is 0 - stop, do not run the window)**, `UNSUPPORTED_BUILD`
(3, build lacks the endpoint), `INCONCLUSIVE_NO_RF` (4), `RECORDER_NOT_ACTIVE`
(5), `DEVICE_UNREACHABLE` (6). It is receiver-agnostic: it does not read any
RTL-SDR or assignment state. Caveat: `UAT_messages_total` also counts frames
from receivers the recorder does not yet cover (Ping/Pong); on those a failure
verdict is expected and correct.

**If this line does not appear within 10 seconds**, do not proceed into
the field window on the assumption recording is running - stop and
diagnose (check `FISBRecordingEnabled` really is `true`, check disk space
under `/var/lib/stratux-data`, check the service is actually running the
instrumented build per Prerequisite 1).

## Monitor (periodically during the field window)

```
ssh pi@192.168.10.1 'sudo -n ls -la /var/lib/stratux-data/fisb-recordings/<session-id>/'
```
The session directory is `root:root 0750` (the recorder runs as root) -
`sudo` is required; a plain `ls` as `pi` fails with "Permission denied"
(confirmed live, not assumed). Growing `gdl90.jsonl.gz`/`snapshots.jsonl.gz` show the
recorder process is alive, but **file size must not be used to judge
`frames.jsonl.gz`**: gzip output buffers, and on 2026-10-01 a zero-byte
`frames.jsonl.gz` for the whole session was a real defect (the recorder was
not receiving the low-power radio's frames), not a benign "no RF" outcome. The
authoritative live signal is `GET /getFISBRecorderStatus` (`framesAccepted`,
`droppedFrames`, ...), which the early-check script and a periodic
`curl http://192.168.10.1/getFISBRecorderStatus` both read. For free space and settings state, no `sudo`/SSH
is needed at all - `curl http://192.168.10.1/getStatus` (watch
`DiskBytesFree`) and `curl http://192.168.10.1/getSettings` (confirm
`FISBRecordingEnabled`) work directly from the MacBook's own shell.

## Stop (at the end of the field window, or at the first bounded-stop
condition)

```
ssh pi@192.168.10.1 '
  curl -s -X POST http://localhost/setSettings \
    -H "Content-Type: application/json" \
    -d "{\"FISBRecordingEnabled\": false}" \
  && sleep 2 \
  && sudo grep fisbRecorder /var/log/stratux.log | tail -5
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

The session directory is `root:root 0750` on the device (the recorder
runs as root) - a plain `scp -r` as `pi` fails with "Permission denied"
before it ever sees the files. Stream it through `sudo tar` instead,
over the same already-open SSH connection:

```
SESSION=<session-id-from-the-start-step>
mkdir -p ~/stratux-field-acceptance/evidence/fisb-recorder/$SESSION
ssh pi@192.168.10.1 'sudo -n tar -czf - -C /var/lib/stratux-data/fisb-recordings '"$SESSION" \
  > ~/stratux-field-acceptance/evidence/fisb-recorder/$SESSION/session.tar.gz
cd ~/stratux-field-acceptance/evidence/fisb-recorder/$SESSION
tar -xzf session.tar.gz && mv "$SESSION"/* . && rmdir "$SESSION" session.tar.gz.d 2>/dev/null

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
(Requires passwordless `sudo` for `pi` on the device, already the case
per PR #51's own kit rehearsal - if that ever changes, `sudo -n` fails
fast with a clear error instead of hanging on a password prompt.)

Then, on the MacBook (or ARS01 once copied further):
```
fisb-recording-tool validate ~/stratux-field-acceptance/evidence/fisb-recorder/$SESSION
```
Exit code 0 and `"classification": "valid"` is the clean case. `"partial"`
still exit-codes non-zero but is a usable bundle with caveats printed in
`warnings` - read them, note them in the results template, do not discard
the bundle. `"unusable"` means stop: do not claim this session as field
evidence; report exactly what `errors` says failed.

Finally, transfer the bundle from the MacBook to ARS01 and verify the
SHA-256 a **third** time there (device manifest -> MacBook -> ARS01, all
three matching is the actual acceptance bar, not just two):
```
# from ARS01:
scp -r ddavis@192.168.0.101:~/stratux-field-acceptance/evidence/fisb-recorder/$SESSION /path/on/ars01/
sha256sum -c <(python3 -c "
import json
m = json.load(open('/path/on/ars01/$SESSION/manifest.json'))
for f in m['files']:
    print(f['sha256'] + '  ' + f['name'])
")
fisb-recording-tool validate /path/on/ars01/$SESSION
```

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

This is the exact, tested acceptance checklist for the **next field
visit** - not the at-home rehearsal already completed (see the banner
above), which deliberately did not require any of the reception-specific
rows below. In addition to PR #51's own L1-L7/F1-F3/B-series gates, a
field trip's recording is acceptance-worthy only if **all** of the
following hold - missing any one means reporting exactly which gate is
missing, not calling the dataset complete:

- [ ] Recorder confirmed active (the `started` log line, or
      `/getSettings`'s `FISBRecordingEnabled:true`) **before** entering
      the reception area - not started after arriving.
- [ ] `scripts/fisb-recorder-early-check.sh` returned `PASS_EARLY` within the
      first minutes (not `RECORDER_PATH_FAILURE`), on the **production
      external low-power UAT radio** - not on a substituted RTL-SDR 978.
- [ ] At least one real tower derived from actually-captured uplink
      frames (a non-empty `frames.jsonl.gz`, confirmed via `validate`'s
      `countedFrames`, and a real `/getTowers` entry while recording).
- [ ] Sustained decoded 978 MHz input for the duration of the window -
      not just one frame at the start - with no unexplained recorder
      drops (`manifest.droppedFrames`/`droppedGdl90`/`droppedSnapshots`
      all 0, or any non-zero value explained by an identified bound, e.g.
      a brief queue-depth spike during a burst - never silently
      accepted).
- [ ] Useful FIS-B products actually received - text and, if actually
      received, NEXRAD - with any missing category reported honestly as
      a gate not met, never manufactured or worked around.
- [ ] Synchronized time-window snapshots: the Stratux weather/cache/tower
      state (from this session's own `snapshots.jsonl.gz`, or a live
      `/getFISBCacheInventory` + `/getTowers` query during the window)
      **and** the ForeFlight device/weather views (photographed, iPad
      clock visible) from roughly the same window - see PR #51's own
      F1-F3 kit for exactly what a ForeFlight observation must show.
- [ ] A clean stop (`stopReason: requested`), a valid manifest
      (`fisb-recording-tool validate` classification `valid`), and
      matching SHA-256 on **all three** machines: the device (via the
      manifest's own recorded hash), the MacBook (after copying), and
      ARS01 (after the MacBook-to-ARS01 transfer).
- [ ] A successful offline replay of the bundle's frames on ARS01 (the
      `replay` step completing without error, frame count matching the
      manifest).
- [ ] If reception was insufficient for the rows above, the **partial**
      bundle is preserved anyway (see "Recovery from an interrupted
      capture" above) and reported with its real `validate` classification
      and exactly which gates were not met - never silently extended or
      called complete instead.
