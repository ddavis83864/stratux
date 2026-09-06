package calprofile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned by Store.Get/Delete when id does not resolve to
// a stored profile.
var ErrNotFound = errors.New("calprofile: profile not found")

// ErrDuplicateName is returned by Save when creating a new profile (an ID
// not already present) whose Name case-insensitively matches an existing
// profile's Name. Renaming an existing profile to match another's name is
// rejected the same way - display names are for humans and are expected
// to be unique enough to tell profiles apart in a list, even though ID is
// what everything else keys on.
var ErrDuplicateName = errors.New("calprofile: a profile with this name already exists")

// ErrTooManyProfiles is returned by Save when creating a new profile would
// exceed MaxProfiles.
var ErrTooManyProfiles = errors.New("calprofile: profile limit reached")

// ErrInvalidID is returned when an ID does not match the expected shape,
// or (from Store methods) does not exactly match a real stored profile -
// the same "fast regex reject, then must match a fresh directory listing"
// principle main/safepath.go's resolveNameInDir uses, reimplemented here
// so this package stays self-contained and importable without main.
var ErrInvalidID = errors.New("calprofile: invalid profile id")

// idPattern is the exact shape NewID produces. Deliberately opaque
// (random hex, no embedded timestamp) - a profile's creation time is
// already recorded in CreatedAt; the ID does not need to encode it, and
// keeping it unpredictable and independent of Name is what "unique stable
// IDs independent of display names" requires.
var idPattern = regexp.MustCompile(`^profile-[0-9a-f]{16}$`)

const profileFileSuffix = ".json"

// NewID generates a fresh, unpredictable profile ID.
func NewID() string {
	var b [8]byte
	// crypto/rand.Read on this platform only fails if the OS entropy
	// source itself is broken, in which case there is no safe fallback -
	// a predictable ID would defeat the "independent of display name"
	// requirement, so this deliberately panics rather than silently
	// degrading to something guessable.
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("calprofile: could not generate a profile id: %s", err))
	}
	return "profile-" + hex.EncodeToString(b[:])
}

// ValidID reports whether id has the shape NewID produces. Exported so
// main/'s HTTP handlers can fast-reject an obviously malformed id from a
// request before ever calling into the store.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// activeStateFileName is the well-known name of the small JSON file
// recording which profile is active. Does not match idPattern, so it can
// never collide with a profile file.
const activeStateFileName = "active" + profileFileSuffix

