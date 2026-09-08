package storagelifecycle

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

// ItemStatus is what the scanner concluded about one top-level entry
// under a namespace's Root.
type ItemStatus string

const (
	// StatusManaged: matches the namespace's expected shape (right
	// ItemKind, right extension if the namespace restricts one) and is
	// not currently active.
	StatusManaged ItemStatus = "managed"
	// StatusActive: matches the namespace's expected shape AND the
	// owning subsystem reported it as currently in use (see
	// ActiveChecker) - never an eviction candidate.
	StatusActive ItemStatus = "active"
	// StatusUnmanaged: present under a registered namespace's Root but
	// not matching that namespace's expected shape - wrong ItemKind, a
	// symlink, an unsupported file type (FIFO/socket/device), or an
	// unrecognized extension. Reported, counted, and NEVER a deletion
	// candidate - see docs/storage-lifecycle.md's unknown-file policy.
	StatusUnmanaged ItemStatus = "unmanaged"
)

// Item is one top-level entry the scanner found under a namespace's Root.
type Item struct {
	Namespace string
	Name      string // single path segment - see SafeJoin
	Path      string // full path, for logging/action only; never re-derive by string concatenation elsewhere
	IsDir     bool
	Status    ItemStatus
	// SizeBytes is the item's logical size: a regular file's own size, or
	// for a directory item, the sum of its immediate regular-file
	// children's sizes (one level deep - a recording's own sample file
	// plus its metadata.json sidecar, not an arbitrary recursive walk).
	// See docs/storage-lifecycle.md's logical-vs-allocated-usage note for
	// why this can differ from actual disk block consumption.
	SizeBytes int64
	// ModTime is the item's own mtime (a directory item's is the
	// directory entry's own mtime, not derived from its children). Used
	// only as an ordering/informational signal - see planner.go's doc
	// comment on why eviction eligibility never depends on this alone.
	ModTime time.Time
}

// ActiveChecker lets an owning subsystem (recording, and later the
// automatic-recording/FIS-B-cache features) tell the scanner which item
// name(s) in a namespace are currently active, so they are classified
// StatusActive and are never eviction candidates - see contracts.go for
// how a real subsystem wires this in without this package importing it.
type ActiveChecker func(namespace, name string) bool

// Clock is the monotonic time source this package uses everywhere a
// duration matters, mirroring the project's established
// avoid-wall-clock-for-durations convention (see power.Manager's
// nowMonotonic). A wall-clock correction must never change a scan's or a
// plan's internal duration math.
type Clock func() float64 // monotonic seconds, arbitrary epoch

const (
	// maxScanDepth/maxScanItemsPerNamespace bound one namespace's scan so
	// a pathological or adversarial directory (many thousands of entries,
	// or something deeper than this package's flat ItemKindDirectory
	// model expects) cannot make a scan run unbounded. A namespace this
	// size would already indicate something is wrong well before either
	// bound is reached on real hardware.
	maxScanItemsPerNamespace = 100000
)

// Inventory is the result of scanning every registered namespace once.
type Inventory struct {
	// GeneratedAtMonotonic is when this inventory was produced (Clock's
	// reading at scan start) - callers use this to judge staleness (see
	// accounting.go), never a wall-clock field.
	GeneratedAtMonotonic float64
	Namespaces           map[string][]Item
	// Errors holds one entry per namespace whose scan hit a non-fatal
	// problem (a permission error, a file that disappeared mid-scan) -
	// that namespace's Items may be a partial result, never a reason to
	// fail the whole Inventory.
	Errors map[string]error
}

// NamespaceUsage summarizes one namespace's Items.
type NamespaceUsage struct {
	Namespace      string
	ManagedCount   int
	ManagedBytes   int64
	ActiveCount    int
	ActiveBytes    int64
	UnmanagedCount int
	UnmanagedBytes int64
}

// Usage summarizes an Inventory across all namespaces.
func (inv Inventory) Usage() map[string]NamespaceUsage {
	out := make(map[string]NamespaceUsage, len(inv.Namespaces))
	for ns, items := range inv.Namespaces {
		u := NamespaceUsage{Namespace: ns}
		for _, it := range items {
			switch it.Status {
			case StatusManaged:
				u.ManagedCount++
				u.ManagedBytes += it.SizeBytes
			case StatusActive:
				u.ActiveCount++
				u.ActiveBytes += it.SizeBytes
			case StatusUnmanaged:
				u.UnmanagedCount++
				u.UnmanagedBytes += it.SizeBytes
			}
		}
		out[ns] = u
	}
	return out
}

// Scanner produces an Inventory by walking every registered namespace's
// Root exactly one level deep (namespaces are flat by design - see
// Namespace.ItemKind). It performs real filesystem I/O through the
// injected FS, and never follows a symlink, never crosses into a nested
// directory beyond the one level ItemKindDirectory namespaces expect, and
// never blocks on a large file's contents (only Lstat/ReadDir size
// metadata is read - see Item.SizeBytes's doc comment on directory items).
type Scanner struct {
	Registry *Registry
	FS       FS
	Clock    Clock
	Active   ActiveChecker // nil is treated as "nothing is active"
}

