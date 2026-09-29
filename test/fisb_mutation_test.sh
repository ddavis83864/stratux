#!/bin/bash
# fisb_mutation_test.sh: proves the FIS-B tests can FAIL. Each mutation re-introduces one
# specific defect (or removes one specific protection) in a throw-away COPY of the tree - the
# repository is never modified - and the tests that guard it must then fail. A mutation that no
# test catches is itself a failure. The un-mutated copy is run first as the control.
#
#   test/fisb_mutation_test.sh [MUTATION_NAME ...]        (default: all)
#
# Needs Go 1.22 with cgo and the RTL-SDR headers (what the `main` package needs to build), and
# `make libdump978.so` buildable. To run inside a container, set GO_RUN_PREFIX to a command that
# runs "$RUN_CMD" there with the copy ($COPY) mounted at /src, e.g.
#   GO_RUN_PREFIX='docker run --rm -v "$COPY":/src -w /src my-go-image bash -c "$RUN_CMD"'
# Opt-in; never touches a device.
set -u
REPO=$(cd "$(dirname "$0")/.." && pwd)
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
overall=0

copy_tree() { # $1 = dest
	mkdir -p "$1"
	(cd "$REPO" && tar --exclude=.git --exclude='./dump1090' --exclude='./ogn' --exclude='./softrf' --exclude='./mapdata' --exclude='./image_build' --exclude='*.deb' -cf - .) | tar -xf - -C "$1"
}
run_in() { # $1 = copy, $2 = command
	local COPY=$1
	if [ -n "${GO_RUN_PREFIX:-}" ]; then
		export COPY RUN_CMD="cd /src && $2"
		eval "$GO_RUN_PREFIX" 2>&1
	else
		(cd "$COPY" && bash -c "$2" 2>&1)
	fi
}
SETUP='make libdump978.so >/dev/null 2>&1; export LIBRARY_PATH=$PWD CGO_CFLAGS_ALLOW="-L$PWD"'
# tests[NAME] = go test invocation(s) that must FAIL on the mutant and pass on the control
declare -A TESTS
TESTS[queue]="$SETUP; go test -count=1 -run 'CaptureForAnInFlightKeyIsNotLost|FISBCacheSoak' ./main/"
TESTS[uat]="set -e; go test -vet=off -count=1 ./uatparse/; $SETUP; go test -count=1 -run 'FISBEndToEnd|FISBRealCapture' ./main/"
TESTS[guard]="$SETUP; go test -count=1 -run 'DataPartitionNotMounted|InitDir' ./main/"
TESTS[domain]="go test -count=1 ./fisbcache/"
TESTS[e2e]="$SETUP; go test -count=1 -run 'FISBEndToEnd|FISBRealCapture|ReplayIntoGDL90' ./main/"
TESTS[fresh]="set -e; go test -count=1 ./fisbcache/; go test -vet=off -count=1 ./uatparse/; $SETUP; go test -count=1 -run 'FISBEndToEnd|FISBRealCapture|EffectiveAgeSurvives' ./main/"
TESTS[limit]="set -e; go test -count=1 ./configbackup/; $SETUP; go test -count=1 -run 'MaxEntries|MaxEntriesLimit|DashboardMaxEntries|PersistedValueAbove|FISBCacheSettings|HandleSetFISBCacheSettings' ./main/"
TESTS[backup]="go test -count=1 ./configbackup/"

mutate() { python3 - "$@" <<'PY'
import sys
d,f,old,new=sys.argv[1:5]
p=f"{d}/{f}"; s=open(p).read()
if old not in s: sys.exit(f"mutation target not found in {f}: {old!r}")
open(p,'w').write(s.replace(old,new,1))
PY
}

