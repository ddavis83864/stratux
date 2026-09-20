// Package epaper implements the optional Waveshare 3.7" e-paper status
// display as a small, pure decision core: configuration validation, GPIO
// pin-mapping conflict checks, change-driven refresh policy, and the
// content model for what the panel should show. Like readiness/preflight/
// alerting/calprofile, this package touches no hardware, no files, and no
// clocks beyond explicit parameters - every function takes already-known
// values and returns a decision, so the whole policy is exercised by
// `go test` with no Raspberry Pi, no GPIO, and no SPI bus attached.
//
// The actual GPIO/SPI I/O, rendering to pixels, and process lifecycle live
// in epaper_main, a separate binary/systemd service - see that package's
// doc comment for why this is a separate process rather than code inside
// the main stratuxrun daemon.
//
// This is a supplemental status surface, not a certified flight
// instrument. See docs/waveshare-epaper-display.md for the full aviation
// disclaimer and scope statement.
package epaper

import "fmt"

// Config is the complete, user-facing configuration for the e-paper
// display. The zero value is the safe default: disabled, touching no
// hardware.
type Config struct {
	// Enabled must be explicitly set true - the zero value (false) is
	// the shipped default, matching every other optional subsystem's own
	// disabled-by-default convention in this codebase.
	Enabled bool `json:"enabled"`

	// Panel identifies the physical panel model. Only "waveshare-3.7in"
	// is supported today; the field exists so a future panel can be
	// added without a breaking config change.
	Panel string `json:"panel"`

	// Rotation is one of 0, 90, 180, 270 (degrees, clockwise).
	Rotation int `json:"rotation"`

	// RefreshIntervalSeconds bounds how often a partial refresh may be
	// considered, even if the underlying status data changes faster -
	// see docs/waveshare-epaper-display.md's update-frequency policy.
	// Zero or negative falls back to DefaultRefreshIntervalSeconds.
	RefreshIntervalSeconds int `json:"refreshIntervalSeconds"`

	// FullRefreshEvery is how many partial refreshes may occur before a
	// full (flashing) refresh is forced, to bound e-paper ghosting. Zero
	// or negative falls back to DefaultFullRefreshEvery.
	FullRefreshEvery int `json:"fullRefreshEvery"`

	// Page selects which status page is shown - see Page* constants.
	Page string `json:"page"`

	// GPIO is the pin mapping to use. A zero-value GPIO (all fields 0)
	// means "use DefaultGPIOMapping()".
	GPIO GPIOMapping `json:"gpio"`
}

// Supported panel identifiers.
const (
	PanelWaveshare37 = "waveshare-3.7in"
)

// Supported status pages.
const (
	PageOverview  = "overview"  // version, readiness, GPS/time trust, storage/overlay
	PageReceivers = "receivers" // 978/1090/GDL90/traffic count
	PageHealth    = "health"    // AHRS/baro/fan/power/temperature
)

// DefaultRefreshIntervalSeconds/DefaultFullRefreshEvery are conservative
// defaults appropriate for a 280x480 1-bit black/white panel (this driver
// never uses the controller's grayscale LUT modes - see epaper_main/
// render.go) with a multi-second full-refresh cost - see
// docs/waveshare-epaper-display.md.
const (
	DefaultRefreshIntervalSeconds = 15
	DefaultFullRefreshEvery       = 20
	// MinRefreshIntervalSeconds is an absolute floor: below this, even a
	// change-driven partial refresh could not keep up with the panel's
	// own physical refresh latency without queuing up work indefinitely.
	MinRefreshIntervalSeconds = 5
	// MaxFullRefreshEvery bounds how long ghosting could accumulate
	// between forced full refreshes.
	MaxFullRefreshEvery = 200
	// StaleDataThresholdSeconds: if the status data driving the display
	// is older than this, the display must honestly show a stale/offline
	// indication rather than a confident-looking but outdated value -
	// see docs/waveshare-epaper-display.md.
	StaleDataThresholdSeconds = 30
)

var validPanels = map[string]bool{PanelWaveshare37: true}
var validPages = map[string]bool{PageOverview: true, PageReceivers: true, PageHealth: true}
var validRotations = map[int]bool{0: true, 90: true, 180: true, 270: true}

// Normalize returns a copy of c with every zero-valued optional field
// filled in with its documented default, and reports whether the
// (already-defaulted) configuration is valid. It never mutates c.
//
// A disabled config (Enabled == false) is always valid regardless of the
// other fields' contents - an optional, disabled feature must never fail
// validation and block unrelated settings changes (e.g. Configuration
// Backup restore) just because its own fields happen to hold stale or
// unrecognized values from a future version.
func Normalize(c Config) (Config, error) {
	if c.Panel == "" {
		c.Panel = PanelWaveshare37
	}
	if c.Page == "" {
		c.Page = PageOverview
	}
	if c.RefreshIntervalSeconds <= 0 {
		c.RefreshIntervalSeconds = DefaultRefreshIntervalSeconds
	}
	if c.FullRefreshEvery <= 0 {
		c.FullRefreshEvery = DefaultFullRefreshEvery
	}
	if c.GPIO == (GPIOMapping{}) {
		c.GPIO = DefaultGPIOMapping()
	}

	if !c.Enabled {
		return c, nil
	}

	if !validPanels[c.Panel] {
		return c, fmt.Errorf("epaper: unsupported panel %q", c.Panel)
	}
	if !validPages[c.Page] {
		return c, fmt.Errorf("epaper: unsupported page %q", c.Page)
	}
	if !validRotations[c.Rotation] {
		return c, fmt.Errorf("epaper: rotation must be 0, 90, 180, or 270, got %d", c.Rotation)
	}
	if c.RefreshIntervalSeconds < MinRefreshIntervalSeconds {
		return c, fmt.Errorf("epaper: refreshIntervalSeconds must be >= %d, got %d", MinRefreshIntervalSeconds, c.RefreshIntervalSeconds)
	}
	if c.FullRefreshEvery > MaxFullRefreshEvery {
		return c, fmt.Errorf("epaper: fullRefreshEvery must be <= %d, got %d", MaxFullRefreshEvery, c.FullRefreshEvery)
	}
	if err := c.GPIO.Validate(); err != nil {
		return c, fmt.Errorf("epaper: %w", err)
	}
	return c, nil
}
