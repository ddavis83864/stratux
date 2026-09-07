# Recording and session-level Preflight metadata

This document covers the on-demand recording subsystem (`main/startRecording`
and friends, backed by the `recording` package) and, in particular, the
durable, versioned **session metadata** that is captured once when a
recording starts and persisted alongside its raw samples.

Automatic flight recording remains disabled. Nothing described here starts a
recording unless a client explicitly calls `POST /startRecording`.

## Why session metadata exists

Every recording already carries ~1Hz [`recording.Sample`](../recording/sample.go)
records (position, attitude, barometer, message rates, ...). Those samples
answer "what was happening during the flight." They deliberately do **not**
answer a different, session-scoped question: "was the aircraft/equipment
actually preflight-ready when this recording began, and under which
calibration profile?" Repeating that answer into every sample would only
duplicate an unchanging value ~3,600 times an hour for nothing.

Session metadata answers that second question exactly once per recording, as
a small, versioned, sanitized side-record.

## Design note (Phase 2)

This section records the design decisions made before implementation, per
this mission's Phase 2 requirement.

**Schema and types.** `recording.SessionMetadata` (new, in the existing pure
`recording` package) has two parts:

- `Snapshot` (`recording.SessionSnapshot`) - captured exactly once, at
  `/startRecording`, and never modified again for that recording. Reuses
  existing, already-sanitized, already-tested domain types rather than
  reinventing them: `[]preflight.CheckResult` for the automated/manual
  check line items (the same type `GET /getPreflightReport` already
  returns - no GPS coordinates, MAC addresses, or credentials, by that
  type's own existing design and test coverage), plus a handful of scalar
  fields (Stratux version/commit, calibration-profile identity, booleans
  for GPS-fix/trusted-time availability at start).
- `Finalization` (`recording.SessionFinalization`) - the few fields that
  legitimately change after the recording starts (stop time, duration,
  sample count, completion/interruption state). Finalization never touches
  `Snapshot`.

`recording.MetadataSchemaVersion` (currently `1`) is bumped whenever either
struct gains, removes, or changes the meaning of a field a consumer should
notice - mirroring `preflight.SchemaVersion`'s existing convention.

Reusing `preflight.CheckResult` means the `recording` package now imports
`preflight`. This does not create an import cycle: `preflight` imports only
`readiness` and `sdrassign`, neither of which (transitively) imports
`recording`.

**File naming and location.** One fixed-name sidecar file per recording
directory: `<recordingsDir>/<recording-id>/metadata.json` - not a
timestamped name like the sample files, since there is exactly one
metadata record per recording and a fixed name makes "does this recording
have metadata" a single `os.Stat`, not a directory scan. This lives on the
same persistent partition as the recording's own samples (never the
temporary root overlay), so it is automatically compatible with the
protected overlay and survives exactly as long as the recording itself.

**Atomic-write strategy.** Mirrors the pattern already established by
`calprofile.Store`'s `atomicWriteJSON` and `readiness.WriteDiagnosticBundle`:
marshal to JSON, write to `metadata.json.tmp` in the same directory,
`fsync` the temp file, close it, then `os.Rename` over the final name.
`os.Rename` within one filesystem is atomic, so a reader never observes a
partially-written file, and a crash between the temp-file write and the
rename leaves, at worst, a harmless orphaned `.tmp` file next to either no
`metadata.json` or the previous valid one - never a corrupt file at the
real name. No directory-level `fsync` is added: no existing persistence
code in this project does one (not even `calprofile.Store`, which the
mission's own recording-metadata design was told to treat as a durability
reference point), so adding it here would be new, unprecedented complexity
this mission's "smallest design that satisfies the requirements" guidance
argues against.

**Finalization strategy.** `stopActiveRecording` reads the existing
`metadata.json` (if any), replaces only its `Finalization` field, and
rewrites atomically via the same temp-file-and-rename path. If the original
`Snapshot` cannot be read back intact, finalization refuses to fabricate
one - see "Interrupted-recording behavior" below.

