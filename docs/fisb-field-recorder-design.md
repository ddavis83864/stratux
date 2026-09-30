# FIS-B field recorder: design (Phase 1) and implementation (Phase 2)

> **Status: implemented and bench-tested; not yet field-validated, not yet
> installed on the grounded device.** Branch `feature/fisb-field-recorder`,
> stacked on PR #51's exact code-bearing candidate commit
> `cdb4d798fec721e0a6cd5cb1543d442ac4893eb4` (not the docs-only commits on top
> of it). This keeps PR #51's own tested artifact identity (package SHA-256
> `1b0a8b432d68cea82a33bcebd5885bd5ea13871b3b5da19b43337961626d8f19`) completely
> distinct from anything built from this branch - an instrumented build from
> here is a **different artifact**, never substituted for PR #51's own.

## What already exists in this fork (verified by reading the code, not assumed)

`AGENTS.md` (this repo's own root doc) says "There are no Go unit tests" and
lists `-replay -uatlog`/`-trace` flags - both true of the generic upstream
shape, but this fork has since grown extensive `*_test.go` suites (fisbcache,
epaper, ota, ...) and the trace facility described below is itself already
fork-specific (`main/trace.go`, `Copyright (c) 2023 Adrian Batzill`). Do not
trust `AGENTS.md` for either claim without checking - confirmed here by
reading `main/trace.go`, `main/sdr.go`, `main/network.go` directly.

**`main/trace.go`'s `TraceLog`** already does a real subset of what this task
wants: a `Start`/`Stop`/`Record(context, data)`/`Replay(...)` facility, gzip'd
CSV (`timestamp, context, data`), with a `godump978` context recorded at
exactly the right point - `main/sdr.go:615`,
`uatReader()`: `TraceLog.Record(CONTEXT_GODUMP978, []byte(uat)); handleUatMessage(uat)`
- immediately before the decoded UAT string is handed to the parser. This
**is** "every decoded UAT uplink frame presented to the parser," byte-for-byte,
in order. `TraceLog.Replay` re-injects it through `handleUatMessage` exactly
as during live capture.

**What `TraceLog` does *not* do, and why a new, narrower recorder is still
needed:**
- Writes to `/var/log/stratux/` - confirmed elsewhere in this project's own
  documentation and prior sessions' device evidence to be the overlay's RAM
  tmpfs, not `/var/lib/stratux-data` (`main/health.go`'s own
  `PersistentDataPath` constant) - the established persistent partition. A
  field-length UAT trace could fill the entire 250 MB overlay tmpfs, which
  backs the whole root filesystem, not just recording. **The new recorder
  writes under `/var/lib/stratux-data` instead.**
- Its own watchdog (`traceLoggerWatchdog`) checks free space on `/` (the
  overlay) even though it writes to `/var/log` - consistent with `/var/log`
  being on the same overlay, and consistent with why disk-bound behavior
  needs its own, correctly-scoped check here.
- Timestamps come from `stratuxClock.Time()` - this project's own
  monotonic-since-boot clock *already*, not wall clock (confirmed by reading
  `main/clock.go`'s own doc comment) - good for ordering, but `TraceLog`
  never separately records real wall-clock/GPS state alongside it, and its
  CSV format has no per-session manifest, hash, or counters.
- No tower/cache/GDL90-egress/periodic-snapshot capture at all - it is a
  generic multi-subsystem input trace, not a FIS-B-session bundle.
- Global toggle (`globalSettings.TraceLog`), always-on-for-everything-it-
  covers - not scoped to a bounded FIS-B recording session with its own
  start/stop and resource bounds.

**Decision: reuse the proven capture point, not the mechanism.** The new
recorder adds its own hook immediately next to `TraceLog.Record(CONTEXT_GODUMP978, ...)`
in `uatReader()` - same channel, same byte string, same position relative to
`handleUatMessage` - rather than depending on or modifying `TraceLog` itself
(which stays exactly as-is, unmodified, still available for its own existing
purpose). This is the least invasive point that preserves complete decoded
UAT uplink frames before weather parsing.

**Explicit limitation, stated once and referenced everywhere the bundle is
described:** `godump978.OutChan` carries *decoded* UAT frames (hex-encoded
payload, signal strength, length - the output of `dump978`'s own FEC-corrected
demodulation), not raw 978 MHz I/Q samples. **Replaying a recording proves
Stratux's parser/cache/GDL90 pipeline is reproducible; it cannot validate RF
demodulation, receiver sensitivity, or antenna/siting performance.** That
question needs the live device in the field, not a replay.

## Tower identification: derived, not recorded separately

`main/gen_gdl90.go`'s `parseInput()`, called from `handleUatMessage()`,
derives `ADSBTowerID` as `fmt.Sprintf("(%f,%f)", uatMsg.Lat, uatMsg.Lon)` from
`uatparse.New(buf).DecodeUplink()`'s own output - a pure function of the raw
frame bytes, with **no hardcoded or injected value anywhere in this path**.
The same call also drives FIS-B cache admission
(`fisbCacheCaptureFrame(f)`, `main/fisbcachecapture.go`) a few lines later, in
the same function.

**Consequence for the design**: replaying the exact recorded frames (bytes,
order) through the real `handleUatMessage`/`parseInput` path - which the
recorder does not need to special-case at all - naturally reproduces the
same tower IDs, product admissions, and cache state as the live session, for
free, using the actual production code rather than a parallel bespoke
re-implementation. The recorder's own job for towers/cache is therefore
**periodic snapshots of the live, in-process state** (what a field session
actually observed) for later comparison against what replay reproduces - not
a separate structured tower/cache event log.

## GDL90 egress: the correct capture point, and why a network sniff is wrong

`main/clientconnection.go`'s `networkConnection` gives each UDP client its
own **connected** `*net.UDPConn` (`Conn.RemoteAddr()` fixed to that one
client), not a shared broadcast socket - `GetConnectionKey()` returns e.g.
`"192.168.10.22:12345"`. The actual write happens in
`main/network.go`'s `connectionWriter(connection)`:
`connection.Writer().Write(msg)`.

**A UDP listener bound on the MacBook to port 4000 would not see this
traffic** - it is unicast, addressed to the iPad's own IP, and a bind-based
listener only receives packets addressed to *its own* address; seeing
someone else's unicast traffic needs promiscuous capture on a shared/mirrored
segment, which a typical AP (client isolation, switched Wi-Fi) does not
provide. Sniffing from the MacBook would at best capture a **subset that
happens to be broadcast**, silently missing every real per-client GDL90
delivery - exactly the invalid assumption this task's own instructions warn
against.

**Decision:** hook `connectionWriter` at the `connection.Writer().Write(msg)`
call itself - in-process, so it sees the exact bytes and the exact
`GetConnectionKey()` destination for every real send, regardless of network
topology. **Implemented to record every write from every connection type**
(UDP/TCP/serial/BLE), not filtered to a GDL90-capability bitmask: `sendMsg`
already filters by `conn.Capabilities() & msgType` at enqueue time (a
connection never receives a message type it can't handle), so recording
unconditionally here cannot under-capture, and a capability-mask filter added
at this hook risked silently dropping a legitimate record on a subtly wrong
bitmask - the wrong failure mode for a field-evidence recorder. A serial/BLE
connection carrying a non-GDL90 relay message type would be recorded too;
`ConnectionKey`'s own format (`"192.168.10.22:12345"` for UDP,
`"TCP:..."`, `"/dev/serialout0"`) is what a reader or the comparison tool
(`fisbrecorder.Compare`) uses to isolate EFB-facing UDP/TCP traffic.

## What the PR #51 field kit can and cannot substantiate from a recording

Reviewed `docs/pr51-field-acceptance-kit.md` and
`docs/pr51-field-results-template.md` (current PR #51 head, re-verified this
session - see Phase 1 state verification below):

| Kit gate | Recording can substantiate? |
|---|---|
| L1-L3 (978/FIS-B counters increasing) | Yes - periodic status snapshots |
| L4-L6 (cache admission/persistence/sample) | Yes - periodic cache snapshots + replay comparison |
| L7 (no-RF vs. defect distinction) | Yes - the recording itself proves whether frames arrived at all |
| Stratux web weather display | Partially - the underlying data is captured; the rendered page itself is not |
| **F1-F3 (ForeFlight traffic/weather/reconnect)** | **No** - these require direct observation on the iPad, unchanged by this work. A byte-identical GDL90 replay proves Stratux sent the right bytes; it does not prove ForeFlight rendered them (a different EFB-side parsing/rendering/caching layer entirely, outside this codebase). The kit's existing F1-F3 rows and their photo-evidence requirement are unchanged. |
| B-series (identity/health/rollback) | Unaffected by this work either way |

## Format version and bundle layout (summary; full spec in the recorder package's own doc comment)

`fisbrecorder` package, format version `1`. One directory per session under
`/var/lib/stratux-data/fisb-recordings/<session-id>/`:

- `frames.jsonl.gz` - one JSON object per decoded UAT frame: monotonic
  elapsed nanoseconds since session start, wall-clock capture time (labeled
  untrusted), raw frame string (hex, exactly as received from
  `godump978.OutChan`), sequence number.
- `gdl90.jsonl.gz` - one JSON object per outbound GDL90 write: monotonic
  elapsed ns, connection key (destination), byte length, message bytes
  (base64), sequence number.
- `snapshots.jsonl.gz` - periodic (default 10s) JSON snapshots: status,
  settings (FIS-B-relevant fields only), FIS-B cache status + inventory,
  towers, GPS/clock state, service health.
- `manifest.json` - format version, embedded build string, session start/stop
  (monotonic + wall clock), frame/GDL90/snapshot counters, dropped-record
  counts, per-file SHA-256 and byte size, and any known gaps (e.g. disk-full
  stop, interrupted session).

Bounded: max session duration, max bundle size (oldest-data-dropped-and-
counted, never silent), all writes buffered/batched off the hot decode path,
recording default **off**, enabled only via an explicit new setting
(`FISBRecordingEnabled`) or CLI flag - never a side effect of any existing
setting. On the persistent partition; never committed to git by default
(`.gitignore`d recordings directory pattern, and the field procedure copies
bundles to `evidence/`, not the repo).

## Phase 1 repository verification performed

- `origin/master` = `3b3aca52...` (unchanged from all prior sessions this
  engagement).
- PR #51 head (docs-only, re-verified) and PR #51's own code-bearing commit
  `cdb4d798...` (this branch's own base) both confirmed via `gh pr view 51`
  and `git log` before branching.
- PR #52 untouched, its own branch/PR unaffected by this work.
- Device (`192.168.10.1`) confirmed still on `2002ad4e...` via a live,
  read-only `getStatus` query.
- Main checkout (`/home/ddavis/git/stratux`) and all unrelated worktrees
  confirmed untouched, not read further than `AGENTS.md` and the files cited
  above.

## Phase 2: what was actually implemented

**Package `fisbrecorder`** (`fisbrecorder/*.go`, no dependency on `main`,
no cgo, builds/vets/tests on any platform):

- `format.go` - `FormatVersion = 1`, `FrameRecord`/`GDL90Record`/`Snapshot`/
  `Manifest`/`FileInfo`/`StopReason` types, all doc-commented with the
  design decisions above.
- `recorder.go` - `Recorder`, bounded non-blocking recording (buffered
  channels, `select`/`default`-drop with counters), `Start`/`Stop`/
  `RecordFrame`/`RecordGDL90`/`RecordSnapshot`/`IsActive`/`checkBounds`
  (MaxDuration/MaxBytes/MinFreeBytes). **Manifest counters distinguish
  attempted-but-dropped from accepted-and-written** (`frameSeq`/`gdl90Seq`/
  `snapshotSeq` assign a gap-free sequence number to every attempt so a
  hole in a written file's own `Seq` values always means "dropped";
  `framesAccepted`/`gdl90Accepted`/`snapshotsAccepted` count only what was
  actually handed to the writer, which is what `Manifest.FrameCount` etc.
  report - `FrameCount + DroppedFrames` always equals the number of
  `RecordFrame` calls, with no double-booking). This exact double-booking
  bug (`FrameCount` was initially wired to the raw attempt counter) was
  caught by `TestRecorder_NeverBlocksWhenQueueFull` during Phase 2's own
  test run and fixed before anything downstream depended on it.
- `recorder_disk.go` - free-space check via the existing
  `github.com/ricochet2200/go-disk-usage/du` dependency (already used by
  `main/trace.go`), scoped to the recording directory itself (see the
  `TraceLog` free-space-scoping mistake this avoids, above).
- `replay.go` - `ReplayFrames(dir, handler, opts)`: reads `frames.jsonl.gz`
  in order, paces by `ElapsedNanos` (`SpeedMultiplier`: 1.0 = original
  pacing, >1 = accelerated, <=0 = as-fast-as-possible), detects sequence
  gaps, supports a bounded `StopAfter`. Touches only `frames.jsonl.gz` -
  never `gdl90.jsonl.gz`/`snapshots.jsonl.gz` - and opens no network
  connection or hardware handle itself; `main` is solely responsible for
  what `FrameHandler` does with each frame (wiring it to the real
  `handleUatMessage` reproduces tower/cache state "for free," per the
  design decision above).
- `validate.go` - `Validate(dir) *Result`: `Classification` in
  {`valid`, `partial`, `unusable`}. Re-hashes every manifest-listed file
  (never trusts the manifest's own claim), re-counts every JSONL file's
  actual records and compares against the manifest's claimed counts,
  checks frame sequence gaps and time-order, and treats a non-`requested`
  stop reason, any dropped-record count, or zero captured frames as
  `partial` (usable, but flagged) rather than silently `valid`. A missing
  file, a hash/size mismatch, a corrupt/unparseable manifest, or an
  unrecognized format version is `unusable`. **A clean hash alone never
  reaches `valid`** - it still has to pass the count-reconciliation and
  gap/time-order checks.
- `compare.go` - `Compare(dirA, dirB, normalize) *CompareReport`: for two
  session bundles (typically a live recording and a bench replay of its
  own frames with recording turned back on), compares outbound GDL90
  records per-connection-key, index-aligned, byte-for-byte by default
  (`normalize` is the identity function unless the caller supplies a
  domain-specific masking function for a field known to legitimately
  differ between a live run and a replay - this package does not itself
  know GDL90's wire format and does not guess which bytes are safe to
  ignore), and compares snapshot labels by count and by-label presence
  (paired by relative position within each bundle, not by absolute or
  elapsed time, since a live session and a replay of it do not share a
  clock epoch - see `SnapshotComparison`'s own doc comment for why per-
  field payload diffing of a clock-dependent value like a cache entry's
  age is left to a caller that knows that label's specific JSON shape).
- `syntheticfixture.go` / `syntheticfixture_test.go` - **explicitly
  synthetic**, hand-constructed (bit-exact per `uatparse.go`'s own decode
  logic, self-verified against the real `uatparse` package on every test
  run) UAT uplink messages: two messages from one synthetic tower
  (44.0, -93.0) - one FIS-B generic-text product, one minimal NEXRAD RLE
  block - and one message from a second, distinct synthetic tower
  (38.5, -77.0). Every constant's doc comment states SYNTHETIC and lists
  the same prohibitions as this task's own instructions (never inject into
  the live receiver, an operational ForeFlight connection, or a field
  evidence bundle). `TestSyntheticFixture_TwoTowersAreDistinct` proves the
  fixture's own tower IDs are derived from frame bytes, not hardcoded, by
  running `uatparse` and comparing the resulting `(lat,lon)` identities.
  `TestSyntheticFixture_RecordReplayRoundTrip` proves record -> validate ->
  replay preserves frame bytes/order using only synthetic data.
- Test suite: 36 tests across `recorder_test.go`, `replay_test.go`,
  `validate_test.go`, `compare_test.go`, `syntheticfixture_test.go`,
  covering the bounded-resource stops, drop accounting, concurrent-caller
  safety, monotonic-vs-wall-clock timing, replay pacing/bounds/fidelity,
  every `Validate` classification path (including hash mismatch, count
  mismatch, sequence gap, out-of-order time, zero-frame session, wrong
  format version), and `Compare`'s byte-mismatch/normalize/only-on-one-
  side/missing-manifest behavior. All pass under `go test -race
  ./fisbrecorder/...` on the host (pure Go, no cgo, so the project's known
  QEMU/cgo `-race` limitation for `main/` does not apply here) and under
  `make dgotest` in the project's own arm64 docker toolchain.

**Wiring into `main`** (all on branch `feature/fisb-field-recorder`, on top
of PR #51's code-bearing commit):

- `main/gen_gdl90.go`: new `settings.FISBRecordingEnabled bool` field (no
  JSON tag, matching this struct's own no-tags convention), defaulted to
  `false` in `defaultSettings()`, and `initFISBRecorder()` /
  `go fisbRecorderWatchdog()` / `go fisbRecorderSnapshotLoop()` added
  alongside the other `init*()`/background-loop calls in `main()`.
- `main/fisbrecorderwiring.go` (new file): the process-wide `fisbRecorder`
  instance (`fisbrecorder.New(filepath.Join(PersistentDataPath,
  "fisb-recordings"), stratuxBuild, fisbrecorder.DefaultOptions())`),
  `initFISBRecorder` (starts at boot if the persisted setting is already
  `true`), `fisbRecorderWatchdog` (polls `globalSettings.FISBRecordingEnabled`
  once a second and starts/stops the session to match - **the exact same
  pattern `traceLoggerWatchdog` already uses for `globalSettings.TraceLog`**,
  chosen deliberately for consistency with this codebase's own existing
  idiom rather than a settings-handler side effect), and
  `fisbRecorderSnapshotLoop`/`recordFISBRecorderSnapshots` (every 10s,
  only while a session is active: `settings`, `status`, `fisbCacheStatus`
  (`fisbCacheStatusSnapshot()`), `fisbCacheInventory`
  (`fisbCacheInventorySnapshot()`, newly factored out of
  `handleGetFISBCacheInventoryRequest` in `main/fisbcacheapi.go` so both the
  HTTP handler and this loop share one implementation), `towers` (a locked
  copy of `ADSBTowers`), and `gpsClock` (`mySituation`'s GPS time/fix
  fields under `mySituation.muGPS`, plus `monotonicSeconds()` and
  `stratuxClock.Time()`).
- `main/sdr.go`'s `uatReader()`: `fisbRecorder.RecordFrame(uat)` added
  immediately after the existing `TraceLog.Record(CONTEXT_GODUMP978, ...)`
  call - alongside it, not instead of it, per the Phase 1 decision above.
- `main/network.go`'s `connectionWriter()`: `fisbRecorder.RecordGDL90(
  connection.GetConnectionKey(), msg)` added right after the existing
  `NetworkDataBytesSent` accounting, on the success path of the real send -
  see the amended GDL90-egress section above for why this records every
  connection type unconditionally rather than filtering by capability
  bitmask.
- `main/settingsvalidate.go` / `main/managementinterface.go`: `"FISBRecordingEnabled"`
  added to `settingsFieldTypes` (as `settingsFieldBool`) and a matching
  `case "FISBRecordingEnabled": globalSettings.FISBRecordingEnabled =
  val.(bool)` in `handleSettingsSetRequest` - sets the field only;
  `fisbRecorderWatchdog` is what actually starts/stops the session, the
  same division of responsibility `"TraceLog"`'s own case already has.
- `Makefile`'s `gotest` target: added `./fisbrecorder/...` to both the
  `go vet` and `go test -v` lines (alongside the existing `sdrassign`/
  `epaper`/`epaper_main` no-cgo bucket) so CI actually runs this package's
  tests - see the note below about a pre-existing, out-of-scope gap this
  surfaced.

**Build verification performed this session** (see the final report for
exact commands/output): `libdump978.so` and `stratuxrun` both built
successfully for arm64 inside this project's own docker toolchain
(`make libdump978.so` + `make stratuxrun`, run manually with the extra
`.git` bind mount a git worktree checkout needs - `make dgotest`/`make
ddpkg` alone hit the same worktree-`.git`-resolution and host/arm64
library-architecture issues documented in this session's own working
notes, not fixed here since they're pre-existing toolchain friction, not
this task's own defect). `make dgotest` (the project's own no-cgo test
bucket, now including `fisbrecorder`) passed cleanly.

**Known, out-of-scope gap surfaced (not fixed here):** the `gotest` Make
target - and therefore CI - runs no test at all for `main/`'s own large
`fisbcache*_test.go` suite (or any other `main/`-package test): `gotest`
only ever covered `sdrassign`/`epaper`/`epaper_main`, all cgo-free packages
that don't need `libdump978.so`. `main/`'s own tests need the cgo/arm64
toolchain this session used manually above, and neither `ci.yml` nor the
Makefile currently runs `go test ./main/...` anywhere. This is a
pre-existing condition of the repository, not introduced by this branch;
fixing it is a separate, larger change (it would need `ci.yml` and/or the
Makefile to build `libdump978.so` and run `go test` under the same
`LIBRARY_PATH`/`CGO_CFLAGS_ALLOW` this session used by hand) and is called
out here rather than folded silently into this PR's own scope.

## Phase 3: closing the bench-acceptance gaps (cache admission, weather GDL90)

**`storagePressure: UNKNOWN` root cause (traced live against a real
daemon, not by inspection alone).** `main/fisbcachestorage.go`'s
`fisbCacheStoragePressureProhibited()` treats `HIGH`/`CRITICAL`/`UNKNOWN`
pressure as prohibiting cache admission (`fisbCachePressureRejected`
increments, `AdmitRejectedUnsupported`-adjacent but distinct - see
`fisbCacheEnqueue` in `main/fisbcacherun.go`). That pressure value comes
from `storagelifecycle.Manager.Status()`, which runs every registered
namespace's own `Scan()` (`main/storagelifecycleapi.go`'s
`initStorageLifecycle`, registering `calibration-profiles`,
`diagnostics`, `recordings`, `exports`, and this feature's own
`fisb-weather-cache` namespace) through a debouncing `Monitor`
(`storagelifecycle/accounting.go`) that reports `Unknown` until it has
seen **3 consecutive agreeing raw samples** - a deliberate anti-flapping
design, correctly refused to bypass or relax per this task's own
instructions. Two separate, both legitimate, causes compounded on a
fresh bench host:

1. **Missing namespace directories are scan ERRORS, not empty-namespace
   results.** `storagelifecycle/inventory.go`'s `scanNamespace` calls
   `Lstat` on each namespace's own root directory first; a directory that
   has never been created (no diagnostics bundle generated, no recording
   ever made/exported) makes that scan return an error, which forces
   `raw = PressureUnknown` for that ENTIRE cycle regardless of the other
   namespaces - not per-namespace pressure. This is not bench-specific:
   any freshly provisioned device with no GPS/diagnostics/recording
   history yet would hit the identical 3 scan errors.
2. Each `Status()` call also samples the `Monitor` (`Observe`), so 3
   *rapid HTTP polls* against an already-error-free scan resolve pressure
   in seconds, not necessarily 3 full 60s scan ticks - useful for bench
   iteration, not a shortcut around the gate itself.

**Fix applied: exercise the SAME namespaces' real, normal, supported
creation paths** - `/generateDiagnostics`, `/startRecording` +
`/stopRecording` + `/exportRecording` - rather than hand-creating empty
directories. Once real files existed under all five namespace roots, the
next scan cycle reported `scanErrorCount: 0` and pressure resolved to a
real value (`ELEVATED` on this shared, multi-tenant dev host - itself
correct and *not* prohibited: only `HIGH`/`CRITICAL`/`UNKNOWN` block
admission, per `fisbCacheStoragePressureProhibited`'s own `switch`).
With pressure no longer prohibited, injecting the synthetic fixture
admitted all three products (`nexrad_tile`: 1, `text`: 2) - see the final
report for the full evidence (status/inventory/payload API results,
persistence-to-disk confirmation, and a record→replay content
comparison).

**Cross-reboot cache recovery could not be demonstrated in this bench
environment - reported as the precise, honest blocker, not bypassed.**
`main/fisbcacherun.go`'s `fisbCacheStartupRecovery` spin-waits for
`fisbCacheTrustedTimeState()` (GNSS- or network-synced time) for up to
`fisbCacheStartupRecoveryTimeout` (30s) before it will even read the
persisted cache directory back; without it, recovery is skipped every
boot, by design (an untrusted clock cannot correctly judge whether a
persisted entry is still fresh). This bench host's own OS clock is
NTP-synchronized, but `readiness.TimeTrust.ObserveNetwork` - the method
that would let NTP satisfy this gate - is never called anywhere in
`main/`; only a real GNSS fix does today. Live admission and on-disk
persistence (files under `fisb-weather-cache/`) were both directly
confirmed during a live session; recovering them across a restart was
not, and remains open pending either a real GPS fix in the field or a
future, separately-authorized decision to wire NTP into this gate.

**Weather-bearing GDL90 comparison: a raw per-connection comparison
across two SEPARATE daemon process launches is confounded by this bench
host's own dynamic client discovery**, not a defect in
`CompareWeatherGDL90` itself. `main/gen_gdl90.go`'s own
`defaultSettings()` seeds exactly three, unbound (`Ip:""`, "any client")
`NetworkOutputs` entries (ports 4000/2000/49002) - the ~30 distinct
`a.b.c.d:4000` connection keys observed in a live bench session are
genuinely discovered at runtime from whatever hosts are reachable on
ARS01's own shared, multi-tenant LAN/docker-bridge network at that
moment, which differs between two separate process launches minutes
apart. `CompareWeatherGDL90` itself is proven correct (13 tests, plus a
same-bundle self-comparison against a real daemon-produced bundle
matching every connection perfectly). Comparing the underlying weather
PAYLOAD SET directly (ignoring which specific dynamic IP received it)
showed the one payload that did reach an already-discovered client
within the replay's short (~11s) observation window was byte-for-byte
identical to the original; the other two were correctly queued
(`sendMsg`'s own 15-minute durability window for `MSGTYPE_UPLINK`,
matching real weather-message semantics) but the replay process exited
before a client eligible to receive them was discovered - a short-
observation-window artifact of this bench environment, not a pipeline
defect. See the final report for the exact counts and the independent,
connection-agnostic cache-admission-level comparison that confirmed all
three products' content byte-for-byte regardless of this confound.
