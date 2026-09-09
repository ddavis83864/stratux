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

## Synchronous admission bounds

This feature's foundation originally enforced `maxCacheBytes`/
`maxEntries` only from the 60-second retention loop - a burst of
distinct new products could transiently hold the cache over its
configured budget for up to that long. This section documents the
strict, synchronous replacement (`main/fisbcacherun.go`,
`main/fisbcachestorage.go`): the exact worst-case bound this cache can
ever exhibit at any instant, and why it holds under real concurrency,
not merely in the single-threaded case.

**Where enforcement now runs, synchronously, not just periodically:**

1. **After every single admission**, on the same goroutine, before that
   entry is ever persisted - `fisbCacheCaptureWorker` calls
   `fisbCacheRunRetention()` immediately after `Store.Admit` succeeds
   and before `fisbCachePersist`. The just-admitted entry is always this
   pass's freshest entry (`ReceivedAtMonotonic` is "now"), so
   `fisbcache.PlanEviction`'s own deterministic oldest-first order never
   selects it ahead of anything already cached - enforcement only ever
   makes room *for* it. Running this *before* the write, not after,
   means the disk is never even momentarily pushed over budget as a
   direct result of admitting one more entry.
2. **At the end of startup recovery** - a persisted set larger than the
   *currently* configured budget (e.g. settings were tightened since
   these files were written on an earlier boot) is trimmed back before
   recovery ever reports itself complete, not left for whatever the
   first periodic tick after boot happens to be.
3. **Immediately on a settings change** - `handleSetFISBCacheSettingsRequest`
   runs the same enforcement pass right after applying a new
   `maxCacheBytes`/`maxEntries`, so tightening the budget takes effect
   at once rather than up to 60 seconds later.
4. **The periodic 60-second loop still exists**, now purely as a
   backstop for pure time-based expiry - an entry can become `EXPIRED`
   purely from the passage of time, with no new admission to trigger
   enforcement, so a periodic sweep is still the only way that case gets
   noticed promptly.

**A single entry that could never fit is rejected before it is ever
admitted.** `fisbCacheEnqueue` rejects an offer outright (counted as
`oversizedRejected`, distinct from `droppedWrites`/`pressureRejected`)
whenever its own payload alone exceeds the currently configured
`maxCacheBytes` - admitting it would either violate the budget outright
or require evicting every other entry, including fresher ones, just to
make room for something that still would not fit twice.

**Concurrency: the exact race this closes.** Retention/enforcement is
always *planned* against a `Store.Snapshot()` taken slightly before it
*executes*. Without further care, a live admission could persist a
fresher copy of a key in the gap between that snapshot and eviction's
own decision to remove it, and eviction would then delete the FRESH
file moments after it was written - `Store.Delete` has no way to notice
an entry changed underneath it. Two mechanisms close this, together:

- **`fisbcache.Store.DeleteIfUnchanged(k, expected)`** - a compare-and-
  delete: removes `k` only if the Store's current entry for `k` is
  still exactly `expected` (`Entry` is a plain, comparable struct). A
  stale plan's delete becomes a safe no-op instead of destroying live
  data.
- **`fisbCacheDiskMu`** (`main/fisbcacherun.go`) - a lock held around
  (a) `fisbCachePersist`'s actual file write and (b) one planned
  eviction's own re-check-then-remove sequence
  (`fisbCacheEvictKeyIfUnchanged`: re-`Get` the key, and only if it
  still matches what was planned, remove the file and then
  `DeleteIfUnchanged`). Because both sides hold this lock for their
  *entire* file-touching sequence, a write can never complete in the
  narrow gap between eviction's own check and its remove - whichever
  side wins the race for the lock finishes first, so eviction's re-check
  can never observe a state a concurrent write is still in the middle of
  changing. `Store.Admit` itself is deliberately left outside this lock
  (a fast, separately-synchronized, non-disk operation); the proof this
  is still sufficient is worked through in `fisbCacheDiskMu`'s own doc
  comment. Proven under `go test -race` by
  `TestFISBCache_ConcurrentAdmitPersistAndRetentionNeverDivergesStoreFromDisk`
  (single-threaded admission - matching `fisbCacheCaptureWorker`'s own
  real architecture - concurrently with a second goroutine hammering
  `fisbCacheRunRetention` on its own, exactly the shape the periodic
  ticker/a settings change can produce in production) and
  `TestFISBCacheEvictKeyIfUnchanged_RefusesStalePlanEvenAfterConcurrentReplacementIsPersisted`
  (a deterministic, single-goroutine reproduction of the exact stale-plan
  sequence, proving the surviving file is provably the fresh write, not
  the stale one).

