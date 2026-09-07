# Operational alerting

Supplemental, non-certified visual (and optional browser-audible) notices for
nearby traffic and important Stratux health-state transitions.

> **This is not collision avoidance.** It is not TCAS, not ACAS, not a
> resolution-advisory system, and it never suggests a maneuver (climb,
> descend, turn, or avoid). It never replaces visual scanning, ATC, ForeFlight,
> or pilot judgment. ForeFlight remains the primary EFB presentation and
> already receives full, unmodified GDL90 traffic from Stratux; this feature
> adds a second, independent, Stratux-side notice on top of that, and never
> changes what Stratux sends to ForeFlight.

## Phase 1 findings: existing traffic/threat logic

Before writing any new classification, the existing traffic pipeline was
inspected in full (`main/traffic.go`, `main/flarm-nmea.go`,
`main/gen_gdl90.go`):

- **`TrafficInfo`** (`main/traffic.go`) already carries everything an alert
  needs, freshly recomputed once per second in `sendTrafficUpdates()`:
  `Distance` (meters, via `common.Distance`), `Bearing` (degrees true),
  `Age` (seconds since `Last_seen`, monotonic via `stratuxClock`),
  `Position_valid`, `BearingDist_valid`, `Alt` (pressure altitude, feet),
  `AltIsGNSS`, `OnGround`, `Icao_addr`, `Tail`, `Last_source`.
- **`isOwnshipTrafficInfo(ti)`** is the existing, tested ownship-identification
  logic (OGN-tracker self-detection and configured `OwnshipModeS` codes, with
  distance/altitude/age plausibility checks). This mission reuses that
  function's result directly rather than re-deriving ownship detection.
- **`isGPSValid()`** (`main/gps.go`) is the existing, tested "is ownship
  position trustworthy" check (fix age < 3s, GPS connected, fix quality > 0)
  and is reused directly as the ownship-validity gate.
- **`computeAlarmLevel(dist, relativeVertical)`** (`main/flarm-nmea.go`) is an
  existing 2-level (of 4 possible) threat classification, but it is
  self-documented as `// TODO: only very simplistic implementation`, has no
  "notice" tier, no hysteresis (recomputed fresh every second with no memory
  of prior state - it would flap continuously at an exact boundary), no
  per-target dwell/cooldown/duplicate-suppression, and exists purely to fill
  in one field of a FLARM NMEA sentence sent every cycle to FLARM-consuming
  EFBs. It is not a suitable building block for a persistent, hysteretic,
  acknowledgeable, mutable alert state machine that must avoid flapping and
  duplicate audio across many polling dashboard clients.
- **`isTrafficAlertable(ti)`** sets the GDL90 traffic-report "alert" bit
  (flat 2 NM, or always-true if bearing/distance unavailable) - this is
  ForeFlight's own input signal, part of the normal GDL90 stream. This
  mission's alerting subsystem is completely independent of this bit and
  never reads, sets, or reinterprets it.

**Conclusion, per Phase 1's instruction to justify a new model only if the
existing one cannot support the feature**: the underlying geometry
(`Distance`, relative vertical from pressure/GNSS altitude, `Age`, ownship
validity, ownship exclusion) is correct, already tested, and is reused
as-is. The *classification and lifecycle* (multi-tier thresholds, entry/exit
hysteresis, dwell, per-target cooldown, acknowledgement, bounded event
history) does not exist anywhere in the project and is implemented fresh, in
a new pure `alerting` package, specifically so it can be unit-tested
deterministically and never touches GDL90/FLARM output.

## Architecture

```
main/traffic.go (sendTrafficUpdates, 1Hz)
        |  builds []alerting.TrafficObservation from TrafficInfo,
        |  IsOwnship via isOwnshipTrafficInfo(ti), OwnshipValid via isGPSValid()
        v
main/alertingapi.go: bounded, nonblocking channel (drop-oldest under overload)
        v
background goroutine -> alerting.Evaluator.EvaluateTraffic(...)  (pure, in-memory)
        v
main/alertingapi.go: snapshot -> HTTP API / dashboard / recording / diagnostics
```

```
readiness.HealthReport / preflight.Report (already computed, already grace-aware)
        v
main/alertinghealth.go: derive alerting.HealthSnapshot (reuses existing component states)
        v
alerting.Evaluator.EvaluateHealth(...)  (pure, edge-triggered on transitions)
```

The `alerting` package itself:

