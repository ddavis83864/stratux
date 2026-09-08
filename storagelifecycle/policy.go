package storagelifecycle

// Quota bounds one namespace's usage. A zero value means "no limit" -
// see NamespaceQuotaPressure.
type Quota struct {
	MaxBytes int64
	MaxItems int
}

// Policy is the full set of tunables one Manager applies on top of a
// Registry. It is static, injected configuration (see main/'s glue),
// never HTTP-supplied.
type Policy struct {
	// Quotas maps a namespace ID to its Quota. A namespace with no entry
	// here has no quota (see Quota's doc comment).
	Quotas map[string]Quota
	// StaleAfterSeconds is how long (monotonic) a completed Inventory may
	// be relied on before it must be treated as stale - see
	// manager.go's Status.
	StaleAfterSeconds float64
	// RequiredConsecutivePressureSamples feeds Monitor's debounce - see
	// accounting.go.
	RequiredConsecutivePressureSamples int
}

// QuotaFor returns ns's configured Quota, or the zero value (no limit) if
// none is configured.
func (p Policy) QuotaFor(namespaceID string) Quota {
	return p.Quotas[namespaceID]
}

// Evictable reports whether this foundation's own planner may ever
// propose evicting a managed item from a namespace of criticality c.
//
// Only CriticalityCache qualifies. CriticalityBounded (diagnostics)
// deliberately does NOT: diagnostics already has its own tested,
// trusted retention mechanism (readiness.WriteDiagnosticBundle's
// maxRetain pruning, driven by main/diagnosticsapi.go) that this
// foundation must observe and report on, never duplicate or second-guess
// - see docs/storage-lifecycle.md's diagnostics-policy section. A future
// revision that wants this planner to actually drive diagnostic eviction
// would need to retire the existing mechanism first, as an explicit,
// separately reviewed change - not something this function silently
// decides on its own.
//
// CriticalityProtected and CriticalityImportant never qualify, by design
// (see Criticality's own doc comment) - completed recordings, calibration
// profiles, and configuration state are never eviction candidates from
// this foundation's own default policy, regardless of pressure.
func Evictable(c Criticality) bool {
	return c == CriticalityCache
}
