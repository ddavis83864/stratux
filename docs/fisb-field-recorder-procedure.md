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
> sequence (frame count honestly zero - no 978 MHz reception at that
> location/time, GDL90/snapshot traffic captured normally); the bundle
> copied off the device, hash-verified on both the MacBook and the
> device; and `validate` run on the copy. That real session is *how* a
> genuine record-accounting race in `Stop()` was found and fixed (see
> `fisbrecorder/recorder.go`'s own commit history) - this procedure's own
> exercise of the real workflow is what caught it, not a synthetic test.
> A second, real rehearsal after the fix confirmed a clean `valid`
> result. This is still **not** field acceptance: no real 978 MHz tower
> or FIS-B product was received in this rehearsal, and no ForeFlight
> observation was made - see the final report's explicit separation of
> synthetic bench proof, grounded-device rehearsal, and real field
> RF/ForeFlight acceptance.

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
ssh pi@192.168.10.1 'sudo -n ls -la /var/lib/stratux-data/fisb-recordings/<session-id>/'
```
The session directory is `root:root 0750` (the recorder runs as root) -
`sudo` is required; a plain `ls` as `pi` fails with "Permission denied"
(confirmed live, not assumed). Growing `frames.jsonl.gz`/`gdl90.jsonl.gz`/
`snapshots.jsonl.gz` file sizes across successive checks is the simplest
live evidence that capture is progressing - a real session showed
`gdl90.jsonl.gz` growing steadily (real heartbeat/traffic output) even
with `frames.jsonl.gz` staying at 0 bytes the whole time (no 978 MHz
reception that session - an honest, valid outcome, not a fault). There is
no separate live counter API yet - this direct file-size check is
deliberately the same low-tech method the design favors over adding a new
HTTP endpoint mid-trip. For free space and settings state, no `sudo`/SSH
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
