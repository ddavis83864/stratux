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
| `main/fisbcacherun.go` | init, capture worker, startup recovery, the asynchronous cleanup worker, shutdown |
| `main/fisbcachereserve.go` | `fisbPendingQueue`, `fisbCacheReservationFits` - the synchronous, pre-enqueue, eviction-free capacity reservation model (see "Synchronous admission bounds," below) |
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
- **Automatic storage eviction is never enabled by this feature.** Only
  this cache's own logic ever deletes a *cache-owned* file, always via
  the same safe, per-key `fisbCacheEvictKeyIfUnchanged` primitive (see
  "Synchronous admission bounds," below) - and always from ONE dedicated
  goroutine, `fisbCacheCleanupWorker` (signaled by `fisbCacheRequestCleanup`,
  or its own 60 s periodic tick as a time-based-expiry backstop), plus the
  confirmed-purge handler, which runs on the calling HTTP goroutine by
  design (see "Purge and retention concurrency," below - a purge is an
  explicit, operator-initiated, bounded-by-current-Store-size action, not
  an unbounded background decision). The live capture path never deletes
  anything itself; the project-wide Storage Lifecycle Foundation's own
  eviction remains exactly as conservative as it already was.

## Synchronous admission bounds

This feature's capacity enforcement went through three designs before
reaching the one described here (`main/fisbcachereserve.go`,
`main/fisbcacherun.go`, `main/fisbcachestorage.go`):

1. Originally, `maxCacheBytes`/`maxEntries` were enforced only from the
   60-second retention loop - a burst of distinct new products could
   transiently hold the cache well over its configured budget for up to
   that long.
2. A first correction made enforcement run synchronously *after* each
   admission, before that entry was persisted - closing the "up to 60
   seconds over budget" gap, but still deciding capacity only after an
   item had already been queued and already occupied a `Store` slot.
   Nothing accounted for what was still sitting in the queue, or being
   actively written, when deciding whether a *new* capture could be
   admitted at all - a large enough burst could still queue up well past
   what the budget could ever hold, all before a single one of those
   items was evaluated against it.
3. A second correction made capacity a **reservation**, decided
   synchronously at `fisbCacheEnqueue` time, strictly *before* an item
   ever occupies a queue slot - closing that gap too, but in doing so
   introduced a genuine regression: when a candidate did not already
   fit, `reserveAndEnqueue` itself evicted committed entries (real file
   deletions) synchronously, right there, on the same goroutine that
   called it - and `fisbCacheEnqueue` is called directly from the live
   UAT/978 decode path (`main/gen_gdl90.go`'s `parseInput`, the same call
   chain that updates live stats and forwards to GDL90/the `/weatherraw`
   websocket). File deletion is genuine filesystem I/O, and this
   project's own established "never block the live decoder" contract for
   this feature explicitly forbids performing that I/O there - a
   sustained-capacity-pressure scenario could measurably slow live
   reception and forwarding, which is exactly the failure this whole
   feature was built to never risk.

This section documents the current, final design: capacity is still a
reservation, decided synchronously and strictly before an item ever
occupies a queue slot, but a reservation that does not already fit is
now **rejected immediately, never evicted-and-retried inline** - eviction
itself moved entirely off the live capture path, onto one dedicated
asynchronous worker goroutine. This is the exact worst-case *capacity*
bound this cache can ever exhibit at any instant, together with an
explicit, separately-proven *latency* bound (the capture path performs no
filesystem I/O, ever, under any outcome) - and why both hold under real
concurrency, not merely in the single-threaded case.

### The reservation model

