# Release process

How this fork versions, builds, verifies, and promotes releases. Written when the first
release candidate (`v2.0.0-rc1`) was cut; update this file itself whenever the process
changes, rather than letting it drift from what actually happens.

## Versioning

`scripts/getversion.sh` is the single source of truth: it runs `git describe --tags
--abbrev=0` (the nearest reachable tag) and strips a leading `v`. There is no separate
hardcoded version constant anywhere in the tree — the Makefile's `LFLAGS` embeds this
script's output as both `main.stratuxVersion` (dashboard/API-visible) and the Debian
package's own `Version:` field, so they can never disagree with each other by construction.

Tag shape: `vMAJOR.MINOR.PATCH[-rcN|-betaN|-alphaN]`, e.g. `v2.0.0-rc1`, `v2.0.0`.

**Git tag vs. package version differ mechanically for a prerelease**, and this is
deliberate: git tag names cannot contain `~`, but Debian's version-comparison algorithm
needs a `~` (not a `-`) for a prerelease to correctly sort *before* its own eventual final
release — verified directly with `dpkg --compare-versions` (see `getversion.sh`'s own doc
comment for the exact evidence). So:

| | Example |
|---|---|
| Git tag (human-facing, release name, filenames) | `v2.0.0-rc1` |
| Package/dashboard/API version (`getversion.sh` output) | `2.0.0~rc1` |

A final, non-prerelease tag (`v2.0.0`) is returned unchanged — nothing to translate.

The project's older, pre-fork `X.Y-preN` tag convention (e.g. `v2.0-pre5`, upstream's own
last preview before their 2.0) is left untouched by this translation — it is a distinct,
already-deployed historical format, not something this fork's versioning changes
retroactively.

## Release candidate vs. stable

An **RC** (`-rcN` suffix) is a release that has passed every automated gate and at least one
real-hardware validation pass, but has not yet completed its acceptance/soak period (see
below). A **stable** release (no suffix) is an RC that has completed that period with no
unresolved critical/high defect. Never call an RC "stable" before that happens.

## Build and artifact pipeline

- **`.github/workflows/ci.yml`** — every push and pull request, native `arm64` GitHub-hosted
  runner (`ubuntu-24.04-arm`), no QEMU. Two jobs: an integration check against the merged PR
  state, and a `build-artifact` job pinned to the exact durable commit being tested, which
  uploads the `.deb` as a workflow artifact.
- **`.github/workflows/release.yml`** — triggered by pushing a tag matching `v*.*`, or
  manually via `workflow_dispatch`. Builds both the `.deb` and a full SD-card image via
  `image_build/build.sh` (which wraps the `pi-gen` submodule — the official Raspberry Pi OS
  image builder, used so this project never has to maintain its own from-scratch image
  builder), and publishes a **draft** GitHub Release with both attached, plus the
  checksum/SBOM/provenance files this release process adds (see
  [artifact-verification.md](artifact-verification.md)).
- Every artifact traces to an exact commit: the `.deb`'s embedded `main.stratuxBuild` is the
  full commit SHA (`-X main.stratuxBuild=$(git log -n1 --pretty=%H)`), independent of the
  version string.

## RC acceptance / stable-promotion plan

Recommended minimum bar before promoting an RC line to stable (owner approves each
promotion; this is a documented plan, not an automatic trigger):

1. **At least one meaningful bench soak** — a multi-hour-or-longer unattended run on real
   hardware after the RC's own initial validation, watching for anything a short validation
   window wouldn't catch (thermal drift, storage growth, slow leaks).
2. **At least one real-flight acceptance session** — the RC exercised in its actual intended
   environment, not just on the bench.
3. **No unresolved critical/high-severity defect** open against the RC.
4. **No data-loss or network-lockout defect** found during the soak — an absolute bar, not a
   severity judgment call.
5. **Successful OTA upgrade** from the previous stable (or, for this first release, from the
   pre-fork upstream baseline) to the RC, verified exactly as this RC's own release notes
   document.
6. **Successful clean installation** from the public image, independently boot-tested.
7. **Successful rollback/recovery verification** — confirmed the documented rollback path
   actually restores a known-good state.
8. **Explicit owner approval** — promotion is a decision, not a checklist auto-pass.

### If an issue is found before promotion

Classify by severity (critical/high/medium/low), fix following the project's own
defect-handling discipline (reproduce, root-cause, regression test, smallest correction, full
gates, rebuild affected artifacts), and cut a **new** RC tag (`rc2`, `rc3`, ...) from the
corrected commit. **Never move or overwrite a published tag or release** — a corrected build
is always a new, distinctly-numbered artifact, so anyone who already has `rc1` can tell
exactly what changed.

### Required final regression suite before stable

The complete automated gate this RC itself ran (see this RC's own release notes for the
exact list) must be green on the exact commit being promoted, plus:
- A fresh upgrade test from the currently-deployed RC to the candidate stable build.
- A fresh clean-install boot test from the candidate stable image.

### Are stable artifacts rebuilt, or promoted unchanged?

**Rebuilt from a new stable tag**, never repurposed from an RC's own artifacts unchanged —
this keeps "which exact tag produced this file" unambiguous for every published artifact,
and matches this project's own "every artifact traces to an exact commit" rule. If an RC's
own commit is promoted completely as-is with zero further changes, the stable tag simply
points at that same commit and the build is repeated from it (not copied), so the stable
release's own checksums are independently regenerated rather than inherited.

## Issue intake

Report issues against this repository's own GitHub Issues (not upstream `stratux/stratux`,
whose issue tracker is for the unmodified baseline). Include the exact version/commit
(`getStatus` API or the dashboard footer), what hardware was in use, and — for anything
Wi-Fi/network/OTA-related — whether the device was ever left unreachable and how it was
recovered.
