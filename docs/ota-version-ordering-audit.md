# OTA version-ordering audit and recommendation (Workstream M)

This is a bounded audit + recommendation only. Per the mission's own instruction, version-order
enforcement is **not implemented** here unless proven necessary for the SSH host-key fix's own
safety — it is not.

## Current behavior (as-is, `origin/master`)

- Version comes from the uploaded `.deb`'s own `dpkg-deb -f ... Version` field, not a filename or
  a client-supplied field (`main/ota.go`, `inspectDebPackage`).
- **No version-ordering comparison exists anywhere in the running code.** The daemon never
  compares the uploaded package's version against the currently-installed version. This is
  already honestly documented in `docs/known-limitations.md`.
- What *does* exist is a **hash/commit identity** check (`ota/decide.go`) — SHA-256 and embedded
  commit equality, used to detect corruption of the staged file across a reboot and confirm the
  install ran the file that was staged. This is a self-consistency check, not an
  authenticity/provenance check, and the code's own comments don't conflate the two.
- **Equal-version reinstall**: allowed unconditionally, and is in fact this project's normal case
  — the semantic package version deliberately does not change per commit, so re-uploading "the
  same version" with a different build is routine, gated only by commit-identity matching.
- **Downgrade**: allowed unconditionally, with no confirmation step in either the API or the
  dashboard UI. `dpkg -i --force-depends` does not itself refuse a downgrade.
- **Rollback**: restores a specific, previously-recorded backup path unconditionally. It never
  re-compares versions — rollback is triggered by dpkg health / attempt-count / commit mismatch,
  never by version ordering, and the restore itself is purely path-based.
- **Malformed/missing version**: an uploaded package with an empty `Version:` field is rejected
  at upload time (HTTP 400) before staging. A missing/malformed `ExpectedVersion` in `state.json`
  does not fail to load (only an unrecognized `Stage` value does) — it just flows through as an
  empty string, which only affects one internal resume-shortcut comparison, falling back to a
  normal retry path rather than erroring.
- **No prerelease-ordering-sensitive logic (`~rc1` vs `~rc2` vs stable) exists at runtime at all**
  — that translation exists only in `scripts/getversion.sh`, a build-time-only script.
- **Authenticity is not established independently of the checksum.** Both `ExpectedSHA256` and
  `ExpectedVersion`/`ExpectedCommit` are derived from the very file the client just uploaded, at
  upload time — there is no GPG/code-signature verification anywhere in the OTA path, and the
  `/updateUpload` endpoint has no additional auth/session middleware beyond whatever protects the
  management interface generally. In practice, whoever can reach that endpoint controls both the
  "package" and its own "expected" metadata simultaneously, so the checksum's real value is
  detecting *later* corruption/tampering of an already-accepted file across a reboot — not
  authenticating who produced it.
- **No existing tests** cover version-ordering, downgrade, or reinstall scenarios specifically.

## Why this is not a blocker for the SSH host-key fix

The SSH host-key fix does not touch the OTA path, install any new OTA-relevant metadata, or rely
on version-ordering for its own correctness in any way — it ships only in the image build, never
in the `.deb` (verified: no reference to it anywhere under `debian/`, `ota/`, or `main/ota.go`).
Nothing about the fix's own safety depends on OTA refusing an older or unrelated package.

## Recommendation (bounded, non-implementing)

1. **Default policy if this is ever implemented**: refuse a downgrade by default, using
   `dpkg --compare-versions <installed> gt <uploaded>` (Debian's own, already-correct-for-this-
   project comparison semantics — no custom parser needed) as the gate, computed server-side from
   the currently-installed package's own recorded version vs. the uploaded package's own
   `Version:` field. Equal-version reinstall should remain allowed unconditionally (it is this
   project's normal, everyday case, per the commit-identity design already in place).
2. **Downgrade should require a second, explicit confirmation step** in the dashboard (e.g. a
   distinct "yes, install this older version anyway" action, not the same single click as a
   normal update) rather than either a silent block or a silent allow — an owner may legitimately
   need to downgrade to recover from a bad update, and the emergency-rollback path already exists
   and must never be gated behind this same confirmation (rollback is a different, already-audited
   code path entirely — see above — and should stay untouched).
3. **Any future enforcement must not interfere with the existing rollback state machine.**
   Rollback's own restore step is (correctly) unconditional and path-based; a version gate belongs
   only at the initial upload/stage decision point (`main/ota.go`'s `handleOTAUploadRequest`),
   never inside `ota/decide.go`'s failure-recovery transitions.
4. **API/dashboard implications**: the upload endpoint would need to expose the currently-
   installed version in its response/preflight so the dashboard can decide whether to show the
   extra confirmation before the user even uploads a file, and the upload response itself would
   need a new distinguishable status (e.g. `"downgrade_confirmation_required"`) rather than
   folding into the existing error paths.
5. **Required tests, if implemented**: same-version reinstall still succeeds unchanged;
   older-version upload without confirmation is refused; older-version upload with confirmation
   proceeds; a malformed/missing installed-version comparison target fails safe (refuses, does not
   silently allow); rollback behavior is provably unaffected (existing rollback tests continue to
   pass unmodified).
6. **Authenticity is a separate, larger gap this recommendation does not attempt to close.**
   Version-order enforcement would not, by itself, establish authenticity/provenance of an
   uploaded package — that would require a separate mechanism (e.g. signature verification against
   a project-controlled key) and is explicitly out of scope for this recommendation. This
   document does not claim checksum-based integrity checking is authentication, and any future
   implementation must preserve that distinction in its own code comments and docs.
7. **Stable-release blocker determination**: **not a blocker for this release.** This is a
   long-standing, already-honestly-documented limitation (`docs/known-limitations.md`), unrelated
   to and untouched by the SSH host-key fix, and its absence does not create a new regression.
   Whether to require it before promoting to stable `v2.0.0` is a separate product decision for
   the repository owner, outside this audit's scope to make.
