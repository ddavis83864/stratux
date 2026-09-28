# FIS-B Weather Viewer (Web UI)

## Purpose

A native page in the Stratux Web UI (**Weather**, at `/#/weather`) for inspecting the FIS-B weather and aeronautical
products this receiver has actually received over 978 MHz - without ForeFlight and without any Internet
connectivity. It is a local, offline receiver-diagnostics/situational-awareness view, **not a replacement for
ForeFlight** and not a substitute for an official preflight weather briefing.

Product architecture this page fits into:

- **ForeFlight** - primary cockpit/navigation/weather presentation.
- **Stratux e-paper display** - receiver health and high-level operational status (unchanged by this feature - see
  `docs/epaper-operating-dashboard.md`; this page does not add weather detail to the e-paper display).
- **Stratux Web UI's Weather page (this feature)** - detailed, local inspection of received FIS-B products.

## Access

Connect to the Stratux Wi-Fi network (no Internet required), open the Web UI, and select **Weather** from the left
navigation. This overhauls the page that previously existed at the same name/URL (a live-only text tail with no
history - see "Relationship to the previous Weather page" below); no new navigation entry was added.

## What this page actually shows (repository-verified)

This codebase decodes and retains exactly these FIS-B product types - never more, regardless of what a page might
imply:

| Category (this page's tab) | Decoded from | Content shown | Notes |
| --- | --- | --- | --- |
| METAR/SPECI | text reports, leading token METAR/SPECI | raw text, station, times, age, freshness | |
| TAF | text reports, leading token TAF/TAF.AMD | raw text, station, times, age, freshness | |
| PIREP | text reports, leading token PIREP | raw text, station, times, age, freshness | |
| Winds/Temps Aloft | text reports, leading token WINDS | raw text, station, times, age, freshness | |
| NEXRAD | product IDs 63/64 (radar tiles) | region (lat/lon/height/width), scale, timestamps, age, freshness, size; an optional decoded-intensity preview | No image format exists anywhere in this codebase - see "NEXRAD" below |
| Other | any other decoded text report | raw text, times, age | cached, but with no specific freshness policy (`UNSUPPORTED`) |
| NOTAM/SIGMET (tab) | reception counters only | a bare frame count | **content is never decoded or retained anywhere in this codebase** |

**Not supported at all, anywhere in this codebase, and not claimed by this page**: AIRMET content (the decoder,
`uatparse.decodeAirmet`, exists but its call site is commented out - dead code), lightning, turbulence, and icing
products. There is no counter or content for any of these three - they do not appear on this page at all, rather
than being shown as an empty/zero state that would misleadingly imply a receiver-side signal exists.

## Freshness and staleness

Two independent freshness concepts are shown, deliberately not conflated:

1. **Per-product freshness** (the badge on each row: `LIVE`/`CACHED_FRESH`/`CACHED_AGING`/`STALE`/`EXPIRED`/
   `UNSUPPORTED`) - computed server-side by the rolling FIS-B cache (`fisbcache.Freshness`, the exact same function
   `/getFISBCacheInventory` already uses), from `EffectiveAge` (the older/more conservative of how long ago the
   product was received and how old the product's own reconstructed source time is), against per-product-type
   thresholds already defined in `fisbcache/policy.go`. A `LIVE`-badged row is one this browser observed arrive just
   now, live, and has not yet appeared in a cache poll.
2. **Receiver-level freshness** (`WX RX RECENT`/`WX RX AGING`/`WX RX STALE`/`NO WX FRAMES YET`, at the top of the
   page) - time since the newest weather-product *frame* was decoded, regardless of any individual product's own
   age. This intentionally reuses the exact same concept, counters, and 5-minute/15-minute thresholds as the
   e-paper dashboard's own `WX RX` tile (`epaper/dashboard_derive.go`'s `DefaultThresholds` and `ProductTotals.
   weather()`), computed independently in this page's own JS (`web/plates/js/weatherlogic.js`'s
   `receiverFreshnessLabel`/`sumWeatherCounters`) because that Go logic lives in an unexported method not callable
   across the process boundary - the underlying counters, formula, and thresholds are the same, so this page and the
   e-paper display can never disagree about what "recent" means.

