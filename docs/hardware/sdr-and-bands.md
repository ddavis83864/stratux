# SDR Dongles & Band Assignment

Stratux uses generic **RTL2832U** RTL-SDR USB dongles (typically with R820T/R820T2 tuners,
or any librtlsdr-compatible tuner). The tuner type is queried and logged but not gated, so
any working RTL-SDR should function. SDR handling lives in `main/sdr.go`; the assignment
decision itself is implemented as a pure, unit-tested function in the `sdrassign` package
(`sdrassign/assign.go`, `sdrassign/assign_test.go`).

Up to **three** dongles are used simultaneously, each demodulated by a different backend:

| Band | Frequency | Device tag | Demodulator |
|---|---|---|---|
| 1090ES (ADS-B) | 1090 MHz | `ES` | `dump1090` (FlightAware fork, supervised subprocess) |
| UAT | 978 MHz | `UAT` | `godump978` (in-process via `libdump978.so`) |
| OGN / FLARM | 868 MHz | `OGN` | `ogn-rx-eu` (prebuilt binary in `ogn/`) |
| AIS (marine) | 161/162 MHz | `AIS` | `rtl_ais` |

A minimal US dual-band setup uses exactly two dongles: one tagged for 978 UAT and one tagged
for 1090 ES, with OGN and AIS disabled.

## Why two identical, untagged dongles cannot be assigned safely

An untagged RTL-SDR dongle is indistinguishable from any other untagged RTL-SDR dongle - there
is nothing in the hardware or the OS that says "this one is wired to the 978 antenna." The only
thing available to tell two of them apart at runtime is the transient index libusb happens to
enumerate them in, and that index is **not** guaranteed stable across reboots or replugs: it
depends on kernel/USB enumeration order, which can change between boots even with nothing
physically moved.

If Stratux picked an assignment for two untagged dongles by enumeration order (or, as it did
historically, by ranging over a Go map - whose iteration order is deliberately randomized by
the language), the practical effect is the same either way: which physical dongle ends up
serving 978 vs. 1090 can silently change from one boot to the next, with no indication to the
pilot other than traffic or weather quietly not working. Stratux now refuses to guess in this
situation: when two or more enabled bands are competing for two or more untagged dongles, both
bands are reported **ambiguous** and neither is assigned, rather than picking one arbitrarily.

## How the 978 and 1090 SDRs are identified

Each dongle can be tagged by writing a string into its **EEPROM serial number**, which Stratux
reads at startup to decide which band the dongle serves. The scheme (matched by regex in
`sdrassign.Assign`, tolerant of a truncated "stratux", e.g. `stx:978:0`):

| Serial prefix | Band |
|---|---|
| `stratux:1090` | 1090ES |
| `stratux:978` | UAT |
| `stratux:868` | OGN |
| `stratux:162` | AIS |

A tagged dongle is **only ever** assigned to its declared band. It is never reassigned to a
different band, even if its own band is disabled, and even across reboots.

## How to tag each receiver

Program the tag with **`debian/sdr-tool.sh`** (run on the Pi), which wraps `rtl_eeprom` and
writes `stx:<freq>:<ppm>` (it offers an 868/978/1090 menu). The `:<ppm>` suffix lets you store a
per-dongle clock correction; if absent, the `PPM` setting is used as the fallback.

Tag receivers **one at a time**: the tool asks you to confirm only one SDR is plugged in before
writing, because `rtl_eeprom` addresses whichever dongle it finds first and cannot tell two
untagged dongles apart either. Plug in only the 978 dongle, tag it, then shut down, swap in the
1090 dongle, and tag it - the script offers to do this swap for you at the end of each run.

> **Note:** `debian/sdr-tool.sh` also offers a "Fall Back" mode that writes `stx:0:<ppm>` to the
> 1090 dongle. This tag is not currently matched by any band-specific behavior in `main/sdr.go`
> or `sdrassign` - a dongle carrying it is simply treated as untagged/anonymous. Do not rely on
> it to provide automatic 978 fallback; tag both dongles explicitly instead.

## Assignment algorithm

`sdrassign.Assign()` is a pure function of the discovered devices and the enabled bands - it
has no dependency on RTL-SDR hardware and produces the same result for the same logical device
set regardless of the order devices are discovered in. `main/sdr.go`'s `configDevices()` calls
it, then starts a demodulator for each device it assigns. Two passes:

1. **Tagged pass** - every device carrying a recognized `stratux:<freq>` tag is bound to its
   declared band, in ascending device-index order. A device tagged for a band that is disabled,
   or a duplicate tag for a band that's already filled, is excluded from the pool below rather
   than reassigned - conflicts are reported, not silently resolved. An unrecognized frequency
   (e.g. a typo) is reported and the device is left unused rather than guessed at.
