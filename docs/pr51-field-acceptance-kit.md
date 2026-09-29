# PR #51 field acceptance kit

> **Status: prepared, not run.** This is a checklist and bounded capture
> tooling for a later, separately authorized owner-attended acceptance
> session. No gate below has been executed by this document's own
> preparation. Do not treat any row as PASS until it has actually been
> observed on the device.

Candidate: branch `integrate/fisb-onto-master`, code-bearing commit
**`cdb4d798fec721e0a6cd5cb1543d442ac4893eb4`** (a later commit,
`8f6f3041` and beyond, adds only documentation - see
[`release-candidate-reconciliation-2026-09-29.md`](release-candidate-reconciliation-2026-09-29.md)
for the full reconciliation record and why this candidate exists: current
`master` alone is missing the FIS-B rolling weather cache, PR #15, and its
native Web UI viewer, PR #41 - both accepted in the lab/bench but still
gated on live-RF/ForeFlight field acceptance). Draft PR:
[#51](https://github.com/ddavis83864/stratux/pull/51).

This kit follows the same gate-table and evidence conventions already
established by this project's prior field sessions -
[`docs/fisb-weather-cache.md`](fisb-weather-cache.md)'s "Field session,
2026-09-27" section and `~/acceptance-evidence/stratux-fisb-live-kit/RUNBOOK.md`
- adapted to PR #51's specific candidate. It does not replace either; it
is the next session's own instance of the same pattern.

## Result classification (used throughout)

- **PASS** - directly observed, evidence captured.
- **FAIL** - directly observed and did not meet the criterion; evidence
  captured; treat as a stop condition (see below) unless explicitly
  overridden by the owner in the moment, with that override recorded.
- **NOT RUN** - not attempted this session (e.g. no RF reception, no
  ForeFlight available, owner not present for a step that requires them).
- **INCONCLUSIVE** - attempted, but the evidence does not cleanly support
  PASS or FAIL (e.g. ambiguous timing, a tool failure unrelated to the
  candidate). Record what was seen and why it doesn't resolve either way;
  never silently upgrade to PASS.

Every gate below states its own pass/fail criterion and where its
evidence goes. Gates marked **[OWNER]** need the owner physically present
at the device. Gates marked **[978 MHz]** need real 978 MHz UAT reception
in range of a ground station - they cannot be forced or simulated live
(see `docs/fisb-weather-cache.md`'s own explicit non-goal: no live-frame
injection into the daemon, ever).

## Evidence directory

Same pattern as the existing kit:

```bash
export EV=~/acceptance-evidence/stratux-pr51-field-$(date -u +%F)
mkdir -p -m 700 "$EV"/{pre,deploy,live,foreflight,post}
```

Everything below writes under `$EV`. Nothing here is committed to the
repository; it is durable local evidence, referenced by path and hash in
the closing report the way every prior session's evidence has been.

## Connectivity

The field laptop joins the Stratux Wi-Fi AP directly - confirmed from the
existing field kit (`RUNBOOK.md`): `nmcli con up Stratux`. The AP has **no
internet route**; this laptop's other network profile (home/hotspot) is
what internet access (GitHub, this repository, `gh`) comes from, and the
two are mutually exclusive on one Wi-Fi radio. In practice, this session's
own preparation repeatedly needed both:

```bash
nmcli con up Stratux    # reach the device at 192.168.10.1
nmcli con up MQ95B       # (or the actual home/hotspot profile name) for internet
```

A repeated flake already observed this session and in prior ones: the
laptop can silently roam off the Stratux AP back to the last-known network
during/after a device reboot. After any device reboot, re-run
`nmcli con up Stratux` before assuming a connectivity failure is real. All
device-facing commands below use a short `curl -m` timeout precisely so a
stale/roamed connection fails fast and visibly rather than hanging.

Evidence capture that needs internet (pushing to GitHub, updating the
issue/PR) is not part of the live session itself - do it afterward, back
on the home/hotspot profile, exactly as this session's own work did.

## Before deployment

Run `test/pr51_field_preflight_capture.sh` (added by this kit; read-only,
never touches OTA state, settings, or the cache - see its own header) to
capture the full baseline in one pass, or do each check individually
below. Either way, confirm every row before proceeding.

| # | Check | Pass criterion | Evidence |
|---|---|---|---|
| B1 | Candidate commit | Locally built package's embedded build string = `cdb4d798fec721e0a6cd5cb1543d442ac4893eb4` | `strings stratuxrun \| grep -E '^[0-9a-f]{40}$'` on the locally built binary, `$EV/pre/candidate-build-string.txt` |
| B2 | Candidate package hash | SHA-256 = `1b0a8b432d68cea82a33bcebd5885bd5ea13871b3b5da19b43337961626d8f19` (rebuild via `make dpkg` if it doesn't match - do not deploy a mismatched artifact) | `sha256sum stratux-2.0.0~rc2-arm64.deb`, `$EV/pre/candidate.sha256` |
| B3 | Device build (before) | Matches the last-known-good build recorded in this session's report | `getStatus.Build`, `$EV/pre/getStatus.json` |
| B4 | Device boot ID | Recorded for later before/after comparison | `ssh pi@192.168.10.1 cat /proc/sys/kernel/random/boot_id`, `$EV/pre/boot-id.txt` |
| B5 | Service/package health | 0 failed units, `dpkg --audit` clean | `$EV/pre/ssh-health.txt` |
| B6 | Settings | `EpaperEnabled`/panel/rotation and FIS-B-relevant settings captured (never posted anywhere - local evidence only) | `getSettings`, `$EV/pre/getSettings.json` |
| B7 | OTA state | `getOTAStatus.Stage == "idle"` - **do not proceed if it is not**; this is a hard precondition, not a warning | `$EV/pre/getOTAStatus.json` |
| B8 | FIS-B cache API baseline | `getFISBCacheStatus` returns 200, current `totalEntries`/`state`/settings recorded | `$EV/pre/getFISBCacheStatus.json` |
| B9 | FIS-B cache inventory baseline | `getFISBCacheInventory` recorded (or its absence noted, if disabled) | `$EV/pre/getFISBCacheInventory.json` |
| B10 | Persisted cache file count/integrity | File count under `fisb-weather-cache/` on the device recorded; each filename is content-addressed (matches `fisbcache/schema.go`'s own key derivation) - a changed count after deployment is itself evidence, not assumed corruption | `$EV/pre/fisb-cache-files.txt` (via SSH `find ... | wc -l` and a full listing) |
| B11 | Free space | `/var/lib/stratux-data`, `/boot/firmware`, `/` all have headroom for the OTA (a bare-ext4 install needs room on the real partition, not just the overlay) | `df -h`, `$EV/pre/df.txt` |
| B12 | Rollback package ready | The known-good `.deb` (matching B3's build) is present locally, hash re-verified immediately before use | `sha256sum`, `$EV/pre/rollback.sha256` |

## Deployment and immediate recovery checks

**[OWNER]** for the OTA operation itself, per this project's established
practice for any real OTA against the grounded device (a stuck/bricked
state needs someone there to physically recover it) - see
`docs/ota-overlayctl-race-investigation.md` (issue #49) for the one known,
non-blocking failure mode to expect and how to handle it (wait ~30s and
retry once if the first attempt rolls back with that specific error).

| # | Check | Pass criterion | Evidence |
|---|---|---|---|
| D1 | Artifact identity on the device | Post-install `getStatus.Build` = B1's build string exactly | `$EV/deploy/post-getStatus.json` |
| D2 | Boot ID changed | New boot ID != B4's | `$EV/deploy/post-boot-id.txt` |
| D3 | Web UI | Root page returns 200, loads normally | `$EV/deploy/webui-check.txt` |
| D4 | Services | 0 failed units, `dpkg --audit` clean, all epaper/stratux units active | `$EV/deploy/post-ssh-health.txt` |
| D5 | OTA result | `getOTAStatus.Stage == "idle"`, no `LastError` | `$EV/deploy/post-getOTAStatus.json` |
| D6 | Configuration retention | B6's settings unchanged post-install | diff against `$EV/pre/getSettings.json` |
| D7 | FIS-B cache API availability | `getFISBCacheStatus` returns 200 (**this is the core regression this whole candidate exists to prevent** - see the reconciliation doc's route table) | `$EV/deploy/post-getFISBCacheStatus.json` |
| D8 | Persisted cache entries reappear/remain | File count under `fisb-weather-cache/` unchanged from B10 (OTA installs new code, never touches `/var/lib/stratux-data`'s own contents - if this count changed, stop and investigate before continuing) | `$EV/deploy/post-fisb-cache-files.txt` |
| D9 | FIS-B cache inventory reflects existing entries | `getFISBCacheInventory` shows the same entries as B9 (allowing for natural aging/freshness-label changes, not count changes) | `$EV/deploy/post-getFISBCacheInventory.json` |

**Explicit rollback trigger**: any of D1-D5 FAIL, or D7/D8 show the FIS-B
route/data genuinely gone (not just still-indexing) → stop, do not proceed
to live-reception testing, go to **Post-test / rollback** below.

## Live reception **[978 MHz]** **[OWNER]** (owner present, in range of a real ground station)

| # | Check | Pass criterion | Evidence |
|---|---|---|---|
| L1 | Timestamped UAT/978 counters | `UAT_messages_total` increasing over the session, with wall-clock timestamps on each sample | periodic `getStatus` snapshots, `$EV/live/status-samples/` |
| L2 | Tower reception | `getTowers` shows at least one tower with recent activity | `$EV/live/getTowers-*.json` |
| L3 | FIS-B product counters | `UAT_METAR_total`/`TAF`/`NEXRAD`/`PIREP`/etc. increase during the window | before/after deltas, `$EV/live/product-counters.txt` |
| L4 | Cache admission | `getFISBCacheStatus.totalEntries` increases during real reception (cache enabled) | before/after `getFISBCacheStatus.json` |
| L5 | Cache persistence | Entries appear under `fisb-weather-cache/` on disk during/after the session (not just in memory) | `$EV/live/fisb-cache-files-during.txt` |
| L6 | Decoded weather products, representative sample | At least one real METAR/TAF/PIREP/NEXRAD-metadata entry captured via `getFISBCachePayload` or the Weather page, content sane (station ID, plausible text/tile metadata) | `$EV/live/sample-products/` |
| L7 | No-RF vs. defect distinction | If L1-L3 show zero movement for the whole window, this is **NOT RUN** for L4-L6 (no reception occurred - not a cache/UI defect) - record `getPowerHealth` and SDR/tower status to rule out a receiver fault as the reason for zero reception, distinct from simply being out of range | `$EV/live/getPowerHealth.json`, SDR status |

## ForeFlight **[OWNER]** **[978 MHz]** (separate device, e.g. the field kit's iPad)

Distinguish these as **three separate results** - do not conflate:

| # | Check | Pass criterion | Evidence |
|---|---|---|---|
| F1 | Traffic | ForeFlight's own traffic display shows targets consistent with what Stratux is receiving, observed directly on ForeFlight | photo of ForeFlight, iPad clock visible, `$EV/foreflight/traffic-*.jpg` |
| F2 | Live weather | ForeFlight's own weather display shows current products, observed directly on ForeFlight (Stratux's own counters are not evidence of what ForeFlight received or displayed - see `docs/fisb-weather-viewer.md`'s own explicit warning) | photo, `$EV/foreflight/weather-*.jpg` |
| F3 | Cached weather (reconnect with a populated cache) | ForeFlight disconnects and reconnects after the cache already holds entries (see the existing kit's Step 7 pattern: Wi-Fi off >2 min, back on); note time-to-display and whether shown products carry a visible age/staleness indication in ForeFlight itself, not just in Stratux | timestamped disconnect/reconnect, photo, `$EV/foreflight/reconnect-*.jpg` |

Record any time-to-display delay or freshness limitation observed in
ForeFlight itself (not inferred from Stratux) as a note against F3, even
if F3 otherwise passes.

## Restart/reconnect (later authorized acceptance run only)

**Not part of this preparation.** For the record, once actually
authorized and performed:

| # | Check | Pass criterion | Evidence |
|---|---|---|---|
| R1 | Clean restart | New boot ID, `previousSessionEndedCleanly: true` (or the exact equivalent field), 0 failed units | `getPowerHealth`/`getStatus` post-restart |
| R2 | Wi-Fi reconnection | Field laptop and ForeFlight's device both rejoin the Stratux AP without manual intervention beyond the normal `nmcli`/iOS reconnect | timestamped |
| R3 | Cache persistence across restart | Same entry count (± natural aging/eviction) as before the restart - text-class entries specifically, per `docs/fisb-weather-cache.md`'s own noted restart-persistence gap for `nexrad_tile`-class entries | before/after `getFISBCacheInventory.json` |
| R4 | Cache re-indexing | `getFISBCacheStatus` reflects the persisted entries again after restart (not stuck at 0 the way this session's own device preflight found - see the reconciliation doc's device-evidence section) | `getFISBCacheStatus.json` post-restart |
| R5 | ForeFlight behavior after restart | Reconnects normally, weather/traffic resume | photo |

## Post-test

| # | Check | Pass criterion | Evidence |
|---|---|---|---|
| P1 | Final health/package audit | 0 failed units, `dpkg --audit` clean | `$EV/post/final-ssh-health.txt` |
| P2 | Evidence hashes | Every captured file listed with its SHA-256 | `$EV/post/SHA256SUMS` (`cd "$EV" && find . -type f \| sort \| xargs sha256sum > post/SHA256SUMS`) |
| P3 | Result classification | Every gate above assigned exactly one of PASS/FAIL/NOT RUN/INCONCLUSIVE, with its evidence path | this session's own closing report |
| P4 | Rollback (if not already triggered) | Owner decides whether to keep the candidate or restore the prior build; if restoring, re-install the verified rollback `.deb`, confirm build string/boot ID/0 failed units/clean audit match the pre-session baseline | `$EV/post/rollback-verify.txt` |
| P5 | FIS-B cache data not lost | Final persisted file count/hashes under `fisb-weather-cache/` match B10 (or L5's grown count if reception occurred and the candidate was kept) - never fewer than the session started with, whichever build ends up installed | `$EV/post/final-fisb-cache-files.txt` |

## Stop conditions (any of these → stop, preserve evidence, do not continue to the next section)

- Candidate package hash or embedded build string does not match B1/B2 exactly.
- `getOTAStatus` reports anything other than `idle` for more than a few
  minutes outside the expected transitional stages.
- FIS-B cache API still 404s (or otherwise absent) after an install that
  otherwise looks healthy.
- Persisted cache file count drops between any two consecutive checks
  without an explained, expected cause (aging/eviction is expected and
  logged; a install/restart-caused drop is not).
- Any failed unit, or `dpkg --audit` dirty.
- Device does not recover to a reachable, healthy state within a
  reasonable window after any reboot.

On a stop condition: mark the triggering gate FAIL, capture whatever
evidence is available, and move to rollback (P4) rather than attempting
to diagnose live on the grounded device without the owner's separate
authorization to do so.