M() { # name suite file old new description
	local name=$1 suite=$2 file=$3 old=$4 new=$5 desc=$6
	[ $# -ge 6 ] || return
	if [ -n "${WANT:-}" ] && [[ " $WANT " != *" $name "* ]]; then return; fi
	local c="$W/m-$name"; copy_tree "$c"
	if ! mutate "$c" "$file" "$old" "$new"; then echo "FAIL: mutation '$name' could not be applied"; overall=1; return; fi
	if run_in "$c" "${TESTS[$suite]}" >"$W/out.$name"; then
		echo "FAIL: mutation '$name' NOT caught by the $suite tests -- $desc"; tail -6 "$W/out.$name" | sed 's/^/        /'; overall=1
	else
		echo "PASS: mutation '$name' caught by the $suite tests -- $desc"
	fi
}

M2() { # name suite file old1 new1 old2 new2 description - a mutation needing two edits in one file
	local name=$1 suite=$2 file=$3 o1=$4 n1=$5 o2=$6 n2=$7 desc=$8
	if [ -n "${WANT:-}" ] && [[ " $WANT " != *" $name "* ]]; then return; fi
	local c="$W/m-$name"; copy_tree "$c"
	if ! mutate "$c" "$file" "$o1" "$n1" || ! mutate "$c" "$file" "$o2" "$n2"; then echo "FAIL: mutation '$name' could not be applied"; overall=1; return; fi
	if run_in "$c" "${TESTS[$suite]}" >"$W/out.$name"; then
		echo "FAIL: mutation '$name' NOT caught by the $suite tests -- $desc"; tail -6 "$W/out.$name" | sed 's/^/        /'; overall=1
	else
		echo "PASS: mutation '$name' caught by the $suite tests -- $desc"
	fi
}

WANT="$*"
echo "=== control: the un-mutated tree passes every suite ==="
c="$W/control"; copy_tree "$c"
for s in "${!TESTS[@]}"; do
	if run_in "$c" "${TESTS[$s]}" >"$W/out.control.$s"; then echo "PASS: control / $s"; else echo "FAIL: control / $s"; tail -5 "$W/out.control.$s"; overall=1; fi
done

echo "=== mutations ==="
M queue-inflight queue main/fisbcachereserve.go $'if !alreadyPending {\n\t\tq.order = append(q.order, key)' $'if !alreadyPending && !alreadyInFlight {\n\t\tq.order = append(q.order, key)' "a capture for an in-flight key is queued but never popped (leaked reservation, key can never be admitted again)"
M uat-overrun uat uatparse/uatparse.go 'pos+2+int(frame_length) > total_len' 'pos+int(frame_length) > total_len' "a malformed frame length slices past the buffer and panics the whole decoder"
M guard-settings guard main/fisbcachesettings.go $'if err := ensurePersistentDataMounted(); err != nil {\n\t\treturn fmt.Errorf("could not persist fisbcache settings' $'if err := error(nil); err != nil {\n\t\treturn fmt.Errorf("could not persist fisbcache settings' "settings written into the RAM overlay when the data partition is not mounted"
M guard-entry guard main/fisbcacherun.go $'if err := ensurePersistentDataMounted(); err != nil {\n\t\treturn fmt.Errorf("could not persist FIS-B cache entry' $'if err := error(nil); err != nil {\n\t\treturn fmt.Errorf("could not persist FIS-B cache entry' "cache entries written into the RAM overlay when the data partition is not mounted"
M guard-initdir guard main/fisbcacherun.go $'func fisbCacheInitDir() {\n\terr := ensurePersistentDataMounted()' $'func fisbCacheInitDir() {\n\tvar err error' "cache directory created in the RAM overlay when the data partition is not mounted"
M older-wins domain fisbcache/store.go $'return candidate.ReceivedAtMonotonic > existing.ReceivedAtMonotonic\n\t\t}\n\t\treturn false' $'return candidate.ReceivedAtMonotonic > existing.ReceivedAtMonotonic\n\t\t}\n\t\treturn true' "a delayed OLDER retransmission overwrites a newer cached product"
M never-supersede e2e fisbcache/store.go $'s.entries[candidate.Key] = candidate\n\t\treturn AdmitSuperseded' $'return AdmitSuperseded' "a newer report never replaces the cached one"
M never-expire domain fisbcache/entry.go $'default:\n\t\treturn FreshnessExpired' $'default:\n\t\treturn FreshnessStale' "products never expire"
M budget-ignored domain fisbcache/retention.go $'if maxEntries > 0 && remainingCount > maxEntries {\n\t\t\treturn true' $'if maxEntries > 0 && remainingCount > maxEntries+1000000 {\n\t\t\treturn true' "the entry budget is not enforced (unbounded cache)"
M replay-accepted e2e main/fisbcachesettings.go $'if s.ReplayEnabled {\n\t\treturn fmt.Errorf("fisbcache: replayEnabled' $'if false {\n\t\treturn fmt.Errorf("fisbcache: replayEnabled' "replay into GDL90 becomes acceptable without a reception-time design"
M backup-no-default backup configbackup/legacy.go $'if verifyLegacyPreEpaperChecksum(doc) {\n\t\tdoc.EpaperSettings = legacyDefaultEpaperSettings\n\t\tdoc.FISBCacheSettings = legacyDefaultFISBCacheSettings' $'if verifyLegacyPreEpaperChecksum(doc) {\n\t\tdoc.EpaperSettings = legacyDefaultEpaperSettings' "a pre-epaper backup restores with an invalid all-zero FIS-B section"
M backup-all-shapes backup configbackup/legacy.go $'if verifyLegacyPreFISBCacheChecksum(doc) {\n\t\tdoc.FISBCacheSettings = legacyDefaultFISBCacheSettings\n\t\treturn doc, true\n\t}\n' '' "the pre-fisbcache backup shape (master's real current format) is no longer restorable"
echo "=== freshness-semantics mutations ==="
M fresh-min-age fresh fisbcache/entry.go 'ok && src >= reception {' 'ok && src <= reception {' "the MINIMUM of reception and source age is used, so an old product looks as fresh as it was received"
M fresh-ignore-source fresh fisbcache/entry.go 'if policy.UsesSourceAge() {' 'if false {' "the source age is ignored (reception-only freshness, the original behaviour)"
M fresh-retransmission-resets fresh fisbcache/store.go $'s.entries[candidate.Key] = candidate\n\t\treturn AdmitSuperseded' $'candidate.Source.UTC = candidate.ReceivedAtUTC\n\t\ts.entries[candidate.Key] = candidate\n\t\treturn AdmitSuperseded' "a retransmission resets the product's source age"
M fresh-missing-as-zero fresh fisbcache/entry.go $'return 0, false\n\t}\n\tlag = e.ReceivedAtUTC' $'return 0, true\n\t}\n\tlag = e.ReceivedAtUTC' "a missing/untrusted source time is treated as a zero-lag source (claims source basis)"
M2 fresh-future-trusted fresh fisbcache/entry.go $'if lag < 0 {\n\t\tlag = 0\n\t}' '' 'ok && src >= reception {' 'ok {' "a future source time is trusted blindly (negative lag makes the product look younger)"
M fresh-fallback-mislabelled fresh fisbcache/entry.go 'return reception, AgeBasisReception' 'return reception, AgeBasisSource' "reception-only fallback is labelled as source-derived"
M fresh-window-6h fresh fisbcache/time.go 'maxPastSkew = 48 * time.Hour' 'maxPastSkew = 6 * time.Hour' "a long-lived product older than 6h is rejected as a source time and looks fresh"
M fresh-lag-uncapped fresh fisbcache/entry.go 'if lag > maxSourceLag {' 'if false {' "an absurd persisted source lag makes the age unbounded"
M fresh-arrival-check-removed fresh main/fisbcacherun.go 'entry.ReceivedAtMonotonic) == fisbcache.FreshnessExpired {' 'entry.ReceivedAtMonotonic) == fisbcache.FreshnessUnsupported {' "an already-expired product is cached (and churns) on every rebroadcast"
M fresh-api-reception-age fresh main/fisbcacheapi.go 'AgeSeconds:          effective.Seconds(),' 'AgeSeconds:          e.ReceptionAge(now).Seconds(),' "the API's ageSeconds reports reception age only"
echo
echo "=== cache-size limit (10,000) mutations ==="
M limit-restored-100000 limit main/fisbcachesettings.go 'const FISBCacheMaxEntriesLimit = 10000' 'const FISBCacheMaxEntriesLimit = 100000' "the maximum is accidentally restored to 100,000"
M limit-accepts-10001 limit main/fisbcachesettings.go 'if s.MaxEntries > FISBCacheMaxEntriesLimit {' 'if s.MaxEntries > FISBCacheMaxEntriesLimit+1 {' "off-by-one: 10,001 is accepted"
M limit-rejects-10000 limit main/fisbcachesettings.go 'if s.MaxEntries > FISBCacheMaxEntriesLimit {' 'if s.MaxEntries >= FISBCacheMaxEntriesLimit {' "off-by-one: 10,000 is rejected"
M limit-backup-bypass limit configbackup/validate.go 'if f.MaxEntries <= 0 || f.MaxEntries > FISBCacheMaxEntries {' 'if f.MaxEntries <= 0 {' "Configuration Backup restore bypasses the entry limit"
M limit-backup-constant-drift limit configbackup/validate.go 'const FISBCacheMaxEntries = 10000' 'const FISBCacheMaxEntries = 100000' "the backup validator's limit drifts from the settings limit"
M limit-api-bypass limit main/fisbcacheapi.go $'if err := s.Validate(); err != nil {\n\t\twriteJSON(w, http.StatusBadRequest' $'if err := error(nil); err != nil {\n\t\twriteJSON(w, http.StatusBadRequest' "the settings API skips its own validation (the value would reach the save path)"
M limit-dashboard-100000 limit web/plates/fisbcache.html 'min="1" max="10000"/>' 'min="1" max="100000"/>' "the dashboard input allows 100,000"
M limit-default-changed limit main/fisbcachesettings.go 'MaxEntries:         2000,' 'MaxEntries:         10000,' "the default cache size changes with the maximum"
echo
[ "$overall" = 0 ] && echo "ALL FIS-B MUTATIONS CAUGHT" || echo "FIS-B MUTATION TEST FAILED"
exit $overall