"Received N minutes ago" is a factual, receiver-side statement. It is never automatically presented as "still valid
for N more minutes" - this page has no aviation validity-rule engine and does not invent one; where the rolling
cache's own per-product-type thresholds already exist, they are shown as-is (see the table above), and nothing
beyond that is claimed.

## Relationship to the rolling FIS-B weather cache

This page is a consumer of the existing rolling cache (`fisbcache/` package, `main/fisbcache*.go` - see
`docs/fisb-weather-cache.md`), not a second cache:

- **Metadata** (identity, freshness, age, size) comes from the existing, unmodified `/getFISBCacheStatus` and
  `/getFISBCacheInventory` endpoints, polled every 3 seconds (the same cadence the existing Weather Cache diagnostics
  page already uses).
- **Raw content** for a product that arrived before this page was open is *not* available from those endpoints by
  design (`/getFISBCacheInventory`'s own doc comment: "never returns raw payload content"). This page adds exactly
  one new, read-only endpoint for that - `GET /getFISBCachePayload?class=<class>&identity=<identity>`
  (`main/fisbcachepayload.go`) - a best-effort readback of that entry's already-persisted file, using the cache's own
  existing filename derivation and strict decoder. It only ever works when the cache's own **persistence** setting is
  on (Weather Cache page); with persistence off, a product's raw content is only ever available while this page was
  open and connected to the live feed when it arrived. Nothing about the cache's own admission, eviction,
  persistence, or budget behavior was changed to support this - it is a pure readback of what was already there.
- **Live-only operation**: this page also works with the rolling cache entirely **disabled** (the default) - the
  live `/weather` text feed (unchanged, pre-existing) still shows freshly-arriving METAR/TAF/PIREP/WINDS reports;
  only history/persistence/NEXRAD-cache-metadata require the cache to be enabled.
- The Web UI's own browser-side state is explicitly bounded (a capped "recently received" ticker,
  `boundedUpsert` in `weatherlogic.js`) so leaving this page open indefinitely cannot grow browser memory without
  bound, independent of the server-side cache's own (separately enforced) budget.

## NEXRAD

There is no raster image format anywhere in this codebase for a NEXRAD tile - `uatparse.decodeNexradFrame` produces
only a flat array of 4-bit intensity levels (0-15) per bin plus the tile's geographic bounding rectangle
(`lat/lon/height/width`) and scale; `fisbcache` stores that array base64-encoded as its `Payload` string. This page
always shows the tile's metadata (region, scale, timestamps, age, freshness, size) regardless of persistence
settings. When persistence is on, an optional "Preview tile" button decodes the intensity array (via
`/getFISBCachePayload`) and draws a plain per-bin color ramp onto an HTML canvas, entirely client-side, with no
network map/basemap dependency. **The canvas arranges the tile's bins into an arbitrary square purely to give a
compact, at-a-glance look at intensity/density - it is not a georeferenced reconstruction of the tile's real shape,
size, or orientation.** This codebase's own NEXRAD decoder (`uatparse.decodeNexradFrame`) never reports a row/column
layout for a tile, only a flat bin list plus the tile's geographic bounding rectangle - so no faithful spatial
rendering is possible without decoding a layout this codebase does not expose; the region shown in the metadata
table (not the preview image) is the only geographic information this page ever claims. This whole preview is
explicitly a bonus, not a requirement - the metadata view alone satisfies this page's minimum NEXRAD requirement,
and no live-broadcast NEXRAD feed was added (the only broadcast channel for raw uplink frames, `/jsonio`, is
shared/multiplexed with traffic/radar/situation data and was deliberately not adopted here to avoid a heavier,
fragile integration for a bonus feature).

## Offline operation

