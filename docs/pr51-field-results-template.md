# PR #51 field acceptance results

> **Template - fill in during/after the actual session.** Linked from
> [`docs/pr51-field-acceptance-kit.md`](pr51-field-acceptance-kit.md),
> which defines every gate's own pass/fail criterion in detail. This
> document is the compact record of what was actually observed; the kit
> is the procedure. Copy this file into the session's own `$EV/` evidence
> directory and fill it in there - do not edit this template in place
> with real results.

**Session date/time (UTC):**
**Operator(s):**
**Candidate under test:** commit `cdb4d798fec721e0a6cd5cb1543d442ac4893eb4`, package SHA-256 `1b0a8b432d68cea82a33bcebd5885bd5ea13871b3b5da19b43337961626d8f19`
**Evidence directory (`$EV`):**

Result values: **PASS**, **FAIL**, **NOT RUN**, **INCONCLUSIVE** - see the
kit's own "Result classification" section for what each means. Never
leave a row blank; if a gate wasn't attempted, say NOT RUN and why.

**A product counter or a cached file's existence alone is never recorded
as proof ForeFlight displayed weather.** Only a directly-observed
ForeFlight screen (photographed, iPad clock visible) counts for the
ForeFlight rows below - see the kit's own F1-F3 warning.

## Device / candidate identity

| Check | Time | Result | Evidence file | Reviewer conclusion |
|---|---|---|---|---|
| B1 Candidate commit matches | | | | |
| B2 Candidate package hash matches | | | | |
| B3 Device build (before) | | | | |
| B7 OTA state idle before starting | | | | |
| D1 Artifact identity on device (after) | | | | |
| D2 Boot ID changed | | | | |
| D5 OTA result clean | | | | |

## Receiver 978 MHz and FIS-B reception

| Check | Time | Result | Evidence file | Reviewer conclusion |
|---|---|---|---|---|
| L1 UAT/978 counters increasing, timestamped | | | | |
| L2 Tower reception (`getTowers`) | | | | |
| L3 FIS-B product counters increasing | | | | |
| L7 No-RF vs. receiver-defect distinction (if L1-L3 flat) | | | | |

## Cache admission, persistence, and reconnect

| Check | Time | Result | Evidence file | Reviewer conclusion |
|---|---|---|---|---|
| D7 FIS-B cache API available post-deploy | | | | |
| D8 Persisted entries reappear/remain post-deploy | | | | |
| L4 Cache admission during live reception | | | | |
| L5 Cache persistence to disk during/after | | | | |
| L6 Representative decoded product sample | | | | |
| R3 Cache persistence across a restart (later session only) | | | | |
| R4 Cache re-indexing after restart (later session only) | | | | |

## Stratux web weather display

| Check | Time | Result | Evidence file | Reviewer conclusion |
|---|---|---|---|---|
| Weather page loads, correct no-data state pre-reception | | | | |
| Category counts/badges update as products arrive | | | | |
| Freshness badges (fresh -> aging -> stale) behave correctly | | | | |
| Weather Cache page (`/fisbcache`) loads and matches API | | | | |

## ForeFlight traffic

| Check | Time | Result | Evidence file (photo, clock visible) | Reviewer conclusion |
|---|---|---|---|---|
| F1 Traffic display consistent with Stratux reception | | | | |

## ForeFlight live weather

| Check | Time | Result | Evidence file (photo, clock visible) | Reviewer conclusion |
|---|---|---|---|---|
| F2 Live weather display, observed directly on ForeFlight | | | | |

## ForeFlight weather after reconnect

| Check | Time | Result | Evidence file (photo, clock visible) | Reviewer conclusion |
|---|---|---|---|---|
| F3 Reconnect with a populated cache - time-to-display | | | | |
| F3 Freshness/age indication shown in ForeFlight itself | | | | |

## Device health, rollback readiness, and post-test state

| Check | Time | Result | Evidence file | Reviewer conclusion |
|---|---|---|---|---|
| D4 Services/package health post-deploy | | | | |
| B12 Rollback package verified on the actual field laptop | | | | |
| P1 Final health/package audit | | | | |
| P4 Rollback executed (if applicable) and verified | | | | |
| P5 FIS-B cache data not lost (final state) | | | | |

## Stop conditions triggered (if any)

| Condition | Time | Action taken | Evidence |
|---|---|---|---|
| | | | |

## Overall session classification

- [ ] All required gates PASS - candidate ready for an owner merge decision on PR #51 (still requires separate authorization; this template does not itself authorize a merge)
- [ ] One or more gates FAIL - candidate needs revision, issue(s) filed:
- [ ] Session INCONCLUSIVE overall - repeat only the affected gates in a follow-up session, not the full run

## Evidence integrity

```sh
cd "$EV" && find . -type f | sort | xargs sha256sum > post/SHA256SUMS
```

Confirm every evidence path referenced above appears in `post/SHA256SUMS`.
