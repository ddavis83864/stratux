/*
manager.go ties Registry + Policy + Scanner + Monitor into the one
stateful object main/'s glue holds - Manager itself performs no periodic
work on its own (no goroutine, no ticker): a caller drives it, exactly
like main/health.go drives readiness's health sampling and power.Monitor.

Manager.Scan is this mission's only production-facing mutating-adjacent
operation, and even it only ever reads the filesystem and updates
Manager's own in-memory snapshot - it never deletes anything. Plan
(planner.go) and PlanRecovery/ExecuteRecovery (recovery.go) remain
available as pure functions any caller (including a future automatic-
recording or FIS-B-cache feature, or this mission's own tests) can call
directly; this foundation's own main/ integration (see
docs/storage-lifecycle.md) calls Scan and Plan for OBSERVATION only and
never calls ExecuteRecovery's sibling, a hypothetical "ExecutePlan," on
live data - no such function exists in this package at all, precisely so
there is no code path this mission could have wired into an HTTP handler
even by mistake. A future, separately authorized mission that wants real
enforcement would add that execution step then, deliberately.
*/
package storagelifecycle

import (
	"fmt"
	"sync"

	"github.com/stratux/stratux/readiness"
)

// FilesystemPressureFunc reports the current whole-filesystem
// utilization percentage - main/'s glue wires this to the already-
// computed readiness.StorageHealth.UtilizationPercent so this package
// never recomputes it. ok is false if no valid reading exists yet (e.g.
// during the same startup grace period readiness itself observes).
type FilesystemPressureFunc func() (utilizationPercent float64, ok bool)

// ManagerConfig is everything Manager needs at construction. All fields
// are static, process-lifetime configuration - see NewManager.
type ManagerConfig struct {
	Registry           *Registry
	Policy             Policy
	FS                 FS
	Clock              Clock
	Active             ActiveChecker
	FilesystemPressure FilesystemPressureFunc
	// FilesystemThresholds is passed straight through to
	// FilesystemPressure's classification - see accounting.go's
	// FilesystemPressure function. Callers should pass
	// readiness.DefaultPersistentStorageThresholds() (or whatever
	// thresholds readiness itself is actually configured with) so the
	// two packages never disagree about what "elevated" means.
	FilesystemThresholds readiness.StorageThresholds
}

// Status is Manager.Status's full snapshot - the shape readiness,
// preflight, diagnostics, and the dashboard all read from, so none of
// them need to know how Manager derived it.
type Status struct {
	HasInventory         bool
	GeneratedAtMonotonic float64
	AgeSeconds           float64 // 0 if !HasInventory
	Stale                bool
	ScanErrors           map[string]error // per-namespace, from the last scan
	Pressure             PressureState
	Namespaces           map[string]NamespaceUsage
	// UnmanagedTotalCount/Bytes sums NamespaceUsage's own unmanaged
	// counters across every namespace - reported at top level because
	// "how much do we not recognize at all" is exactly the kind of
	// single number a readiness/preflight caution should be able to show
	// without iterating every namespace itself.
	UnmanagedTotalCount int
	UnmanagedTotalBytes int64
	ProtectedTotalBytes int64 // sum of every non-cache-criticality namespace's managed+active bytes
}

// Manager holds the one live Inventory snapshot and debounced pressure
// reading for a fixed Registry/Policy. Safe for concurrent use - see
// Scan's doc comment for exactly what "concurrent Scan calls coalesce"
// means and why.
type Manager struct {
	cfg     ManagerConfig
	scanner *Scanner
	monitor *Monitor

	mu           sync.Mutex
	inventory    Inventory
	hasInventory bool
	scanning     bool
}

