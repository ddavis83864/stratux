package calprofile

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestProfile(name string) Profile {
	p := Profile{
		ID:            NewID(),
		Name:          name,
		Kind:          KindUser,
		SchemaVersion: SchemaVersion,
		CreatedAt:     time.Now().UTC(),
		ModifiedAt:    time.Now().UTC(),
	}
	return p
}

func TestStore_EmptyFirstStartup(t *testing.T) {
	s := NewStore(t.TempDir())
	profiles, err := s.List()
	if err != nil {
		t.Fatalf("List on a never-used directory should not error: %v", err)
	}
	if len(profiles) != 0 {
		t.Errorf("expected zero profiles, got %d", len(profiles))
	}
	if _, err := s.Active(); err != ErrNoActiveProfile {
		t.Errorf("Active() on an empty store should return ErrNoActiveProfile, got %v", err)
	}
	if n, err := s.Count(); err != nil || n != 0 {
		t.Errorf("Count() = %d, %v; want 0, nil", n, err)
	}
}

func TestStore_SaveAndGet(t *testing.T) {
	s := NewStore(t.TempDir())
	p := newTestProfile("Cherokee Six")
	if err := s.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Get(p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != p.Name || got.ID != p.ID {
		t.Errorf("Get returned %+v, want name/id matching %+v", got, p)
	}
}

func TestStore_AtomicSave_NoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	p := newTestProfile("Test")
	if err := s.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a .tmp file was left behind after a successful save: %s", e.Name())
		}
	}
}

func TestStore_ReloadAfterRestart(t *testing.T) {
	dir := t.TempDir()
	p := newTestProfile("Persisted")
	s1 := NewStore(dir)
	if err := s1.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s1.SetActiveID(p.ID, time.Now().UTC()); err != nil {
		t.Fatalf("SetActiveID: %v", err)
	}

	// Simulate a restart: a brand new Store value over the same directory.
	s2 := NewStore(dir)
	got, err := s2.Active()
	if err != nil {
		t.Fatalf("Active after reload: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("reloaded active profile ID = %q, want %q", got.ID, p.ID)
	}
}

func TestStore_CorruptJSON_SkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	good := newTestProfile("Good")
	if err := s.Save(good); err != nil {
		t.Fatalf("Save: %v", err)
	}
	badID := NewID()
	if err := os.WriteFile(filepath.Join(dir, badID+profileFileSuffix), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles, err := s.List()
	if err != nil {
		t.Fatalf("List should not fail outright on a corrupt file: %v", err)
	}
	if len(profiles) != 1 || profiles[0].ID != good.ID {
		t.Errorf("List should return only the good profile, got %+v", profiles)
	}
	_, bad, err := s.ListWithErrors()
	if err != nil {
		t.Fatalf("ListWithErrors: %v", err)
	}
	if len(bad) != 1 {
		t.Errorf("ListWithErrors should report exactly one bad file, got %v", bad)
	}
}

func TestStore_UnsupportedSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	id := NewID()
	future := `{"id":"` + id + `","name":"Future","schemaVersion":999}`
	if err := os.WriteFile(filepath.Join(dir, id+profileFileSuffix), []byte(future), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles, bad, err := s.ListWithErrors()
	if err != nil {
		t.Fatalf("ListWithErrors: %v", err)
	}
	if len(profiles) != 0 {
		t.Errorf("a from-the-future schema version should not be loaded, got %+v", profiles)
	}
	if len(bad) != 1 {
		t.Errorf("expected the unsupported-schema-version file to be reported as bad, got %v", bad)
	}
}

func TestStore_MissingDirectory_ListIsEmptyNotError(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "does", "not", "exist"))
	profiles, err := s.List()
	if err != nil {
		t.Fatalf("List on a missing directory should not error: %v", err)
	}
	if len(profiles) != 0 {
		t.Errorf("expected zero profiles, got %d", len(profiles))
	}
}

func TestStore_SaveCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "profiles")
	s := NewStore(dir)
	p := newTestProfile("Test")
	if err := s.Save(p); err != nil {
		t.Fatalf("Save should create the missing directory: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory was not created: %v", err)
	}
}

func TestStore_ProfileCountLimit(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for i := 0; i < MaxProfiles; i++ {
		p := newTestProfile("Profile " + string(rune('A'+i)))
		if err := s.Save(p); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}
	over := newTestProfile("One Too Many")
	if err := s.Save(over); err != ErrTooManyProfiles {
		t.Errorf("expected ErrTooManyProfiles at the limit, got %v", err)
	}
}

func TestStore_ProfileCountLimit_UpdatingExistingIsNotBlocked(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	var ids []string
	for i := 0; i < MaxProfiles; i++ {
		p := newTestProfile("Profile " + string(rune('A'+i)))
		if err := s.Save(p); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
		ids = append(ids, p.ID)
	}
	// Updating an EXISTING profile at the limit must still be allowed.
	existing, err := s.Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	existing.MountingNote = "updated"
	if err := s.Save(existing); err != nil {
		t.Errorf("updating an existing profile at the count limit should be allowed, got %v", err)
	}
}

