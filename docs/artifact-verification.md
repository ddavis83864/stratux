# Verifying a release artifact

Every file attached to a GitHub Release of this fork should be verified before you trust or
install it. This is how.

## What's published

| File | What it is |
|---|---|
| `stratux-<version>-arm64.deb` | The installable Debian package. |
| `stratux-<version>.img.xz` | Compressed, sanitized, bootable SD-card image. |
| `SHA256SUMS` | SHA-256 of every other published file, one line each, in the standard `sha256sum` format. |
| `stratux-<version>.sbom.json` | Software bill of materials (CycloneDX JSON). |
| `stratux-<version>.provenance.json` | Build provenance: repository, tag, exact commit, workflow run, runner architecture, toolchain versions. |
| Release notes | This release's own `docs/releases/<version>.md`, also in the GitHub Release description. |

`<version>` in every published filename is the git tag with its leading `v` stripped (e.g.
`2.0.0-rc1`) - deliberately **not** the Debian package's own internal version string
(`2.0.0~rc1`, with a `~`). GitHub silently rewrites `~` to `.` in uploaded asset filenames
(confirmed by publishing a real release), which would otherwise make `SHA256SUMS`'s own
filename references not match what actually got published - so filenames use the
already-GitHub-safe git tag instead. `dpkg-deb -f <file> Version` on the downloaded package
will still correctly report `2.0.0~rc1` - that mismatch between the filename and the
package's own internal version is expected, not a defect.

## 1. Verify the checksums

```sh
sha256sum -c SHA256SUMS
```

Every listed file must report `OK`. If any file is missing or reports a mismatch, do not use
it — redownload, and if it still fails, report it rather than proceeding.

## 2. Verify the tag and commit

```sh
git clone https://github.com/ddavis83864/stratux.git
cd stratux
git tag -v v2.0.0-rc1        # only prints something if the tag is GPG-signed - see below
git show v2.0.0-rc1 --no-patch --format='%H %s'
```

Compare the commit SHA against the `provenance.json` file's own `commit` field — they must
match exactly.

**Signing:** unless a specific release's notes say otherwise, this release's tag and
artifacts are **not cryptographically signed** — verification is by checksum and provenance
comparison only, not signature. This is stated plainly rather than described as signed when
it isn't.

## 3. Verify the package

```sh
dpkg-deb -f stratux-2.0.0-rc1-arm64.deb Version Architecture
dpkg-deb -x stratux-2.0.0-rc1-arm64.deb /tmp/stratux-extract
strings /tmp/stratux-extract/opt/stratux/bin/stratuxrun | grep -o 'vcs.revision=[0-9a-f]\{40\}'
```

The embedded `vcs.revision` must equal the exact commit from step 2.

## 4. Verify the image (structural check, no private data expected)

```sh
xz -t stratux-2.0.0-rc1.img.xz   # integrity of the compressed stream
xz -d -k stratux-2.0.0-rc1.img.xz
fdisk -l stratux-2.0.0-rc1.img   # confirm partition table looks sane
```

See [clean-install-guide.md](clean-install-guide.md) for writing it to media, and this
release's own release notes for the specific sanitization checks that were run against it
before publication.

## 5. Verify the SBOM

```sh
python3 -m json.tool stratux-2.0.0-rc1.sbom.json > /dev/null   # valid JSON
```

The SBOM lists Go modules, native library dependencies, and included binaries. It should
contain no filesystem paths specific to the build machine and no credentials — if you find
either, report it; that is itself a release defect.

## If anything fails

Do not install or boot from an artifact that fails any of the above. Open an issue against
this repository with which check failed and the exact output.