`fisbCacheEnqueue` never merely queues a capture and hopes there is room
later, and it never evicts. It calls `fisbPendingQueue.reserveAndEnqueue`
(`main/fisbcachereserve.go`), which - as one atomic operation, holding
the pending-queue's own lock throughout - computes the **projected**
total (everything already **committed**, i.e. `Store.Snapshot()`, plus
everything already **reserved**, i.e. every other currently queued or
in-flight capture, plus this new candidate) via `fisbCacheReservationFits`,
a PURE, in-memory computation over two already-in-memory snapshots - no
disk access whatsoever, and never followed by one here. If the candidate
already fits, the reservation is granted immediately. If it does not,
`reserveAndEnqueue` does exactly two things: reject the offer
(`capacityRejected`, distinct from
`oversizedRejected`/`pressureRejected`/`droppedWrites` - see "Exact
limits, bounds, and safety caps," above) and call `fisbCacheRequestCleanup`
(`main/fisbcacherun.go`) - a non-blocking, coalescing signal to the
asynchronous cleanup worker (see "The asynchronous cleanup worker,"
below) - then return immediately. Nothing here ever waits for cleanup to
run, and a rejected capture is never retried by this feature itself; a
later retransmission of the same product (FIS-B ground stations rebroadcast
periodically) is admitted normally once cleanup has actually freed room.

`fisbPendingQueue` replaces the earlier plain channel with a bounded,
**per-key-coalescing** structure: at most one pending reservation per
product `Key`. A second capture for a key that is already queued always
*supersedes* its reservation (updating the reserved size to the latest
offer) rather than adding a second, separate one - this is what makes
"reserved bytes"/"reserved entries" a precise count of *distinct
products* with an outstanding claim, never inflated by how many times
one of them was re-broadcast while still queued. A reservation's
lifecycle has three states, all counted as "reserved": **queued**
(`q.items`, not yet dequeued), **in-flight** (`q.inFlight`, dequeued by
the capture worker, `Store.Admit` not yet decided), and finally
released - the instant `Store.Admit` decides the item's fate (accepted,
superseded, or rejected), regardless of outcome, since at that point the
key's contribution to capacity is either now reflected in `committed`
(accepted/superseded) or nothing at all (rejected).

Once a reservation is granted and the capture worker (`fisbCacheCaptureWorker`
/ `fisbCacheProcessOneCaptureItem`) actually processes it, it calls
`fisbCacheRequestCleanup` immediately after `Store.Admit`, before
persisting - a non-blocking request, never a synchronous enforcement
pass on the worker's own goroutine, so a slow disk can never make one
capture's processing wait on another's cleanup. See "The asynchronous
cleanup worker," below, for why this is still enough to keep committed
state converging back under budget promptly, and "settings-limit
reductions," below, for why this is also what makes the "grandfathered
reservation" exception safe.

### The asynchronous cleanup worker

Every disk-mutating capacity/retention decision this feature ever makes
- `fisbCacheRunRetention`, the only function that actually plans and
executes an eviction - runs on exactly ONE dedicated goroutine,
`fisbCacheCleanupWorker` (`main/fisbcacherun.go`), started once from
`initFISBCache`. It wakes on either of two things: a signal on
`fisbCacheCleanupSignal` (sent by `fisbCacheRequestCleanup` - a
buffered(1), coalescing, non-blocking channel send, mirroring
`fisbPendingQueue`'s own `wake` channel pattern already established
elsewhere in this feature) or its own periodic `fisbCacheRetentionInterval`
(60 s) tick, a backstop for pure time-based expiry that needs no new
admission or request to become due. Because this worker is the *only*
caller of `fisbCacheRunRetention`, at most one cleanup pass ever runs at
a time, by construction - no separate locking is needed to enforce that.