// NewManager constructs a Manager. It performs no I/O itself - the first
// real filesystem access happens on the first call to Scan.
func NewManager(cfg ManagerConfig) *Manager {
	return &Manager{
		cfg: cfg,
		scanner: &Scanner{
			Registry: cfg.Registry,
			FS:       cfg.FS,
			Clock:    cfg.Clock,
			Active:   cfg.Active,
		},
		monitor: NewMonitor(maxInt(cfg.Policy.RequiredConsecutivePressureSamples, 1)),
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Scan runs a fresh Scanner.Scan and updates Manager's snapshot. If a
// scan is already in progress on another goroutine, Scan returns
// immediately with performed=false and the CALLER should treat the
// existing (possibly slightly older) snapshot, available via Status, as
// good enough - this is the "coalesce" behavior the concurrency
// requirements call for: two overlapping callers (e.g. a periodic health
// tick and an on-demand diagnostics request) never both pay the cost of a
// redundant filesystem walk, and never race to write two different
// results into Manager's state.
//
// The actual filesystem walk (Scanner.Scan) runs with Manager's own mutex
// released - only the brief "am I already scanning" check-and-set and the
// final "store the result" step hold it - so a slow namespace (a huge
// directory, a stalled mount) never blocks a concurrent Status() call, and
// Scan never holds any lock while calling the injected ActiveChecker,
// which is main/'s glue's own responsibility not to block on (see
// ActiveChecker's doc comment in inventory.go).
func (m *Manager) Scan() (inv Inventory, performed bool) {
	m.mu.Lock()
	if m.scanning {
		snapshot := m.inventory
		m.mu.Unlock()
		return snapshot, false
	}
	m.scanning = true
	m.mu.Unlock()

	result := m.scanner.Scan()

	m.mu.Lock()
	m.inventory = result
	m.hasInventory = true
	m.scanning = false
	m.mu.Unlock()

	return result, true
}

// Status returns Manager's current snapshot without scanning. Safe to
// call from any goroutine, including while a Scan is in progress
// elsewhere (it reads whatever the last completed scan produced).
func (m *Manager) Status() Status {
	m.mu.Lock()
	inv := m.inventory
	has := m.hasInventory
	m.mu.Unlock()

	st := Status{HasInventory: has}
	if !has {
		st.Pressure = m.monitor.Observe(PressureUnknown)
		return st
	}

	st.GeneratedAtMonotonic = inv.GeneratedAtMonotonic
	nowMono := m.cfg.Clock()
	st.AgeSeconds = nowMono - inv.GeneratedAtMonotonic
	if m.cfg.Policy.StaleAfterSeconds > 0 && st.AgeSeconds > m.cfg.Policy.StaleAfterSeconds {
		st.Stale = true
	}
	st.ScanErrors = inv.Errors
	st.Namespaces = inv.Usage()

	raw := PressureUnknown
	if len(inv.Errors) > 0 || st.Stale {
		raw = PressureUnknown
	} else {
		raw = PressureNormal
		if m.cfg.FilesystemPressure != nil {
			if pct, ok := m.cfg.FilesystemPressure(); ok {
				raw = worse(raw, FilesystemPressure(pct, m.cfg.FilesystemThresholds))
			} else {
				raw = worse(raw, PressureUnknown)
			}
		}
		for nsID, usage := range st.Namespaces {
			quota := m.cfg.Policy.QuotaFor(nsID)
			raw = worse(raw, NamespaceQuotaPressure(usage.ManagedBytes+usage.ActiveBytes, quota))
		}
	}
	st.Pressure = m.monitor.Observe(raw)

	for _, ns := range m.cfg.Registry.Namespaces() {
		usage, ok := st.Namespaces[ns.ID]
		if !ok {
			continue
		}
		st.UnmanagedTotalCount += usage.UnmanagedCount
		st.UnmanagedTotalBytes += usage.UnmanagedBytes
		if ns.Criticality != CriticalityCache {
			st.ProtectedTotalBytes += usage.ManagedBytes + usage.ActiveBytes
		}
	}
	return st
}

// Plan returns a RetentionPlan for one registered namespace, using
// Manager's last completed scan - callers needing a fresh view should
// call Scan first. Returns an error if the namespace is not registered or
// no scan has ever completed.
func (m *Manager) Plan(namespaceID string, params PlanParams) (RetentionPlan, error) {
	ns, ok := m.cfg.Registry.Lookup(namespaceID)
	if !ok {
		return RetentionPlan{}, fmt.Errorf("storagelifecycle: namespace %q is not registered", namespaceID)
	}
	m.mu.Lock()
	inv := m.inventory
	has := m.hasInventory
	m.mu.Unlock()
	if !has {
		return RetentionPlan{}, fmt.Errorf("storagelifecycle: no inventory has been scanned yet")
	}
	items := inv.Namespaces[namespaceID]
	quota := m.cfg.Policy.QuotaFor(namespaceID)
	return Plan(ns, items, quota, params), nil
}
