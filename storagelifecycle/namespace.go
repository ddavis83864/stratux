/*
Package storagelifecycle is the shared, hardware-independent foundation
for tracking and reasoning about everything Stratux keeps on the
persistent data partition (/var/lib/stratux-data on real hardware) - the
storage-side counterpart to the readiness/preflight/diagnostics packages.

It exists so two upcoming features - automatic flight recording and a
rolling FIS-B weather cache - share one inventory, quota, retention, and
atomic-write model instead of each inventing its own. This package itself
does not enable either feature: see contracts.go for the interfaces they
will use, and docs/storage-lifecycle.md for the full design and the
explicit "not enabled yet" statement.

Every namespace this package manages must be registered application
configuration (see Namespace below) - never an HTTP-supplied path. This
package performs no network I/O and never automatically deletes anything
on its own initiative; see manager.go's doc comment for exactly what
"foundation, not enforcement" means here.
*/
package storagelifecycle

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Criticality classifies how a namespace's contents may be treated by
// future retention/eviction logic. It is deliberately coarser than a
// numeric priority: the mission-mandated ordering (see Policy) is fixed
// and not meant to be re-derived from an arbitrary number per namespace.
type Criticality string

const (
	// CriticalityProtected means: never an eviction candidate, regardless
	// of pressure. Calibration profiles, configuration state, and
	// session/shutdown markers belong here.
	CriticalityProtected Criticality = "protected"
	// CriticalityImportant means: user-meaningful data (completed
	// recordings) that must not be evicted by this foundation's own
	// default policy - only a future, explicit, separately authorized
	// recording-retention policy may ever make these eligible.
	CriticalityImportant Criticality = "important"
	// CriticalityBounded means: the namespace already has its own
	// existing, tested retention mechanism (diagnostics' maxRetain
	// pruning) that this package must observe and report, never
	// reinterpret or duplicate.
	CriticalityBounded Criticality = "bounded"
	// CriticalityCache means: fully regenerable data (the future FIS-B
	// cache). This is the only criticality this package's planner may
	// ever propose evicting under pressure.
	CriticalityCache Criticality = "cache"
)

// Namespace is one registered, statically-configured area of the
// persistent partition this package knows how to inventory. Namespaces
// are never derived from HTTP input - see RegisterNamespace.
type Namespace struct {
	// ID is a short, stable, log/API-safe identifier (e.g. "recordings",
	// "diagnostics") - never a filesystem path.
	ID string
	// Root is the absolute directory this namespace owns. Must not be
	// empty, must not equal or contain another registered namespace's
	// Root (see Registry.Register), and is never followed through a
	// symlink by this package (see inventory.go's scanner).
	Root string
	// Criticality governs default eviction eligibility - see the
	// Criticality constants above.
	Criticality Criticality
	// AllowedExtensions, if non-empty, restricts which file extensions
	// (including the leading dot, e.g. ".json") this namespace's
	// top-level items may have. A directory-shaped item (e.g. one
	// recording's own subdirectory) is matched by ItemKindDirectory
	// instead and ignores this list. Empty means "any extension is this
	// namespace's own concern to validate" (used by namespaces like
	// recordings, whose items are directories).
	AllowedExtensions []string
	// ItemKind is what one top-level entry under Root represents to this
	// namespace's owner - see ItemKind.
	ItemKind ItemKind
	// MinRetention is the minimum time an item must exist before it can
	// ever be proposed for eviction, regardless of pressure - zero means
	// "no minimum beyond Criticality's own protection." Measured against
	// the injected Clock's monotonic reading at scan time, never wall
	// clock (see docs/storage-lifecycle.md's clock-correction rationale).
	MinRetention float64 // seconds
}

// ItemKind says what shape one top-level entry in a namespace takes.
type ItemKind int

const (
	// ItemKindFile means each top-level entry is a single regular file
	// (e.g. diagnostics, calibration profiles, the alert-settings file).
	ItemKindFile ItemKind = iota
	// ItemKindDirectory means each top-level entry is itself a directory
	// whose contents are that item's concern, not individually inventoried
	// as separate items (e.g. one recording's own directory, which may
	// contain a sample file and an optional metadata.json sidecar).
	ItemKindDirectory
)