Four call sites request a cleanup pass, all non-blocking, all safe to
call from any goroutine including the live decode path itself:
`reserveAndEnqueue` (a capacity rejection), `fisbCacheProcessOneCaptureItem`
(the post-`Admit` backstop, above), `fisbCacheStartupRecovery` (once, at
the end - see "Startup recovery," below), and
`handleSetFISBCacheSettingsRequest` (so a tightened budget takes effect
promptly rather than waiting for the next periodic tick, without the HTTP
handler itself ever performing the disk work - see "Settings-limit
reductions," below). Every `fisbCacheRequestCleanup` call increments
`fisbCacheCleanupRequested` regardless of whether it actually triggers a
new pass or coalesces into one already pending; `fisbCacheCleanupRuns`
only increments once a pass actually executes, and `fisbCacheCleanupRunning`
is true for that pass's duration - all three, plus `shutdownRejected`,
are exposed via `/getFISBCacheStatus` so an operator (or a test) can
observe this worker's actual activity, not just infer it.

### Every concern this reservation model accounts for

- **Persisted entries** - `Store.Snapshot()`, the *committed* side of
  every projection.
- **Queued entries** - `fisbPendingQueue.items`, counted as *reserved*.
- **In-flight writes** - `fisbPendingQueue.inFlight` (dequeued, not yet
  `Admit`-decided), also counted as *reserved*, so the one item the
  capture worker is actively processing is never invisible to a
  concurrent reservation's own projection.
- **Replacements / superseded queued updates** - a second capture for an
  already-queued key updates (never adds to) its reservation; a
  candidate that already has a *committed* entry is projected using the
  candidate's own size, never double-counted against its own
  about-to-be-superseded committed value.
- **Reserved bytes / reserved entry-count deltas** - `fisbCacheProjectedTotals`
  merges committed and reserved into one set, reserved's value always
  winning for a key present in both (a key mid-transition from reserved
  to committed, still counted exactly once).
- **Failed, rejected, canceled, or interrupted work** - a reservation
  that never actually reaches the queue (structurally full, or capacity
  could not be freed) never reserves anything in the first place; an
  `Admit`-rejected item (an older copy of an already-cached product)
  releases its reservation exactly like an accepted one and is never
  persisted (`TestFISBCacheProcessOneCaptureItem_RejectedAdmitReleasesReservationAndPersistsNothing`).
- **Settings-limit reductions** - see below.
- **Purge and retention concurrency** - see below.
- **Startup recovery** - recovery re-admits directly into `Store`
  (bypassing reservation entirely, since it is reading already-persisted
  files, not live captures); `fisbCacheRequestCleanup` is called once at
  the end, promptly bounding recovery's own contribution to budget (see
  "Startup recovery," below, for why this is asynchronous rather than
  synchronous, and how the recovery-time quarantine race this used to
  document as a known limitation is now fully closed).

### Settings-limit reductions

A reservation already granted under a looser budget is **not**
retroactively cancelled when settings tighten before it is processed
(cancelling in-flight work mid-flight would need its own, more invasive
machinery than this narrow correction warrants). Instead, this is a
deliberate, bounded, self-correcting exception: `fisbCacheRunRetention`
always re-validates against the settings current when *it* runs, not
whatever was current at reservation time or even at commit time - so
`committed` state (what is actually taking up disk space) converges back
under the *current* budget once the cleanup worker's next pass runs
(requested promptly at commit time by `fisbCacheProcessOneCaptureItem`,
above - not deferred to the periodic tick), however many "grandfathered"
reservations were outstanding when the tightening happened. Proven by
`TestFISBCacheProcessOneCaptureItem_SettingsTighteningDuringOutstandingReservationSelfCorrects`.
`handleSetFISBCacheSettingsRequest` also calls `fisbCacheRequestCleanup`
immediately on every settings change, for the *already-committed* state
that exists at that moment - deliberately a request, never an inline
enforcement pass: an HTTP handler goroutine running a potentially large,
unbounded eviction pass synchronously would block that response on
however much disk I/O a large tightening happens to require. Proven by
`TestHandleSetFISBCacheSettings_TighteningBudgetRequestsCleanupNotInlineEviction`,
which asserts the Store may still exceed the just-tightened budget the
instant the response returns, but that a cleanup pass was requested and
converges it once run.

### Purge and retention concurrency

The confirmed-purge handler (`handleConfirmFISBCachePurgeRequest`)
first calls `fisbPendingQueue.clear()`, discarding everything still
**queued** (not yet dequeued), so a purge is never immediately, silently
undone by whatever was waiting behind it - deliberately leaving anything
already **in-flight** alone (the capture worker may already be
`Admit`-deciding it; this feature's established "never abandon in-flight
work" posture applies here too, and the resulting failure mode is
narrow: at most the one in-flight item can reappear immediately after a
purge). It then deletes every entry named in its own `Store.Snapshot()`
via `fisbCachePurgeUsingSnapshot` - the SAME per-key, check-then-delete
primitive (`fisbCacheEvictKeyIfUnchanged`) retention and reservation
eviction use, not a blind batch delete - so a concurrent capture that
supersedes a key between the purge's snapshot and its own per-key
deletes can never have its fresh write destroyed by a purge decision
made against the older, now-stale snapshot value. Proven by
`TestFISBCachePurgeUsingSnapshot_NeverDestroysAConcurrentReplacement`
and `TestHandleConfirmFISBCachePurge_ClearsQueuedReservations`.

