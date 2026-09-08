# Storage Lifecycle Foundation

> **Status: foundation only, observational.** This document describes the `storagelifecycle`
> package and its integration into the daemon. **Automatic recording is not enabled.
> FIS-B weather caching is not enabled. Automatic eviction/enforcement is not enabled.**
> This foundation does not automatically delete completed recordings, calibration
> profiles, configuration state, backups, or unknown files. See "Status of this release"
> below.

## Why this exists

Two upcoming features - automatic flight recording and a rolling FIS-B weather cache -
both need the same underlying capability: know what is on the persistent partition, know
how much of it belongs to whom, decide (without guessing) what could safely be reclaimed
under pressure, and write new files in a way that survives an interrupted write. Building
that once, as a shared, pure, heavily-tested package, is the entire scope of this change.

## Storage ownership matrix

Read-only inspection of the live device (`/var/lib/stratux-data`) found:

| Entry | Owning subsystem | Shape | This foundation's namespace |
|---|---|---|---|
| `calibration-profiles/` | `calprofile` (via `main/calprofilesapi.go`) | flat `.json` files (`active.json` pointer + `profile-*.json`) | `calibration-profiles` (`CriticalityProtected`) |
| `diagnostics/` | `readiness`/`main/diagnosticsapi.go` | flat `.json` files, already bounded (`diagnosticsMaxRetain = 10`) | `diagnostics` (`CriticalityBounded`) |
| `recordings/` | `main/recordingapi.go` | one directory per recording, each holding a sample `.jsonl` file and an optional `metadata.json` sidecar | `recordings` (`CriticalityImportant`) |
| `exports/` | `main/recordingapi.go` | flat export files (`.csv`/`.gpx`/`.kml`), one per exported recording | `exports` (`CriticalityImportant` - see below) |
| `alert-settings.json` | `main/alertsettings.go` | a single bare file at the partition root | **not inventoried** - see limitations |
| `power-session.json` | `main/powerapi.go` | a single bare file at the partition root | **not inventoried** - see limitations |
| `updates/` (`backup/`, `staged/`) | `ota` (`main/ota.go`) | nested subdirectories, its own existing bounded retention (`otaMaxRetainedPackages = 3`) | **not inventoried** - see limitations |
| `health/` | none found in the current codebase | empty directory | **not inventoried** - no owning subsystem found, not registered |
| `lost+found/` | ext4 itself | reserved, root-only (`700`) | not application data - never inspected |

**Precision note**: "not inventoried" here is a stronger statement than "reported as
unmanaged." `Scanner.Scan` only ever calls `FS.ReadDir` on a *registered namespace's own
`Root`* (`Registry.Namespaces()`) - it never lists `/var/lib/stratux-data` itself. None of
`alert-settings.json`, `power-session.json`, `updates/`, or `health/` lives inside any of
the four registered namespace roots (`calibration-profiles/`, `diagnostics/`,
`recordings/`, `exports/`), so none of them is ever listed, classified, or counted by any
scan - they do not appear as `StatusUnmanaged` entries either, because `StatusUnmanaged`
only applies to an entry the scanner actually observed inside a namespace it scanned. They
are simply outside this feature's field of view entirely, and this feature performs no
filesystem operation of any kind against them.

### Why `exports/` is `CriticalityImportant`, not `CriticalityCache`

