# Real-flight acceptance checklist

A short, owner-executed checklist for accepting a specific build of this fork (a release
candidate, or a change under evaluation before it's trusted day-to-day) during an actual
flight. It is a record of what was observed, not a new procedure to fly by.

> **This checklist is supplemental only.** It never replaces the aircraft's required
> preflight inspection, checklists, or the pilot in command's own see-and-avoid and
> situational-awareness duties - see the
> [README's full disclaimer](../README.md#safety-certification-and-operational-disclaimer).
> Every item here is either a **ground-only** check (done with the engine off, before or
> after the flight itself) or a **glance-only** check meant to take a couple of seconds at a
> natural pause (established cruise, before/after a frequency change) - never something to
> read continuously or use in place of looking outside. If any item conflicts with flying the
> aircraft or maintaining a visual scan, skip it and note that in the evidence log below.
> Nothing on this device is certified, and nothing in this checklist changes that.

## How to use this

1. Print or copy the evidence template below before the flight.
2. Work through each section at the point in the flight named by its heading - most items
   are ground-only, and every airborne item is a single glance, not a monitoring task.
3. Mark each line **PASS**, **FAIL**, or **N/A** (with a one-line reason for N/A).
4. Any **FAIL** on an item marked **(blocking)** means: do not rely on this device for that
   function this flight, and stop using this checklist to "accept" the build - file it as a
   defect instead (see [reporting](../README.md) / open a repository issue with the exact
   version and evidence captured).
5. Keep the filled-in evidence log with the flight/maintenance record for this device.

## Before engine start (ground, power already on)

| # | Check | How | Blocking? |
|---|---|---|---|
| 1 | Correct build is running | Dashboard footer or `/getStatus` shows the exact version/commit you intended to accept | Yes |
| 2 | Overall readiness is honest | Dashboard/preflight page shows `READY` or a `CAUTION`/`NOT_READY` you already understand and accept for this flight - not an unexplained state | Yes |
| 3 | GPS has a valid fix | GPS status shows a real solution (not searching/no fix) before taxi | Yes |
| 4 | AHRS/baro/fan health has no unexplained fault | No unexpected fault/error on the health page | Yes |
| 5 | Storage is healthy | No storage-pressure warning; if Automatic Flight Recording is enabled, confirm it shows armed, not an error | No |
| 6 | Wi-Fi is in its expected state | Wi-Fi Admin page shows the configuration you expect (SSID, security) - no unexpected reconnection prompt mid-checklist | No |
| 7 | No open OTA/update state | OTA status is idle - not mid-update, not `failed`/`recovery_required` | Yes |
| 8 | Known limitations reviewed | You've read [known-limitations.md](known-limitations.md) for this build and accept them for this flight | Yes |

## Taxi / departure (glance only)

| # | Check | How | Blocking? |
|---|---|---|---|
| 9 | Traffic display appears live | One glance during taxi: traffic (if any is present) updates, isn't frozen | No |
| 10 | No alert popup blocking the display | If an alert fired, it's dismissible and the underlying display is still usable | Yes |
| 11 | Ownship/ADS-B-out state (if applicable) matches expectation | One glance, no continuous monitoring | No |

## En route (one glance per item, at a natural pause)

| # | Check | How | Blocking? |
|---|---|---|---|
| 12 | Traffic/weather display remains responsive | One glance at an established point (e.g. after a frequency change) - not continuous monitoring | No |
| 13 | No unexpected reboot occurred | Dashboard still shows continuous uptime/boot ID consistent with departure - a mid-flight reboot is worth noting even if the device recovers | Yes |
| 14 | No alerting storm / stuck alert | Alerts (if any) are relevant and clear normally, not repeating or stuck | No |
| 15 | GPS/AHRS remain healthy | One glance at health status, if convenient - not required if it would take attention from flying | No |

## Arrival / shutdown (ground)

| # | Check | How | Blocking? |
|---|---|---|---|
| 16 | Device is still on the version you started with | Dashboard/`/getStatus` version/commit unchanged from item 1 (no unexpected update or rollback occurred) | Yes |
| 17 | Uptime/boot ID confirms no unexplained reboot | Compare to your own note from before engine start | Yes |
| 18 | No new failed-unit / error state accumulated | Health page has no new fault that wasn't present before engine start | Yes |
| 19 | Recording (if enabled) captured the flight | If Automatic Flight Recording was armed, confirm a recording exists for this flight's time window | No |
| 20 | Controlled shutdown completes normally | If you shut the device down deliberately, it reports a clean/expected shutdown status, not an error | No |

## Evidence template

Copy this block per flight. Never record coordinates, other people's identities, Wi-Fi
passwords, SSH credentials, or full diagnostic bundle contents here - a version string,
a status word, and a short note is enough.

```
Date (UTC):
Aircraft / device serial or label:
Build under acceptance (version/commit):
Pilot in command:

Before engine start:  items 1-8   PASS / FAIL / N/A (list any non-PASS + reason):
Taxi/departure:        items 9-11  PASS / FAIL / N/A:
En route:               items 12-15 PASS / FAIL / N/A:
Arrival/shutdown:       items 16-20 PASS / FAIL / N/A:

Any BLOCKING failure? (Y/N, and what):
Overall acceptance decision (accept this build for continued use / hold for investigation):
Notes (short, no personal data, no coordinates):
```

## What this checklist is not

- Not a certification, an airworthiness determination, or a "safe to fly" statement -
  see the [README disclaimer](../README.md#safety-certification-and-operational-disclaimer),
  which controls over anything here.
- Not a replacement for the aircraft's own required preflight inspection or checklists.
- Not a monitoring task - every en route item is a single glance at a natural pause, never
  a reason to look at a screen instead of outside.
- Not a substitute for the automated readiness/preflight model in
  [preflight-readiness.md](preflight-readiness.md), which this checklist deliberately reuses
  rather than duplicates - most "how" columns above just point at that existing page.
