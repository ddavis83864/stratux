# Rolling FIS-B Weather Cache

## Why this exists

Stratux's existing FIS-B (978 MHz UAT) weather path is entirely live and
transient: `uatparse` decodes a frame, `main/gen_gdl90.go` relays it to the
`/weather` websocket and to connected GDL90 clients, and the only thing
kept in memory afterward is a ~1-minute rolling `msgLog` used for stats
counters (`UpdateUATStats`). There is no in-memory "current weather" store,
no deduplication, and nothing survives a restart. A brief signal dropout -
descending into terrain shadow, taxiing behind a hangar, a receiver power
blip - means whatever was last received is simply gone.

The Rolling FIS-B Weather Cache is an **opt-in, disabled-by-default**
addition that remembers the most recent genuinely-received copy of a
bounded set of FIS-B products, tags it with its own age and provenance,
and exposes it for **display only** - never as a live feed, never
injected back into GDL90.

> **This cache never downloads, requests, or synthesizes any weather. It
> only ever stores a product this receiver's own 978 MHz radio genuinely
> decoded. It has no internet connection and adds no internet requirement.
> Cached weather can be older than currently broadcast data - always
> check a product's own age before use. The absence of a cached product
> does not indicate the absence of a hazard. This is never a substitute
> for an official preflight briefing or current airborne weather
> sources.**

> **The Rolling FIS-B Weather Cache is disabled by default and never
> deletes an existing recording, calibration profile, diagnostic bundle,
> configuration backup, OTA artifact, or unrelated file to make room.**

## Existing live FIS-B data path (as found, before this feature)

Established by direct inspection, not assumption:

- Raw UAT frames arrive via `godump978` (cgo, RTL-SDR) or
  `main/lowpower_uat.go` (external radio, serial); both converge on
  `main/gen_gdl90.go`'s `parseInput()`/`relayMessage()`.
- FIS-B decode lives in package `uatparse`: `uatparse.New`,
  `UATMsg.DecodeUplink()`, `UATFrame.decodeInfoFrame()`. Only product ID
  413 (generic text) and 63/64 (NEXRAD) have real structured decode.
  `decodeAirmet()` (AIRMET/SIGMET/NOTAM, product IDs 8/11/13) is dead code
  - its only call site is commented out.
- There is **no deduplication anywhere** in the live pipeline: a ground
  station rebroadcasting the same uplink message is relayed again,
  unchanged, every time.
- GDL90 forwarding relays the raw UAT payload bytes verbatim as message ID
  `0x07` ("Uplink Data") via `relayMessage()`, with the GDL90 "time of
  reception" field **hardcoded to `0x00`** - proving no existing
  age-signal mechanism exists in this project's GDL90 encoding to let a
  downstream EFB distinguish a just-received product from a replayed one.
  This single fact is the basis for this feature's replay decision (see
  "Replay: deliberately deferred," below).

This cache changes none of that: the live decode path, the `/weather`
websocket, and GDL90 forwarding are read from, never modified.

## Architecture: pure domain model, impure glue

Following this project's established convention (`alerting`, `readiness`,
`autorecord`, `storagelifecycle`):

- **`fisbcache`** (new Go package): pure, hardware-independent domain
  logic. No filesystem, radio, HTTP, or wall-clock calls of its own -
  every input is caller-supplied. Fully unit-tested without a radio,
  a clock, or a disk.
- **`main/fisbcache*.go`**: the glue that hooks the real capture path,
  owns settings, runs the persistence/recovery/retention loops, and
  serves the HTTP API.

| File | Responsibility |
|---|---|
| `fisbcache/product.go` | `ProductClass`, `Key`, `ClassifyProductID` |
| `fisbcache/entry.go` | `Entry`, `FreshnessState`, `Freshness` |
| `fisbcache/policy.go` | per-product `ProductPolicy` (the freshness table below) |
| `fisbcache/time.go` | `ReconstructSourceTime` - FIS-B's partial broadcast time vs. a trusted receive time |
| `fisbcache/nexrad.go` | `NexradTileIdentity`/`NexradKey` |
| `fisbcache/store.go` | `Store` (in-memory index), `Admit`, `Stats` |
| `fisbcache/retention.go` | `PlanEviction` |
| `fisbcache/schema.go` | `PersistedEntry`, `EncodePersistedEntry`/`DecodePersistedEntry` |
| `fisbcache/state.go` | `State`, `DetermineState` |
| `main/fisbcachesettings.go` | versioned, atomically-persisted settings |
| `main/fisbcachestorage.go` | the `storagelifecycle.CacheLifecycle` adapter |
| `main/fisbcachecapture.go` | the one file that imports `uatparse` - frame-to-`Entry` translation |
| `main/fisbcacherun.go` | init, capture queue/worker, startup recovery, retention loop, shutdown |
| `main/fisbcacheapi.go` | HTTP status/inventory/settings/purge endpoints |

