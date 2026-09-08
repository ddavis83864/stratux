# Configuration backup and restore

## Purpose and scope

Lets the owner download a portable backup of Stratux's **supported application
configuration** and restore it later - after a factory reset, a settings mistake, or when
setting up a second, identically-mounted receiver. This is **not**:

- SD-card imaging or filesystem cloning.
- Operating-system recovery (network, boot, or systemd configuration).
- Credential backup - Wi-Fi passphrases, SSH keys, tokens, and certificates are never
  read or written by this subsystem.
- A replacement for recordings or diagnostic bundles, which are explicitly excluded.

The pure schema/validation/diff/token logic lives in the `configbackup` package
(hardware/I/O-free, like `alerting`/`readiness`/`recording`/`calprofile`); the HTTP
glue, in-memory restore-operation state machine, and every read/write of
`globalSettings`/`AlertSettings`/`calprofile.Store` live in `main/configbackupapi.go`.

## Exported sections

| Section | What | Restore behavior |
|---|---|---|
| `configuration` | An explicit allowlist of `globalSettings` fields (radio enablement, display prefs, GPS/OGN device config, region) - see `configbackup.ConfigurationSection`. | Overwrites the allowlisted fields; everything else in `globalSettings` is untouched. |
| `configuration.privacySensitive` | Ownship-identifying fields: `ownshipModeS`, `ognAddr`, `ognReg`, `ognPilot`. **Excluded by default** - only present when the owner explicitly opts in (dashboard checkbox, or `?includePrivacySensitive=true`); see "Privacy-sensitive fields" below. | When included, always named distinctly in preview and applied transactionally. When omitted (the default), restore leaves the device's current values completely untouched - omission is never interpreted as "clear these." |
| `calibrationProfiles` | Every stored `calprofile.Profile` (name, mounting metadata, calibration vectors, validity, timestamps). | Additive/update only via `calprofile.Store.Save` - a profile absent from the backup is **never** deleted. |
| `activeCalibrationProfileId` | Which profile is active. | Applied via `calprofile.Store.SetActiveID`, which itself refuses a dangling reference; the active profile's calibration is also mirrored into `globalSettings` (the same `applyProfileToGlobalSettingsLocked` helper `/activateCalibrationProfile` already uses). |
| `alertSettings` | Every persisted operational-alerting preference (thresholds, audio toggles, volume, cooldowns) **except** mute state. | Overwrites those fields; `SchemaVersion` and `Muted`/`MutedIndefinitely`/`MuteUntilUnixSeconds` are always preserved from the *current* settings, never the backup. |
| `autoRecordSettings` | Every persisted Automatic Flight Recording setting (enabled, start/stop groundspeed and dwell thresholds, GPS-loss grace, restart cooldown, minimum recording duration) - see `docs/automatic-flight-recording.md`. | Overwrites those fields (`SchemaVersion` preserved from current); never changes any existing recording's own recorded origin (manual/automatic), only the going-forward configuration. |

## Excluded sections (never read or written by this subsystem)

- Wi-Fi/network/BLE/serial output configuration (`WiFi*`, `StaticIps`, `NetworkOutputs`,
  `SerialOutputs`, `BleOutputs`, `NoSleep`) - credential/network surface, out of scope.
- Debug/developer toggles (`DEBUG`, `ReplayLog`, `TraceLog`, `AHRSLog`,
  `PersistentLogging`, `ClearLogOnStart`, `DeveloperMode`).
- The legacy top-level `IMUMapping`/`SensorQuaternion`/`C`/`D` calibration fields -
  superseded by `calibrationProfiles`; restoring both would create two sources of truth.
- `PersistentDataUUID` - pins *this* device's storage; must never travel in a portable
  backup (restoring it on different hardware would corrupt storage identity checks).
- `WatchList` - free text that may reference other aircraft.
- Mute state (`AlertSettings.Muted*`) - see "Excluded: mute state" below.
- Manual Preflight acknowledgements - boot-session-scoped, never durable configuration.
- Active traffic-alert target state - runtime state, never persisted even by alerting
  itself.