**Locking strategy.** All metadata file I/O and all `preflight.Report`
construction happen *outside* `recMu`, exactly like the existing
`recMu` self-deadlock fix already established for the initial preflight
snapshot (see `main/recordingapi.go`'s `handleStartRecordingRequest`
comment). Metadata persistence never blocks the 1Hz sampler goroutine: it
is written once at start and once at stop, never per-sample.

**API contract.** `GET /getRecordingMetadata?id=<id>` - reuses
`validRecordingDir` (the same directory-listing-based path-safety check
`/downloadRecording` and `/exportRecording` already use) so no new
path-traversal surface is introduced. `GET /downloadRecordingMetadata?id=<id>`
streams the same JSON as a named-file download. Both distinguish "no
metadata (legacy recording)" from "metadata present" from "metadata present
but unreadable/corrupt" with distinct, honest responses - never silently
treating corruption as absence, and never fabricating a value.

**Legacy behavior.** A recording directory with no `metadata.json` is not
an error: it is a legacy recording (created before this feature, or by a
build that failed to persist metadata) and is reported as such, explicitly.
Existing recordings are never rewritten to retroactively add a
(fabricated) snapshot.

**Interrupted-recording behavior.** If a daemon restarts (or the device
reboots) while a recording is active, the in-memory session, its stop
channel, and its sampler goroutine are gone. On the *next* `/startRecording`
or `/getRecordings`, nothing in this design tries to detect or "fix" the
old session; its `metadata.json` already on disk (written at start) still
has `Finalization.Complete == false` because the normal stop path never
ran. That absence of a `true` completion flag *is* the honest signal
that the recording was interrupted, not cleanly stopped - no special
"crash recovery" pass is needed to produce it. `handleListRecordingsRequest`
and the metadata endpoints surface `finalization.complete == false` plainly
rather than inferring or fabricating a stop time.

**Corruption handling.** A truncated or invalid `metadata.json` is
detected by a normal `json.Unmarshal` failure and reported through a
distinct `corrupt` status - it is never presented as "no metadata" (which
would hide a real, if partial, capture) nor as valid data.

**Failure isolation.** A `metadata.json` write failure at recording start
is logged and surfaces as a non-blocking field on the recording's status
response; the recording itself still proceeds (matching this project's
existing rule that a profile/preflight subsystem problem must never block
or interrupt a recording - see `populateSessionCalibrationProfile`'s
identical stance). A write failure at stop is likewise logged and
non-fatal to the stop operation. Neither failure mode touches ADS-B/GDL90/
GPS/AHRS/barometer/fan-control, which share none of `recMu` or the
recording directory.

**Sensitive-field exclusions.** `SessionSnapshot` carries no GPS
coordinates, no client MAC addresses, no credentials, no raw settings, and
no unbounded log text - it is built entirely from already-sanitized
`preflight.CheckResult`/`preflight.ProfileSummary`-shaped data plus a
handful of scalar identifiers, the same sanitization guarantee
`preflight.Report` itself already carries and is already tested for.

**Dashboard presentation.** Extends the existing Recording panel on the
Readiness page (`web/plates/readiness.html` / `js/readiness.js`) rather
than introducing a new page or framework: a small always-visible summary
per recording (Preflight state at start, profile name, complete/legacy/
interrupted) plus a "Details" toggle that expands the full sanitized
snapshot inline, explicitly labeled "at recording start" so it is never
mistaken for the live Preflight state.

## Schema