## Product scope: only what this codebase actually decodes

`fisbcache.ClassifyProductID` recognizes exactly two classes, mirroring
`uatparse.UATFrame.decodeInfoFrame`'s own live decode paths one-for-one:

- **`ClassText`** - product ID 413, further identified by the leading
  token of each decoded text line (`main.registerADSBTextMessageReceived`'s
  own `Type`/`Location` pair - METAR/SPECI/TAF/TAF.AMD/WINDS/PIREP).
- **`ClassNexradTile`** - product IDs 63/64, identified by radar
  type/scale and the tile's own quantized geographic bounds.

Every other product ID is `ClassUnsupported` and is **never persisted** -
tracking "N frames of product ID X were observed" remains the existing
`UpdateUATStats` counters' job, not this cache's.

## Freshness policy: explicit, per-product, conservative

There is deliberately no single universal TTL. Every limit below is a
conservative, documented policy choice based on each product's
well-established FAA/NWS issuance cadence and operational currency
expectation - not measured from anything this codebase tracks (it tracks
no issuance cadence today), and always set toward the shorter side of
that cadence per this feature's "expire conservatively" requirement.

| Product | Fresh | Aging | Stale (evict-eligible after) |
|---|---|---|---|
| METAR / SPECI | 15 min | 75 min | 3 h |
| TAF / TAF.AMD | 3 h | 8 h | 30 h |
| Winds/temps aloft | 3 h | 9 h | 18 h |
| PIREP | 20 min | 60 min | 2 h |
| NEXRAD tile (any type/scale) | 10 min | 20 min | 45 min |

A cached entry moves through `FreshnessState` values
(`CACHED_FRESH` &rarr; `CACHED_AGING` &rarr; `STALE` &rarr; `EXPIRED`) purely
as a function of monotonic age and its product's own policy
(`fisbcache.Freshness`). `LIVE` is reserved for a value just received,
never assigned to anything served from the cache - see "Live versus
cached labeling," below. A product with no known policy is
`UNSUPPORTED` and was never admitted in the first place.

## Deduplication: this cache's own, not the live pipeline's

The live pipeline has no dedup at all (see above). `fisbcache.Store.Admit`
adds it, but only within the cache's own bounded index, never touching
the live relay path: a rebroadcast of an already-cached product updates
`ReceivedAtMonotonic` in place (`AdmitSuperseded`) rather than creating a
second entry, and a delayed/rebroadcast **older** copy can never regress
an already-cached newer one (`AdmitRejectedOlder`) - ordering is judged
by each copy's own reconstructed `SourceTime` when both are trusted,
falling back to receive order only when neither is.

## Time model

- **Monotonic time** drives every runtime freshness decision
  (`Entry.Age`/`Freshness`) - a wall-clock correction (GPS time jump, NTP
  step) never makes a live entry look expired or vice versa. This is the
  same discipline `storagelifecycle.CacheEntryMetadata` and `autorecord`
  already established, and directly answers the lesson from commit
  `64d3e83b` ("stop a stale boot-time clock from nuking settings" -
  see git log): an uncorrected startup clock is never trusted for
  anything time-sensitive in this project.
- **Trusted GNSS/UTC** (`readiness.TimeGNSSSynced`/`TimeNetworkSynced`,
  the same `readiness.TimeTrust` gate `autorecord` uses) is used only for
  (a) reconstructing a product's own broadcast/issue time
  (`ReconstructSourceTime`) and (b) bridging age across a reboot (below).
  It never drives a runtime freshness decision directly.
- **`ReconstructSourceTime`** is the only place this feature ever invents
  a year (FIS-B never encodes one) or a month/day (two of FIS-B's four
  time-format options omit them). It requires a currently-trusted receive
  clock, applies at most one conservative correction (a year rollback for
  month/day-bearing frames, a day rollback otherwise) to handle a
  broadcast-just-before/received-just-after date-boundary crossing, and
  otherwise rejects (`!Trusted`) anything more than 5 minutes in the
  future or 6 hours in the past relative to receive time - a corrupt or
  misdecoded time field is far more likely than a real broadcast that far
  from now.