- Opens no sockets, does no disk I/O, never touches `recMu` or any daemon
  global directly - it only ever sees values passed into `EvaluateTraffic`/
  `EvaluateHealth`.
- Never modifies, delays, or reads GDL90/FLARM output.
- Never issues maneuver guidance - every message is a neutral notice
  ("Traffic nearby - check position"), never a command.
- Uses an injected `func() time.Time` clock (mirrors
  `preflight.NewManualAckStore`'s existing pattern) so all hysteresis/cooldown
  timing is monotonic and independently testable with a fake clock.

## Traffic data inputs and validity rules

A target is evaluated only when **all** of the following hold (tested
exhaustively in `alerting/traffic_test.go`):

- `OwnshipValid` (caller's `isGPSValid()` result) is true.
- The observation is not `IsOwnship` (caller's `isOwnshipTrafficInfo(ti)`
  result).
- `PositionValid` and `BearingDist_valid`-equivalent (`DistanceValid`) are
  true.
- `AgeSeconds` is below `Config.StaleAfterSeconds` (default matches the
  existing `sendTrafficUpdates()` "current" window's spirit, but is its own
  named, documented constant - not silently borrowed).
- `DistanceMeters` and `RelativeAltitudeFeet` (when required for the
  candidate tier) are finite (`!math.IsNaN`, `!math.IsInf`) and
  non-negative for distance.
- The target is within `Config.MonitoringHorizontalMeters` /
  `Config.MonitoringVerticalFeet` (targets outside this envelope are simply
  not evaluated, and any existing state for them decays toward removal like
  any other target that stops updating).
- Altitude information is available when required by the candidate tier: a
  target with a horizontal-only match (no valid relative altitude) can reach
  `TRAFFIC_NOTICE` (horizontal-only, conservative) but never a `CAUTION`
  tier, since caution tiers require both dimensions to be trustworthy.

Any of these failing makes the target ineligible for that cycle - not an
error, not a fabricated level; it is dropped from consideration and its
existing state (if any) simply stops refreshing and eventually expires.

## Ownship-exclusion behavior

Enforced twice, independently (defense in depth):

1. The caller (`main` package) never constructs an observation for a target
   `isOwnshipTrafficInfo` identifies as ownship or tells the caller to
   ignore.
2. `alerting.TrafficObservation.IsOwnship` is also checked inside
   `EvaluateTraffic` itself and any such observation is refused - so the
   pure package's own test suite can prove self-alert is impossible without
   depending on the caller's filtering being correct.

## Threshold policy

Named, documented, **project-defined defaults** - not FAA, TCAS, ACAS, or any
regulatory separation standard:

| Envelope | Horizontal | Vertical | Meaning |
|---|---|---|---|
| Monitoring | 5 NM (9260 m) | 2000 ft | Outside this, a target is never evaluated at all. |
| Notice | 3 NM (5556 m) | 1200 ft | `TRAFFIC_NOTICE` - "worth a look." |
| Caution | 2 NM (3704 m) | 800 ft | `TRAFFIC_CAUTION`, tier 2. |
| High caution | 1 NM (1852 m) | 500 ft | `TRAFFIC_CAUTION`, tier 3 - same public `Level`, more urgent wording/tone/shorter cooldown, only when the target remains fully eligible (fresh, valid altitude). |

A target must be *inside both* the horizontal and vertical bound for a tier
to apply (conservative AND, not OR) - a target 0.3 NM away but 3000 ft above
is not a caution.

Exit thresholds are **20% wider** than entry thresholds
(`Config.ExitHysteresisFactor = 1.2`) at every tier boundary, so a target
sitting exactly on a boundary cannot flap.

## Hysteresis and lifecycle

Per-target state machine (`alerting/traffic.go`):

- **Entry**: a target reaching a tier's entry threshold immediately
  escalates to that tier (or higher, if it qualifies for more than one in
  the same cycle) - "immediate escalation when a higher level is proven."
- **Exit/de-escalation**: a target must cross the *wider* exit threshold
  for its *current* tier and then remain outside it for
  `Config.DeescalateDwellSeconds` (default 10s) before the tier drops - this
  is the delayed de-escalation that prevents flapping from one noisy sample.
- **Expiration**: a target with no refreshed observation for
  `Config.ExpireAfterSeconds` (default 30s) is removed entirely, clearing all
  memory of it (acknowledgement, tier, cooldown timestamps).
- **Re-entry**: because state is cleared on expiration (and also whenever a
  target's tier returns all the way to zero), a target that leaves the
  envelope and later re-enters starts fresh - "a previously acknowledged or
  muted target may alert again if it leaves the alert envelope and later
  re-enters, or if it escalates materially" is satisfied structurally, not
  as a special case.
- **Bounded memory**: at most `Config.MaxTrackedTargets` (default 500,
  matching this project's own documented ~500-target practical ForeFlight
  limit) target states are retained; beyond that, the oldest-updated states
  are evicted first.
- **No cross-restart persistence**: target state lives only in the
  `Evaluator`'s memory, which is recreated at daemon startup - "reset on
  daemon restart" and "do not persist active traffic-alert state across
  reboot" are satisfied by construction (there is no code path that writes
  target state to disk).

## Duplicate suppression and cooldowns

- **Per-target duplicate suppression**: re-observing a target already at its
  current tier, with no material change, never produces a new alert *event*
  (it still refreshes `LastSeen` so the target doesn't expire) - only a tier
  change (or fresh entry) produces an event.
- **Per-target audio cooldown**: even when a new event is produced,
  audio-eligibility for that target is additionally gated by a per-tier
  cooldown (`Config.AudioCooldownSeconds` per level) since the *last audible*
  event for that specific target - escalating faster than the cooldown still
  produces a new visual event, but audio for the same target is rate-limited.
- **Global minimum audio spacing**: independent of any single target,
  `Config.GlobalMinAudioSpacingSeconds` bounds how close together *any* two
  audio-eligible events may be signaled, across all targets combined -
  protects against a burst of several targets crossing thresholds in the
  same second producing a wall of tones.

## Escalation/de-escalation summary

Escalation is immediate and proof-based (this cycle's data qualifies for a
higher tier now). De-escalation is delayed and dwell-based (must sit outside
the wider exit band for the full dwell period). This asymmetry is
intentional: an emerging hazard should never wait to be reported, but a
transient dip below a threshold should never be reported as "all clear" only
to immediately re-alert on the next noisy sample.

## Health-transition behavior

`alerting.EvaluateHealth` never re-derives component health - it receives an
already-computed `HealthSnapshot` (built by `main/alertinghealth.go` from the
existing `readiness.HealthReport` and `preflight.Report`, both of which
already bake in their own startup grace periods, environmental-absence
handling, no-tachometer honesty, and FIS-B/traffic-absence-is-not-a-failure
rules - see `docs/preflight-readiness.md`). The evaluator only detects
**edge transitions** between snapshots:

- A transition from a better state to `NOT_READY`-equivalent (or a defined
  worse `CAUTION`-equivalent for specific components: storage, overlay,
  thermal, receivers, GPS, trusted time, GDL90 clients, AHRS, baro, fan,
  active profile) produces a `SYSTEM_CAUTION` or `SYSTEM_NOT_READY` alert,
  once, at the transition.
- A transition back to a healthy state produces a quiet, visual-only
  recovery event by default (no audio) - "recovery should produce a quiet
  visual recovery event by default, not another alarm tone."
- No transition, no event - repeatedly observing the same `CAUTION`/
  `NOT_READY` state across cycles never re-fires.
- Health-alert audio is independently configurable
  (`AlertSettings.SystemHealthAudioEnabled`, default **disabled**) and is
  never enabled implicitly by enabling traffic audio.

## Startup grace periods

Not reimplemented here - inherited entirely from `readiness`/`preflight`'s
already-tested grace-period logic (SDR discovery 120s, GPS acquisition 90s,
GNSS time 90s, network client 60s, fan-status 30s - see
`docs/preflight-readiness.md`). Because `EvaluateHealth` only reacts to the
*already grace-aware* `Overall`/component states, a component reporting
`UNKNOWN` during its own grace period never presents as a transition to
`SYSTEM_CAUTION`.

## Configuration and persistence

`main/alertsettings.go` implements `AlertSettings` (schema-versioned,
atomically persisted at `<PersistentDataPath>/alert-settings.json`, mirroring
`calprofile.Store`'s established temp-file/fsync/rename pattern). See
`docs/http-api.md` for the full field list and defaults. Numeric fields are
range-validated on every write (rejecting `NaN`/`Inf`/negative
distances/excessive mute durations); a corrupt or missing settings file
degrades to documented, safe defaults rather than failing daemon startup.
Persistence I/O never happens while any traffic- or GDL90-relevant lock is
held.

## Recording integration

See "Recording" in the API/dashboard sections of this document and
`docs/recording.md` - a bounded `AlertingSnapshot` is captured once at
recording start (schema version, master/visual/audio/system-alert enabled
flags, mute state, active-alert counts by level, threshold profile), never
repeated per-sample, and a separate bounded alert-event sidecar file records
material alert transitions during the recording without touching the
existing JSONL/CSV formats.

## Diagnostics

A bounded, sanitized alerting summary is included in every diagnostic
bundle: enabled/audio/mute state, current counts by level, recent event
counts, suppressed/dropped/stale-rejected counters, last alert time, last
health transition - never exact coordinates, MAC addresses, or unbounded
history.

## Failure isolation

An alert-evaluation failure (a panic during evaluation, a full event channel,
a metadata-persistence failure) is recovered, logged, and counted - it never
stops traffic ingestion, GDL90 output, recording, or any other subsystem.
The channel between traffic ingestion and alert evaluation is bounded and
nonblocking: a full channel drops the newest evaluation cycle (counted) and
`sendTrafficUpdates()` proceeds immediately, unaffected.

## Browser audio and its limitations

Uses the Web Audio API (`AudioContext` + `OscillatorNode`) - already
available in every browser this project targets, no new dependency. Audio
can only start after the operator taps/clicks **Enable Sound** (a real user
gesture, satisfying iOS/iPadOS's autoplay restrictions) and stays disabled
(shown as "unavailable") until then. The dashboard explicitly discloses,
verbatim, that: audio may not play when Safari is backgrounded, the page is
suspended, the device is locked, or another app has exclusive audio; a
browser tone cannot be claimed to reach an aviation headset; there is no
Bluetooth audio output from Stratux itself; and ForeFlight remains the
primary EFB presentation. Tones are short, three audibly distinct patterns
(notice/caution/system-caution), rate-limited by the same cooldown/global
spacing rules as visual events, never continuous or rapidly repeating, and
recovery is silent by default. Test Sound plays a tone without creating a
real alert event, and - like every other tone - is silenced immediately by
an active mute (caught during owner field validation: the server-side
`/testAlertSound` call is a pure reachability no-op, so the client has its
own mute check rather than inheriting one from a real alert's server-gated
`audioEligible` path).

## Known limitations

- Closure-rate/closest-point-of-approach estimation is **not implemented**.
  Only proximity-based (current distance/relative-altitude) classification
  is provided, stated honestly rather than inferring an unreliable closure
  rate from too few observations (per this mission's own explicit
  guidance: "if reliable closure cannot be demonstrated, implement
  proximity-based notices only").
- Ground-target suppression is available as a documented, off-by-default
  setting (`SuppressGroundTraffic`) - "if reliably supported," since
  `OnGround` is itself sourced data of varying reliability across sources.
- Speech synthesis is not implemented (tones only, per this mission's
  explicit preference).
- No direct Bluetooth audio output; no ForeFlight audio injection - neither
  is in scope for this mission and neither is implemented.

## Owner field-validation checklist

None of these steps require creating an actual traffic-proximity hazard -
every threshold-crossing behavior is proven by the automated test suite
against fabricated data; this checklist is for the parts only a human can
confirm (physical UI/audio behavior, real (already-occurring) traffic
comparison).

1. Open the **Alerts** page. Confirm the disclaimer is visible and the page
   renders without overlapping/clipped controls at desktop, tablet, and
   mobile widths.
2. Confirm the small bell indicator is visible in the navbar on every page
   (Status, Traffic, Readiness, etc.), and does not cover or replace any
   existing button.
3. Tap **Enable Sound**. Confirm the audio state changes to "armed" only
   after this tap - never before it.
4. Tap **Test Sound**. Confirm a short, non-startling tone plays, and that
   the Alerts page's active/recent-history lists are unaffected (no real
   alert was created).
5. If real ADS-B traffic is currently being received, compare any resulting
   traffic notice's approximate range/clock/relative-altitude against the
   existing Traffic page for the same target - they should agree
   approximately, and no notice should ever name your own aircraft.
6. Tap **Mute**. Confirm no further tones play, including from **Test
   Sound** (visual notices, if any, should still update). Tap **Unmute**
   and confirm tones - including Test Sound - can resume.
7. Change a setting (e.g. audio volume) and reload the page - confirm it
   persisted.
8. Reboot the device. Confirm the mute preference (muted or not) matches
   what you left it as, and that no stale traffic notice reappears
   immediately after boot (fresh state - see "Startup grace periods").
9. Confirm ForeFlight (or another connected EFB) remains connected and
   still shows normal traffic throughout all of the above - this feature
   must never visibly affect the GDL90 stream.