### Concurrency: the exact races this closes

**Race 1 - a stale eviction plan destroying fresh data.** Every eviction
this feature ever performs (`fisbCacheRunRetention`, run exclusively by
`fisbCacheCleanupWorker` - see "The asynchronous cleanup worker," above;
`reserveAndEnqueue` itself performs none) is always *planned* against a
`Store.Snapshot()` taken slightly before it *executes*. Without further
care, a live admission could persist a fresher copy of a key in the gap
between that snapshot and eviction's own decision to remove it, and
eviction would then delete the FRESH file moments after it was written -
`Store.Delete` has no way to notice an entry changed underneath it. Two
mechanisms close this, together:

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
  `DeleteIfUnchanged` - itself re-checked, never assumed to succeed just
  because the file removal did, since `Store.Admit` is deliberately not
  gated by this lock and can still supersede the key in the narrow gap
  between the two). Because both sides hold this lock for their *entire*
  file-touching sequence, a write can never complete in the narrow gap
  between eviction's own check and its remove. Proven under
  `go test -race` by
  `TestFISBCache_ConcurrentAdmitPersistAndRetentionNeverDivergesStoreFromDisk`
  and `TestFISBCacheEvictKeyIfUnchanged_RefusesStalePlanEvenAfterConcurrentReplacementIsPersisted`.
- **A missing file is never treated as a failure to evict.** If the file
  to be removed simply does not exist - persistence disabled
  (in-memory-only mode never writes one), or this exact entry was just
  `Admit`-ted and the cleanup worker's own pass (requested by
  `fisbCacheProcessOneCaptureItem`'s post-`Admit` backstop) happens to
  run *before* that admission's own `fisbCachePersist` call completes -
  `fisbCacheEvictKeyIfUnchanged` still removes the `Store` entry: there
  is nothing left on disk to protect, and refusing to correct the
  `Store` index in this case would make such a key permanently
  un-evictable. Any *other* removal error (permission denied, I/O
  failure, a path-safety rejection) still refuses to touch the `Store` -
  proven by `TestFISBCacheEvictKeyIfUnchanged_DeletionFailureIsNeverCountedAsSuccess`,
  which injects exactly such a failure and confirms it is neither
  reported as a success nor allowed to touch the `Store`.

**Race 2 - a reservation's own capacity check using a momentarily stale
committed snapshot (superseded, then closed differently).** An earlier
revision of this reservation model had `reserveAndEnqueue` itself evict
committed entries to make a candidate fit, then re-verify against a
freshly re-read `Store.Snapshot()` before trusting that eviction had
actually worked. That entire mechanism no longer exists:
`reserveAndEnqueue` never evicts anything (see "Synchronous admission
bounds," above, for why performing that eviction on the live capture
path was itself the regression this design corrects), so there is no
evict-then-re-verify sequence left to have a stale-plan problem in the
first place. What remains is simpler and different in kind:
`fisbCacheReservationFits` evaluates `committed` (`Store.Snapshot()`) +
`reserved` (`fisbPendingQueue`'s own ledger, read while holding its
lock) + the candidate, all in one pass - but `Store.Admit` is not gated
by that lock, so this `committed` snapshot can still be momentarily
behind a concurrent commit finishing at the same instant.
`fisbCacheProjectedTotals`'s own merge rule (reserved always wins for a
key present in both committed and reserved - see "Every concern this
reservation model accounts for," above) is what keeps a key from ever
being double-counted across that transition; that a key is also never
*under*-counted across it - the actual concern for the required
`projected <= budget` invariant - is not merely argued here but directly,
empirically proven under genuine concurrent load: 150 concurrent
captures across 10 goroutines racing real admission against real
projection reads, zero invariant violations observed, in
`TestInvariant_ProjectedNeverExceedsMaximumUnderRealConcurrentLoad`
(`go test -race`).

