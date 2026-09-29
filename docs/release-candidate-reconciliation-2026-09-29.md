# Release candidate reconciliation — 2026-09-29

> **Status: integration candidate prepared and CI-verified. NOT deployed, NOT
> physically tested, NOT merged.** Do not treat this document as acceptance
> evidence for the FIS-B cache or viewer - their own field-acceptance gates
> (below) are unchanged and still owner-gated. See PR #51 (draft, not for
> merge) for the candidate itself.

## Why this document exists

The device's grounded test Pi was last verified running build `2002ad4e`,
which has a working FIS-B rolling weather cache (12 persisted entries on
disk, a working `/getFISBCacheStatus` API) and the native FIS-B Weather Web
UI. During PR #48's physical acceptance (issue #43), deploying a build from
current `master` made `/getFISBCacheStatus` return 404: **`master` does not
contain the FIS-B cache feature at all** - it only exists on two open,
unmerged PRs. This document reconciles that gap before any further device
deployment is attempted.

## Verified starting state (this session)

| | Verified value | How verified |
|---|---|---|
| `origin/master` | `3b3aca523878c0f926adf6991133ae0fd7ec060c` | `git fetch` + `rev-parse`, matches PR #47's merge commit |
| Device installed build | `2002ad4e8294e1b475d6ca9ee3f838971f35a119` | Live `getStatus` + SSH `strings stratuxrun` on the device, this session |
| PR #15 head | `b316e64c474a4d31ea5697df14ba616000aba0f5` | `gh pr view 15` |
| PR #41 head | `2002ad4e8294e1b475d6ca9ee3f838971f35a119` | `gh pr view 41` — **exactly equal to the device's installed build**, confirmed by direct SHA comparison, not inferred from the `Build` string alone |
| Main checkout (`/home/ddavis/git/stratux`) | dirty, `feature/fisb-weather-cache` @ `bf9f74be`, 158 behind `origin/feature/fisb-weather-cache` | preserved untouched throughout; confirmed `bf9f74be` is an **ancestor** of PR #15's current head (stale, not ahead) |