**Known, narrow, documented limitation:** startup recovery's own
quarantine deletes (a corrupt/future-dated/unaged/expired persisted
file, found during the directory scan) are individually serialized
against a concurrent live write via `fisbCacheDiskMu`, but the decision
to quarantine a given file is still made from content read moments
earlier, outside that lock - a live capture that persists a fresh
replacement for the exact same product in the narrow window between
recovery reading a stale file and deciding to quarantine it could still
have that fresh file wrongly removed. This is real, but startup-window-
only (recovery runs once, briefly, at boot) and self-healing (the live
capture's own in-memory `Store` entry - what every current consumer of
this feature actually reads - is unaffected, since recovery's
quarantine path never touches the `Store` for a key it did not itself
`Admit`; only the on-disk file of an about-to-be-superseded stale copy
is at risk). Documented here rather than silently claimed closed - see
`fisbCacheStartupRecovery`'s own doc comment for the full reasoning.

**The strict maximum disk-overhead bound this cache can ever exhibit, at
any instant:**

```
maxCacheBytes (configured, hard-capped at 256 MiB)
  + maxPersistedEntryBytes (69,632 bytes - one entry's own hard cap)
```

The first term is the enforced steady-state budget. The second is the
maximum *additional*, strictly transient overhead: `AtomicWriter.Write`
creates a uniquely-named temp file, writes and syncs it, then renames it
over the final path - during that brief window, the old file (if this
write is a replacement) and the new temp file can coexist on disk. Since
this feature has exactly one writer for its own namespace at a time
(`fisbCacheCaptureWorker` is a single goroutine, and `fisbCacheDiskMu`
excludes any concurrent eviction from touching the same window), at most
one such temp file can ever exist at once, and its size is bounded by
`maxPersistedEntryBytes` regardless of how large `maxCacheBytes` is
configured. At the 256 MiB hard cap this is a ~0.03% relative overhead;
at the 16 MiB default, ~0.4% - a small, fixed, always-known constant,
not a function of traffic volume or how long since the last periodic
tick.

