package preflight

import (
	"sync"
	"testing"
	"time"
)

func newTestStore(session string, start time.Time) (*ManualAckStore, *time.Time) {
	now := start
	return NewManualAckStore(session, func() time.Time { return now }), &now
}

func TestManualAckStore_AcknowledgeAndSnapshot(t *testing.T) {
	store, _ := newTestStore("sess-1", time.Unix(1000, 0))
	if _, err := store.Ack(CheckAntennasAttached, nil); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	snap := store.Snapshot(DefaultAckExpiration)
	if snap[CheckAntennasAttached] == nil {
		t.Error("expected CheckAntennasAttached to be acknowledged")
	}
	if snap[CheckMountSecure] != nil {
		t.Error("expected CheckMountSecure to be unacknowledged")
	}
}

func TestManualAckStore_ReAcknowledgeIsIdempotent(t *testing.T) {
	store, now := newTestStore("sess-1", time.Unix(1000, 0))
	if _, err := store.Ack(CheckAntennasAttached, nil); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	a2, err := store.Ack(CheckAntennasAttached, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := store.Snapshot(DefaultAckExpiration)
	if snap[CheckAntennasAttached] == nil || !snap[CheckAntennasAttached].AckedAtMono.Equal(a2.AckedAtMono) {
		t.Error("re-acknowledging did not refresh the acknowledgement")
	}
}

func TestManualAckStore_Clear(t *testing.T) {
	store, _ := newTestStore("sess-1", time.Unix(1000, 0))
	store.Ack(CheckAntennasAttached, nil)
	if err := store.Clear(CheckAntennasAttached); err != nil {
		t.Fatal(err)
	}
	if store.Snapshot(DefaultAckExpiration)[CheckAntennasAttached] != nil {
		t.Error("expected check to be cleared")
	}
	// Clearing an already-clear check must not error.
	if err := store.Clear(CheckAntennasAttached); err != nil {
		t.Errorf("Clear() on an already-clear check returned error: %v", err)
	}
}

func TestManualAckStore_ResetAll(t *testing.T) {
	store, _ := newTestStore("sess-1", time.Unix(1000, 0))
	store.Ack(CheckAntennasAttached, nil)
	store.Ack(CheckMountSecure, nil)
	store.ResetAll()
	snap := store.Snapshot(DefaultAckExpiration)
	for id, a := range snap {
		if a != nil {
			t.Errorf("check %s still acknowledged after ResetAll", id)
		}
	}
}

func TestManualAckStore_UnknownCheck(t *testing.T) {
	store, _ := newTestStore("sess-1", time.Unix(1000, 0))
	if _, err := store.Ack("not_a_real_check", nil); err != ErrUnknownManualCheck {
		t.Errorf("Ack(unknown) error = %v, want ErrUnknownManualCheck", err)
	}
	if err := store.Clear("not_a_real_check"); err != ErrUnknownManualCheck {
		t.Errorf("Clear(unknown) error = %v, want ErrUnknownManualCheck", err)
	}
}

func TestManualAckStore_Expiration(t *testing.T) {
	store, now := newTestStore("sess-1", time.Unix(1000, 0))
	store.Ack(CheckAntennasAttached, nil)
	*now = now.Add(DefaultAckExpiration - time.Second)
	if store.Snapshot(DefaultAckExpiration)[CheckAntennasAttached] == nil {
		t.Error("acknowledgement expired too early")
	}
	*now = now.Add(2 * time.Second)
	if store.Snapshot(DefaultAckExpiration)[CheckAntennasAttached] != nil {
		t.Error("acknowledgement did not expire after the documented window")
	}
}

func TestManualAckStore_SessionChangeInvalidatesAcks(t *testing.T) {
	store, _ := newTestStore("sess-1", time.Unix(1000, 0))
	store.Ack(CheckAntennasAttached, nil)
	// Simulate a reboot: a fresh store gets a new session ID and starts
	// with an empty map - acknowledgements never persist across process
	// restarts (see ManualAckStore's package comment), so there is
	// nothing for a new store to inherit. This test instead confirms
	// that a stale entry inserted "as if" from a different session (the
	// only way that could happen: bypassing Ack, which never occurs in
	// real code) is correctly treated as invalid, precisely because
	// Snapshot compares SessionID field-by-field rather than trusting
	// map membership alone.
	store.mu.Lock()
	stale := store.acks[CheckAntennasAttached]
	stale.SessionID = "sess-0"
	store.acks[CheckAntennasAttached] = stale
	store.mu.Unlock()
	if store.Snapshot(DefaultAckExpiration)[CheckAntennasAttached] != nil {
		t.Error("an acknowledgement stamped with a different session must never read as valid")
	}
}

func TestManualAckStore_NoPersistenceAcrossRestart(t *testing.T) {
	store1, _ := newTestStore("sess-1", time.Unix(1000, 0))
	store1.Ack(CheckAntennasAttached, nil)
	// A "restart" is simply constructing a new store - there is no
	// shared backing file/state for a new instance to read, by design.
	store2, _ := newTestStore("sess-2", time.Unix(2000, 0))
	if store2.Snapshot(DefaultAckExpiration)[CheckAntennasAttached] != nil {
		t.Error("a freshly-constructed store must never see a prior instance's acknowledgements")
	}
}

func TestManualAckStore_ConcurrentAccess(t *testing.T) {
	store, _ := newTestStore("sess-1", time.Unix(1000, 0))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				store.Ack(CheckAntennasAttached, nil)
				store.Snapshot(DefaultAckExpiration)
				store.Clear(CheckMountSecure)
			}
		}()
	}
	wg.Wait()
}

func TestIsKnownManualCheck(t *testing.T) {
	for _, d := range ManualCheckDefinitions {
		if !IsKnownManualCheck(d.ID) {
			t.Errorf("IsKnownManualCheck(%s) = false, want true", d.ID)
		}
	}
	if IsKnownManualCheck("nonexistent") {
		t.Error("IsKnownManualCheck(nonexistent) = true, want false")
	}
}