- **Cross-reboot age bridging**: a persisted entry's on-disk record never
  stores a monotonic value (meaningless after a restart) - only its
  trusted wall-clock receive time, if one was ever recorded. At startup,
  once trusted time is available again, `fisbCacheStartupRecovery`
  computes each entry's real elapsed age from that persisted wall-clock
  time against the current trusted wall clock, then sets
  `ReceivedAtMonotonic = nowMonotonic - age` - so a recovered entry's age
  continues correctly from where it left off, never resets to zero, and
  never derives from an uncorrected boot-time clock (recovery itself
  waits, bounded by `fisbCacheStartupRecoveryTimeout`, for trusted time
  before touching anything).
- **Timestamps are never altered.** A superseded entry's `SourceTime`
  only ever moves forward (see `Admit`/`newerSource`); a persisted
  entry's original receive/source times are exactly what
  `DecodePersistedEntry` returns, never rewritten to "now."

## Persistence, atomicity, and recovery

Reuses this project's existing, already-tested primitives rather than
inventing new ones:

- **`storagelifecycle.CacheLifecycle`** (`main/fisbcachestorage.go`'s
  `fisbCacheLifecycle`) is this cache's only integration point with
  storage-wide accounting - `CriticalityCache` (the only evictable
  criticality), `.json`-only, one namespace (`fisb-weather-cache`) under
  `PersistentDataPath`.
- **`storagelifecycle.AtomicWriter`/`PlanRecovery`/`ExecuteRecovery`/
  `SafeJoin`** perform every actual file write, interrupted-write scan,
  and path-safety check - unmodified. This feature adds no second
  storage-policy engine.
- **`fisbcache.PersistedEntry`** (`schema.go`) is the on-disk JSON shape:
  a fixed `origin: "FISB"` marker, schema version, product identity,
  trusted-or-absent source/receive times, and a payload checksum
  (corruption detection only - explicitly not authentication, exactly
  like `configbackup.Document.ContentChecksum`). One file per entry,
  named deterministically from its `Key` - a second write to the same key
  is always an atomic replace, never a second file. Decoding is strict:
  wrong schema version, wrong/missing origin marker, oversized payload,
  checksum mismatch, an unrecognized product, or an implausibly-future
  timestamp are all rejected outright, never best-effort repaired.
- **Retention** (`fisbcache.PlanEviction`) is this cache's own
  deterministic decision, not `storagelifecycle.Plan()`: the in-memory
  `Store` snapshot already IS the authoritative index of what is
  persisted, so no separate filesystem re-scan is needed. Every
  `EXPIRED` entry is always removed first, regardless of budget; if the
  remainder still exceeds the configured byte/entry limits, additional
  entries are evicted oldest-received-first with a deterministic
  tie-break - never a recording, profile, diagnostic bundle, backup, OTA
  artifact, or any file this cache did not itself write (the namespace's
  `.json`-only extension allowlist and its own dedicated directory are
  the structural guarantee).
- **Automatic storage eviction is never enabled by this feature.** This
  cache's own bounded retention loop (`fisbCacheRetentionLoop`, 60 s) is
  the only thing that ever deletes a *cache-owned* file; the project-wide
  Storage Lifecycle Foundation's own eviction remains exactly as
  conservative as it already was.

## Storage-pressure interaction

Mirrors `autorecord`'s own established pattern: when `storageManager`'s
current pressure is `HIGH`/`CRITICAL`/`UNKNOWN`, this cache stops
**admitting new entries** (`StatePressureInhibited`) but keeps serving
whatever is already cached - denied storage means "do not write more,"
never "delete something to make room," and never blocks on a fresh
filesystem scan (it only ever reads the existing cached `Status()`
snapshot).

## Operational state

`fisbcache.DetermineState` (pure, evaluated the same way
`autorecord`'s and `power`'s own state functions are) - checked in this
order, most restrictive first: `DISABLED` (feature off, always wins
regardless of any other input) &rarr; `ERROR` (startup recovery hit a
condition serious enough to prevent normal operation) &rarr;
`STARTUP_GRACE` (recovery still running) &rarr;
`WAITING_FOR_TRUSTED_TIME` &rarr; `DEGRADED` (recovery quarantined
individually-corrupt entries, non-fatal) &rarr; `READ_ONLY` (deliberately
stopped accepting writes, e.g. during shutdown) &rarr;
`PRESSURE_INHIBITED` &rarr; `LIVE` (the permissive fallback).

## Capture path: isolated from live ADS-B/GDL90