**What this bound deliberately does not cover:** items sitting in
`fisbCaptureQueue` (up to 256, bounded separately - see "Exact limits,
bounds, and safety caps," above) have not yet reached `Store.Admit` and
so never count toward this disk bound at all; they are an *in-process
memory* bound (256 × up to 64 KiB ≈ 16 MiB worst case), unrelated to,
and not a substitute for, the disk-overhead model above.

## Storage-pressure interaction

Mirrors `autorecord`'s own established pattern: when `storageManager`'s
current pressure is `HIGH`/`CRITICAL`/`UNKNOWN` (or `storageManager`
itself is not yet initialized), this cache genuinely stops **admitting
new entries** - not merely a reported label. `fisbCacheEnqueue`
(`main/fisbcacherun.go`) calls the same
`fisbCacheStoragePressureProhibited()` classification
(`main/fisbcachestorage.go`) the status/dashboard snapshot reports, so
the two can never drift apart; a rejected offer is counted separately
from a queue-overflow drop (`pressureRejected` vs. `droppedWrites`) so
an operator can tell the two failure modes apart. Already-cached entries
are still served while admission is inhibited - denied storage means "do
not write more," never "delete something to make room," and never
blocks on a fresh filesystem scan (it only ever reads the existing
cached `Status()` snapshot). Proven by
`TestFISBCacheEnqueue_HighPressureRejectsAdmissionNotJustLabel` and the
full pressure-band matrix in `main/fisbcachestorage_test.go`.

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
| `maxCacheBytes` | 16 MiB (configured default) | user-adjustable, 1 byte - 256 MiB (hard safety cap) |
| `maxEntries` | 2000 (configured default) | user-adjustable, 1 - 100000 (hard safety cap) |

### Exact limits, bounds, and safety caps

Terminology, used consistently below: a **configured default** is what a
never-explicitly-configured install actually runs with; a
**user-adjustable bound** is a range the settings API will accept; a
**hard safety cap** is a limit this build enforces unconditionally,
never exposed as a setting; a **Storage Lifecycle pressure decision** is
`storageManager`'s own whole-filesystem judgment, entirely independent
of any limit below. "No invented limits" is not a claim this document
makes - every bounded system necessarily chooses limits; the ones below
are documented as conservative engineering policy, not as derived from
any external standard.

| Limit | Kind | Value | What happens at the limit |
|---|---|---|---|
| `maxCacheBytes` | Configured default | 16 MiB | Below this, no eviction is driven by size. |
| `maxCacheBytes` | User-adjustable bound | 1 byte - 256 MiB | `Validate()` rejects anything outside this range (0, negative, or > 256 MiB), for both the live settings API and Configuration Backup restore. |
| `maxEntries` | Configured default | 2000 | Below this, no eviction is driven by count. |
| `maxEntries` | User-adjustable bound | 1 - 100000 | Same rejection rule as above. |
| Individual payload size | Hard safety cap | 65536 bytes (64 KiB) | `fisbcache.EncodePersistedEntry` refuses to build a persisted record at all - the capture path simply never persists that one product (it is still visible live via the in-memory Store, exactly like any other non-persisted entry when persistence is off). Not user-configurable. |
| Persisted entry file size (payload + metadata/framing) | Hard safety cap | 69632 bytes (payload cap + 4096 bytes headroom) | `DecodePersistedEntry` rejects a file larger than this outright during recovery - quarantined (removed), never partially read. |
| Capture-queue capacity | Hard safety cap | 256 items | `fisbCaptureText`/`fisbCaptureNexrad` drop-and-count (`droppedWrites`) rather than block once full - see "Capture path," above. |
| Recovery-scan per-file read bound | Hard safety cap | 131072 bytes (128 KiB) | `fisbReadFileBounded` never reads more than this from any one file during startup recovery, regardless of the file's actual on-disk size - defense in depth beyond the entry-file-size cap above. |
| Purge confirmation token lifetime | Hard safety cap | 5 minutes | An unconfirmed prepare expires; a confirmed one is single-use - see "APIs," below. |
| Inventory/status response size | Bounded indirectly | Equal to the current entry count | Never separately capped beyond `maxEntries` itself - the inventory endpoint returns one summary row per currently-cached entry, and the entry count can never exceed `maxEntries` (enforced synchronously - see "Synchronous admission bounds," below). |
| Single-entry admission relative to `maxCacheBytes` | Enforced at admission | Rejected if `payload size > maxCacheBytes` | `fisbCacheEnqueue` rejects the offer outright (counted separately, `oversizedRejected`) before it ever occupies a queue slot or a Store entry - see "Synchronous admission bounds," below. |
| Product-specific limits | None | - | No product class has its own separate size/count cap beyond the whole-cache `maxCacheBytes`/`maxEntries` budget and the universal per-payload hard cap above. |

**Zero is always invalid, never "disabled" or "unlimited," for either
user-facing setting.** `maxCacheBytes <= 0` and `maxEntries <= 0` are
both rejected by `Validate()` - a disabled cache is expressed by
`enabled: false`, never by zeroing a budget. (`fisbcache.PlanEviction`
itself, the pure function the retention loop calls, does treat
`maxBytes <= 0`/`maxEntries <= 0` as "that specific budget is not
enforced" - but this build's own `Validate()` never lets a live or
restored settings value reach it in that state; this distinction exists
so `PlanEviction` remains a general-purpose, freely-reusable function
in its own right, exercised directly by
`fisbcache/retention_test.go`.)

**Budget enforcement is synchronous, at admission time - not merely
periodic.** See "Synchronous admission bounds," below, for the full
model and its proof; the summary: `maxCacheBytes`/`maxEntries` are
brought back into compliance immediately after every single admission,
before that admission's own entry is ever written to disk - not left to
however long until the next periodic tick.

**Why these defaults are appropriate without assuming a specific
partition size:** 16 MiB is a small, fixed absolute number - it does
not scale with, or assume, this project's typical several-gigabyte
persistent-data partition. It remains reasonable even on the smallest
installations this project supports (this project's own microSD
guidance targets a 4 GB card at minimum): 16 MiB is roughly 0.4% of a 4
GB card, and a negligible fraction of any larger one. An owner with a
particularly constrained installation, or who wants to cache
substantially more NEXRAD imagery, can raise `maxCacheBytes` up to the
256 MiB hard cap through the settings API; the default is deliberately
conservative rather than sized to any one installation's actual
headroom.

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

Both request-body-accepting endpoints (`/setFISBCacheSettings`,
`/confirmFISBCachePurge`) reject a body containing more than one JSON
value - `encoding/json`'s `Decoder.Decode` only ever consumes the first
value in a stream and silently ignores anything after it, so a second,
concatenated object (or trailing garbage) is rejected explicitly
(`fisbCacheDecodeStrictJSON`) rather than silently succeeding on the
first value alone.

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

- `fisbcache/*_test.go` (55 tests, pure, no I/O): product classification;
  freshness state transitions at every policy boundary; `Store.Admit`
  accept/supersede/reject-older/reject-unsupported, including the
  trusted-vs-untrusted source-time tie-breaking rules; `Store.DeleteIfUnchanged`'s
  compare-and-delete semantics (deletes on an exact match, safely no-ops
  when the entry changed or is already absent - the primitive
  `fisbCacheEvictKeyIfUnchanged` builds on, see "Synchronous admission
  bounds," above); `ReconstructSourceTime`'s month/day-present and
  month/day-absent paths, including the New Year's Eve/Day boundary case;
  persisted-entry encode/decode round-tripping and every strict-rejection
  case (bad schema version, missing origin marker, checksum mismatch,
  oversized payload, implausible future timestamp, unsupported product);
  `PlanEviction`'s full decision matrix on its own (`retention_test.go`)
  - expired-first-then-oldest-of-remaining ordering, independent
  byte/entry budget disable-at-zero-or-negative semantics, and
  determinism (ten repeated runs against an all-tied input, proven
  identical despite Go's randomized map iteration).
- `main/fisbcachesettings_test.go` (10 tests): default is disabled and
  self-valid; missing/corrupt/schema-mismatched/invalid persisted files
  degrade to defaults; save rejects invalid settings without partially
  writing; exact-boundary values accepted, one unit past rejected;
  integer overflow in a request body rejected as a clean 400.
- `main/fisbcacheapi_test.go` (20 tests): every endpoint's method guard,
  malformed/multi-value/oversized/unknown-field body rejection, the full
  purge token lifecycle, and an 8-worker concurrent status/settings/purge
  stress test with a deadlock timeout.
- `main/fisbcachestorage_test.go` (11 tests) / `main/fisbcacherun_test.go`
  (15 tests): production-level integration tests against a real temp
  filesystem and a real `storagelifecycle.Manager` - storage-pressure
  admission gating across every pressure band; the namespace registers
  exactly once; eviction deletes only its named keys, proven against a
  real directory containing an untouched unrelated file; startup recovery
  skips a real symlink and subdirectory, quarantines a corrupt entry,
  bridges age correctly across a simulated reboot (including an
  already-expired-by-bridged-age entry, removed and never re-admitted),
  and its wait for trusted time is genuinely bounded; retention deletes
  from both disk and the in-memory index; the capture queue drops and
  counts under overflow without ever blocking. Synchronous admission
  bounds specifically (see that section, above): an oversized single
  entry is rejected before it is ever queued or admitted, while an
  exactly-at-budget one is accepted; a stale eviction plan is refused
  even after the same key has been concurrently re-persisted (both a
  deterministic single-goroutine reproduction and a genuine concurrent
  `-race` test mirroring production's actual single-threaded-admission-
  plus-concurrent-retention-callers shape); the entry-count budget is
  never exceeded across many sequential admissions; startup recovery
  enforces a since-tightened budget before ever reporting itself
  complete; and a settings-driven budget tightening takes effect
  synchronously inside the settings handler itself, not on the next
  periodic tick.
- `main/configbackupapi_test.go`: the FIS-B cache settings section
  round-trips through backup/restore, defaults correctly from both
  recognized legacy backup shapes, and is cross-checked against
  `DefaultFISBCacheSettings()` so the two can never silently drift apart.
- Native `go test -race` (genuine amd64, not QEMU-emulated) passes
  cleanly for every pure package this feature touches, and for this
  feature's own code within `main` - see "A note on this feature's own
  race-testing scope," below.

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
- Startup recovery's quarantine-delete decision has one narrow,
  documented, self-healing race against a concurrent live write for the
  exact same product in the same narrow startup window - see
  "Synchronous admission bounds," above, for the precise scope.

### A note on this feature's own race-testing scope

Native `go test -race` was run against every package this feature adds
to or modifies, including `main` (this project's cgo-linked daemon
package). This foundation's original verification pass found every race
that appeared traced to one of two locations, both pre-existing and
untouched by this feature's own diff at the time: `main/`'s foundational
shared monotonic clock (`monotonic.go`, unmodified since this project's
original 2015-2016 implementation) and the pre-existing `/setSettings`
handler (`managementinterface.go`, unrelated `globalSettings`
mutation). **Both have since been fixed**, on a dedicated hotfix branch
(`hotfix/main-concurrency-races`, merged to `master` and then merged
into this feature branch via a normal merge commit before the
synchronous-admission-bounds work above) - `monotonic` now uses
`sync/atomic` throughout, and a new `globalSettingsMu` serializes every
HTTP-triggered `globalSettings` read/write. Full native
`go test -race -count=1 ./main/...`, repeated 3x, is clean after both
fixes and after this foundation's own synchronous-admission-bounds
changes, except for one remaining, still out-of-scope,
still-pre-existing race: `TestAutoRecordAwaitMountAndReload_ReloadsOnceMountBecomesReady`
(`main/autorecordrun_test.go`) - a test-code-only synchronization gap
between a test goroutine and its own helper, unrelated to this feature
or to either of the two fixed races, documented for separate owner
review. One genuinely in-scope race *was* found and fixed during this
foundation's original verification pass: `storagelifecycle.Monitor` (a
dependency this feature's own storage-pressure check calls into) - see
that package's own commit history for the fix and its dedicated
regression test.

## Rollback plan

This feature is disabled by default and additive-only: no existing
setting, recording, calibration profile, backup format, or live
ADS-B/GDL90 behavior is altered in a way that requires migration to roll
back. Reverting is either leaving `enabled: false` (the shipped default
- the daemon then performs no capture, no persistence, and uses no
storage beyond an already-registered, empty namespace), or a plain `git
revert` of this branch's commits. A previously-created cache-owned
namespace directory left on disk after a revert contains only this
feature's own `.json` entry files and is safe to leave in place or
remove manually - nothing else on the persistent partition ever
references it.

## Future deployment-validation procedure

Not yet executed - this foundation has not been installed on any
device (see the mission's own final report for current build/artifact
status). A future, owner-authorized hardware-validation pass should, at
minimum: enable the feature on a device with live 978 UAT reception and
confirm entries appear with correct freshness transitions over real
time; enable persistence and confirm entries survive a reboot with
correctly bridged (not reset) age; confirm storage-pressure inhibition
under a small configured `maxCacheBytes`/`maxEntries`; confirm the
synchronous admission-time budget enforcement (see "Synchronous
admission bounds," above) under sustained real traffic against a
deliberately small budget; confirm the two-step purge
clears only this cache's own namespace, with every other namespace
(recordings, profiles, diagnostics, backups, OTA state) fully intact;
and confirm live ADS-B/UAT reception and GDL90 forwarding are completely
unaffected with the feature enabled under real traffic load, including
after deliberately saturating the capture queue.

## Future replay work (explicitly not part of this foundation)

Should a future change close the GDL90 reception-time gap described
under "Replay," above, replay would still need its own dedicated
protocol design (how a replayed product is distinguished from a live
one in the GDL90 stream itself) and its own EFB-side validation (at
minimum, confirming ForeFlight - and ideally other common EFBs -
correctly ages, displays, and never presents replayed data as live)
before `replayEnabled` could ever be accepted by this build's own
`Validate()`. That work is out of scope for this foundation and was not
attempted here.