Every data source this page uses (`/getFISBCacheStatus`, `/getFISBCacheInventory`, `/getFISBCachePayload`,
`/weather`, `/status`, `/getTowers`) is served directly by the Stratux daemon over the Wi-Fi network it provides;
none of them, nor anything added for this page, ever calls out to the Internet. The page loads no external
scripts, fonts, or map tiles.

## Relationship to ForeFlight / GDL90

This page is a read-only, additional consumer of already-existing data. It does not change the live UAT/GDL90
receive-and-relay path, the rolling cache's admission/eviction/persistence logic, or the (still intentionally
unimplemented) GDL90 replay design. Verified after implementation: the live GDL90 relay path
(`main/gen_gdl90.go`'s `relayMessage`) has no reference to this page's new endpoint or to the rolling cache beyond
the capture call site it already had; a Web UI client reading this page cannot affect what is sent to ForeFlight or
any other GDL90 client.

**Not performed as part of this change** (see the PR's own final report for exact status): a live-RF field session
and a ForeFlight session observed side-by-side with this page. Bench/unit-level verification (Go handler tests,
`weatherlogic.js` unit tests) is not a substitute for that and is not presented as one.

## Relationship to the previous Weather page

The page previously at this same name/URL was a live-only tail of the `/weather` websocket (no cache integration, no
history, text products only, no NEXRAD/NOTAM/SIGMET representation at all). It has been overhauled in place rather
than left alongside a second, confusingly-similarly-named page; its original live-feed behavior (the `/weather`
websocket itself, and the server-side code that feeds it) is unchanged and is still exactly what this page's live
tab content is built on.

## Limitations

- NOTAM and SIGMET/AIRMET: reception-counter only, never decoded content, anywhere in this codebase today.
- Lightning, turbulence, icing: not decoded, not counted, not shown.
- NEXRAD: metadata and an optional non-calibrated intensity preview only - never a georeferenced map overlay.
- Raw content for a product received before this page was open requires the rolling cache's persistence setting to
  be on; with it off, only live-observed-this-session content is ever recoverable.
- "Other" (a decoded text report whose leading token isn't one of the six known types) is cached with an
  `UNSUPPORTED` freshness classification - shown, but with no specific staleness threshold applied to it.

## Field-test procedure (next live 978 MHz session)

Automated (Go handler tests, `weatherlogic.js` unit tests) and bench/lab verification are not physical acceptance.
Separately record:

1. **Automated** - `go test ./main/... -run FISBCachePayload` (or the relevant subset) and
   `node --test test/weatherlogic_test.js` both pass.
2. **Bench** - with the daemon running (real or simulated hardware), the Weather page loads, shows the correct
   no-data state before any reception, and its category counts/badges update immediately as synthetic/replayed
   entries are admitted to the cache (lab-only, never injected into the live daemon/production network - see
   `docs/fisb-weather-cache.md`'s own replay-isolation notes for why no live-frame injection is ever appropriate).
3. **Live-RF** (owner-performed, in range of a 978 MHz ground station):
   - Stratux boots normally; Web UI and Weather page load; before reception, the page shows `NO WX FRAMES YET`.
   - As UAT tower reception begins, `Active towers` and the receiver-freshness badge change appropriately.
   - Product counters/tabs populate as METAR/TAF/PIREP/WINDS/NEXRAD products arrive; the NOTAM/SIGMET tab's counters
     move independently (content still never shown).
   - Timestamps/ages advance correctly; a product's badge moves fresh -> aging -> stale as expected for its type.
   - After reception stops, previously-cached entries remain visible with a visibly aging/stale badge - never
     silently removed.
   - Concurrently confirm (recorded separately, never conflated with the above): ForeFlight traffic is unaffected;
     ForeFlight's own weather behavior (observed on the EFB itself, not inferred from this page); the e-paper
     dashboard remains operational; no material CPU/memory regression on the Pi.
4. **ForeFlight** (owner-performed, separately recorded) - a live ForeFlight session must be observed directly; this
   page's own live-feed connection state (top of page) is not evidence of what ForeFlight received or displayed.

Do not report step 3 or 4 as passed unless actually, physically performed.