- Recordings, recording metadata, and diagnostic bundles.

### Excluded: mute state

`AlertSettings.Muted`/`MutedIndefinitely`/`MuteUntilUnixSeconds` are technically
persisted across a restart (see `docs/alerting.md`), but restore treats them as
operational, time-bound state, not durable configuration: silently re-muting alerts
from a stale backup - possibly hours or days old - is a real safety footgun. Restoring
never touches them, in either direction.

## Sensitive-data policy

- No passwords, Wi-Fi credentials, SSH keys, tokens, or certificates - ever.
- No exact GPS coordinates or historical tracks - alerting/traffic/GPS state isn't part
  of this document at all.
- No client MAC addresses.
- Ownship-identifying fields (`ownshipModeS`, `ognAddr`, `ognReg`, `ognPilot`) are
  privacy-sensitive but not secret - see "Privacy-sensitive fields" below for the
  full opt-in design.

## Privacy-sensitive fields

Excluded from every export **by default**. The ordinary Download button (and a bare
`GET /downloadConfigurationBackup`) produces a sanitized document:
`configuration.privacySensitiveIncluded: false` and `configuration.privacySensitive`
holding only zero values. Including them requires an explicit, off-by-default opt-in:
the dashboard's unchecked "Include aircraft/owner identification fields" checkbox, or
`?includePrivacySensitive=true` on the download request - any other query value is
rejected (`400`), and omitting the parameter always means `false`.

The document explicitly records which case it is
(`configuration.privacySensitiveIncluded`), rather than leaving a reader to guess from
whether the fields happen to be empty - a device with no ownship identifier configured
and a sanitized export both produce empty strings, but only the flag says whether that
absence was *deliberate disclosure of "there is nothing here"* or *"this was never
captured."* `Validate` enforces the distinction is never lied about: a document
claiming `false` while actually carrying non-empty privacy fields is rejected outright
(`ErrPrivacySectionMismatch`), not merely warned about.

Restore behavior follows the flag precisely:

- **Omitted** (`privacySensitiveIncluded: false`, the default/sanitized case): the
  device's current ownship/owner-identifying settings are left completely untouched.
  Preview shows an explicit note ("were not included... existing values will be
  preserved") rather than silently saying nothing.
- **Included** (`privacySensitiveIncluded: true`, explicit opt-in): every affected field
  name is listed in preview, the values are validated and applied transactionally
  exactly like any other configuration field, and are included in rollback's snapshot
  like everything else.

Diagnostics and logs never include these values in either case - the diagnostics
summary and this subsystem's logging never touch backup content at all (see
"Diagnostics").

## Backup schema

Top-level fields (`configbackup.Document`):

```
schemaVersion               int
createdAtUTC                *time.Time  (omitted if trusted/GNSS time wasn't available)
sourceVersion, sourceCommit string
minimumCompatibleVersion    int
configuration                configbackup.ConfigurationSection
  (includes privacySensitiveIncluded bool + privacySensitive - see
  "Privacy-sensitive fields" below)
calibrationProfiles          []calprofile.Profile
activeCalibrationProfileId   string
alertSettings                configbackup.AlertSettingsSection
sectionChecksums              map[string]string  ("configuration"/"calibrationProfiles"/"alertSettings")
contentChecksum                string
```

Every checksum is the hex SHA-256 of the relevant value's canonical JSON encoding
(`encoding/json`'s struct-field order is declaration order, and its map-key order is
always sorted, so the same underlying state always produces the same bytes - calibration
profiles are additionally sorted by ID before checksumming, so profile-listing order
never affects the result).

### Checksum limitations