// Scan walks every registered namespace and returns one Inventory. It
// never returns an error itself - a namespace-level problem is recorded
// in Inventory.Errors and that namespace's Items reflects whatever was
// successfully observed, so one bad namespace (or one bad entry within
// it) never prevents reporting on every other namespace. This mirrors
// this project's established failure-isolation convention (see
// docs/alerting.md's "Failure isolation" section) - a storage-inventory
// problem must never disrupt ADS-B/GPS/GDL90 processing.
func (s *Scanner) Scan() Inventory {
	inv := Inventory{
		GeneratedAtMonotonic: s.Clock(),
		Namespaces:           make(map[string][]Item),
		Errors:               make(map[string]error),
	}
	for _, ns := range s.Registry.Namespaces() {
		items, err := s.scanNamespace(ns)
		inv.Namespaces[ns.ID] = items
		if err != nil {
			inv.Errors[ns.ID] = err
		}
	}
	return inv
}

func (s *Scanner) scanNamespace(ns Namespace) ([]Item, error) {
	rootInfo, err := s.FS.Lstat(ns.Root)
	if err != nil {
		return nil, fmt.Errorf("namespace %q root: %w", ns.ID, err)
	}
	if rootInfo.IsSymlink {
		return nil, fmt.Errorf("namespace %q root %q is a symlink - refusing to scan", ns.ID, ns.Root)
	}
	if !rootInfo.IsDir {
		return nil, fmt.Errorf("namespace %q root %q is not a directory", ns.ID, ns.Root)
	}

	entries, err := s.FS.ReadDir(ns.Root)
	if err != nil {
		return nil, fmt.Errorf("namespace %q: %w", ns.ID, err)
	}
	if len(entries) > maxScanItemsPerNamespace {
		// Report what we can (truncate deterministically, sorted first)
		// rather than refusing the whole namespace - see the doc comment
		// on maxScanItemsPerNamespace.
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		entries = entries[:maxScanItemsPerNamespace]
	}

	items := make([]Item, 0, len(entries))
	var firstErr error
	for _, e := range entries {
		path, joinErr := SafeJoin(ns.Root, e.Name)
		if joinErr != nil {
			// An entry the filesystem itself handed us that SafeJoin
			// refuses (should be unreachable - ReadDir names are always
			// single path segments - but never trust that blindly).
			if firstErr == nil {
				firstErr = fmt.Errorf("namespace %q: entry %q: %w", ns.ID, e.Name, joinErr)
			}
			continue
		}
		item := s.classify(ns, e, path)
		items = append(items, item)
	}

	// Deterministic ordering regardless of the filesystem's own readdir
	// order (ext4 does not guarantee lexical order) - required by
	// planner.go's determinism guarantee.
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, firstErr
}

func (s *Scanner) classify(ns Namespace, e DirEntry, path string) Item {
	item := Item{
		Namespace: ns.ID,
		Name:      e.Name,
		Path:      path,
		IsDir:     e.IsDir,
		ModTime:   e.ModTime,
	}

	shapeOK := s.matchesShape(ns, e)
	if !shapeOK {
		item.Status = StatusUnmanaged
		item.SizeBytes = e.Size
		return item
	}

	if e.IsDir {
		item.SizeBytes = s.sumDirectChildren(path)
	} else {
		item.SizeBytes = e.Size
	}

	if s.Active != nil && s.Active(ns.ID, e.Name) {
		item.Status = StatusActive
	} else {
		item.Status = StatusManaged
	}
	return item
}

// matchesShape reports whether e is the kind of entry ns expects at all
// (right ItemKind, right extension if restricted) - it does not (and
// cannot, without the owning subsystem's help) validate an item's
// internal content.
func (s *Scanner) matchesShape(ns Namespace, e DirEntry) bool {
	if e.IsSymlink || e.IsOther {
		return false
	}
	switch ns.ItemKind {
	case ItemKindDirectory:
		return e.IsDir
	case ItemKindFile:
		if !e.IsRegular {
			return false
		}
		if len(ns.AllowedExtensions) == 0 {
			return true
		}
		ext := filepath.Ext(e.Name)
		for _, allowed := range ns.AllowedExtensions {
			if ext == allowed {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// sumDirectChildren sums the sizes of dirPath's immediate regular-file
// children only - one level deep, never recursive, matching
// ItemKindDirectory's flat model (a recording directory's own sample file
// and optional metadata.json sidecar, not an arbitrarily nested tree). A
// child that disappears between ReadDir and this sum, or an unreadable
// directory, is tolerated (contributes 0) rather than failing the scan -
// consistent with Scan's overall failure-isolation contract.
func (s *Scanner) sumDirectChildren(dirPath string) int64 {
	children, err := s.FS.ReadDir(dirPath)
	if err != nil {
		return 0
	}
	var total int64
	for _, c := range children {
		if c.IsRegular {
			total += c.Size
		}
	}
	return total
}