```json
{
  "schemaVersion": 1,
  "recordingId": "rec-20260101T120000Z",
  "snapshot": {
    "capturedAtUtc": "2026-01-01T12:00:00Z",
    "capturedAtMonoSeconds": 512.34,
    "stratuxVersion": "2.0-pre5",
    "stratuxCommit": "<commit>",
    "preflightBootSessionId": "preflight-...",
    "preflightGeneratedAt": "2026-01-01T12:00:00Z",
    "preflightGeneratedAtMonoSeconds": 512.30,
    "preflightOverallState": "CAUTION",
    "preflightRequiredActionCount": 0,
    "preflightCautionCount": 6,
    "preflightAutomated": [ /* []preflight.CheckResult, same shape as GET /getPreflightReport */ ],
    "preflightManual":    [ /* []preflight.CheckResult */ ],
    "trustedTimeAvailable": true,
    "gpsFixAvailable": false,
    "calibrationProfileId": "profile-...",
    "calibrationProfileName": "Current Installation",
    "calibrationProfileKind": "migrated",
    "calibrationValid": true,
    "calibrationProfileAvailable": true
  },
  "finalization": {
    "complete": true,
    "interrupted": false,
    "stoppedAtUtc": "2026-01-01T12:05:00Z",
    "durationSeconds": 300,
    "sampleCount": 300
  }
}
```

`snapshot` is immutable once written. `finalization` is the only part ever
rewritten, and only once, when the recording stops normally.

## Backward compatibility

- Existing recordings created before this feature have no `metadata.json`
  and remain fully listable/downloadable/CSV-exportable; they are labeled
  `metadataAvailable: false` (legacy) wherever metadata availability is
  reported.
- The pre-existing zero-byte recording (`rec-20260830T074855Z`, an empty
  `.jsonl` left over from an earlier, already-fixed `recMu` deadlock
  reproduction) is exactly this case: no metadata, an empty sample file.
  It is neither deleted nor rewritten by this change.
- No existing JSONL sample field changes shape or meaning.
- No existing CSV column is added, renamed, reordered, or removed. New
  session-level information is available only through the separate
  metadata endpoints/download, never repeated into CSV rows.
- Existing API responses (`/getRecordingStatus`, `/getRecordings`,
  `/exportRecording`, `/downloadRecording`, `/downloadExport`) gain only
  new, additive, `omitempty`-style fields; a client that ignores unknown
  JSON fields continues to work unmodified.

## API

See [http-api.md](http-api.md#recording) for the full endpoint reference.
New endpoints:

- `GET /getRecordingMetadata?id=<id>` - `200` with the metadata for a
  recording that has one; a distinct, documented `available:false`
  response for a legacy recording; `404` for an unknown id; `400` for a
  malformed id.
- `GET /downloadRecordingMetadata?id=<id>` - downloads the same JSON as a
  named file (`<id>.metadata.json`), `application/json`,
  `Content-Disposition: attachment`.

## Diagnostics

`GET /generateDiagnostics` bundles now include a bounded
`RecordingMetadataSummary` (recording count, metadata-backed count, legacy
count, incomplete count, corrupt-metadata count, and the most recent
recording's id/schema/start-state availability) - never the full contents
of every recording's metadata.

## Rollback

Purely additive: no existing recording, sample file, or CSV export is
rewritten or reinterpreted. Rolling back this change leaves every
`metadata.json` file in place, unread by the prior build, with no data
loss - the prior build simply never looks for it, exactly as it already
behaves today for recordings that predate this feature.

## Cross-references

- [preflight-readiness.md](preflight-readiness.md) - the Preflight report
  and manual-check model this metadata snapshots.
- [aircraft-calibration-profiles.md](aircraft-calibration-profiles.md) -
  the calibration-profile identity captured in the snapshot.
- [readiness-and-time-trust.md](readiness-and-time-trust.md) - the
  trusted-time/GPS-fix signals reflected in `trustedTimeAvailable` /
  `gpsFixAvailable`.
- [http-api.md](http-api.md) - full endpoint reference, including
  diagnostics and calibration-profile APIs this feature reuses conventions
  from.
- [alerting.md](alerting.md) - the operational-alerting subsystem whose
  session-level configuration/counts snapshot and bounded recent-event tail
  are captured into `SessionSnapshot`/`SessionFinalization` (schema
  version 2) alongside the Preflight snapshot this document defines.