// Validate reports a namespace's own configuration errors - called once
// by Registry.Register, never re-validated per scan.
func (n Namespace) Validate() error {
	if n.ID == "" {
		return fmt.Errorf("storagelifecycle: namespace ID must not be empty")
	}
	if strings.ContainsAny(n.ID, "/\\.") {
		return fmt.Errorf("storagelifecycle: namespace ID %q must not contain path separators or dots", n.ID)
	}
	if n.Root == "" {
		return fmt.Errorf("storagelifecycle: namespace %q: Root must not be empty", n.ID)
	}
	if !filepath.IsAbs(n.Root) {
		return fmt.Errorf("storagelifecycle: namespace %q: Root %q must be an absolute path", n.ID, n.Root)
	}
	if filepath.Clean(n.Root) != n.Root {
		return fmt.Errorf("storagelifecycle: namespace %q: Root %q must already be filepath.Clean", n.ID, n.Root)
	}
	switch n.Criticality {
	case CriticalityProtected, CriticalityImportant, CriticalityBounded, CriticalityCache:
	default:
		return fmt.Errorf("storagelifecycle: namespace %q: unknown criticality %q", n.ID, n.Criticality)
	}
	if n.MinRetention < 0 {
		return fmt.Errorf("storagelifecycle: namespace %q: MinRetention must not be negative", n.ID)
	}
	return nil
}

// Registry holds the fixed set of namespaces one Manager operates over.
// It is built once at startup from static configuration (see main/'s
// glue) and never mutated by an HTTP request.
type Registry struct {
	byID  map[string]Namespace
	order []string // registration order, for deterministic iteration
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]Namespace)}
}

// Register validates and adds ns. It rejects a duplicate ID, an empty/
// relative Root, and a Root that is equal to, contains, or is contained by
// an already-registered namespace's Root (namespace roots must never
// overlap - each byte of the persistent partition this package manages
// belongs to exactly one namespace).
func (r *Registry) Register(ns Namespace) error {
	if err := ns.Validate(); err != nil {
		return err
	}
	if _, exists := r.byID[ns.ID]; exists {
		return fmt.Errorf("storagelifecycle: namespace ID %q already registered", ns.ID)
	}
	for _, existingID := range r.order {
		existing := r.byID[existingID]
		if rootsOverlap(ns.Root, existing.Root) {
			return fmt.Errorf("storagelifecycle: namespace %q root %q overlaps namespace %q root %q", ns.ID, ns.Root, existing.ID, existing.Root)
		}
	}
	r.byID[ns.ID] = ns
	r.order = append(r.order, ns.ID)
	return nil
}

// rootsOverlap reports whether a and b are equal or one is an ancestor
// directory of the other.
func rootsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	aWithSep := a + string(filepath.Separator)
	bWithSep := b + string(filepath.Separator)
	return strings.HasPrefix(bWithSep, aWithSep) || strings.HasPrefix(aWithSep, bWithSep)
}

// Namespaces returns every registered namespace in registration order.
func (r *Registry) Namespaces() []Namespace {
	out := make([]Namespace, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// Lookup returns the namespace with the given ID, if registered.
func (r *Registry) Lookup(id string) (Namespace, bool) {
	ns, ok := r.byID[id]
	return ns, ok
}

// SafeJoin joins name onto a namespace's Root, rejecting any name that
// would escape Root: path traversal (".."), an absolute path, a name
// containing a path separator at all (every namespace this package
// manages has flat, single-segment item names - a recording's own
// internal files are that recording's own concern, not this package's),
// and an empty name. It never touches the filesystem itself - see
// inventory.go/atomicwrite.go for where the result is actually used, and
// note that neither this function nor its callers ever follow a symlink:
// resolving one is exactly the class of trick this function exists to
// prevent from being reachable via a namespace name in the first place.
func SafeJoin(root, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("storagelifecycle: empty item name")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("storagelifecycle: item name %q must not be an absolute path", name)
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("storagelifecycle: item name %q must not contain a path separator", name)
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("storagelifecycle: item name %q is not a valid item name", name)
	}
	joined := filepath.Join(root, name)
	rootWithSep := root + string(filepath.Separator)
	if joined != root && !strings.HasPrefix(joined, rootWithSep) {
		// Defense in depth - the single-segment/no-separator checks above
		// already make this unreachable for any name that passed them,
		// but a result outside root must never be returned regardless.
		return "", fmt.Errorf("storagelifecycle: item name %q escapes its namespace root", name)
	}
	return joined, nil
}