2. **Anonymous pass** - among the untagged devices left over, remaining enabled bands are
   filled only when the outcome is unambiguous:
   - **One** enabled band still needs a receiver and **one or more** untagged devices exist:
     the lowest-index device is assigned deterministically (any extras are left unused).
   - **Two or more** enabled bands still need a receiver and **any** untagged devices exist:
     every affected band is reported **ambiguous** and none of them are assigned.
   - **No** untagged devices exist for a band that still needs one: that band is reported
     enabled-but-not-detected (a plain missing-hardware state, not ambiguity).

## Verifying assignment in the status page

The Status page shows a `978 UAT Receiver` and `1090 ES Receiver` badge next to the message
counters, each backed by the `/getStatus` JSON fields below (also pushed over the `/status`
WebSocket):

| Field (`UAT_*` / `ES_*`) | Meaning |
|---|---|
| `Enabled` | The band is turned on in Settings. |
| `Detected` | A compatible SDR was found for this band (tagged, or an available untagged spare). |
| `Assigned` | A receiver is bound to this band. |
| `DeviceSerial` / `DeviceIndex` | Identity of the bound receiver (empty/`-1` if unassigned). |
| `AssignmentSource` | `"tagged"`, `"anonymous"`, or `"none"`. |
| `Ambiguous` | Two or more untagged dongles could serve this band and Stratux could not tell them apart - **tag your SDRs**. |
| `Conflict` | More than one device is tagged for this band; the first is used, the rest are ignored. |
| `DecoderRunning` | The demodulator (`dump1090` for 1090, the in-process 978 decoder) is currently believed to be running. Distinct from `Assigned`: `dump1090` is a supervised subprocess that briefly restarts after a crash, during which the receiver stays assigned but the decoder is not running. |
| `Receiving` | At least one valid message was decoded in the last 60 seconds. |
| `Degraded` | Enabled but not fully healthy (`!Assigned \|\| !DecoderRunning`). |
| `DiagnosticReason` | Human-readable explanation shown in the UI. |

A badge of **Disabled** (gray) is expected when the band is off - it is never shown as faulted.
**Ambiguous** or **Not Detected** / **Tag Conflict** (red) mean action is needed; **Starting**
(amber) is a transient decoder-restart state; **No Traffic** (amber) means the receiver is
running but nothing has been decoded in the last minute - this is normal with no nearby RF
traffic, not necessarily a fault; **Receiving** (green) means messages are actively being
decoded.

### What an ambiguous-assignment warning means

Two or more untagged dongles are connected and two or more enabled bands need one, so Stratux
cannot determine which dongle should serve which band without guessing. Neither band is
assigned in this state - traffic and weather will not work until you tag the dongles with
`debian/sdr-tool.sh` (or remove the extras, leaving exactly one candidate per unmet band).

### What to do when one receiver is missing

If a band shows **Not Detected**, confirm the corresponding dongle is plugged in and, if
tagged, that the tag matches the table above (`rtl_eeprom -d <index>` on the Pi reads it back).
A tagged dongle that is unplugged or fails will never cause the other band's dongle to be
reassigned to cover for it - each band's status is independent.

### Verifying after a cold reboot

Because assignment is now a deterministic function of tags (or, for a single untagged spare, of
an unambiguous 1-enabled-band/1-device match), a correctly tagged dual-band setup, or a
single-band setup with one spare SDR, assigns the same way every boot. After a cold reboot,
open the Status page and confirm both badges show the expected receiver (**Receiving** once RF
traffic is present, or **No Traffic** if none is currently in range) rather than **Ambiguous**.

Traffic and weather reception remain supplemental to other means of separation and situational
awareness, and are always dependent on RF line-of-sight and coverage - a healthy **Receiving**
badge reflects that messages are being decoded, not that all nearby traffic or weather products
are guaranteed to be received.

## Gain and PPM

- **1090ES** gain comes from `Dump1090Gain` (default `37.2`).
- **UAT** uses a fixed internal tuner gain.
- **PPM** is taken from the dongle serial suffix if present, otherwise from the `PPM` setting.

## Hot reconfiguration

`sdrWatcher()` monitors for changes and reconfigures the SDR assignment on the fly when:

- the number of connected dongles changes,
- a band enable flag (`UAT_Enabled` / `ES_Enabled` / `OGN_Enabled` / `AIS_Enabled`) changes, or
- the gain setting changes.

Each external demodulator subprocess (`dump1090`, `ogn-rx-eu`, `rtl_ais`) is spawned with
`exec.Command`, monitored, and **auto-restarted on crash**. UAT is decoded in-process and
does not run as a subprocess.

See [settings-reference.md](../settings-reference.md) for `UAT_Enabled`, `ES_Enabled`,
`OGN_Enabled`, `AIS_Enabled`, `PPM`, and `Dump1090Gain`, and [http-api.md](../http-api.md) for
the full `/getStatus` schema.
