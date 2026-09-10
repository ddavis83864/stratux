# configbackup/testdata

## `legacy-pre-trafficcpa-backup.json`

An **authentic** Configuration Backup document, produced by literally
running commit `936c38e4281edf4886be4e0fe3b6c11fc2d1aa8b`'s (`master`,
the merge of PR #18, immediately before the closure-rate/closest-point-
of-approach traffic-alerting enhancement) exact `configbackup.BuildDocument`
- not hand-written or simulated. Its section-checksum key set is exactly
`{configuration, calibrationProfiles, alertSettings, autoRecordSettings}`
- no `trafficCpaSettings` key, and no such field in the document body at
all - the shape `legacy.go`'s `verifyLegacyPreTrafficCPAChecksum`
recognizes.

Regenerated the same way as `legacy-pre-autorecord-backup.json` below,
substituting the commit and output filename; its one calibration profile
is named "Legacy Backup Aircraft (pre-CPA)" for the same
name-uniqueness reason given below.

## `legacy-pre-autorecord-backup.json`

An **authentic** Configuration Backup document, produced by literally
running commit `5b8509fca1ced937a6b66f257918f04abf967a36`'s (`master`,
the merge of PR #13, immediately before Automatic Flight Recording)
exact `configbackup.BuildDocument` - not hand-written or simulated.

This is the only Configuration Backup shape that has ever existed on
this project's `master` branch: `configbackup` (including
`alertSettings`, present from the package's very first commit, PR #9)
was never released in any tagged version, so there is no earlier
"before alertSettings" schema-2 shape and no real schema-1 document in
the wild (schema 1 was superseded by a deliberate, documented,
non-additive break - see `configbackup.SchemaVersion`'s own doc comment
- before this feature ever shipped one).

### How it was regenerated (for anyone who needs to reproduce or extend it)

```sh
git worktree add .worktrees/master-baseline 5b8509fc
# write a small program under .worktrees/master-baseline/fixturegen/main.go
# that imports "github.com/stratux/stratux/configbackup" (resolved to
# THAT worktree's own source, since it has its own go.mod) and calls
# BuildDocument with the same inputs configbackup/document_test.go's own
# testBuildInputs() used at that commit, then json.MarshalIndent's the
# result to a file.
./docker_run.sh "cd /data/.worktrees/master-baseline && go run ./fixturegen"
git worktree remove .worktrees/master-baseline
```

Do not hand-edit this file to "fix" a test - if the shape needs to
change, regenerate it from the actual historical commit, or (if no
such historical commit exists for the desired shape) do not claim the
fixture is historical at all.

Its one calibration profile is deliberately named "Legacy Backup
Aircraft" rather than "Current Installation" - `main/configbackupapi_test.go`'s
own `withConfigBackupTestEnv` test helper already seeds a profile named
"Current Installation" for every test, and `calprofile.Store` enforces
name uniqueness; using a different name here is a test-fixture
convenience, unrelated to (and not weakening) the historical-shape
reproduction itself.
