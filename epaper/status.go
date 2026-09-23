package epaper

import "time"

// ServiceState is the epaper_main process's own bounded, honest
// self-reported state - never a raw error string blindly surfaced to the
// dashboard, so a driver failure is always classified into one of these
// before anything else sees it.
type ServiceState string

const (
	// StateDisabled: Config.Enabled is false. No GPIO/SPI is touched.
	// This is the shipped default and must never be reported as
	// degrading overall Stratux readiness.
	StateDisabled ServiceState = "DISABLED"
	// StateRunning: the panel was detected and the last refresh (if any
	// was due) succeeded.
	StateRunning ServiceState = "RUNNING"
	// StateNotDetected: enabled, but the panel did not respond (BUSY
	// line never went idle on init, or an SPI probe failed) - most
	// commonly means no display is physically connected.
	StateNotDetected ServiceState = "NOT_DETECTED"
	// StateError: enabled and a panel was previously detected, but a
	// refresh attempt failed (BUSY timeout, SPI write error, etc.). See
	// Health.LastErrorCategory for the specific kind.
	StateError ServiceState = "ERROR"
)

// ErrorCategory bounds what LastErrorCategory may say - never a raw Go
// error string, so a driver bug can't leak an unbounded or sensitive
// message onto the dashboard.
type ErrorCategory string

const (
	ErrorNone           ErrorCategory = ""
	ErrorBusyTimeout    ErrorCategory = "BUSY_TIMEOUT"
	ErrorSPIWrite       ErrorCategory = "SPI_WRITE_FAILED"
	ErrorGPIOOpen       ErrorCategory = "GPIO_OPEN_FAILED"
	ErrorPanelNotFound  ErrorCategory = "PANEL_NOT_FOUND"
	ErrorConfigInvalid  ErrorCategory = "CONFIG_INVALID"
	ErrorStatusSourceUp ErrorCategory = "STATUS_SOURCE_UNAVAILABLE"
)

// Health is the complete, bounded, self-reported runtime status written
// by epaper_main and read by the main daemon for dashboard/diagnostics
// display - the same self-reporting pattern as
// common.FanControllerStatus. Every field is either a small enum, a
// count, or a timestamp - never a raw payload, coordinate, or credential,
// matching the mission's observability requirements.
type Health struct {
	UpdatedAt time.Time `json:"updatedAt"`

	State           ServiceState `json:"state"`
	ConfiguredPanel string       `json:"configuredPanel,omitempty"`
	// PanelDetected is protocol-success-based, not identity-based: it
	// means the configured driver's last refresh attempt completed its
	// BUSY handshake without a timeout or SPI error - never that the
	// physically-connected hardware has been confirmed to actually be
	// ConfiguredPanel. A real hardware-validation finding: the 3.7in
	// driver reported PanelDetected=true (and completed real refresh
	// cycles) while a Waveshare 4.2in V2 panel was the one actually
	// wired, because both panels' controllers respond enough to the
	// generic reset/BUSY handshake this check relies on for Init() to
	// succeed, even though the panel-specific RAM addressing/content
	// would be wrong. ConfiguredPanel (from the owner's own EpaperPanel
	// setting) is the only authoritative source of which panel is
	// configured - this field can never substitute for it, and no
	// hardware identity register exists to check instead.
	PanelDetected bool          `json:"panelDetected"`
	LastErrorCat  ErrorCategory `json:"lastErrorCategory,omitempty"`

	LastSuccessfulRefresh time.Time `json:"lastSuccessfulRefresh,omitempty"`
	ConsecutiveFailures   int       `json:"consecutiveFailures"`
	BusyTimeoutCount      int       `json:"busyTimeoutCount"`
	FullRefreshCount      int       `json:"fullRefreshCount"`
	PartialRefreshCount   int       `json:"partialRefreshCount"`

	// DataAgeSeconds is how old the status snapshot behind the
	// currently-displayed content is, as of UpdatedAt - lets the
	// dashboard (and the panel's own stale-data indicator, see
	// ShouldShowStale) be judged independently.
	DataAgeSeconds float64 `json:"dataAgeSeconds"`
}
