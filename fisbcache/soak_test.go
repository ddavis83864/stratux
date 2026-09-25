package fisbcache

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// TestSoak_AcceleratedLongUptimeStaysBoundedAndExpiresOnSchedule simulates
// 72 hours of continuous reception in a busy FIS-B market, with no wall-clock
// waiting: an explicit simulated monotonic clock drives Admit and the same
// PlanEviction the production retention worker uses.
//
// Traffic model (deterministic, seeded):
//   - 120 METAR stations re-broadcast every 5 minutes, each at a new report
//     time roughly hourly;
//   - 60 TAF stations re-broadcast every 5 minutes;
//   - 800 NEXRAD tiles re-broadcast every 5 minutes;
//   - plus an adversarial stream: every second, a brand-new never-repeated
//     NEXRAD tile and a never-repeated METAR station (unbounded key space),
//     which is what would grow an unbounded cache without limit.
//
// After every simulated minute a retention pass runs (as the cleanup worker
// does) with a 1500-entry / 6 MiB budget (room for the 980 repeating products plus
// a bounded amount of the adversarial junk). Note the documented policy: when
// over budget the OLDEST-received entries go first, so a flood of new distinct
// products larger than the headroom would evict older live ones - that is the
// design, not asserted away here. Asserted continuously: the entry and
// byte budgets hold after every pass, nothing past its stale window survives a
// pass, and the retained set contains only entries the policy still allows.
func TestSoak_AcceleratedLongUptimeStaysBoundedAndExpiresOnSchedule(t *testing.T) {
	const (
		maxEntries = 1500
		maxBytes   = 6 << 20
		hours      = 72
	)
	rng := rand.New(rand.NewSource(20260924))
	store := NewStore()
	start := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	type product struct {
		key  Key
		size int64
	}
	var repeating []product
	for i := 0; i < 120; i++ {
		repeating = append(repeating, product{TextKey("METAR", fmt.Sprintf("K%03d", i)), 60 + int64(rng.Intn(80))})
	}
	for i := 0; i < 60; i++ {
		repeating = append(repeating, product{TextKey("TAF", fmt.Sprintf("K%03d", i)), 250 + int64(rng.Intn(300))})
	}
	for i := 0; i < 800; i++ {
		repeating = append(repeating, product{NexradKey(63, 0, 40+float64(i)*0.0667, -100+float64(i%50)*0.8, 0.0667, 0.8), 1200})
	}

	var admits, accepted, superseded, rejected, evicted uint64
	maxSeen := 0
	seqNew := 0
	for sec := 0; sec < hours*3600; sec++ {
		now := float64(sec)
		wall := start.Add(time.Duration(sec) * time.Second)
		mk := func(k Key, size int64) Entry {
			return Entry{Key: k, ReceivedAtMonotonic: now, ReceivedAtUTC: wall, SizeBytes: size,
				Source: SourceTime{Trusted: true, UTC: wall.Add(-time.Duration(rng.Intn(120)) * time.Second)}}
		}
		// Every 5 minutes the whole repeating set is broadcast again.
		if sec%300 == 0 {
			for _, p := range repeating {
				admits++
				switch store.Admit(mk(p.key, p.size)) {
				case AdmitAccepted:
					accepted++
				case AdmitSuperseded:
					superseded++
				default:
					rejected++
				}
			}
		}
		// Adversarial, never-repeated keys, one of each kind per second.
		seqNew++
		for _, k := range []Key{
			NexradKey(63, 0, 25+float64(seqNew%2000)*0.0007+float64(seqNew)*1e-6, -120+float64(seqNew%977)*0.001, 0.0667, 0.8),
			TextKey("METAR", fmt.Sprintf("ZZ%06d", seqNew)),
		} {
			admits++
			if r := store.Admit(mk(k, 100)); r == AdmitAccepted {
				accepted++
			} else {
				rejected++
			}
		}
		if sec%60 != 59 {
			continue
		}
		// Retention pass, as the cleanup worker runs it.
		snap := store.Snapshot()
		for _, k := range PlanEviction(snap, maxBytes, maxEntries, now) {
			store.Delete(k)
			evicted++
		}
		post := store.Snapshot()
		var bytes int64
		for k, e := range post {
			bytes += e.SizeBytes
			if st := Freshness(e, PolicyFor(k), now); st == FreshnessExpired {
				t.Fatalf("t=%ds: an EXPIRED entry survived a retention pass: %v", sec, k)
			}
		}
		if len(post) > maxEntries || bytes > maxBytes {
			t.Fatalf("t=%ds: budget breached after a retention pass: %d entries (max %d), %d bytes (max %d)", sec, len(post), maxEntries, bytes, int64(maxBytes))
		}
		if len(post) > maxSeen {
			maxSeen = len(post)
		}
		// Right after each broadcast the whole repeating set must still be cached:
		// the junk received since is newer but far smaller than the headroom, so it
		// may only ever push out its own older entries, never a live product.
		if sec%300 == 59 {
			for _, p := range repeating {
				if _, ok := post[p.key]; !ok {
					t.Fatalf("t=%ds: live product %v was evicted by junk traffic within one broadcast interval", sec, p.key)
				}
			}
		}
	}
	// Between passes the store may briefly exceed the budget by at most one
	// minute of admissions (the production path bounds this synchronously via
	// its reservation model; this test bounds the domain layer's own growth).
	t.Logf("simulated %dh, %d admissions (%d accepted, %d superseded, %d rejected), %d evicted, peak retained after a pass = %d entries",
		hours, admits, accepted, superseded, rejected, evicted, maxSeen)
	if admits < 400000 {
		t.Fatalf("soak drove only %d admissions - the traffic model is not doing its job", admits)
	}
	// Retransmissions of an already-cached product must be recognised as such
	// (in place, no growth) - the repeating set is ~846k of the admissions.
	if superseded < 700000 {
		t.Fatalf("only %d retransmissions were recognised as supersessions, want the great majority of ~846k", superseded)
	}
	if maxSeen == 0 {
		t.Fatal("nothing was ever retained")
	}
}