**Build `2002ad4e` traced to its exact source commit** (not inferred): the
worktree `/home/ddavis/git/stratux-fisb-weather-viewer` is checked out
exactly at `2002ad4e8294e1b475d6ca9ee3f838971f35a119`, clean, on branch
`feature/fisb-weather-viewer` (PR #41's own branch). PR #15's head
(`b316e64c`) is confirmed an ancestor of it (`git merge-base --is-ancestor`).
So: **device build 2002ad4e = PR #41's exact head = PR #15's cache work plus
the native weather viewer on top.**

## Reconciliation: exact comparison

### Divergence size

| | Since common ancestor `9af2e404` (PR #40 merge) |
|---|---|
| `origin/master` | 17 commits, 42 files (+1726/-415) |
| `feature/fisb-weather-viewer` (= device build) | 47 commits, 89 files (+14992/-412) |
| Files touched by **both** sides | exactly 2: `main/managementinterface.go`, `web/js/main.js` |

### What deploying current `master` unchanged would remove

Confirmed by direct route-table diff (`grep HandleFunc`), not inference:

| Route | On `master`? | On device (`2002ad4e`)? |
|---|---|---|
| `/weather` (live-only text tail) | yes | yes (overhauled by PR #41) |
| `/getFISBCacheStatus` | **no** | yes |
| `/getFISBCacheInventory` | **no** | yes |
| `/getFISBCachePayload` | **no** | yes |
| `/getFISBCacheSettings` / `/setFISBCacheSettings` | **no** | yes |
| `/prepareFISBCachePurge` / `/confirmFISBCachePurge` / `/cancelFISBCachePurge` | **no** | yes |

The entire `fisbcache` Go package (10 files: `entry.go`, `schema.go`,
`store.go`, `policy.go`, `retention.go`, `checksum.go`, `nexrad.go`,
`product.go`, `state.go`, `time.go`) **does not exist on `master`** (`grep
-rl fisbcache` returns nothing). Web assets `web/plates/fisbcache.html`,
`web/plates/js/fisbcache.js`, and the overhauled `web/plates/weather.html` /
`weatherlogic.js` are likewise absent from `master`.

**Conclusion: yes, installing current `master` unchanged would remove an
API** (all eight routes above), and would leave the device's 12 already-
persisted cache files on disk inaccessible (the code that reads them
wouldn't be running) — though the files themselves would not be deleted
(`/var/lib/stratux-data` is untouched by any package install/removal). No
persisted-data *format* change is involved in that direction, only loss of
the code that serves it.

### Persisted schema / migration risk

`fisbcache/schema.go`: `SchemaVersion = 1`, "There is no earlier version:
this is the first release of this cache." The device's own persisted
`fisbcache-settings.json` reports `"schemaVersion": 1`, matching. The
candidate (below) carries `fisbcache/schema.go` and `fisbcache/entry.go`
**byte-identical** to `feature/fisb-weather-viewer` (`git diff` = 0 lines) -
confirmed diff, not assumption. **No migration is required or implemented
by this reconciliation**; none was needed.

### Feature/commit matrix

| Feature | Already on `master`? | Only on device (2002ad4e)? | Only in a PR? | Status |
|---|---|---|---|---|
| Power-control consolidation | yes (PR #42) | — | — | merged |
| Legible shutdown splash | yes (PR #48) | — | — | merged |
| Dashboard golden-image fix | yes (PR #46) | — | — | merged |
| epaper/epaper_main CI coverage | yes (PR #47) | — | — | merged |
| FIS-B rolling weather cache (`fisbcache` package, 8 routes) | no | yes | PR #15 (head `b316e64c`) | **open, live-RF/ForeFlight acceptance pending** |
| Native FIS-B Weather Web UI viewer | no | yes | PR #41 (head `2002ad4e`, = device build) | **open, live-RF/ForeFlight acceptance pending** |
| `/getFISBCachePayload` readback endpoint | no | yes | PR #41 (built on PR #15) | same as above |

### Other differences noted, not integrated

- PR #15's own body records two known, unresolved limitations independent of
  this reconciliation: `STRATUX_FISB_GDL90_REPLAY_DESIGN_REVIEW_REQUIRED`
  (GDL90 replay intentionally not implemented) and
  `STRATUX_CONFIGURATION_BACKUP_EPAPER_SELF_VALIDATION_DEFECT_REVIEW_REQUIRED`
  (the device's own Configuration Backup fails validation because e-paper is
  enabled with `EpaperRefreshIntervalSeconds` unset while current `master`'s
  validator requires >= 5). Neither is touched or fixed here - out of scope
  for this reconciliation, flagged for the owner's own backlog.
- Package builds are not byte-reproducible (PR #15's own noted limitation,
  `STRATUX_PACKAGE_BUILD_REPRODUCIBILITY_REVIEW_REQUIRED`) - unrelated to
  this integration, carried forward unchanged.

## Candidate preparation

**Integration method:** `git merge --no-ff origin/feature/fisb-weather-viewer`
onto a new branch (`integrate/fisb-onto-master`) created from current
`origin/master`. No rebase, no cherry-pick, no manual file copying from the
device. Both overlapping files auto-merged with **zero conflicts** (`git
merge` reported "Automatic merge went well"), reviewed by hand afterward:
each additively registers its own routes/URLs alongside the other's.

**Candidate branch:** `integrate/fisb-onto-master`
**Candidate commit (merge):** `cdb4d798fec721e0a6cd5cb1543d442ac4893eb4`
**Parents:** `3b3aca52` (master) + `2002ad4e` (feature/fisb-weather-viewer, PR #41 head)
**Draft PR (review/CI only, not for merge):** [#51](https://github.com/ddavis83864/stratux/pull/51)

**Changed vs. `master`:** the full `feature/fisb-weather-viewer` diff (89
files, +14992/-412) plus the two auto-merged files. **Changed vs.
`feature/fisb-weather-viewer`:** nothing in `fisbcache/` or `configbackup/`
(verified 0-line diff); only `main/managementinterface.go` and
`web/js/main.js` differ, and only additively (master's own routes/states
folded in alongside).

## Candidate package identity

| | Value |
|---|---|
| Source commit | `cdb4d798fec721e0a6cd5cb1543d442ac4893eb4` |
| Package | `stratux-2.0.0~rc2-arm64.deb`, 86,904,144 bytes |
| Package SHA-256 | `1b0a8b432d68cea82a33bcebd5885bd5ea13871b3b5da19b43337961626d8f19` |
| Embedded build string (`stratuxrun`) | `cdb4d798fec721e0a6cd5cb1543d442ac4893eb4` - confirmed exact match to source commit |
| Build pipeline | `make dpkg` (the repository's normal pipeline, matching CI's `build-artifact` job exactly), via `docker compose run cli` |
| Web/display assets confirmed present | `web/plates/fisbcache.html`, `web/plates/js/fisbcache.js`, `web/plates/weather.html`, `web/plates/js/weatherlogic.js`; `lib/systemd/system/stratux_epaper_shutdown.service`; `stratuxrun` binary contains `main.handleGetFISBCacheStatusRequest` and the `/getFISBCacheStatus` route string; `epaperd` binary contains the shutdown-splash strings |

## Tests and CI

| Suite | Platform | Result |
|---|---|---|
| `go build`/`go vet` — `epaper`, `epaper_main`, `fisbcache`, `configbackup`, `sdrassign`, `power`, `ota` | arm64 (Docker `--platform linux/arm64`, matching CI's `ubuntu-24.04-arm`) | clean |
| `go test` — same packages | arm64 | **all pass**, including `TestDashboardGoldenImages` all 12 subtests, and `fisbcache`'s own suite (34.7s - persistence, freshness, retention, checksum, mutation-style coverage) |
| `go test` — same packages | amd64 (local host) | `06-fisb-stale`/`12-worst-case-text` fail — **confirmed pre-existing, environment-only** (identical failure reproduces on unmodified `master` and on unmodified `feature/fisb-weather-viewer` under amd64; passes on both under arm64 - a real amd64-vs-arm64 icon anti-aliasing rounding difference, not a regression from this integration) |
| `go test ./main/... ./fisbcache/...` (cgo, via Docker) | arm64 | `ok` for both, including `fisbcache` (cached) |
| Package ownership: static | — | 15/15 PASS |
| Package ownership: archive | — | 9/9 PASS |
| Package ownership: lifecycle (install/upgrade/OTA/idempotency/rollback/downgrade/removal/purge/hostile-input) | — | **72/72 PASS** |
| Real CI on the candidate branch/PR #51 | GitHub Actions, `ubuntu-24.04-arm` | **`build-artifact` PASS, `integration-test` PASS** — run [`36514479486`](https://github.com/ddavis83864/stratux/actions/runs/36514479486) |

No migration test was needed or run: the persisted schema is unchanged
(verified above), so there is nothing to migrate. No experiment was run
against the device's live cache; all schema/format verification was by
source diff, not by touching data.

## PR #15 acceptance status (verified this session, not assumed)

**Final candidate lab-validated and targeted-physical-verified on the bench
Pi; ready for review; NOT merged.** Explicit open gate, in the PR's own
words: *"Live FIS-B reception, the ForeFlight session and the
reconnect-with-populated-cache test are still pending (final live
acceptance)."* Implementation head `b26686c7` (current branch head `b316e64c`
adds only documentation on top - confirmed by the PR body's own statement,
not re-derived). CI on PR #15's own head: `integration-test` SUCCESS,
`build-artifact` SUCCESS.

## PR #41 acceptance status

**Implemented, unit/handler-tested, NOT merged. Live-RF and ForeFlight
verification not performed.** CI on PR #41's own head (= device build
`2002ad4e`): both checks SUCCESS.

**Neither gate is satisfied by this reconciliation.** Nothing here performs
or substitutes for live FIS-B reception or a ForeFlight session.

## Issue #49 disposition for deployment

**Not a release blocker.** Reviewed both the issue's own evidence and the
current OTA code (`ota/state.go`, `ota/decide.go`, `main/ota.go`):

- `requestOverlayDisable()`'s failure path correctly enters `StageFailed`,
  which the state machine's own `Decide` function handles with a **bounded,
  automatic recovery/rollback loop** (`MaxRecoveryAttempts`,
  `RecoveryBackoff`) - not a design the device can get stuck in indefinitely.
- The one observed instance resolved exactly as designed: automatic rollback
  to a clean, healthy, unmodified state (`dpkg --audit` clean, 0 failed
  units, device remained on its prior build) - confirmed in the issue's own
  evidence, re-read this session.
- A manual reproduction of the exact `overlayctl unlock` → write → `lock`
  sequence immediately afterward succeeded without issue, and an unmodified
  retry of the same `/updateUpload` request succeeded cleanly end to end.
- This is unrelated to the FIS-B integration or to PR #48 - `main/ota.go`
  and `ota/` are untouched by both.

**Conclusion: the candidate can be deployed with the existing rollback
controls as-is.** No OTA update was started in this task to further
investigate #49 (per instruction). Recommendation carried into the runbook
below: if a first OTA attempt reports this specific rolled-back error, wait
for the state to settle and retry once before treating it as a real
problem; escalate only if it recurs.

Issue #50 (one-off boot-splash BUSY timeout) remains separately classified,
not touched here. The deferred undervoltage investigation was not resumed.

## OTA readiness / deployment runbook (for a later, separately authorized owner-attended session)

**Not performed in this task.** For when the owner is ready:

1. **Preflight** (remote, no owner presence required): confirm device
   reachable, `getOTAStatus.Stage == "idle"`, `dpkg --audit` clean, 0 failed
   units, current build, `EpaperEnabled`/panel/rotation settings, FIS-B
   cache API/file baseline (`getFISBCacheStatus`, file count under
   `/var/lib/stratux-data/fisb-weather-cache`) - all exactly as this
   session's own evidence file already captures a template for.
2. **Verified backup/rollback ready**: the known-good `2002ad4e` `.deb` (already
   on hand from the PR #48 session, hash-verified) stays the rollback
   target; re-verify its hash immediately before use.
3. **Candidate identity check**: re-verify the candidate `.deb`'s SHA-256 and
   embedded build string match this document's table, immediately before
   upload - rebuild if the branch has moved.
4. **OTA operation** (owner should be present, per this project's established
   pattern for any real OTA against the grounded device): `POST
   /updateUpload`; poll `getOTAStatus` to `idle`; if it reports
   `rolled_back` with the issue #49 error, wait ~30s and retry once before
   escalating.
5. **Post-reboot health** (owner presence not required for this step alone):
   new boot ID, `dpkg --audit` clean, 0 failed units, `getStatus.Build`
   matches the candidate's embedded string exactly.
6. **FIS-B cache/API/data preservation** (owner presence not required):
   `/getFISBCacheStatus` returns 200 (not 404); persisted file count under
   `fisb-weather-cache` unchanged from the preflight baseline; settings
   (`fisbcache-settings.json`) unchanged.
7. **Power and e-paper checks** (owner presence not required for the
   non-destructive checks; **owner must be present** for anything that
   triggers a real shutdown): `getPowerHealth`, e-paper `getHealth` READY/
   RUNNING, panel detected, 0 consecutive failures; boot-splash-to-dashboard
   handoff observed clean.
8. **Failure stop conditions**: any of - candidate hash/build mismatch after
   install; FIS-B API still 404 after a healthy-looking install; cache file
   count dropped; any failed unit; `dpkg --audit` dirty; OTA stuck outside
   `idle`/expected transitional stages for more than a few minutes. On any
   of these: stop, preserve evidence, roll back.
9. **Rollback verification**: re-install the verified `2002ad4e` `.deb`
   through the same supported path; confirm build string, boot ID, 0 failed
   units, clean audit, and FIS-B cache API/file count match the original
   preflight baseline exactly.

**Owner physically present, specifically:** any step that triggers a real
shutdown/power-off test (this document's own runbook doesn't include one -
that would be a distinct, separately authorized acceptance run, as with
issue #43's own PR #48 acceptance); and, as this project's established
practice, present for the OTA operation itself even though it's remotely
triggerable, so a stuck/bricked state can be caught and physically
recovered without delay.

## READY / NOT READY classification

**READY for a controlled test-device deployment/acceptance run**, subject to
the owner's own scheduling and the runbook above - the candidate builds
cleanly from the repository's normal pipeline, passes its full test suite
(including package lifecycle/rollback tests) and real CI on arm64, carries
no code or schema changes to the already-field-validated FIS-B cache, and
issue #49 is assessed as non-blocking.

**NOT READY, and not to be labeled production-ready or physically
validated**, for anything beyond that: PR #15's and PR #41's own live-RF/
ForeFlight field-acceptance gates remain **unperformed** - this reconciliation
does not touch, advance, or substitute for either. **Not merged, not
deployed, not flight-device-ready.**

## Confirmation

The test device was not touched during this task: still running
`2002ad4e8294e1b475d6ca9ee3f838971f35a119` (re-verified live, this session),
its persisted FIS-B cache files (12 entries, `fisb-weather-cache/`) and
`fisbcache-settings.json` untouched (read-only evidence capture only). The
owner's dirty main checkout (`/home/ddavis/git/stratux`, branch
`feature/fisb-weather-cache`) and all unrelated worktrees were left exactly
as found.
