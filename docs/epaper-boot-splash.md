# ARS e-paper splash: approved artwork, production asset, acceptance gate

> **Status: `ARS_EPAPER_SPLASH_PHYSICAL_ACCEPTANCE_PASSED_BOOT_INTEGRATION_READY`.
> The manual splash render and the hand-off back to the operational display
> were physically validated by the owner on the real Stratux Pi and
> Waveshare 4.2" V2 panel on 2026-09-23 (see
> [Acceptance record](#acceptance-record)). The splash is not yet wired into
> boot.**

This covers only the Waveshare **4.2" V2** panel (400×300; see
[waveshare-epaper-display.md](waveshare-epaper-display.md)). It is a
supplemental branding image, not flight information.

## Files

| File | Role |
|---|---|
| `epaper/splash/assets/source/ars-splash-source.png` | **Approved artwork - the visual source of truth.** 1448×1086 (exactly 4:3), opaque. The rectangular outer frame was removed from it by owner decision (see [Source artwork: frame removal](#source-artwork-frame-removal)). Never edited by the generator. |
| `epaper/splash/assets/ars-splash-400x300.bin` | **Production asset**, the only file the runtime consumes (embedded into `epaperd`). |
| `epaper/splash/assets/ars-splash-400x300.preview.png` | Human-review rendering of the `.bin` (1-bit PNG). Derived; never read at runtime. |
| `epaper/splash/assets/CHECKSUMS.sha256` | SHA-256 of the source and the `.bin` (`sha256sum -c` format). |
| `epaper/splash/splash.go` | The converter and validator (`splash.Convert`, `splash.Validate`). |
| `epaper/splash/cmd/splashgen/` | The generator command. |
| `epaper_main/splash.go` | The one-shot renderer behind `epaperd -splash`. |

The splash is the oval ARS border, mountains, evergreen tree line, swoosh
elements, "AERIAL" and "RESPONSE SYSTEMS" - and nothing else: no rectangular
frame, no tagline, no additional text or branding. Apart from the frame
removal below, the artwork is as supplied: nothing was redrawn, and it is not
regenerated from the color logo. The old "ADVANCED TECHNOLOGY FOR FIRST
RESPONDERS" line is absent from the source and must stay absent.

## Source artwork: frame removal

The artwork as first supplied had a thin rectangular frame around the whole
image (8 px thick in the 1448×1086 source, 12-21 px in from each edge). By
owner decision it is not part of the ARS presentation and was removed **from
the authoritative source image**, not from the generated bitmap; the
production asset was then regenerated through the normal pipeline.

Exact change: every pixel within **25 px of any edge** of the source was set to
pure white (255,255,255). The frame, including its anti-aliased fringe, lies
entirely inside that band; the band's inner neighbours (22-25 px) are empty,
and the oval's first content begins at 26 px, so no oval pixel was touched.
Image size (1448×1086), color mode (RGB, opaque) and every pixel more than
25 px from an edge are unchanged (verified bit-for-bit when the edit was
made). Nothing was scaled, cropped, or moved, so the composition and the
oval's size and position are exactly as approved.

| | SHA-256 |
|---|---|
| Original supplied artwork (with frame) | `20eca3427883ef92056a335e4c2fea37599fd22535560c208866dfcb1859eac4` |
| Current source (frame removed) | `ed1311aa9af0fbdc03991e76297719103151919a18ffa66d8134cfdba6492395` |

The original is not stored in the repository. To reproduce the current source
from it (Python with Pillow and numpy; the PNG bytes may differ by encoder,
the pixels will not):

```python
import numpy as np
from PIL import Image
a = np.array(Image.open("original.png").convert("RGB"))
H, W, _ = a.shape; B = 25
a[:B, :] = 255; a[H-B:, :] = 255; a[:, :B] = 255; a[:, W-B:] = 255
Image.fromarray(a, "RGB").save("ars-splash-source.png", optimize=True)
```

Effect on the bitmap: exactly 2,720 pixels changed, all black to white, all
within 5 px of the panel edge; every other pixel is identical to the
previously generated bitmap.

## Production format

Confirmed against the validated driver (`epaper_main/driver_4in2v2.go`,
`epaper.NativeDimensions(waveshare-4.2in-v2)`):

- 400×300 px, native landscape orientation (`EpaperRotation` 0)
- 1 bit/pixel, MSB-first, row-major, 50 bytes/row × 300 rows = **15000 bytes**
- **bit 1 = white, bit 0 = black** (the driver's own `blankBitmap` is `0xFF`)
- no header, no alpha, no grayscale, no padding bits (400 is a multiple of 8)

This is exactly what `PanelDriver.Update` takes, so the runtime does no
conversion: it hands the embedded bytes to the driver.

## Conversion (what is and isn't done)

1. Composite over white (a no-op for this opaque source), take luminance
   (BT.601 integer weights).
2. Scale proportionally to fit 400×300 with **exact area averaging** (a true
   box filter in integer arithmetic). This source is exactly 4:3, so it fills
   the panel with no padding; a non-4:3 source would be centered on white.
3. Threshold at 50% luminance. Because step 2 is a true average, 50% keeps
   every stroke at its proportional width. **No dithering**, no sharpening,
   no gray.

With the frame gone the oval is the outermost element: the artwork's black
bounding box is x 7-391, y 20-277, so at least 6 px of white remains at every
panel edge (asserted by a test) and nothing is clipped. The source is still
4:3 and is scaled uniformly, so the aspect ratio is unchanged.

Everything is integer arithmetic: no floats, no third-party imaging code, no
map iteration. The output is byte-identical for the same source on any
platform and Go version. (As a one-time check when this was written, an
independent floating-point implementation of the same area-average and
threshold was compared with the committed `.bin`: 0 of 120,000 pixels
differed. That check is not part of the repo.)

## Regenerate

From the repository root (needs only Go; no Docker, Pi, or network beyond
module download):

```sh
make epaper-splash            # = go run ./epaper/splash/cmd/splashgen
make epaper-splash-check      # = go run ./epaper/splash/cmd/splashgen -check
```

`epaper-splash` rewrites the `.bin`, the preview PNG and `CHECKSUMS.sha256`
from `source/ars-splash-source.png`. `-check` writes nothing and exits
non-zero if the committed `.bin` differs from a fresh conversion or the
checksum file is stale. To adopt a newly approved artwork, replace
`source/ars-splash-source.png`, run `make epaper-splash`, review the
preview, and commit all four files together - then repeat the physical
acceptance gate.

Current checksums (also in `CHECKSUMS.sha256`; verify with
`cd epaper/splash/assets && sha256sum -c CHECKSUMS.sha256`):

```
ed1311aa9af0fbdc03991e76297719103151919a18ffa66d8134cfdba6492395  source/ars-splash-source.png
51039f8a375a2ecc44ed25fe7b6f373b31e7695e7a854bd192d605695b5b73dc  ars-splash-400x300.bin
```

The preview PNG is deliberately not checksummed: PNG compression output is
not guaranteed stable across Go versions, whereas the source and the packed
bitmap are. A test instead requires its decoded pixels to equal the `.bin`.

## Automated validation

`go test ./epaper/splash/...` (also run by `go test ./...`) proves, for the
committed asset:

- exactly 400×300 at 1 bit/pixel (15000 bytes, stride 50)
- not blank; black and white populations both substantial (33,047 black /
  86,953 white, 27.5% black), and not inverted (black fraction must lie in
  10-50%)
- **upright**: 40×30 block means match the source, and each of mirrored,
  upside-down, 180° and inverted variants is at least 3× worse
- **no rectangular frame and no clipping**: the outer band (6 px left/right,
  15 px top/bottom) is entirely white
- **oval border complete**: white flood-filled in from the panel edge cannot
  reach the interior (no gap in the closed curve), and the double-line oval is
  present at all four sides
- the source's outer 25 px band is clean white while the oval artwork inside
  it is intact
- source is opaque; preview PNG is an opaque 2-color image with identical pixels
- regeneration is byte-identical to the committed `.bin` (twice), and both
  SHA-256 entries match the files

The tests were checked to fail on a vertically flipped, mirrored, inverted and
single-byte-corrupted `.bin`, on a `.bin` with a frame drawn back in, on a
`.bin` with gaps cut in the oval, and on the original framed source.

These tests cannot judge how the panel renders the image. That is the
physical gate.

## Physical acceptance gate

**Passed on 2026-09-23** (owner-performed on real hardware; evidence in the
[Acceptance record](#acceptance-record)). This section is kept as the
procedure that was run, and must be repeated if the artwork changes.

The renderer is a manual, run-to-completion mode of `epaperd`:
`Init → Clear → one full refresh → Sleep`, then it releases SPI/GPIO and
exits. Nothing calls it at boot.

### Get a binary that has `-splash`

The installed `epaperd` predates this option. Use a separate test binary so
nothing installed is modified. `epaperd` is pure Go, so cross-compiling
needs no cgo toolchain:

```sh
# on a dev machine, from the repo root
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o epaperd-splash-test ./epaper_main/
scp epaperd-splash-test <pi>:/tmp/
```

(Or `make epaperd` on the Pi itself.)

### Run it (on the Pi)

Prerequisite: the panel is wired per
[waveshare-epaper-display.md](waveshare-epaper-display.md) and has already
been validated with the normal status display (`EpaperEnabled` true,
`EpaperPanel` `waveshare-4.2in-v2`).

```sh
# 1. Release the panel from the normal service.
sudo systemctl stop stratux_epaper

# 2. Render the approved splash once.
#    Add `-splash-rotation 180` if EpaperRotation is 180 on this unit.
sudo /tmp/epaperd-splash-test -splash
echo "exit=$?"        # expect 0

# 3. Renderer released the hardware?
pgrep -a epaperd-splash-test || echo "renderer not running"

# 4. Hand the panel back to the normal display.
sudo systemctl start stratux_epaper
```

Step 2 flashes twice (a clear, then the image) and prints
`done: splash drawn, panel asleep` followed by
`released SPI/GPIO; this process no longer owns the panel`. If it is run
while the service is up, it refuses (exit 2) rather than fight for the bus;
`-splash-force` overrides that and should not normally be used. Refusals
(unsupported panel or rotation, service running) happen before any hardware
is touched. Exit codes: 0 done, 1 hardware/driver failure, 2 refused.

Inspect the panel after step 2, before step 4, then after step 4 (the
service initializes the panel on its first poll cycle, typically within
~20 s, showing "Starting..." and then the status page). Confirm the service
owns the panel again:

```sh
systemctl is-active stratux_epaper                      # active
cat /run/stratux-epaper/status.json                     # state RUNNING, panelDetected true
```

### Checklist

Result of the 2026-09-23 physical test. Only what the owner actually
reported is marked PASS; anything not separately reported is marked as such
rather than inferred.

| # | Check | Result |
|---|---|---|
| 1 | Entire oval border visible (double line, unbroken, all four sides) | PASS (owner: "complete ARS oval visible") |
| 2 | Mountains recognizable | PASS |
| 3 | Tree line distinct | PASS (owner: "evergreen tree line distinct") |
| 4 | "AERIAL" sharp and readable | PASS (owner: "clearly readable") |
| 5 | "RESPONSE SYSTEMS" sharp and readable | PASS (owner: "readable") |
| 6 | Swoosh elements distinct | PASS (owner also noted the lower graphic elements visible) |
| 7 | No clipping, and no rectangular frame around the display | PASS (owner: "no clipping", "no unwanted rectangular outer frame", appropriate white margin) |
| 8 | No stretching (oval looks like the approved artwork's oval) | Not separately reported; owner reported overall composition appropriate for the panel |
| 9 | Correct orientation (upright, not mirrored) | PASS |
| 10 | Acceptable contrast | PASS |
| 11 | Acceptable ghosting | Not separately reported |
| 12 | No corrupted display regions | PASS (owner: "no visible framebuffer corruption") |
| 13 | Step 2 exited 0 and printed the "released SPI/GPIO" line | PASS (`exit=0`, both closing lines printed) |
| 14 | No renderer process left running (step 3) | PASS (`renderer not running`) |
| 15 | After step 4, the normal Stratux display took over (status RUNNING) | PASS (`active`; status file RUNNING, `panelDetected` true, 0 failures). The owner declared takeover PASSED; the status file and journal are the recorded evidence, and the status page's on-panel appearance was not separately described |

Owner's overall disposition: visual acceptance **PASSED**; operational
takeover acceptance **PASSED**.

Things to watch: the thinnest strokes (the oval's outer lines are only 1-2 px
wide at this resolution, and the oval sits about 7 px from the left/right
panel edges) and the small "RESPONSE
SYSTEMS" text. If any of 1-12 fail, do not adjust the artwork ad hoc: record
what was seen and revisit the conversion (threshold or scale) as a deliberate,
re-tested change.

### Known limitations

- **Rotation:** `-splash` supports 0 and 180. 90/270 are refused: the artwork
  is 4:3 landscape and a portrait layout would need its own approved image.
- **Panel:** 4.2" V2 only. The 3.7" panel is refused.
- The hardware path (`gpiobus.go`) is the already-validated status-display
  code. The splash-specific logic above it is unit-tested against a fake bus
  (exact bytes to controller RAM, deep sleep last, release on every path) and
  has now also driven the real panel (2026-09-23, see the acceptance record).

## Acceptance record

### Manual splash render + operational takeover (physical)

| Field | Value |
|---|---|
| Physical acceptance | **PASSED** (manual render and takeover) |
| Date | 2026-09-23 |
| Unit | The owner's real Stratux Raspberry Pi with the Waveshare 4.2" V2 panel, `stratux_epaper` previously validated on it |
| Build | `epaperd-splash-test`, built by the owner from this branch's working tree with the frame-removed artwork (no frame was observed on the panel) |
| Tester | Owner |
| Visual inspection | Owner inspected and photographed the real panel; all reported observations are in the checklist above |

Sequence and evidence, as reported by the owner:

1. `stratux_epaper.service` was `active (running)` before the test.
2. `sudo systemctl stop stratux_epaper`, then `systemctl is-active stratux_epaper` → `inactive`.
3. `sudo /tmp/epaperd-splash-test -splash` printed, in order:
   ```
   initializing panel...
   clearing panel (full refresh)...
   drawing ARS splash (full refresh)...
   done: splash drawn, panel asleep
   released SPI/GPIO; this process no longer owns the panel
   ```
   `exit=0`; `pgrep -af epaperd-splash-test || echo "renderer not running"` → `renderer not running`.
4. `sudo systemctl start stratux_epaper`, then `systemctl is-active stratux_epaper` → `active`.
5. Runtime status afterwards (`/run/stratux-epaper/status.json`):
   ```json
   {"updatedAt":"2026-09-23T15:58:20.071434843Z","state":"RUNNING",
    "configuredPanel":"waveshare-4.2in-v2","panelDetected":true,
    "lastSuccessfulRefresh":"2026-09-23T15:58:10.403534756Z",
    "consecutiveFailures":0,"busyTimeoutCount":0,
    "fullRefreshCount":1,"partialRefreshCount":0,
    "dataAgeSeconds":0.016187278}
   ```
6. Journal:
   ```
   Sep 23 15:51:21 stratux systemd[1]: Stopping stratux_epaper.service...
   Sep 23 15:51:21 stratux systemd[1]: stratux_epaper.service: Deactivated successfully.
   Sep 23 15:51:21 stratux systemd[1]: Stopped stratux_epaper.service...
   Sep 23 15:58:00 stratux systemd[1]: Started stratux_epaper.service...
   ```
   No SPI, GPIO or display errors were observed.

What this proves: operational renderer → clean stop → splash acquires the
panel → full-refresh ARS splash → panel sleep → SPI/GPIO release → splash
process exits → operational renderer starts → reacquires the panel →
successful normal refresh.

Items 8 (no stretching) and 11 (ghosting) were not separately reported; the
owner's overall visual acceptance stands. The approved artwork is frozen:
any change to it repeats this gate.

This validates the **manual** path only. It does not validate boot
integration; see the cold-boot gate in the boot-integration section.