An export is technically regenerable from its source recording, which would make
`CriticalityCache` (this foundation's only evictable criticality) a defensible choice. This
release deliberately does **not** make that call: a user may already be relying on a
specific downloaded export, and there is no existing product policy or explicit mission
authorization to treat existing export files as disposable. A future, separately reviewed
policy could reclassify this namespace.

### Known scope limitation: three entries are not covered yet

`alert-settings.json` and `power-session.json` are single bare files directly at the
partition root, not directories of like-kind items - this package's namespace model (one
namespace = one directory, `ItemKindFile` or `ItemKindDirectory` items one level deep) does
not cover a lone file at a shared root that also holds other namespaces' directories (a
namespace's `Root` cannot overlap another namespace's `Root` - see `Registry.Register`).
Both files are already correctly, atomically managed by their own owning subsystems
(`alertsettings.go`, `power`'s `WriteSessionMarkerAtomic`) - this is a scope limitation of
the shared inventory, not a gap in how those two files are actually written.

`updates/` has its own nested `backup/`/`staged/` subdirectories (not flat items directly
under `updates/`) and its own existing, tested bounded retention (`otaMaxRetainedPackages`)
- registering it here would either misrepresent its structure or duplicate a retention
mechanism this project already trusts. Left unregistered, by design.

## Mandatory policy principles

This package is built around these non-negotiable rules, all enforced in code (not just
documentation) and covered by tests:

1. Unknown files/entries are never deleted, and never even considered eligible.
2. Unknown/unregistered directories are never recursively traversed.
3. Symlinks are never followed for any lifecycle purpose (scanning, writing, or recovery).
4. Namespace roots never cross a mount boundary implicitly - each is a single, static,
   application-configured absolute path.
5. Active files (see below) can never become eviction candidates.
6. Calibration profiles and configuration state (`CriticalityProtected`) are never eviction
   candidates.
7. Completed recordings (`CriticalityImportant`) are never eviction candidates by this
   foundation's own default policy.
8. Existing diagnostic retention (`CriticalityBounded`) is observed and reported, never
   duplicated or re-implemented by this package's planner - see `Policy.Evictable`.
9. Only `CriticalityCache` namespaces (today: none registered on the live device - the
   future FIS-B cache is the only namespace this criticality is meant for) are ever
   eviction-eligible.
10. Namespace roots are static application configuration - never derived from an HTTP
    request.
11. A `Plan` never evicts an item solely because its wall-clock timestamp looks old; a
    namespace's `MinRetention` gate is conservative when the wall clock is not currently
    trusted (see "Retention planning" below).
12. Planning (`Plan`) and execution (`ExecuteRecovery`) are always separate steps - a plan
    never mutates anything by itself.

## Namespace / path safety

Every namespace is a `Namespace{ID, Root, Criticality, ItemKind, AllowedExtensions,
MinRetention}` registered once, at startup, into a `Registry` - never from HTTP input.
`Registry.Register` rejects a relative root, a non-`filepath.Clean` root, and any root that
equals, contains, or is contained by an already-registered root.

Every item name within a namespace passes through `SafeJoin(root, name)`, which rejects:
an empty name, an absolute name, a name containing a path separator, `.`/`..`, and (as a
final defense-in-depth check) any result that would not actually live under `root`. Item
names are always single path segments - a namespace's items are never nested more than one
level below `Root`.

## Inventory architecture

`Scanner.Scan()` walks every registered namespace's `Root` exactly one level deep, through
an injected `FS` interface (see `fs.go`) - never through the real `os` package directly, so
every scanning decision is testable against a fake filesystem with precise fault injection.
For each entry, the scanner:

- `Lstat`s it (never resolves a symlink);
- classifies it as `StatusManaged` (matches the namespace's expected shape), `StatusActive`
  (matches the shape AND the injected `ActiveChecker` reports it in use), or
  `StatusUnmanaged` (present but not matching - wrong kind, wrong extension, a symlink, or
  an unsupported file type such as a FIFO/socket/device file);
- for a directory item (`ItemKindDirectory`, e.g. one recording), sums only its *immediate*
  regular-file children's sizes - never an arbitrary recursive walk, and never reads a
  large file's content, only its size metadata.

A namespace-level error (a missing root, a permission error, a symlinked root) is recorded
per-namespace in `Inventory.Errors` and never aborts the rest of the scan - matching this
project's established failure-isolation convention (a storage problem must never disrupt
ADS-B/GPS/GDL90 processing). Item order within a namespace is always lexical by name,
regardless of the filesystem's own readdir order.

## Filesystem accounting

This package does **not** recompute the persistent partition's own free/used/percentage
figures. It reuses `readiness.StorageHealth.UtilizationPercent` - the same root-daemon
`Bfree`-based model documented in `readiness/storage.go` (this project's own daemon runs as
root, so it can use ext4's reserved-blocks percentage the same as any other root-owned
write - `Bfree`, not the unprivileged `Bavail`, is the correct basis; see that file's own
extensive doc comment and the measured evidence behind it). `storagelifecycle.Manager`'s
`FilesystemPressureFunc` is wired, in `main/storagelifecycleapi.go`, straight to
`globalHealth.Storage.UtilizationPercent` - never a second, independent `statfs` call.

Namespace-level accounting is a **separate, new** dimension: each namespace's own logical
byte total (the sum of its managed+active items' sizes) against an optional
per-namespace `Quota`. A namespace's logical usage will not generally equal its actual disk
block consumption (filesystem block rounding, sparse files, and directory-entry overhead
are not accounted for) - this package reports logical size because that is what a
retention decision actually needs ("how much would removing this item free"), not because
it claims to match `du`.

## Pressure states and hysteresis

`PressureState` is `NORMAL` / `ELEVATED` / `HIGH` / `CRITICAL` / `UNKNOWN`.
`FilesystemPressure` classifies the whole-filesystem percentage using **readiness's own
already-validated thresholds** (`WarnPercent=80`, `CriticalPercent=90`,
`RecordingProhibitedPercent=95` - see `readiness.DefaultPersistentStorageThresholds`),
never new, invented numbers. `NamespaceQuotaPressure` classifies one namespace's own usage
against its own `Quota` using parallel bands (75/90/100% of `Quota.MaxBytes`) - an
unconfigured quota (`MaxBytes <= 0`) always reports `NORMAL` ("no limit," not "already
exceeded").

`Manager.Status()` reports the **worse** of the filesystem reading and every registered
namespace's quota reading (`UNKNOWN` ranks worse than `NORMAL` but better than a confirmed
`ELEVATED`/`HIGH`/`CRITICAL` - an inventory failure is never silently reported as "fine",
and a confirmed problem elsewhere is never masked by one namespace being fine).

`Monitor` debounces the raw sample stream against `RequiredConsecutive` (default: 3)
agreeing samples before its *reported* state actually changes - the same pattern (and the
same rationale) as `power.Monitor` from the Power/Shutdown-Resilience feature, reused here
for consistency rather than inventing a second hysteresis mechanism in this codebase.

## Retention planning

`Plan(namespace, items, quota, params)` is a pure function: given the same inputs, it always
produces the same `RetentionPlan`. It never touches the filesystem and never calls a clock
internally - `params.NowMonotonic`/`params.NowWallClock` are the caller's own values.

Eligibility, in order: (1) the namespace's `Criticality` must permit eviction at all -
`Policy.Evictable` returns true **only** for `CriticalityCache` (diagnostics'
`CriticalityBounded` is deliberately excluded - see "Diagnostics policy" below); (2) an
item's `Status` must be `StatusManaged` (`StatusActive`/`StatusUnmanaged` are always
excluded); (3) if the namespace has a `MinRetention` and the wall clock is currently
trusted (`NowWallClock` is non-zero), an item younger than `MinRetention` is excluded - if
the wall clock is **not** trusted, every `MinRetention`-gated item is conservatively
excluded rather than guessing it is "old enough."

Ordering among eligible candidates is oldest-`ModTime`-first, with the item `Name` as a
deterministic tie-breaker. A `RetentionPlan` reports every skipped item and why
(`SkipActive`/`SkipProtectedCriticality`/`SkipUnmanaged`/`SkipBelowMinRetention`/
`SkipTargetAlreadyMet`), the projected usage if every proposed candidate were evicted, and
an honest `TargetMet=false` with a reason when the namespace's protected/active data alone
already exceeds quota - this package never silently reports success by refusing to act.

### Diagnostics policy

`Policy.Evictable` never returns true for `CriticalityBounded`. Diagnostics already has its
own existing, tested retention mechanism (`readiness.WriteDiagnosticBundle`'s `maxRetain`
pruning, driven by `main/diagnosticsapi.go`'s `diagnosticsMaxRetain = 10`) - this foundation
observes and reports that namespace's usage, but never plans eviction against it. A future
revision that wanted this planner to actually drive diagnostic retention would need to
retire the existing mechanism first, as its own explicit, separately reviewed change.

## Atomic write and directory sync

`AtomicWriter.Write` is the one reusable way any future caller (recordings do not use this
yet - see "Integration scope" below) replaces or creates a file in a registered namespace:

1. Validate the namespace/name via `SafeJoin`.
2. Create a uniquely-named (`.slctmp-<name>.<random-suffix>`), package-owned temp file in
   the **same directory** as the final name, exclusively (`O_CREATE|O_EXCL`, plus
   `O_NOFOLLOW` on Linux) - never a shared/predictable path, never following a symlink.
3. Stream content through the caller's `WriteFunc`.
4. Apply the caller's `Mode` explicitly (never relies on the process umask).
5. `Sync()` the temp file's content, then `Close()` it.
6. Run the caller's optional `ValidateFunc` against the closed temp file.
7. `Rename` the temp file onto the final name - same directory, so this is atomic on the
   same filesystem; `AtomicWriter` never creates a temp file anywhere else, so a
   cross-filesystem rename can never happen.
8. `SyncDir` the containing directory, so the rename itself is durable.

On any failure before the rename, `Write` removes only the exact temp file it created -
never the destination (untouched either way) and never anything else. A directory-sync
failure *after* a successful rename still reports the final path as written (the content
genuinely is there) alongside the error, rather than claiming the write failed when it did
not.

## Interrupted-write recovery

`PlanRecovery` inspects a namespace's raw directory entries for this package's own owned
temp files (recognized by the exact `.slctmp-<name>.<suffix>` naming convention -
`ownedTempFinalName`) and proposes, without touching anything, one of: `RecoveryRemoveTemp`,
`RecoveryPromoteTemp`, or `RecoverySkipInUse`. Any entry that does not match this exact
naming convention is completely ignored - this package has no opinion about, and never
touches, a temp file it cannot prove it created.

Decision policy: a valid, already-committed destination always wins (the temp is removed,
regardless of whether it would itself have validated); among multiple owned temp candidates
for the same final name, only the lexically-first is ever considered for promotion, and
every other one is removed; a symlink or non-regular entry at an otherwise-matching temp
name is never promoted; promotion requires both `RecoveryOptions.AllowPromotion` (a
per-call, per-namespace decision - most namespaces should leave this false) **and** a
caller-supplied `TempValidator` that the temp file's content actually passes - an
unvalidated file is never promoted, regardless of `AllowPromotion`.

`ExecuteRecovery` is the separate, explicit step that actually performs the filesystem
operations one `RecoveryPlan` describes - `PlanRecovery` itself never mutates anything. One
item's failure never stops the rest of the batch, and this package has no general-purpose
"clean all temp files" routine - only the exact, provably-owned naming convention above is
ever considered at all.

## Concurrency and lock ordering

`Manager` holds one internal mutex, used only to (a) briefly check-and-set an in-progress
scan flag, and (b) briefly store a completed scan's result - **never** held during the
actual filesystem walk or while calling the injected `ActiveChecker`. Two overlapping
`Scan()` calls coalesce: the second one returns the current (possibly slightly older)
snapshot immediately rather than starting a redundant walk or racing to store two different
results.

`storageLifecycleActiveChecker` (`main/storagelifecycleapi.go`) takes the existing
recording-subsystem `recMu` only for a plain, fast read (never held across `storagelifecycle`
scanning any namespace) - the same non-blocking-lock discipline this project already uses
elsewhere (see `main/preflightapi.go`'s `recordingReadinessForPreflight`, which uses
`TryLock` for exactly this reason).

## Failure isolation

A namespace-level scan error never aborts the whole `Inventory`, a manager-level scan error
never panics or stops `storageLifecycleUpdateLoop` (the next tick simply tries again), and
nothing in this package's periodic scan can block or otherwise affect ADS-B/GPS/GDL90/
FIS-B/GDL90 processing - the same failure-isolation convention every other supplemental
subsystem in this project (alerting, preflight, power) already follows.

## Readiness integration

`readiness.StorageLifecycleHealth` (a new, separate field on `HealthReport`, never added to
the existing `Storage`/`TemporaryOverlay` `StorageHealth` type, so their own already-tested
filesystem-level semantics stay completely untouched) reports:

- `UNKNOWN`: no inventory has completed yet (startup grace).
- `NOT_READY`: pressure is `CRITICAL`.
- `DEGRADED`: the inventory is stale, the last scan reported a namespace error, or pressure
  is `HIGH`/`ELEVATED`/`UNKNOWN`.
- `READY`: a current inventory exists, no scan errors, pressure `NORMAL`.

This state participates in `Overall`'s rollup like every other component (a genuinely
critical storage-lifecycle pressure reading is a real operational concern), but `UNKNOWN`
during startup grace is excluded from dragging `Overall` down, per `Rollup`'s existing rule.
This policy never reports `READY` merely because deletion could theoretically reclaim
space - reclaimability plays no part in this decision at all.

## Preflight integration

A single, concise "Storage" → "Storage lifecycle" check
(`preflight.storageLifecycleChecks`): `NOT_APPLICABLE`/`INFO` while no inventory is
available yet (never a caution - this is an unpopulated foundation, not a fault),
`NOT_READY`/`BLOCKING` for confirmed `CRITICAL` pressure (matching this project's existing
treatment of real resource exhaustion, e.g. the "power_thermal" check), `CAUTION` for
`HIGH`/`ELEVATED`/`UNKNOWN` pressure or a stale inventory, `READY`/`INFO` otherwise.

## Diagnostics integration

`readiness.DiagnosticBundle.StorageLifecycleSummary` (opaque `interface{}`, following the
exact `PowerSummary`/`AlertingSummary`/`ConfigBackupSummary` precedent) carries: whether an
inventory exists, pressure, staleness, per-namespace managed/active/unmanaged counts and
byte totals, and scan-error count. Never a file name, a path beyond a namespace's own short
identifier, or file content.

## Dashboard

A new Storage page (`#/storage`, `web/plates/storage.html` / `web/plates/js/storage.js`)
shows: pressure, last-scan age/staleness, protected-data bytes, unmanaged-entry count/bytes,
scan-error count, whether automatic enforcement is enabled (always "disabled" in this
release), and a per-namespace table (managed/active/unmanaged counts and bytes). It always
displays the same "automatic cleanup is not enabled" and "recordings/profiles are never
automatically deleted" notes the API itself returns. **There is no delete control anywhere
on this page**, and no such API exists.

## Future automatic-recording contract

`storagelifecycle.RecordingLifecycle` (see `contracts.go`) is the exact interface a future
automatic-recording feature will implement: `ReserveSpace` (backed by the pure
`EvaluateRecordingSpace` decision function - critical/unknown pressure denies, high/elevated
pressure cautions, an expected size that would exceed the recordings namespace's quota
denies, otherwise allowed; an unestimated request is never denied on size alone),
`RegisterActive`/`Complete` (wiring into the Scanner's `ActiveChecker`), and
`NoAutomaticDeletion() bool` - a method, not only a comment, specifically so a contract test
can assert a real implementation honestly reports it never introduces new deletion
behavior. **No production code implements this interface in this mission** -
`contracts_test.go`'s fake is test-only.

## Future FIS-B-cache contract

`storagelifecycle.CacheLifecycle` is the exact interface a future FIS-B cache will
implement: `ReplaceProduct` (atomic, via `AtomicWriter` - a reader never observes a
partially-written product), `IsExpired` (backed by the pure, monotonic-only
`IsCacheEntryExpired` - a GNSS/NTP wall-clock correction can never make an unexpired product
look expired or vice versa), and `ReclaimPriority` (always `CriticalityCache` - a method, so
a contract test can assert a real implementation never claims a higher priority for itself
than this foundation's fixed ordering allows). **No production code implements this
interface, ingests, persists, or serves any FIS-B product in this mission.**

## Status of this release

- Automatic recording: **not enabled.** No flight-detection, no automatic start, no change
  to the existing manual `/startRecording` behavior.
- FIS-B weather caching: **not enabled.** No product is ingested, persisted, or served.
- Automatic eviction/enforcement: **not enabled.** `Manager` has no `Execute`-a-plan method
  at all - `RetentionPlan`/`RecoveryPlan` are always produced and never automatically acted
  on in production. The only filesystem-mutating production code path in this whole feature
  is the periodic, bounded, read-only inventory *scan* itself, which never deletes, moves,
  or renames anything.
- The `/getStorageLifecycle` endpoint is the only one this feature adds, and it is
  read-only (`GET` only) - there is no corresponding mutation/deletion endpoint anywhere in
  this codebase for this feature.

## Test strategy

112 tests in `storagelifecycle` (110 against a complete in-memory fake filesystem with
per-call fault injection - see `fakefs_test.go` - plus 2 against a real `t.TempDir()`
filesystem, exercising the real `osFS`/`NewOSFS()` implementation end to end) plus 11 in
`main/storagelifecycleapi_test.go`. Coverage includes: namespace/path safety (traversal,
absolute paths, separator-containing names, overlapping roots, sibling-prefix false
positives); inventory classification (managed/active/unmanaged, symlinks, unsupported file
types, missing/permission-denied namespaces, deterministic ordering, directory-item sizing);
pressure bands and hysteresis (every threshold, streak reset, recovery, no-flapping under
alternating samples); retention planning (protected/bounded/active/unmanaged exclusion,
oldest-first ordering with a stable tie-break, `MinRetention` with a trusted/untrusted
clock, integer-overflow safety, determinism); atomic-write fault injection at every
documented stage (create, write, chmod, sync, close, validation, rename, directory sync)
plus concurrent-writer scenarios; recovery (every documented decision-table case, repeated-
recovery idempotency, one-failure-does-not-stop-the-batch); and Manager-level concurrency
(concurrent scans coalescing without a data race).

## Deployment checklist (for the build-and-verify gate this mission itself runs)

1. Full repository test suite passes, including this feature's own packages.
2. `go vet`/`gofmt` clean on every changed file (pre-existing, unrelated formatting debt in
   untouched files is left alone - see this project's established policy on that).
3. Race-detector attempt on the concurrency-relevant tests (expect the documented ARM64/
   QEMU `unsupported VMA range` environment limitation, not a real failure).
4. ARM64 `.deb` built and independently verified: filename, size, SHA-256, embedded commit
   matches the exact feature-branch HEAD, storage-lifecycle symbols present in the compiled
   binary, no automatic-recording/FIS-B-cache/eviction code path reachable.
5. **Not deployed.** This mission's PR stays in draft.

## Later hardware-validation checklist (prepared, not executed)

This feature has not been deployed to any device, and this checklist has not been run - see
the Power/Shutdown-Resilience feature's own identical precedent for the same reasoning
(hardware validation is reserved for a future, separately authorized mission).

1. Confirm the repository/device baseline before deploying (matching this mission's own
   Phase-1 reconciliation).
2. Confirm the persistent-partition UUID and mount are unaffected by deployment.
3. Compare `/getStorageLifecycle`'s reported namespace usage against an independent, manual
   `du`/`find` count on the live device for each registered namespace.
4. Confirm `/getHealth`'s `StorageLifecycle` and `/getPreflightReport`'s "Storage lifecycle"
   check both read sensibly against the live inventory.
5. Confirm the diagnostics bundle's `StorageLifecycleSummary` is present and sanitized.
6. Check the dashboard Storage page on iPad portrait/landscape and iPhone portrait/
   landscape.
7. Start a real recording and confirm it is reported `StatusActive` (never a hypothetical
   eviction candidate) while the inventory is scanned mid-recording.
8. Stop that recording and confirm it is reported `StatusManaged` on the next scan.
9. Perform a controlled shutdown (see docs/power-shutdown-resilience.md) while an inventory
   scan could plausibly be in flight, and confirm no interaction/deadlock.
10. Perform a configuration restore and an OTA update and confirm neither is disrupted by,
    nor disrupts, the storage-lifecycle scan loop.
11. Confirm the previous-session marker (power feature) is unaffected.
12. Five controlled reboots; confirm the inventory re-populates cleanly each time.
13. A 30-minute stability window watching for goroutine/file-descriptor growth from the
    periodic scan loop specifically.
14. Confirm zero live eviction and zero data deletion occurred anywhere in the above.
15. Confirm every pre-existing recording, diagnostic, and calibration profile is unchanged.
16. Regression-check 978/1090/GPS/GDL90/AHRS/barometer/fan/alerts.
17. Rollback plan: revert to the previously validated baseline commit and redeploy via the
    established OTA state machine. No data mutation is expected in the initial
    observational deployment. Automatic eviction and production plan execution are
    disabled. Existing data remains protected, but deployment validation is still required
    to confirm inventory and integration behavior on real hardware.

## Rollback plan

This feature is entirely additive (new package, new read-only endpoint, new dashboard page,
additive fields on `HealthReport`/`DiagnosticBundle`/`preflight.Input`) and performs no
destructive operation in production. Rolling back means reverting to the prior deployed
commit and redeploying through the established OTA state machine - no data migration, no
cleanup, and no schema change either direction.

No data mutation is expected in the initial observational deployment. Automatic eviction
and production plan execution are disabled. Existing data remains protected, but
deployment validation is still required to confirm inventory and integration behavior on
real hardware.

## Known limitations

- `alert-settings.json`, `power-session.json`, and `updates/` are not covered by this
  foundation's namespace inventory - see "Known scope limitation" above. Each is already
  correctly, independently managed by its own owning subsystem.
- `exports/`'s `CriticalityImportant` classification is a documented judgment call, not an
  explicit mission mandate - see that section above.
- Namespace logical-size accounting does not reflect actual disk block consumption
  (sparse files, block rounding, directory overhead).
- Retention-order determinism rests on `Plan` itself calling no clock and reading no global
  state - the underlying `ModTime` values themselves still come from the filesystem, so a
  wall-clock correction that straddles the exact moment two items were created could still
  affect their *relative* order the next time a fresh `Inventory` is scanned. This is an
  inherent property of any mtime-based ordering, not something `Plan`'s own determinism
  guarantee claims to solve.