**Checksums detect accidental corruption and incomplete modification. They do not
authenticate the backup or protect against deliberate modification.** A checksum here
catches a truncated download, a flipped byte, or a half-written file. It is **not
authentication**: there is no signing key, no trusted-signer concept, and an attacker
who can modify the backup can recompute a matching checksum for their edit exactly as
this package does - a passing checksum proves the document is internally
self-consistent, never that it came from a trusted source or wasn't deliberately
altered. This subsystem implements no signing, encryption, or key management. **Restore
only a backup you trust.** Separately: anyone who can already reach
`/applyConfigurationBackup` on this device can change every setting it covers through
the existing, unauthenticated management API - this subsystem does not change that
trust boundary.

### Compatibility rules

`schemaVersion` must fall within `[MinimumCompatibleSchemaVersion, SchemaVersion]` for
the running build (both `2` today - schema 1, which exported privacy-sensitive fields
unconditionally, was superseded before this feature's first release and is
intentionally rejected, not migrated - see `configbackup/document.go`'s `SchemaVersion`
doc comment). A version outside that range is rejected outright by `Validate`, before
any content is trusted. Bumping either constant requires an explicit, documented
migration note in `configbackup/document.go`.

## Validation

`configbackup.Validate(doc, rawSize)` is pure - **no writes, ever** - and checks, in
order: exact upload size against the bound, schema-version compatibility, every
checksum (recomputed from the document's own content, before trusting anything else in
it), then bounds/content rules (profile count/IDs/duplicates, dangling active-profile
reference, numeric ranges, string lengths, NaN/Inf rejection). It is safe to call
repeatedly on the same input - it always returns the same result - and is called twice
in the real flow: once at `/validateConfigurationBackup`, and again, from scratch, at
the top of `/applyConfigurationBackup` (never trusting the earlier preview to still
describe reality).

## Preview

`configbackup.ComputePreview(doc, current)` diffs a validated document against a live
`CurrentState` snapshot and reports, per section, exactly what would change: added/
updated/unchanged calibration profiles, an active-profile change (if any), a generic
field-by-field diff for `configuration`/`alertSettings` (privacy-sensitive fields are
reported separately, never as an anonymous diff entry), whether the backup's schema is
compatible, and a documented **heuristic** `restartRequired` flag - true whenever a
radio/hardware-facing `configuration` field changes (this project's existing settings
handlers already require a restart for those to take effect), never for alert-settings
or calibration-profile changes, which are always applied live.

## Confirmation token

A successful validate issues a short-lived (5 minute), one-time confirmation token
bound to:

- The exact uploaded document's `contentChecksum`.
- A `configbackup.Fingerprint` of the exact live configuration state the preview was
  computed against (this subsystem's substitute for an explicit, globally-incremented
  "configuration generation" counter, which would require instrumenting every existing
  settings/profile-mutating handler in `main/`; a fingerprint change reliably detects
  *that* something changed without needing to enumerate what).
- The current daemon boot session ID (reused from `preflightSessionID` - a restart
  always invalidates every outstanding token).
- A monotonic (never wall-clock) expiry.

`/applyConfigurationBackup` re-validates the re-uploaded document from scratch, then
calls `configbackup.VerifyToken`, which fails the request if: the token was already
used, it expired, the daemon restarted since issuance, the uploaded content changed
since preview, or the current device configuration changed since preview (through this
API, the existing settings APIs, or a calibration-profile API). There is only ever one
outstanding token at a time - a new `/validateConfigurationBackup` call replaces it
outright, which is also how concurrent-restore prevention works at this layer (the
recording/OTA/in-progress-restore guards below are the apply-time layer).

## Apply behavior

`/applyConfigurationBackup` first refuses outright (`409`) if a recording is active, an
OTA update is in progress, or another restore is already applying. It then re-validates,
re-checks the token, confirms persistent storage is writable, and only then begins
transactional apply.

## Transactionality and rollback

`applyConfigBackupTransaction` (`main/configbackupapi.go`) snapshots the complete
pre-apply state (every current calibration profile, the active pointer, the allowlisted
`configuration` fields, and `AlertSettings`) before changing anything. Order of
operations: alert settings first (its own atomic temp-file/fsync/rename write, cheapest
to detect a failure on), then every calibration profile via `calprofile.Store.Save`
(validated again at that boundary), then the `configuration` allowlist and any
active-profile change together in memory, persisted with one final `saveSettings()`
call.