// activeState is the on-disk shape of the active-profile pointer.
type activeState struct {
	ActiveProfileID string    `json:"activeProfileId"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// Store is a directory of profile JSON files plus one active-profile
// pointer file, all written atomically (temp file + rename). One Store
// per directory; safe for concurrent use by multiple goroutines - all
// methods hold Store's own mutex for their full duration, matching the
// dedicated-mutex-per-subsystem convention main/recordingapi.go
// (recMu)/main/diagnosticsapi.go (diagnosticsMu) already use, since
// globalSettings itself has no mutex to borrow (see main/'s wiring for
// why this package never touches globalSettings directly).
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore returns a Store rooted at dir. It does not create dir - callers
// that need it to exist should call EnsureDir first (or rely on Save's own
// os.MkdirAll).
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// Dir returns the store's root directory, for callers (main/'s storage
// certification checks) that need the path without reaching into an
// unexported field.
func (s *Store) Dir() string { return s.dir }

func (s *Store) profilePath(id string) string {
	return filepath.Join(s.dir, id+profileFileSuffix)
}

// atomicWriteJSON marshals v and writes it to path atomically: a temp file
// in the same directory, fsync'd, then renamed over path - matching
// readiness.WriteDiagnosticBundle/common.WriteFanControllerStatus's
// established pattern, with an added fsync before rename (profile data is
// rarer-written and more precious than a diagnostic bundle or a 1Hz status
// snapshot, so the extra durability is worth the small cost here).
func atomicWriteJSON(path string, v interface{}) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("could not create directory: %w", err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("could not create temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("could not write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("could not sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not close temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not finalize file: %w", err)
	}
	return nil
}

// List returns every stored profile, sorted by Name then ID for a stable,
// predictable listing order. A profile file that exists but fails to
// parse (corruption, partial write that somehow survived, foreign
// content) is skipped rather than failing the whole call - see
// ListWithErrors for a variant that also reports which files were
// unreadable, used by diagnostics.
func (s *Store) List() ([]Profile, error) {
	profiles, _, err := s.listWithErrors()
	return profiles, err
}

// ListWithErrors is List, but also returns a map of filename -> error for
// any profile file that could not be read/parsed, so a caller (diagnostics
// bundle generation) can report corruption honestly instead of silently
// pretending those profiles do not exist.
func (s *Store) ListWithErrors() ([]Profile, map[string]string, error) {
	return s.listWithErrors()
}

func (s *Store) listWithErrors() ([]Profile, map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listWithErrorsLocked()
}

func (s *Store) listWithErrorsLocked() ([]Profile, map[string]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var profiles []Profile
	var badFiles map[string]string
	for _, e := range entries {
		if e.IsDir() || e.Name() == activeStateFileName {
			continue
		}
		if !idPattern.MatchString(trimJSONSuffix(e.Name())) {
			continue
		}
		p, err := loadProfileFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			if badFiles == nil {
				badFiles = make(map[string]string)
			}
			badFiles[e.Name()] = err.Error()
			continue
		}
		p.RecomputeValidity()
		profiles = append(profiles, p)
	}
	sort.Slice(profiles, func(i, j int) bool {
		if profiles[i].Name != profiles[j].Name {
			return profiles[i].Name < profiles[j].Name
		}
		return profiles[i].ID < profiles[j].ID
	})
	return profiles, badFiles, nil
}

func trimJSONSuffix(name string) string {
	if len(name) > len(profileFileSuffix) && name[len(name)-len(profileFileSuffix):] == profileFileSuffix {
		return name[:len(name)-len(profileFileSuffix)]
	}
	return name
}

func loadProfileFile(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return Profile{}, fmt.Errorf("corrupt profile JSON: %w", err)
	}
	if p.SchemaVersion > SchemaVersion {
		return Profile{}, fmt.Errorf("unsupported schema version %d (this build understands up to %d)", p.SchemaVersion, SchemaVersion)
	}
	return p, nil
}

// Get returns the stored profile with the given id, resolved only via an
// exact match against a fresh directory listing (never by directly
// building the path from an unvalidated id) - the same safety principle
// as main/safepath.go's resolveNameInDir, reimplemented here so this
// package has no dependency on main.
func (s *Store) Get(id string) (Profile, error) {
	if !ValidID(id) {
		return Profile{}, ErrInvalidID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id)
}

func (s *Store) getLocked(id string) (Profile, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Profile{}, ErrNotFound
		}
		return Profile{}, err
	}
	name := id + profileFileSuffix
	for _, e := range entries {
		if !e.IsDir() && e.Name() == name {
			p, err := loadProfileFile(filepath.Join(s.dir, name))
			if err != nil {
				return Profile{}, err
			}
			p.RecomputeValidity()
			return p, nil
		}
	}
	return Profile{}, ErrNotFound
}

// Count returns how many profiles are currently stored (corrupt files
// included, since they still occupy a slot toward MaxProfiles until
// deleted).
func (s *Store) Count() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && idPattern.MatchString(trimJSONSuffix(e.Name())) {
			n++
		}
	}
	return n, nil
}

// Save validates p, then atomically writes it, creating a new profile if
// p.ID does not already exist or overwriting the existing one if it does.
// Save is the only way this package ever writes a profile file - every
// mutation (create, metadata edit, calibration capture) goes through a
// full Profile value and this one atomic-write path.
func (s *Store) Save(p Profile) error {
	if !ValidID(p.ID) {
		return ErrInvalidID
	}
	p.RecomputeValidity()
	if err := ValidateProfile(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.getLocked(p.ID)
	isNew := err == ErrNotFound
	if err != nil && !isNew {
		return err
	}
	if isNew {
		n, err := s.countLocked()
		if err != nil {
			return err
		}
		if n >= MaxProfiles {
			return ErrTooManyProfiles
		}
	}
	// Duplicate-name check: case-insensitive, against every OTHER
	// profile (a no-op re-save of the same profile under its own
	// existing name must not reject itself).
	all, _, err := s.listWithErrorsLocked()
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.ID == p.ID {
			continue
		}
		if equalFoldRunes(other.Name, p.Name) {
			return ErrDuplicateName
		}
	}
	return atomicWriteJSON(s.profilePath(p.ID), p)
}

func (s *Store) countLocked() (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && idPattern.MatchString(trimJSONSuffix(e.Name())) {
			n++
		}
	}
	return n, nil
}

func equalFoldRunes(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	if len(ra) != len(rb) {
		return false
	}
	for i := range ra {
		if toLowerRune(ra[i]) != toLowerRune(rb[i]) {
			return false
		}
	}
	return true
}

func toLowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// Delete removes the stored profile with the given id. Deleting the
// currently-active profile is refused (ErrActiveProfile) - callers
// (main/'s HTTP handler) must activate a different profile first; this
// store never leaves the active pointer referencing a profile that no
// longer exists.
var ErrActiveProfile = errors.New("calprofile: cannot delete the active profile")

func (s *Store) Delete(id string) error {
	if !ValidID(id) {
		return ErrInvalidID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.getLocked(id); err != nil {
		return err
	}
	active, err := s.activeIDLocked()
	if err != nil {
		return err
	}
	if active == id {
		return ErrActiveProfile
	}
	if err := os.Remove(s.profilePath(id)); err != nil {
		return err
	}
	return nil
}

// ActiveID returns the currently active profile's ID, or "" if none has
// ever been set (a fresh, un-migrated store).
func (s *Store) ActiveID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeIDLocked()
}

func (s *Store) activeIDLocked() (string, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, activeStateFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var st activeState
	if err := json.Unmarshal(data, &st); err != nil {
		return "", fmt.Errorf("corrupt active-profile state: %w", err)
	}
	return st.ActiveProfileID, nil
}

// Active returns the currently active profile. Distinguished errors:
// ErrNoActiveProfile if no active pointer has ever been set (fresh store,
// migration not yet run); ErrNotFound if the pointer references a profile
// that no longer exists (corrupt/inconsistent state - the profile it
// pointed to was removed outside this package, or its file is corrupt).
var ErrNoActiveProfile = errors.New("calprofile: no active profile is set")

func (s *Store) Active() (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.activeIDLocked()
	if err != nil {
		return Profile{}, err
	}
	if id == "" {
		return Profile{}, ErrNoActiveProfile
	}
	return s.getLocked(id)
}

// SetActiveID atomically sets the active-profile pointer to id, after
// confirming id resolves to a real, stored profile - the active pointer
// may never reference a profile that does not exist.
func (s *Store) SetActiveID(id string, now time.Time) error {
	if !ValidID(id) {
		return ErrInvalidID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.getLocked(id); err != nil {
		return err
	}
	return atomicWriteJSON(filepath.Join(s.dir, activeStateFileName), activeState{
		ActiveProfileID: id,
		UpdatedAt:       now,
	})
}