`main/fisbcachecapture.go` is the only file that imports `uatparse`. Its
hook is called from `main/gen_gdl90.go` immediately after the existing
`weatherRawUpdate.SendJSON(f)` call - **after**, never in place of, and
never blocking it. Frames are handed off through a bounded channel
(`fisbCacheQueue`, capacity 256) to a dedicated worker goroutine; when the
queue is full, the frame is simply dropped and counted
(`fisbCacheDroppedWrites`), never blocking the capture call site. A
disabled cache, a saturated queue, a corrupt persisted entry, or a
retention-loop error can therefore never interrupt live ADS-B/UAT
reception or GDL90 forwarding - the one thing this feature must never be
allowed to do.

## Live versus cached labeling

`FreshnessLive` is never assigned by this package to anything read back
from the cache - it exists purely as a shared vocabulary value so the
dashboard and API can say "this is the value just received," distinct
from every value actually served **from** the cache
(`CACHED_FRESH`/`CACHED_AGING`/`STALE`). The inventory and status
endpoints, and the dashboard's Weather Cache page, always show a
product's freshness state and age alongside its content - there is no
path that presents a cached value without also showing that it is cached
and how old it is.

## Replay: deliberately deferred

This build **never re-injects a cached product into GDL90 or any
connected EFB**. `FISBCacheSettings.ReplayEnabled` exists as an explicit
field (not silently absent) so a future release that proves a specific
product's replay safe has somewhere to turn it on without a settings
schema break - but this build's own `Validate()` always rejects
`ReplayEnabled: true`, and the API does the same.

The reason is evidence, not caution for its own sake: `relayMessage()`
forwards a UAT payload as GDL90 message ID `0x07` with its "time of
reception" field **hardcoded to `0x00`** (see "Existing live FIS-B data
path," above). There is no existing mechanism in this project's GDL90
encoding for a downstream EFB to tell a just-received product apart from
a replayed one, and building one would mean changing the *live* encoding
path - explicitly out of scope for this feature (never change existing
live-decoder/encoder behavior merely to enable caching). Until that gap
is closed on its own merits, replay cannot be proven safe, so it stays
off. Persistence, aging, storage, the API, and the dashboard are all
otherwise complete and independently useful without it (see
`FISB_WEATHER_CACHE_FOUNDATION_IMPLEMENTED_REPLAY_DEFERRED`).

## Settings

`main/fisbcachesettings.go` mirrors `autorecordsettings.go`'s exact
pattern: schema-versioned JSON, a mutex-protected temp-file/fsync/rename
atomic write, and a missing/corrupt/schema-mismatched/failed-validation
file all degrade safely to `DefaultFISBCacheSettings()` - disabled,
persistence off, replay off, 16 MiB / 2000-entry limits - never fatal,
never silently enabling the feature.

| Field | Default | Meaning |
|---|---|---|
| `enabled` | `false` | master on/off - no capture, no persistence, no storage use when off |
| `persistenceEnabled` | `false` | when `false` even while enabled, the in-memory index still tracks live products for the dashboard, but nothing is written to disk or survives a restart |
| `replayEnabled` | `false` | always rejected by `Validate()` in this release - see "Replay," above |
| `maxCacheBytes` | 16 MiB | hard cap, 1 byte - 256 MiB |
| `maxEntries` | 2000 | hard cap, 1 - 100000 |

## HTTP API

| Endpoint | Method | Purpose |
|---|---|---|
| `/getFISBCacheStatus` | GET | operational state, counters, freshness/product breakdown |
| `/getFISBCacheInventory` | GET | per-entry identity/freshness/age/size - never raw payload content |
| `/getFISBCacheSettings` | GET | persisted settings only |
| `/setFISBCacheSettings` | POST | validate, persist, and apply new settings |
| `/prepareFISBCachePurge` | POST | step one of a two-step confirmed purge |
| `/confirmFISBCachePurge` | POST | step two - deletes every entry in this cache's own namespace only |
| `/cancelFISBCachePurge` | POST | explicitly discards a pending purge token |

The inventory endpoint deliberately never returns raw payload content -
identity/freshness/age/size only. A station identifier plus a decoded
METAR/TAF's own text is exactly what this project's existing `/weather`
websocket already displays live; duplicating it here was not justified
by anything this feature needs, and keeping the surface narrow costs
nothing.

## Dashboard