On any failure, rollback restores the active-profile pointer first (so a subsequent
delete of a newly-added profile never targets the currently-active one), then restores
or removes each touched profile from the pre-apply snapshot, restores `configuration`
and re-saves settings, and restores `AlertSettings`. Rollback is attempted regardless of
which step failed - reverting to already-correct values is always safe. If rollback
itself encounters an error, the response says so explicitly
(`rollbackFailed: true`) rather than claiming a clean recovery, and the failure is
logged for manual investigation - this subsystem never fabricates success after a
partial application.

## Calibration-profile safeguards

- Every profile is validated by `calprofile.ValidateProfile` again at the store
  boundary, not just by `configbackup.Validate`.
- `calprofile.Store.Save` itself enforces the profile-count cap, ID format, and
  duplicate-name rejection - restore uses the same supported API every other profile
  mutation uses, not a raw file write.
- A restore never deletes a profile merely because it is absent from the backup -
  additive/update only.
- The active pointer can never dangle: `SetActiveID` refuses a target that doesn't
  resolve to a stored profile, and this is re-checked by `Validate` before apply is even
  attempted.
- **Restoring a calibration profile only makes sense if the physical AHRS mounting
  matches the one it was captured for.** Applying a profile made for a different
  mounting will make AHRS pitch/roll incorrect. The dashboard states this explicitly;
  this subsystem has no way to verify a physical mounting itself.

## Recording/OTA concurrency restrictions

A restore is rejected (`409`) while a recording is active or an OTA update is in
progress, and starting either of those while a restore is genuinely applying is likewise
expected to be rejected by their own existing guards. This is conservative by design -
the alternative (proving every affected setting is immutable mid-recording) was judged
not worth the complexity for a feature this infrequently used.

## Interrupted restore / power-loss behavior

Every underlying write this subsystem performs is one this project's existing
persistence primitives already handle safely: `calprofile.Store.Save`/`SetActiveID` and
`saveAlertSettings` are atomic (temp file, fsync, rename); `saveSettings()` (the
`configuration` allowlist) uses the same non-atomic truncate-and-write every other
existing settings handler in this codebase already uses - a pre-existing characteristic
of `globalSettings` persistence, not something this feature changes or worsens. A
power-loss mid-restore can therefore leave `configuration` partially written (matching
the existing risk for *any* settings change in this project) while calibration profiles
and alert settings each land atomically in an all-or-nothing state. The in-memory
restore-operation state (confirmation tokens, `configBackupState`) never survives a
restart by design - every outstanding token is invalidated by the boot-session check.

## Dashboard

`web/plates/configbackup.html` / `web/plates/js/configbackup.js` (linked from the
sidebar as "Backup", and from the Settings page): a plain download link, a file picker,
a "Validate Backup" step that renders the full preview (section-by-section changes,
added/updated/unchanged profiles, privacy and restart warnings, blocking errors), an
explicit confirmation checkbox, and an "Apply Restore" button disabled until validation
passed *and* the checkbox is checked. All backup-derived text is rendered via Angular's
`{{ }}` interpolation (auto-escaped, never `ng-bind-html`) - a backup can never inject
arbitrary HTML into the page.

## API

```
GET  /downloadConfigurationBackup       - the current configuration as a Document.
                                           ?includePrivacySensitive=true opts into the
                                           ownship/owner-identifying fields (default/
                                           omitted = false; any other value is 400).
POST /validateConfigurationBackup       - validate + preview; no writes; issues a token
POST /applyConfigurationBackup          - {confirmationToken, backup}; transactional apply
GET  /getConfigurationRestoreStatus     - idle/validating/preview-ready/applying/
                                           verifying/rolling-back/complete/failed
```

See `docs/http-api.md` for the full request/response/status-code reference.

## Diagnostics

