# Traffic closure-rate / closest-point-of-approach (CPA) alerting

## Purpose

An additive, conservative trend input to this project's existing
operational-alerting subsystem (`alerting`, see `docs/alerting.md`):
given ownship's and one traffic target's own reported ground track,
speed, and (when reliable) vertical rate, estimate whether the two are
converging, roughly when they would be closest, and how close - so a
target already inside the existing distance/altitude envelope can be
escalated to a more urgent alert level slightly earlier, when its own
reported trajectory genuinely supports that.

## Explicit non-goals

This is **not**:

- Collision avoidance, TCAS, ACAS, or a resolution-advisory system.
- Maneuver guidance of any kind - no "climb," "descend," "turn,"
  "maintain," or "avoid" is ever produced.
- A replacement for the existing distance/altitude alerting, visual
  scanning, ATC, or a certified traffic system - it is a strictly
  additive input to the *existing* policy, never a parallel or
  substitute one.
- A predictor other traffic will act on - it estimates from ownship's
  and the target's *own currently-reported* motion, with no knowledge
  of either aircraft's actual intent.
- Enabled by default - `CPAEscalationEnabled`/`escalationEnabled`
  default to `false` pending real-hardware validation (see "Hardware-
  validation checklist," below). Passive calculation and display are
  always on regardless (see "API fields").

## Architecture

Three layers, mirroring `fisbcache`/`storagelifecycle`/`alerting`'s own
established "pure package + thin glue" split:

- **`trafficcpa`** (new, pure, stdlib-only): `Compute(ownship, target
  Track, cfg Config) Result` - the entire mathematical model. No
  networking, filesystem, hardware, global state, or wall-clock access.
- **`alerting`** (existing, pure, extended additively):
  `TrafficObservation.CPA *trafficcpa.Result`; `Config.CPAEscalationEnabled`/
  `CPAMinClosureRateKnots`; `classifyTier` now also returns whether a
  valid CPA estimate raised the tier it already computed from distance/
  altitude alone. `Alert` gained CPA* fields for API/dashboard/
  diagnostics/recording consumption.
- **`main`** (glue, new files `trafficcpasettings.go`/`trafficcpaapi.go`,
  small additive edits to `alertingapi.go`/`diagnosticsapi.go`/
  `preflightapi.go`/`recordingmetadataapi.go`/`managementinterface.go`):
  builds `trafficcpa.Track` from the existing `TrafficInfo`/`mySituation`,
  calls `trafficcpa.Compute` inline inside `buildTrafficObservation`
  (`main/alertingapi.go`), persists/serves settings, and wires bounded
  summaries into diagnostics/recording/preflight.

## Data sources

Traced from the existing call graph (`main/traffic.go`'s
`sendTrafficUpdates`, ~1Hz):

| Track field | Ownship source | Target source |
|---|---|---|
| Latitude/longitude | `mySituation.GPSLatitude/GPSLongitude` | `TrafficInfo.Lat/Lng` |
| Position valid | `isGPSValid()` | `TrafficInfo.Position_valid` |
| Altitude | `AltIsGNSS+GPS → GPSHeightAboveEllipsoid`, else pressure → `BaroPressureAltitude`, else `GPSAltitudeMSL` - the SAME precedence `relativeAltitudeFeetForAlerting` already uses, selected per-target by that target's own `AltIsGNSS` flag, so both this file's vertical geometry and the existing current-relative-altitude field always agree on which reference is in use | `TrafficInfo.Alt` (0 treated as unknown, matching `relativeAltitudeFeetForAlerting`'s own established convention) |
| Altitude valid | per the precedence above; false only if no source is currently trusted | `Alt != 0` |
| Track/speed | `mySituation.GPSTrueCourse`/`GPSGroundSpeed` | `TrafficInfo.Track`/`Speed` |
| Ground-velocity valid | `isGPSGroundTrackValid()` (accuracy < 30m - a stricter bar than plain `isGPSValid`, since an inaccurate ground track would corrupt the whole relative-velocity vector) | `TrafficInfo.Speed_valid` |
| Vertical rate | `AltIsGNSS+GPS → GPSVerticalSpeed*60` (ft/s→ft/min), else `BaroVerticalSpeed` (already ft/min) | `TrafficInfo.Vvel` (already ft/min) |
| Vertical-rate valid | same gate as altitude above | `Speed_valid` (see "Known limitations" - this codebase has no dedicated `Vvel_valid` flag) |
| Age | `stratuxClock.Since(mySituation.GPSLastFixLocalTime)` | `TrafficInfo.Age` |

## Coordinate and relative-motion model

An equirectangular tangent-plane approximation centered on ownship -
the same convention this project's own `common.DistRect` already uses
for short-range traffic distance/bearing, reimplemented independently in
`trafficcpa` (not imported) to keep that package's dependency graph
limited to the Go standard library.

```
dLat = normalize(targetLat - ownLat)      // wraps to (-180, 180]
avgLat = (targetLat + ownLat) / 2
dLon = normalize(targetLon - ownLon)      // wraps to (-180, 180], handles antimeridian crossing
north = radians(dLat) * R                  // R = 6,371,008.8 m (mean earth radius)
east  = radians(dLon) * R * cos(radians(avgLat))
```

`r = (north, east)` is the target's position relative to ownship, in
meters. Ground velocity for each aircraft converts track (degrees true)
and speed (knots) into `(north, east)` meters/second components; `v` is
the **target's velocity relative to ownship** (`v = v_target - v_own`).

**Validity envelope:** `MaxHorizontalSeparationMeters` (derived
dynamically from the *current* `alerting.Config.MonitoringHorizontalMeters`
- see "Settings and defaults" for why these two envelopes are
deliberately kept in lock-step) and `MaxAbsoluteLatitudeDeg` (fixed at
80°, a property of the approximation's own accuracy, not a user-tunable
policy value) bound where this flat-earth approximation is trusted.
Outside either bound, `Compute` rejects the input outright
(`ReasonOutsideEnvelope`) rather than silently reporting false precision.

## Units and sign conventions

- Distances: meters internally (`trafficcpa`), nautical miles at the API/
  dashboard boundary (matching `alerting`'s own existing
  meters-internal/NM-external split).
- Speed: knots. Vertical rate: feet per minute. Time: seconds.
- **Closure rate**: positive = closing (separation shrinking), negative
  = opening. Defined as `-(r·v)/|r|` (the negative rate of change of
  horizontal separation at the current instant); `0` at coincident
  positions (`|r|=0`), which is the correct limiting value, not an
  arbitrary placeholder (`d/dt|r+vt|² = 2(r·v)`, which is `0` when
  `r=0`, for any `v`).
- **Vertical closure rate**: `target vertical rate - ownship vertical
  rate` (rate of change of `target altitude - ownship altitude`);
  positive means the target is gaining on ownship vertically.

## TCPA/CPA equations

```
t_CPA_raw = -(r·v) / (v·v)
t_CPA     = clamp(max(t_CPA_raw, 0), 0, HorizonSeconds)
r_CPA     = r + v * t_CPA
predicted_horizontal_separation = |r_CPA|
```

A raw, unclamped **negative** `t_CPA_raw` means the true closest point
of approach already occurred - reported as `TCPASeconds = 0` ("looking
forward from now"), **never** a negative number. `Trend` is computed
separately, from the *unclamped* closure rate, so a target whose true
CPA is in the past still honestly reports `DIVERGING` even though
`TCPASeconds` reads `0` - these two fields answer different questions
("what does the predicted-forward geometry look like" vs. "is this
target's own current motion opening or closing") and must be read
together, never one substituted for the other.

A **raw, unclamped** `t_CPA_raw` **beyond** `HorizonSeconds` is clamped
to the horizon (`TCPAClampedToHorizon = true`); the predicted-separation
fields then describe the geometry *at the horizon*, not at the true CPA.

Vertical: `predicted_vertical_separation = current_vertical_separation +
vertical_closure_rate_fpm * (t_CPA / 60)` - computed **only** when both
tracks' vertical rates are valid; otherwise left explicitly invalid (see
"Vertical-rate behavior").

## Prediction horizon

`HorizonSeconds` (persisted, default 180s/3 minutes) bounds the maximum
*useful* TCPA - beyond it, a "converging" classification is not
actionable enough to be worth reporting as an imminent trend. Also
governs `alerting`'s own escalation gate: a horizon-clamped TCPA never
escalates (see "Alert escalation policy").

## Freshness and quality requirements

- `MaxAgeSeconds` (fixed at 6s, matching `alerting.Config.StaleAfterSeconds`
  - the same freshness window `sendTrafficUpdates` itself already treats
  as "current"): either track older than this is rejected
  (`ReasonOwnshipStale`/`ReasonTargetStale`).
- `MinRelativeSpeedKnots` (persisted, default 20kt): below this, the
  TCPA/closure-rate math is numerically unstable (dividing by a
  near-zero `v·v`) and is rejected outright (`ReasonRelativeSpeedTooLow`)
  rather than reported as a low-confidence number.
- Ownship ground-track quality is gated at `isGPSGroundTrackValid()`
  (accuracy < 30m) - a stricter bar than the plain GPS-fix validity used
  elsewhere, since an inaccurate ground track would corrupt the entire
  relative-velocity vector this whole computation depends on.

## Confidence states and rejection reasons

`Result.Confidence`: `NONE` (invalid result), `MEDIUM` (horizontal
estimate valid, no reliable vertical prediction), `HIGH` (both valid).

`Result.RejectReason` (`trafficcpa.RejectReason`, exhaustive):
`ownship-position-invalid`, `target-position-invalid`,
`invalid-latitude-longitude`, `outside-approximation-envelope`,
`ownship-stale`, `target-stale`, `ownship-velocity-invalid`,
`target-velocity-invalid`, `relative-speed-too-low`,
`non-finite-result`. Every rejection is specific - never a generic
"unavailable" - so a diagnostic summary or a UI can report exactly why.

## Vertical-rate behavior

Missing or unreliable vertical rate on **either** track never assumes a
zero climb/descent rate. `CurrentVerticalSeparationFeet` (needs only
both altitudes) is reported whenever available regardless;
`PredictedVerticalSeparationFeet`/`VerticalClosureRateFPM` stay
explicitly invalid, and `Confidence` stays `MEDIUM`, whenever either
track's vertical rate is not reliable.

**Known, disclosed limitation:** this codebase has no dedicated validity
flag for `TrafficInfo.Vvel` (unlike `Speed_valid` for `Speed`) - a
genuinely level target and one whose vertical rate was simply never
reported both read `Vvel == 0`. `VerticalRateValid` is gated on
`Speed_valid` alone (the same message class that populates `Vvel` for
every live traffic source in this project), not also on `Vvel != 0`,
which would wrongly treat the single most common case - genuinely level
traffic - as unknown. The failure mode this trades for (trusting a zero
rate that was actually just never updated yet) is bounded: it can never
escalate more than one tier, decays back toward the always-trustworthy
*current* vertical separation as fresher data arrives, and never
persists.

## Closure-rate behavior

See "Units and sign conventions" for the exact formula and sign
convention; `NearlyZero`/`BothNearlyStationary`/`CoincidentPositions`
edge cases are all covered explicitly by `trafficcpa`'s own test suite
(`trafficcpa/compute_test.go`) - never a divide-by-zero, never a NaN
propagated into a Valid result.

## Alert escalation policy

`alerting.classifyTier` computes the tier from distance/altitude alone
**exactly as before this feature existed** - completely unmodified logic
- then, only if that tier is strictly between `NONE` and `HighCaution`,
asks `cpaWarrantsEscalation` whether the CPA estimate justifies raising
it by **exactly one level**. Every one of the following must hold:

1. `Config.CPAEscalationEnabled` is true (persisted default: `false`).
2. `TrafficObservation.CPA` is present and `Valid`.
3. The target is not ground-capped (`SuppressGroundTraffic && OnGround`).
4. The observation's own **current** relative altitude is itself valid -
   an escalation is never based on a vertical *prediction* when the
   *current* relative altitude could not already be trusted (the
   existing altitude-unavailable path already caps the base tier at
   `Notice` for exactly this reason; checked explicitly here too).
5. `TCPAClampedToHorizon` is false - a target whose true closest
   approach falls beyond the configured horizon is not yet an imminent
   trend.
6. `HorizontalClosureRateKnots >= Config.CPAMinClosureRateKnots` - a
   **policy** minimum (persisted default 30kt), deliberately independent
   of and stricter than `trafficcpa`'s own `MinRelativeSpeedKnots`
   numerical-stability floor (default 20kt): an estimate that is merely
   computable should not by itself be enough to escalate a real alert.
7. `PredictedHorizontalSeparationMeters` fits within the **next tier's
   own existing horizontal threshold** - reusing the already-reviewed
   Notice/Caution/HighCaution distances rather than inventing a new,
   separately-calibrated CPA-specific number.
8. If `Confidence == HIGH` (a vertical prediction is available), it must
   **also** fit the next tier's own vertical threshold, or escalation is
   refused even though the horizontal trend alone would have qualified.
   If `Confidence == MEDIUM` (no vertical prediction), escalation is
   still permitted on the horizontal trend alone - by exactly one tier,
   matching `classifyTier`'s own existing altitude-unavailable
   conservatism.

### Proof that CPA can never weaken an existing alert

Structural, not just tested: the escalation step only ever executes
`tier++`, and only when `tier` is already strictly above `NONE` (i.e.
distance/altitude alone already decided this target is alert-eligible)
and strictly below `HighCaution` (there is nowhere higher to escalate
to). There is no code path anywhere that decrements a tier, skips the
distance/altitude computation, or substitutes a CPA-derived tier for the
base one - CPA can only ever ratchet the SAME decision one step higher,
never override it. `alerting/cpa_policy_test.go`'s
`TestClassifyTier_CPANeverWeakensAnAlert` proves this directly: for
every base tier and every CPA state (nil, invalid, diverging, or even a
maximally strong converging one), the CPA-enabled result is never lower
than the CPA-disabled one.

## Hysteresis, dwell, and cooldown interaction

CPA escalation produces exactly the same `tier` value hysteresis/dwell/
cooldown/audio/history already operate on (`targetTrafficState.currentTier`)
- there is no separate CPA alert state machine. `cpaEscalated` is tracked
alongside `currentTier` per target (`targetTrafficState.cpaEscalated`),
carried forward unchanged whenever hysteresis holds a target at its
prior tier, and reset to `false` only when a target's tier genuinely
clears (the same "wholly new encounter" reset every other per-target
memory already gets). A CPA-escalated event is otherwise indistinguishable
from any other tier change in the eyes of the evaluator: same
audio-eligibility check, same per-target and global cooldowns, same
duplicate suppression, same bounded history.

## Ownship and stale-target handling

Both happen in the caller, strictly **before** `buildTrafficObservation`
(and therefore `computeTrafficCPA`) is ever invoked for a given target:
`main/traffic.go`'s `sendTrafficUpdates` calls `isOwnshipTrafficInfo`
and only calls `buildTrafficObservation` when `!shouldIgnore`;
`buildTrafficObservation` itself additionally skips CPA computation
entirely when `isOwnshipTi` is true. Target-side staleness is enforced a
second time, independently, inside `trafficcpa.Compute` itself
(`MaxAgeSeconds`), so a target that ages past freshness between the
caller's own check and this function's own gate still cannot produce a
valid, escalation-eligible estimate.

## Settings and defaults

`main.TrafficCPASettings` (own file, own schema version, own atomically-
persisted JSON, mirroring `AlertSettings`'s exact temp-file/fsync/rename
pattern):

| Field | Default | Bound |
|---|---|---|
| `escalationEnabled` | `false` | - |
| `horizonSeconds` | 180 | (0, 600] |
| `minRelativeSpeedKnots` | 20 | (0, 500] |
| `minClosureRateKnots` | 30 | (0, 500], and `>= minRelativeSpeedKnots` |

`Validate()` rejects NaN/Infinity, non-positive or excessive values, and
an inverted `minClosureRateKnots < minRelativeSpeedKnots` (a policy
minimum looser than the computation's own validity floor could never
bind on anything). A malformed/missing settings file degrades safely to
these defaults, never a fatal error.

**No filesystem I/O on the traffic-evaluation path:** settings are read
from an in-memory, mutex-guarded cache (`currentTrafficCPASettings`,
populated once at `initTrafficCPA`, updated only by
`handleSetTrafficCPASettingsRequest` and Configuration Backup restore) -
`computeTrafficCPA` never touches disk. This is a deliberate departure
from `AlertSettings`'s own simpler "always read fresh from disk" pattern
(safe there because that read happens once per settings *change*, not
once per target per ~1Hz cycle) - see `main/trafficcpaapi.go`'s own doc
comment.

`alerting.Config`'s own `CPAEscalationEnabled`/`CPAMinClosureRateKnots`
are kept in sync with the persisted `TrafficCPASettings` via
`mergedAlertingConfig` (`main/trafficcpaapi.go`), called by every site
that used to call `toAlertingConfig` alone
(`initAlerting`/`handleSetAlertSettingsRequest`) plus
`handleSetTrafficCPASettingsRequest` itself - so either settings file
changing keeps the one live `alertEvaluator.Config` consistent.
`trafficcpa.Config.MaxHorizontalSeparationMeters` is likewise always
derived from the *current* `alerting.Config.MonitoringHorizontalMeters`
at computation time, never a second, independently-drifting envelope
value.

## API fields

`GET /getTrafficCPASettings` / `POST /setTrafficCPASettings` - the
settings above, validated identically to every other settings endpoint
in this project (strict single-JSON-document body, unknown fields
rejected, atomic persisted write, applied immediately).

`GET /getAlerts`'s existing `Alert` objects (traffic category only) gain,
purely additively: `cpaValid`, `cpaConfidence`, `cpaRejectReason`,
`cpaClosureRateKnots`/`cpaClosureRateValid`, `cpaTcpaSeconds`/
`cpaTcpaValid`/`cpaTcpaClampedToHorizon`,
`cpaPredictedHorizontalMeters`/`cpaPredictedHorizontalValid`,
`cpaPredictedVerticalFeet`/`cpaPredictedVerticalValid`, `cpaTrend`,
`cpaEscalated`. Every numeric field is paired with its own `*Valid`
flag, exactly like the pre-existing `distanceMeters`/`relativeAltitudeFeet`
fields - never a fabricated zero for an unavailable estimate.

## Dashboard

Extends the existing Alerts page (`web/plates/alerts.html`/`alerts.js`) -
each active traffic alert card now shows its own trend/closure-rate/TCPA/
predicted-separation/confidence/escalation line (or an explicit
"unavailable (reason)" note), and a new settings panel exposes the
escalation toggle and the three thresholds, with the required disclaimer
and an explicit note that CPA can only ever raise, never lower, an
alert. See that page's own doc comment for the exact fields; not
independently screenshotted or rendered on a physical device in this
implementation-only mission (see "Hardware-validation checklist").

## Diagnostics integration

`TrafficCPADiagnosticsSummary` (embedded in every diagnostic bundle as
`TrafficCPASummary`): `escalationEnabled`, `evaluations`, `validResults`,
`rejectionsByReason` (a bounded map, one entry per distinct
`RejectReason` actually observed), `fallbackToExistingPolicyCount`
(`evaluations - validResults`), `escalatedAlertCount`, `panicCount`,
`lastEvaluationAgeSeconds`, and the current settings. Never a raw target
list, a track history, or a coordinate.

## Recording integration

Additive on both existing mechanisms, no new one:

- **Per-event**: `alerting.Event`/`Alert` (already captured, bounded, in
  `SessionFinalization.AlertEvents`) now simply carries its own CPA
  fields too - no separate change needed.
- **Session-level**: `SessionSnapshot` gained `TrafficCPASchemaVersion`/
  `TrafficCPAEscalationEnabled`/`TrafficCPAHorizonSeconds`/
  `TrafficCPAMinClosureRateKnots`, captured once at recording start,
  mirroring `AutoRecordSettings`'s own session-snapshot fields.
  `recording.MetadataSchemaVersion` bumped 5→6 (purely additive, per this
  project's own established convention); `alerting.SchemaVersion` bumped
  1→2 for the same reason (new `Config`/`Alert` fields).

## Readiness and Preflight integration

No new `readiness.HealthReport` component - like `alerting` itself, this
is a consumer of existing traffic/GPS health, not a hardware subsystem
readiness reports on. `preflight.trafficCPAChecks` (mirrors
`autoRecordChecks`' own restraint): disabled (the default) is always
`NOT_APPLICABLE`/`Info` - a deliberate opt-out is never a caution or
blocking condition; settings failing validation is `CAUTION`/`Caution`,
never `Blocking`, since that only means this enhancement falls back to
distance/altitude-only alerting, never that alerting itself stops
working. CPA availability never implies flight-readiness or
certification.

## Configuration Backup compatibility

`configbackup.TrafficCPASettingsSection` - new section, checksummed,
validated, diffable, following `AutoRecordSettingsSection`'s exact
pattern. A document produced by code that predates this section (today's
current shape, or the older pre-`autoRecordSettings` shape) is
recognized by exact section-checksum key-set equality and full
content-checksum recomputation - never a partial match - and normalized
to the safe, disabled default (`legacyDefaultTrafficCPASettings`), never
the bare Go zero value (which would itself fail `Validate`'s bounds
check) and never silently enabling escalation. See
`configbackup/legacy.go` and its own test fixture
(`configbackup/testdata/legacy-pre-trafficcpa-backup.json`, generated by
literally running the actual pre-this-feature commit's own
`BuildDocument` - never hand-simulated).

## Concurrency and failure isolation

- `computeTrafficCPA` runs inline, synchronously, on the same goroutine
  `buildTrafficObservation`/`sendTrafficUpdates` already runs on (~1Hz) -
  safe specifically because `trafficcpa.Compute` is pure, bounded
  CPU-bound arithmetic with no I/O and no lock beyond
  `currentTrafficCPASettings`'s own short-held mutex. A `recover()`
  inside `computeTrafficCPA` means an unexpected panic there is caught
  and counted, never propagated into the surrounding traffic-evaluation
  cycle (defense in depth - `trafficcpa.Compute` itself is a pure
  function over already-validated Go values, never expected to panic).
- CPA escalation happens entirely inside the existing
  `alertTrafficEvaluationLoop` goroutine (the sole caller of
  `EvaluateTraffic`) - no new goroutine, no goroutine per target.
- No mutation of `TrafficInfo`/GDL90 target data anywhere in this
  feature; GDL90 forwarding and FIS-B processing are entirely
  unaffected.
- Deterministic for the same ordered inputs (`trafficcpa.Compute` is a
  pure function; `alerting`'s own evaluator was already deterministic
  for a given input sequence).

## Memory/history bounds

No new per-target history beyond what `alerting.targetTrafficState`
already keeps (a single `cpaEscalated bool`, alongside the tier/
acknowledgement/audio-cooldown fields it already tracked) - bounded by
the same `MaxTrackedTargets` cap `alerting.Config` already enforces. No
raw position/track history is retained anywhere by this feature; nothing
here is persisted to disk except the small, fixed-shape settings struct.

## Test strategy

- `trafficcpa/compute_test.go` (30 tests): the full required mathematical
  matrix - head-on/overtake/crossing/parallel/diverging geometries,
  stationary ownship/target, near-zero and exactly-zero relative
  velocity, CPA now/beyond-horizon/at-the-exact-boundary, horizontal
  miss, vertical crossing/level/opposing climb-descent, missing vertical
  rate/track/speed, stale data, invalid lat/lon, longitude wraparound, a
  high-latitude envelope boundary, coincident positions, NaN/Infinity,
  unit conversion, sign convention, determinism.
- `alerting/cpa_policy_test.go` (11 tests): the mandatory policy proofs -
  CPA never weakens any alert (across every base tier and CPA state);
  escalation is disabled by default and exactly one tier at a time,
  never past HighCaution; ground-capped/horizon-clamped/sub-threshold-
  closure-rate/missing-current-altitude/predicted-separation-too-far
  cases all correctly refuse to escalate; a high-confidence estimate
  additionally requires the vertical prediction to fit; a CPA-escalated
  event uses the exact same hysteresis/duplicate-suppression framework
  as any other event.
- `configbackup/legacy_trafficcpa_test.go` (11 tests): the new section's
  checksum/validation/legacy-normalization/preview-diff behavior,
  including an authentic historical fixture and a proof the two
  recognized legacy shapes (pre-autoRecordSettings, pre-trafficCpaSettings)
  are mutually exclusive.
- `preflight/trafficcpa_checks_test.go` (3 tests): the new preflight
  check's disabled/invalid/ready states.
- Existing `alerting`/`main`/`configbackup`/`readiness`/`recording`/
  `preflight` test suites all pass unmodified except the small, additive
  argument-list changes their own call sites needed (e.g.
  `BuildDiagnosticBundle` gained one parameter).

Exact commands and full pass/fail results are in the mission's own final
report, not restated here.

## Known limitations

- No dedicated `Vvel_valid` flag exists in this codebase for target
  vertical rate - see "Vertical-rate behavior" above for the exact,
  disclosed, bounded-impact tradeoff this makes.
- The equirectangular approximation is deliberately bounded (envelope
  check) rather than geodesically exact - appropriate for this project's
  own short traffic-alert ranges (a few NM), not a general-purpose
  long-range navigation calculation.
- CPA escalation is gated on ownship's own GPS ground-track accuracy
  (<30m) - a Stratux with a poor GPS fix or no fix at all will simply
  never escalate via CPA (distance/altitude alerting is unaffected).
- This feature was not deployed to, or validated against, real live
  traffic on physical hardware during this mission - see the checklist
  below.

## Hardware-validation checklist

Not performed during this implementation-only mission (see this
mission's own explicit "no deployment" constraint). Before enabling
`escalationEnabled` on a real device:

- [ ] Confirm the dashboard's CPA fields render correctly on a physical
      iPad/iPhone, portrait and landscape (this mission verified layout
      reuse and JavaScript syntax only, not physical rendering).
- [ ] Confirm live GPS ground-track accuracy is actually reported below
      30m in normal flight, so CPA computation is not silently gated off
      by `isGPSGroundTrackValid` far more often than expected.
- [ ] Confirm against real or simulated converging traffic that an
      escalation actually fires, uses the correct audio path, and never
      duplicates the existing distance/altitude alert.
- [ ] Confirm `Vvel`'s known ambiguity (see "Known limitations") does
      not produce misleading vertical-prediction UI text in practice for
      the traffic sources this device actually receives (1090 ES, UAT,
      OGN/FLARM, AIS).
- [ ] Confirm settings persist correctly across a reboot and a
      Configuration Backup round trip on real hardware.

## Rollback plan

Set `escalationEnabled: false` via `/setTrafficCPASettings` (or the
dashboard checkbox) - CPA fields continue to be computed and shown for
awareness, but immediately stop affecting any alert's tier; no reboot
required. To remove the feature entirely, revert this branch's commits;
no other subsystem's own data or settings are touched by doing so
(TrafficCPASettings is its own file; AlertSettings/AutoRecordSettings
are never modified in shape).

## Supplemental/non-certified disclaimer

> Closure-rate and closest-point-of-approach values are estimates
> derived from received traffic data. They may be delayed, incomplete,
> inaccurate, or unavailable. This is supplemental, non-certified
> situational-awareness information only and must not be used for
> maneuver guidance, collision avoidance, or separation assurance.

This is shown verbatim on the dashboard's CPA settings panel and is
consistent with, not a duplicate or contradiction of,
`alerting.Disclaimer` (shown on every alert and on the page's own
"Operational Alerting" panel already).
