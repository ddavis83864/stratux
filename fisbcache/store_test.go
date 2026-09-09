package fisbcache

import "testing"

func TestStore_AdmitNew(t *testing.T) {
	s := NewStore()
	k := TextKey(TextProductMETAR, "KSEA")
	e := Entry{Key: k, ReceivedAtMonotonic: 100}
	if got := s.Admit(e); got != AdmitAccepted {
		t.Fatalf("got %q, want accepted", got)
	}
	if s.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", s.Len())
	}
}

func TestStore_AdmitUnsupportedRejected(t *testing.T) {
	s := NewStore()
	e := Entry{Key: Key{Class: ClassUnsupported, Identity: "x"}}
	if got := s.Admit(e); got != AdmitRejectedUnsupported {
		t.Fatalf("got %q, want rejected_unsupported", got)
	}
	if s.Len() != 0 {
		t.Fatal("expected nothing admitted")
	}
}

func TestStore_SupersedeWithNewerReceiveWhenNeitherTrusted(t *testing.T) {
	s := NewStore()
	k := TextKey(TextProductMETAR, "KSEA")
	s.Admit(Entry{Key: k, ReceivedAtMonotonic: 100})
	got := s.Admit(Entry{Key: k, ReceivedAtMonotonic: 200})
	if got != AdmitSuperseded {
		t.Fatalf("got %q, want superseded", got)
	}
	if s.Len() != 1 {
		t.Fatalf("expected exactly one entry (dedup), got %d", s.Len())
	}
	e, _ := s.Get(k)
	if e.ReceivedAtMonotonic != 200 {
		t.Errorf("expected the newer receipt to win, got ReceivedAtMonotonic=%v", e.ReceivedAtMonotonic)
	}
}

func TestStore_RejectOlderReceiveWhenNeitherTrusted(t *testing.T) {
	s := NewStore()
	k := TextKey(TextProductMETAR, "KSEA")
	s.Admit(Entry{Key: k, ReceivedAtMonotonic: 200})
	got := s.Admit(Entry{Key: k, ReceivedAtMonotonic: 100})
	if got != AdmitRejectedOlder {
		t.Fatalf("got %q, want rejected_older", got)
	}
	e, _ := s.Get(k)
	if e.ReceivedAtMonotonic != 200 {
		t.Error("expected the existing newer entry to be kept")
	}
}

func TestStore_TrustedSourceTimeWinsOverUntrusted(t *testing.T) {
	s := NewStore()
	k := TextKey(TextProductMETAR, "KSEA")
	// Existing entry has no trusted source time (e.g. received before
	// GNSS sync), but a LATER receive-monotonic than the trusted one
	// below - trust must still win.
	s.Admit(Entry{Key: k, ReceivedAtMonotonic: 500})
	trusted := Entry{Key: k, ReceivedAtMonotonic: 100, Source: SourceTime{Trusted: true, UTC: mustUTC("2026-06-15T12:00:00Z")}}
	if got := s.Admit(trusted); got != AdmitSuperseded {
		t.Fatalf("got %q, want superseded (trusted source time must win over untrusted regardless of receive order)", got)
	}
}

func TestStore_NewerTrustedSourceTimeWins(t *testing.T) {
	s := NewStore()
	k := TextKey(TextProductMETAR, "KSEA")
	older := Entry{Key: k, ReceivedAtMonotonic: 100, Source: SourceTime{Trusted: true, UTC: mustUTC("2026-06-15T12:00:00Z")}}
	newer := Entry{Key: k, ReceivedAtMonotonic: 50, Source: SourceTime{Trusted: true, UTC: mustUTC("2026-06-15T13:00:00Z")}}
	s.Admit(older)
	if got := s.Admit(newer); got != AdmitSuperseded {
		t.Fatalf("got %q, want superseded (newer trusted source time wins even with an earlier receive time - e.g. multi-path reception)", got)
	}
}

func TestStore_OlderTrustedSourceTimeRejected(t *testing.T) {
	s := NewStore()
	k := TextKey(TextProductMETAR, "KSEA")
	newer := Entry{Key: k, ReceivedAtMonotonic: 100, Source: SourceTime{Trusted: true, UTC: mustUTC("2026-06-15T13:00:00Z")}}
	older := Entry{Key: k, ReceivedAtMonotonic: 200, Source: SourceTime{Trusted: true, UTC: mustUTC("2026-06-15T12:00:00Z")}}
	s.Admit(newer)
	if got := s.Admit(older); got != AdmitRejectedOlder {
		t.Fatalf("got %q, want rejected_older (a delayed/rebroadcast older product must never supersede a newer one already cached)", got)
	}
}

func TestStore_DistinctKeysDoNotInterfere(t *testing.T) {
	s := NewStore()
	a := TextKey(TextProductMETAR, "KSEA")
	b := TextKey(TextProductMETAR, "KPDX")
	s.Admit(Entry{Key: a, ReceivedAtMonotonic: 100})
	s.Admit(Entry{Key: b, ReceivedAtMonotonic: 100})
	if s.Len() != 2 {
		t.Fatalf("expected 2 independent entries, got %d", s.Len())
	}
}

func TestStore_DeleteAndSnapshot(t *testing.T) {
	s := NewStore()
	k := TextKey(TextProductMETAR, "KSEA")
	s.Admit(Entry{Key: k, ReceivedAtMonotonic: 100})
	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatal("expected 1 in snapshot")
	}
	s.Delete(k)
	if s.Len() != 0 {
		t.Fatal("expected deletion to remove the entry")
	}
	// The earlier snapshot must be a copy, unaffected by the later Delete.
	if len(snap) != 1 {
		t.Fatal("snapshot must be a copy, not a live view")
	}
}

func TestStore_ExpiredKeys(t *testing.T) {
	s := NewStore()
	k := TextKey(TextProductMETAR, "KSEA")
	policy := PolicyFor(k)
	s.Admit(Entry{Key: k, ReceivedAtMonotonic: 0})
	expired := s.ExpiredKeys(policy.ExpireLimit.Seconds() + 1)
	if len(expired) != 1 || expired[0] != k {
		t.Fatalf("expected %v to be expired, got %v", k, expired)
	}
	notYet := s.ExpiredKeys(policy.FreshLimit.Seconds())
	if len(notYet) != 0 {
		t.Fatalf("expected nothing expired yet, got %v", notYet)
	}
}

func TestComputeStats(t *testing.T) {
	k1 := TextKey(TextProductMETAR, "KSEA")
	k2 := NexradKey(63, 0, 40, -120, 1, 1)
	snap := map[Key]Entry{
		k1: {Key: k1, ReceivedAtMonotonic: 0, SizeBytes: 100},
		k2: {Key: k2, ReceivedAtMonotonic: 0, SizeBytes: 200},
	}
	st := ComputeStats(snap, 0)
	if st.TotalEntries != 2 {
		t.Errorf("TotalEntries = %d, want 2", st.TotalEntries)
	}
	if st.TotalBytes != 300 {
		t.Errorf("TotalBytes = %d, want 300", st.TotalBytes)
	}
	if st.ByProductClass[ClassText] != 1 || st.ByProductClass[ClassNexradTile] != 1 {
		t.Errorf("ByProductClass = %+v", st.ByProductClass)
	}
	if st.ByFreshness[FreshnessCachedFresh] != 2 {
		t.Errorf("expected both fresh at age 0, got %+v", st.ByFreshness)
	}
}