A bounded, sanitized summary (availability, schema version, last restore's
success/sections-applied/sections-rolled-back/restart-required/failure category) is
included in every generated diagnostics bundle (`DiagnosticBundle.ConfigBackupSummary`)
- never backup content, calibration values, or credentials.

## Recording integration

A recording's session-metadata snapshot (`recording.SessionSnapshot`, schema version 3)
carries three additive fields: this subsystem's schema version, an opaque configuration
fingerprint (the same checksum `configbackup.Fingerprint` computes - safe by
construction, since it's a SHA-256 of already-non-sensitive-by-allowlist fields), and
whether a restore completed during the current boot. Never the full backup document,
never a calibration value beyond what the existing `CalibrationProfile*` fields already
carry. One-second sample structure and CSV columns are unchanged.

## Device migration

- **Same-device restore** (the intended, tested case): re-establishing configuration
  after a factory reset, an accidental settings change, or a fresh OTA install onto the
  same hardware.
- **Different-device restore**: technically possible (the format doesn't check device
  identity), but calibration profiles only make physical sense if the new device's AHRS
  is mounted identically - see "Calibration-profile safeguards" above. GPS
  device/port fields (`GpsManualDevice`/`GpsManualChip`) may also not apply if the
  hardware differs. Proceed with the same caution as moving a calibration profile
  between airframes.

## Version upgrade/downgrade policy

A backup's `schemaVersion` must be within `[MinimumCompatibleSchemaVersion,
SchemaVersion]` for the currently-running build - see "Compatibility rules" above.
**Schema 2 is the only backup schema this build accepts; compatibility with schema 1 or
an older Stratux build is not implemented or implied.** There is no cross-version field
migration - a future schema bump must add an explicit migration path rather than
silently reinterpreting old field meanings.

## SD-card recovery limitations

This subsystem cannot recover from a lost or corrupted SD card, a failed OS install, or
any operating-system-level configuration loss (network, boot, systemd). It restores only
the supported application-configuration surface described above; use the project's
normal OTA/reinstall process for the OS itself, then restore application configuration
from a backup taken before the loss.

## Testing

- `configbackup` (pure): document/checksum determinism, schema/checksum/bounds
  validation (oversized, malformed, unsupported schema, duplicate/dangling profile IDs,
  out-of-range values), preview diffing (no-op, single change, added/updated/unchanged
  profiles, active-profile change, privacy warnings, the restart-required heuristic),
  and confirmation-token verification (expiry, reuse, boot-session/content/state
  mismatches).
- `main` (API-level, `httptest`): every endpoint's status-code matrix, a real
  no-op restore round-trip (byte-identical sections, active profile unchanged), a real
  benign-setting-change-and-restore round-trip, malformed/truncated/tampered input
  rejection, expired/reused/unknown tokens, rejection during an active recording or an
  in-progress OTA update, concurrent-apply rejection, concurrent-validation success, and
  an additive profile-restore that leaves the pre-existing profile intact.
- `go test -race` on this package set hits the same pre-existing, already-documented
  ARM64/QEMU `ThreadSanitizer: unsupported VMA range` sandbox limitation as every other
  package in this repository - not a defect in this feature.

## Deployment

Built into the same ARM64 `.deb` as every other feature (`make dpkg`), independently
verified (embedded commit, package SHA-256, packaged web assets) via host-based
extraction, and deployed exclusively through the existing OTA mechanism
(`/updateUpload`) - see `docs/ota.md`.

## Operational checklist

1. Download a backup before any experimentation, OTA-adjacent change, or factory reset.
2. Store it off-device (it contains no secrets, but it is still your only portable copy
   of your alert thresholds and calibration-profile identity).
3. To restore: select the file, review the preview in full (especially any
   privacy-sensitive-field or active-profile-change warning), check the confirmation
   box, then apply.
4. If a restart is flagged as required, restart the daemon (or reboot) afterward.
5. Re-check AHRS attitude after restoring a calibration profile onto a different
   physical installation than the one it was captured on.