A "Weather Cache" page (`web/plates/fisbcache.html` /
`web/plates/js/fisbcache.js`, `FISBCacheCtrl`), added to the sidebar
alongside Weather/Towers and mirroring the Auto Record page's structure
and 3-second polling convention. It shows: enabled/disabled and
persistence state; operational state; trusted-time state; storage
pressure; cache byte/entry usage against configured limits; queue
depth and dropped-capture count; entries broken down by freshness and by
product class; last cleanup outcome; a per-entry inventory table with
freshness/age labeling; and a two-step confirmed purge control. The
disclaimer text from the top of this document is shown verbatim, and a
note explicitly states that replay to EFB apps is not implemented in
this release.

## Readiness, Preflight, and diagnostics integration

- **Readiness** (`readiness.FISBCacheHealth`/`BuildFISBCacheHealth`): a
  disabled cache never degrades overall system readiness - it reports
  `NOT_INSTALLED` and nothing more.
- **Preflight** (`preflight.fisbCacheChecks`): informational only, mirrors
  `autoRecordChecks`'s own pattern. (Note: this project already has an
  unrelated `fisbChecks` function for live tower/reception checks -
  `fisbCacheChecks` is deliberately named distinctly to avoid confusion
  with it.)
- **Diagnostics** (`fisbCacheDiagnosticsSummary`): state, counts, and
  schema version only - never a station identifier, raw payload, or
  exact coordinate.

## Configuration Backup/Restore integration

Mirrors `AutoRecordSettingsSection`'s established pattern exactly
(`configbackup/document.go`, `main/configbackupapi.go`'s
`fisbCacheSettingsSectionFromCurrent`/`applyFISBCacheSettingsSection`):
this cache's settings are one additional, independently checksummed
section of the configuration backup document, applied atomically as part
of the existing restore transaction. A restore never touches the cache's
own stored entries - settings only.

A backup created before this feature existed still restores successfully
- see `docs/configuration-backup-restore.md`'s "Legacy backup
compatibility" section: its checksums are verified against the exact
historical shape that produced them (now two recognized historical
shapes: pre-`autoRecordSettings` and pre-`fisbCacheSettings`), and the
missing `fisbCacheSettings` section is filled in disabled, with the
standard default limits - never left as an invalid zero value (an
all-zero section would itself fail `maxCacheBytes`/`maxEntries` bounds
checking), and never carrying over whatever the feature happens to be
configured as on the live device at restore time.

## Non-goals (explicitly out of scope for this feature)

- No replay of cached products to GDL90 or any connected EFB (see
  "Replay," above).
- No internet-sourced weather of any kind, and no new internet-connection
  requirement.
- No change to the live UAT/FIS-B decode path, `uatparse`, or GDL90
  encoding, beyond the one additive capture hook described above.
- No automatic storage eviction beyond this cache's own bounded, cache-
  owned retention loop.
- No structured decode of any product this codebase does not already
  decode (AIRMET/SIGMET/NOTAM remain out of scope, matching
  `decodeAirmet`'s existing dead-code status).

## Test strategy

- `fisbcache/*_test.go` (39 tests): product classification; freshness
  state transitions at every policy boundary; `Store.Admit`
  accept/supersede/reject-older/reject-unsupported, including the
  trusted-vs-untrusted source-time tie-breaking rules; `PlanEviction`'s
  expired-first-then-oldest-first ordering and budget math;
  `ReconstructSourceTime`'s month/day-present and month/day-absent paths,
  including the New Year's Eve/Day boundary case; persisted-entry
  encode/decode round-tripping and every strict-rejection case (bad
  schema version, missing origin marker, checksum mismatch, oversized
  payload, implausible future timestamp, unsupported product).
- `main/fisbcachesettings_test.go` (9 tests): default is disabled and
  self-valid; missing/corrupt/schema-mismatched/invalid persisted files
  degrade to defaults; save rejects invalid settings without partially
  writing.
- `main/configbackupapi_test.go`: the FIS-B cache settings section
  round-trips through backup/restore, defaults correctly from both
  recognized legacy backup shapes, and is cross-checked against
  `DefaultFISBCacheSettings()` so the two can never silently drift apart.

## Known limitations

- No product beyond generic FIS-B text and NEXRAD tiles is ever cached -
  this reflects what this codebase can actually decode today, not a
  deliberately narrower policy choice; extending it requires first
  extending `uatparse`'s own structured decode.
- Freshness thresholds are documented policy choices based on published
  FAA/NWS issuance cadences, not measured from this device's own
  observed traffic - an operator in an area with unusually sparse ground
  station coverage may see products age past their fresh window sooner
  than a busy-airspace operator would.
- Replay to EFB apps is not available in this release (see "Replay,"
  above) - this cache is a display/diagnostic aid only until that gap is
  closed.