func TestStore_DuplicateName_Rejected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	a := newTestProfile("Cherokee Six")
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	b := newTestProfile("cherokee six") // case-insensitive match
	if err := s.Save(b); err != ErrDuplicateName {
		t.Errorf("expected ErrDuplicateName, got %v", err)
	}
}

func TestStore_DuplicateName_ReSavingSameProfileIsFine(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	p := newTestProfile("Cherokee Six")
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	p.MountingNote = "moved to the other window"
	if err := s.Save(p); err != nil {
		t.Errorf("re-saving the same profile under its own existing name should succeed, got %v", err)
	}
}

func TestStore_InvalidID_Rejected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if _, err := s.Get("../../etc/passwd"); err != ErrInvalidID {
		t.Errorf("Get: expected ErrInvalidID, got %v", err)
	}
	if err := s.Delete("../../etc/passwd"); err != ErrInvalidID {
		t.Errorf("Delete: expected ErrInvalidID, got %v", err)
	}
	if err := s.SetActiveID("../../etc/passwd", time.Now()); err != ErrInvalidID {
		t.Errorf("SetActiveID: expected ErrInvalidID, got %v", err)
	}
}

func TestStore_TraversalAttempt_NeverEscapesDir(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	// Even if somehow past ValidID, Get resolves only via directory
	// listing match - create a file OUTSIDE dir and confirm it can never
	// be reached.
	outside := filepath.Join(filepath.Dir(dir), "outside-secret.json")
	os.WriteFile(outside, []byte(`{"id":"profile-0000000000000000","name":"leak"}`), 0o644)
	defer os.Remove(outside)
	if _, err := s.Get("profile-0000000000000000"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound for an id with no real file in the store dir, got %v", err)
	}
}

func TestStore_DeleteInactiveProfile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	a := newTestProfile("A")
	b := newTestProfile("B")
	s.Save(a)
	s.Save(b)
	s.SetActiveID(a.ID, time.Now().UTC())
	if err := s.Delete(b.ID); err != nil {
		t.Errorf("deleting an inactive profile should succeed, got %v", err)
	}
	if _, err := s.Get(b.ID); err != ErrNotFound {
		t.Error("deleted profile should no longer be gettable")
	}
}

func TestStore_DeleteActiveProfile_Rejected(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	a := newTestProfile("A")
	s.Save(a)
	s.SetActiveID(a.ID, time.Now().UTC())
	if err := s.Delete(a.ID); err != ErrActiveProfile {
		t.Errorf("expected ErrActiveProfile, got %v", err)
	}
	// Confirm it was NOT actually deleted.
	if _, err := s.Get(a.ID); err != nil {
		t.Errorf("active profile must still exist after a rejected delete, got %v", err)
	}
}

func TestStore_DeleteUnknownID(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Delete(NewID()); err != ErrNotFound {
		t.Errorf("expected ErrNotFound for an unknown (but validly-shaped) id, got %v", err)
	}
}

func TestStore_SetActiveID_UnknownProfile_Rejected(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.SetActiveID(NewID(), time.Now()); err != ErrNotFound {
		t.Errorf("expected ErrNotFound when activating a profile that was never saved, got %v", err)
	}
}

func TestStore_ListSortOrder(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	s.Save(newTestProfile("Zebra"))
	s.Save(newTestProfile("Alpha"))
	s.Save(newTestProfile("Mike"))
	profiles, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 3 || profiles[0].Name != "Alpha" || profiles[1].Name != "Mike" || profiles[2].Name != "Zebra" {
		t.Errorf("expected alphabetical order, got %v", []string{profiles[0].Name, profiles[1].Name, profiles[2].Name})
	}
}

func TestStore_ConcurrentReadsAndWrites(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	base := newTestProfile("Concurrent")
	if err := s.Save(base); err != nil {
		t.Fatal(err)
	}
	s.SetActiveID(base.ID, time.Now().UTC())

	var wg sync.WaitGroup
	errCh := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.List(); err != nil {
				errCh <- err
			}
		}()
		go func(n int) {
			defer wg.Done()
			p, err := s.Get(base.ID)
			if err != nil {
				errCh <- err
				return
			}
			p.MountingNote = "concurrent update"
			if err := s.Save(p); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent access error: %v", err)
	}
	// Final state must still be internally consistent (readable, single
	// profile, still active).
	if _, err := s.Get(base.ID); err != nil {
		t.Errorf("profile should still be readable after concurrent access: %v", err)
	}
	active, err := s.Active()
	if err != nil || active.ID != base.ID {
		t.Errorf("active profile should be unchanged after concurrent access: %v, %+v", err, active)
	}
}