**Observability caveat - not an enforcement gap.** The status API's
`projectedBytes`/`projectedEntries` fields, like any external reader,
combine `Store.Snapshot()` (its own mutex) and the pending queue's own
reservation ledger (a *different* mutex) into one number. Read as two
independent, separately-locked snapshots, this combination can
momentarily describe a mix of two different real instants that never
truly co-existed (e.g. one taken just before a reservation's own
eviction completes, the other just after) - a pure artifact of
non-atomic combination, never a real admission-time violation (the
admission decision itself, inside `reserveAndEnqueue`, is never affected
by this - it always computes committed+reserved while holding the
pending queue's own lock throughout, which *is* atomic relative to every
other reservation). Where this distinction actually matters -
*proving* the bound, not just reporting it for a dashboard -
`fisbCacheProjectedSnapshotAtomic` holds the pending queue's lock across
*both* nested reads, closing the gap; `TestInvariant_ProjectedNeverExceedsMaximumUnderRealConcurrentLoad`
uses it for exactly this reason.

**Race 3 - startup recovery's own quarantine-delete decision (CLOSED).**
An earlier revision of this design documented a known, narrow limitation
here: recovery's quarantine deletes (a corrupt/future-dated/unaged/
expired persisted file, found during the directory scan) were
individually serialized against a concurrent live write via
`fisbCacheDiskMu`, but the DECISION to quarantine a given file was made
from content read moments earlier, outside that lock - so a live capture
persisting a fresh replacement for the exact same product in the narrow
window between recovery reading a stale file and deciding to quarantine
it could have that fresh file wrongly removed. This is now fully closed,
using the same pattern as Race 1: `fisbCacheQuarantineIfStillWarranted`
re-reads and re-classifies (`fisbCacheClassifyRecoveryFile`, the same
pure classification logic recovery's own initial scan uses) each file's
CURRENT content from scratch, immediately before removing it, while
holding `fisbCacheDiskMu` for the entire re-check-then-remove sequence -
the same lock a concurrent `fisbCachePersist` for that exact path also
requires, so whichever side wins the race for the lock completes
entirely before the other starts. Critically, the re-check also fetches
its OWN fresh `now`/`nowMono` rather than reusing the scan's own, by-then-
stale reference time: an early implementation of this fix reused the
scan's frozen `nowUTC`, which - since a genuinely fresh concurrent
write's own receive time can only be at or after that frozen value -
made every genuinely fresh replacement look *future-dated* relative to
it and get wrongly quarantined anyway, silently defeating the whole
re-check under a different guise. Proven by
`TestFISBCacheStartupRecovery_QuarantineNeverDestroysAConcurrentReplacement`,
which caught exactly that bug during this feature's own verification.

### The required invariants, and where each is proven

```
committed cache bytes                                <= configured maximum bytes
committed cache entries                               <= configured maximum entries
projected bytes (committed + queued/in-flight)        <= configured maximum bytes
projected entries (committed + queued/in-flight)      <= configured maximum entries
temporary atomic-write overhead                       <= one explicitly derived hard bound
queue payload bytes (application-retained, not RSS)   <= one explicitly derived hard bound
```

- **Committed bytes/entries `<=` budget:**
  `TestInvariant_CommittedBytesAndEntriesNeverExceedConfiguredMaximum`
  (a real burst through the actual reservation+admission pipeline,
  cross-checked against what is actually on disk).
- **Projected bytes/entries `<=` budget, including queued/in-flight
  work:** `TestInvariant_ProjectedBytesAndEntriesNeverExceedConfiguredMaximumMidBurst`
  (deterministic, single-threaded, checked after every single enqueue
  call, with no worker draining anything - proving the bound from
  `fisbCacheEnqueue`'s own synchronous reservation alone) and
  `TestInvariant_ProjectedNeverExceedsMaximumUnderRealConcurrentLoad`
  (genuinely concurrent, `go test -race`, using
  `fisbCacheProjectedSnapshotAtomic` - see the observability caveat
  above for why that specific accessor, not two separate reads, is what
  this proof requires).
- **Temporary atomic-write overhead `<=` an explicit hard bound:**
  `TestInvariant_TemporaryAtomicWriteOverheadHasAnExplicitHardBound`
  names the bound explicitly (69,632 bytes = `fisbcache`'s own
  `maxPersistedPayloadBytes` + framing headroom) and proves
  `EncodePersistedEntry` actually enforces it. The reasoning this bound
  is *itself* the maximum transient overhead is unchanged from the
  prior design: `AtomicWriter.Write` creates a uniquely-named temp file,
  writes and syncs it, then renames it over the final path - the old
  file (if this write is a replacement) and the new temp file can
  coexist on disk for that brief window - and since this feature has
  exactly one writer for its own namespace at a time
  (`fisbCacheCaptureWorker` is a single goroutine, `fisbCacheDiskMu`
  excludes any concurrent eviction from the same window), at most one
  such temp file can ever exist at once, regardless of how large
  `maxCacheBytes` is configured. At the 256 MiB hard cap this is a
  ~0.03% relative overhead; at the 16 MiB default, ~0.4%.
- **Queue payload bytes `<=` an explicit hard bound:**
  `TestInvariant_QueueMemoryHasAnExplicitHardBound` fills a pending
  queue to its own structural capacity (`fisbCachePendingCapacity`, 256
  distinct keys - unchanged in value from the prior design's plain
  channel depth, kept for continuity) with maximum-sized payloads and
  proves the `(N+1)`th distinct key is rejected, never silently
  accepted past `fisbCachePendingCapacity * maxPersistedPayloadBytes`
  (≈16 MiB worst case). **What this bound precisely measures and does
  not measure:** it is the sum of `len(payload)` across every queued and
  in-flight `fisbCaptureItem` (`fisbPendingQueue`'s own `queuedBytes` +
  `inFlightBytes` accounting) - application-retained payload bytes this
  feature can actually, precisely account for. It is deliberately **not**
  a claim about this feature's total Go runtime footprint or process
  RSS: Go's own per-string/per-struct/per-map-entry overhead, `Entry`'s
  non-payload fields, goroutine stacks, and GC bookkeeping are all real
  memory this feature also uses but that this bound neither measures nor
  bounds - reporting them would require runtime introspection
  (`runtime.MemStats` or similar) this feature does not perform, and
  claiming a precise figure for memory this code does not itself track
  would be dishonest. "Queue memory" in earlier revisions of this
  document meant this same payload-bytes quantity; this revision names it
  more precisely to avoid that ambiguity. This bound is deliberately
  **separate** from, and not a substitute for, the disk-overhead bound
  above: items sitting in the pending queue have not yet reached
  `Store.Admit`, so they never count toward committed *disk* bytes at all
  - only toward this payload-bytes bound.

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
never blocking it. Frames are handed off through `fisbCacheEnqueue`'s own
synchronous capacity reservation (`fisbPendingQueue`, structural capacity
256 distinct keys - see "Synchronous admission bounds," above) to a
dedicated worker goroutine; when that structural capacity is exhausted,
or capacity genuinely cannot be freed, the frame is simply dropped/
rejected and counted (`fisbCacheDroppedWrites`/`fisbCacheCapacityRejected`),
never blocking the capture call site. Capacity enforcement itself never
runs on this path either - a candidate that does not fit is rejected and
a cleanup pass is only *requested*, asynchronously, of a separate
dedicated goroutine (see "The asynchronous cleanup worker," above);
`TestFISBCacheEnqueue_NeverPerformsFilesystemIO` proves this path
performs zero filesystem I/O under every outcome. A disabled cache, an
exhausted queue, a corrupt persisted entry, or a cleanup-worker error can
therefore never interrupt live ADS-B/UAT reception or GDL90 forwarding -
the one thing this feature must never be allowed to do.

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
| Pending-reservation queue capacity | Hard safety cap | 256 distinct product keys | `fisbCacheEnqueue` drop-and-count (`droppedWrites`) rather than block once the structural cap is reached - see "Capture path," above, and "Synchronous admission bounds," below, for the separate byte/entry *capacity* rejection (`capacityRejected`). |
| Recovery-scan per-file read bound | Hard safety cap | 131072 bytes (128 KiB) | `fisbReadFileBounded` never reads more than this from any one file during startup recovery, regardless of the file's actual on-disk size - defense in depth beyond the entry-file-size cap above. |
| Purge confirmation token lifetime | Hard safety cap | 5 minutes | An unconfirmed prepare expires; a confirmed one is single-use - see "APIs," below. |
| Inventory/status response size | Bounded indirectly | Equal to the current entry count | Never separately capped beyond `maxEntries` itself - the inventory endpoint returns one summary row per currently-cached entry, and admission itself never grants a reservation that would push the entry count past `maxEntries` (a hard gate at reservation time, before any item occupies a queue slot - see "Synchronous admission bounds," below; the one narrow, deliberate, self-correcting exception is a reservation granted under a since-tightened budget, converging back under the new budget once the asynchronous cleanup worker's next pass runs, not necessarily instantly). |
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

`/getFISBCacheStatus` also reports the reservation model's own state
directly: `queueDepth`/`inFlightEntries` (queued vs. actively
`Admit`-deciding reservations), `reservedBytes`/`reservedEntries` (their
combined size/count), `projectedBytes`/`projectedEntries` (committed +
reserved, combined via `fisbCacheProjectedSnapshotAtomic` - see
"Synchronous admission bounds," above, for why this specific accessor is
used rather than two separate reads), `capacityRejected` (offers rejected
because a candidate did not fit even the projected budget - distinct
from `oversizedRejected`, a single entry that can never fit regardless of
anything else), and `shutdownRejected` (captures rejected because this
feature was already shutting down when they arrived - see "Concurrency,"
above). It also exposes the asynchronous cleanup worker's own activity
directly, rather than leaving it to be inferred: `cleanupRunning` (true
for the duration of one in-progress pass), `cleanupRequested` (every
`fisbCacheRequestCleanup` call, whether or not it triggered a new pass -
see "The asynchronous cleanup worker," above), and `cleanupRuns` (passes
actually executed - `cleanupRequested > cleanupRuns` is expected and
healthy under sustained load; it is coalescing working as designed, not
lost work).

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
  (16 tests): production-level integration tests against a real temp
  filesystem and a real `storagelifecycle.Manager` - storage-pressure
  admission gating across every pressure band; the namespace registers
  exactly once; eviction deletes only its named keys, proven against a
  real directory containing an untouched unrelated file; startup recovery
  skips a real symlink and subdirectory, quarantines a corrupt entry,
  bridges age correctly across a simulated reboot (including an
  already-expired-by-bridged-age entry, removed and never re-admitted),
  its wait for trusted time is genuinely bounded, and its quarantine
  decision survives a concurrent replacement of the exact same product
  (Race 3, see "Concurrency," above); retention deletes from both disk
  and the in-memory index; a queue overflow (a full structural
  distinct-key capacity) drops and counts under overflow without ever
  blocking; an oversized single entry is rejected before it is ever
  queued or admitted, while an exactly-at-budget one is accepted; a stale
  eviction plan is refused even after the same key has been concurrently
  re-persisted (both a deterministic single-goroutine reproduction and a
  genuine concurrent `-race` test); startup recovery and a tightened
  settings request both a prompt cleanup pass (never enforce inline) that
  converges committed state to the new budget once it runs; a deletion
  failure is never counted as a success and leaves the Store untouched.
- `main/fisbcachereserve_test.go` (16 tests): the reservation model in
  isolation - `fisbCacheReservationFits`'s full decision matrix (fits
  within budget; rejects over byte budget without touching committed
  state; rejects over entry budget; a reserved-but-not-yet-committed
  value counts toward the budget; `maxBytes`/`maxEntries`\<=0 both mean
  "no limit," matching `PlanEviction`'s own convention - never evicts,
  by construction, since this function performs no eviction at all, only
  a pure in-memory fits-check);
  `fisbCacheProjectedTotals`'s own reserved-wins-over-stale-committed
  merge rule; `fisbPendingQueue`'s coalescing (a second reservation for
  an already-queued key supersedes, never adds), structural capacity
  (the `(N+1)`th distinct key rejected, a supersede of an
  already-reserved key still succeeds at full capacity), FIFO pop order
  and the queued-to-in-flight transition, `releaseInFlight` fully ending
  a reservation, and `clear` discarding queued-but-not-in-flight
  reservations (purge's own use); a full end-to-end pass through the
  REAL `fisbCacheCaptureWorker` goroutine (`fisbCacheEnqueue` through
  `Store.Admit` through `fisbCachePersist`); a rejected (older-copy)
  admission releasing its reservation and persisting nothing; the
  confirmed-purge handler's own per-key deletes
  (`fisbCachePurgeUsingSnapshot`) refusing to destroy a concurrently
  superseded key, and clearing queued-but-not-in-flight reservations;
  and a reservation accepted under a looser budget self-correcting to a
  since-tightened one once a cleanup pass actually runs (the settings
  handler itself only requests that pass - see "Settings-limit
  reductions," above).
- `main/fisbcachecapturepath_test.go` (8 tests): Phase 2/6's explicit
  regression and fault-injection requirements. `fisbFaultFS` decorates a
  REAL `storagelifecycle.FS` (a real temp directory) with per-call fault
  injection, rather than reimplementing a fake in-memory filesystem, so
  an injected short write genuinely leaves a genuinely truncated file for
  the real validate-before-rename step to genuinely catch.
  `TestFISBCacheEnqueue_NeverPerformsFilesystemIO` drives admits, a
  same-key supersession, and a capacity rejection directly through
  `fisbCaptureText`/`fisbCaptureNexrad` against an FS stub that fails the
  test on ANY call, proving the live capture path performs zero
  filesystem I/O for every reservation outcome. Five tests fault-inject
  each step of the write path (write failure, a short write that lies
  about succeeding, fsync failure, rename failure, directory-sync
  failure) against the real `fisbCachePersist`, proving each is reported
  honestly - no partial/corrupt file ever left in place, except the one
  documented exception (a directory-sync failure is reported as an error
  even though the rename that preceded it already succeeded, so the file
  genuinely is written). One proves a deletion failure is never counted
  as a success and leaves Store accounting conservative. One proves
  `fisbCacheEnqueue` rejects and counts every capture during shutdown,
  queuing nothing. "Cancellation" and "supersession"/"reservation
  release" are deliberately not duplicated here -
  `TestFISBCachePurge_CancelInvalidatesToken`
  (`main/fisbcacheapi_test.go`, see its own updated doc comment) and
  `main/fisbcachereserve_test.go`'s existing coalescing/release tests
  already cover them precisely; this feature's write pipeline has no
  other real, reachable cancellation path (`ReplaceProduct` hardcodes
  `context.Background()`) to fault-inject against.
- `main/fisbcacheinvariants_test.go` (5 tests): the six required
  invariants from "Synchronous admission bounds," above, each proven
  directly and explicitly - committed bytes/entries never exceed budget
  across a real burst through the actual pipeline, cross-checked against
  what is actually on disk; projected bytes/entries never exceed budget
  mid-burst, both deterministically (single-threaded, checked after
  every enqueue call) and under genuine concurrent load
  (`go test -race`, via `fisbCacheProjectedSnapshotAtomic`); the
  temporary atomic-write overhead's exact hard bound (69,632 bytes),
  proven enforced, not merely documented; and the pending-queue's own
  application-retained payload-bytes hard bound (explicitly NOT a claim
  about Go runtime/process RSS - see that invariant's own bullet, above),
  proven by filling it to structural capacity with maximum-sized payloads
  and confirming the next distinct key is rejected.
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
- (Resolved - see "Concurrency: the exact races this closes," above)
  Startup recovery's quarantine-delete decision used to carry a narrow,
  documented, self-healing race against a concurrent live write for the
  exact same product in the same narrow startup window; this is now
  fully closed (`fisbCacheQuarantineIfStillWarranted`), not merely
  narrowed or documented as acceptable.

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
