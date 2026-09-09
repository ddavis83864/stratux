/*
Package fisbcache is the pure, hardware-independent domain model for a
bounded, freshness-aware rolling cache of weather products genuinely
received over 978 MHz FIS-B. It implements storagelifecycle.CacheLifecycle
(see main/fisbcachestorage.go for the production adapter) rather than
inventing a second storage-pressure/retention engine of its own.

This package never opens a radio device, never imports main, never
depends on cgo, and starts no goroutine of its own - it is deterministic
logic only: given a decoded product and the current monotonic/wall-clock
state, it decides whether to admit, supersede, or reject the product, and
what freshness state an already-cached entry is currently in. Runtime
glue (the capture-path hook, the bounded queue, the persistence loop, the
HTTP API) lives in main/ - see docs/fisb-weather-cache.md.

Scope, established from direct inspection of this project's existing
uatparse/main FIS-B pipeline (see docs/fisb-weather-cache.md's "Existing
live FIS-B data path" section for full citations) rather than assumed:

  - Only two product classes have ANY structured decode in this codebase
    today: generic FIS-B text (product ID 413, itself further identified
    by the leading token of each decoded text line - METAR/TAF/PIREP/
    WINDS/etc.) and NEXRAD raster tiles (product IDs 63/64). Every other
    product ID is presently discarded past a bare product-ID tag - see
    uatparse.UATFrame.decodeInfoFrame's own "don't know what to do with
    product id" default branch, and the fact that its own
    structured AIRMET/SIGMET/NOTAM decoder (decodeAirmet) is dead code
    (its only call site is commented out). This package therefore only
    ever admits ClassText and ClassNexradTile entries; every other
    product ID is ClassUnsupported and is never persisted (see
    ClassifyProductID).
  - FIS-B's own broadcast-time encoding never includes a year, and two of
    its four time-format options omit month/day entirely - see
    ReconstructSourceTime for how (and how conservatively) this package
    fills that gap using a trusted receive time.
*/
package fisbcache

// ProductClass is this package's own coarse classification of what a
// received product actually is, independent of - but derived from - the
// raw FIS-B product ID. Never confuse this with storagelifecycle's
// opaque CacheProductKey.ProductType string (see Key.StorageProductType).
type ProductClass string

const (
	// ClassText is generic FIS-B text (product ID 413), further
	// identified by TextProductType - covers METAR/SPECI/TAF/TAF.AMD/
	// WINDS/PIREP text reports exactly as this project's own
	// main.registerADSBTextMessageReceived already classifies them.
	ClassText ProductClass = "text"
	// ClassNexradTile is a NEXRAD Regional/CONUS raster tile (product ID
	// 63 or 64).
	ClassNexradTile ProductClass = "nexrad_tile"
	// ClassUnsupported is every other FIS-B product ID this codebase has
	// no structured decode for today (see this package's own doc
	// comment) - never admitted to the cache. Tracking "N frames of this
	// product ID were observed" remains this project's existing
	// UpdateUATStats counters' job, not this cache's.
	ClassUnsupported ProductClass = "unsupported"
)

// ClassifyProductID reports which ProductClass a raw FIS-B product ID
// belongs to, using exactly the same two live decode paths this
// project's uatparse.UATFrame.decodeInfoFrame already implements (see
// this package's doc comment) - never a guess at a product ID this
// codebase does not actually decode.
func ClassifyProductID(productID uint32) ProductClass {
	switch productID {
	case 413:
		return ClassText
	case 63, 64:
		return ClassNexradTile
	default:
		return ClassUnsupported
	}
}

// TextProductType is the leading token of one decoded FIS-B text line -
// exactly what main.registerADSBTextMessageReceived already treats as
// identifying the report type (its own x[0]). Kept as a plain string,
// not an enum, because the live text stream can legitimately contain
// tokens this package has never seen (a new NWS product prefix, a typo'd
// ground station) - see the freshness-policy table (policy.go) for the
// subset this package actually knows a specific, evidence-based
// freshness policy for.
type TextProductType = string

const (
	TextProductMETAR      TextProductType = "METAR"
	TextProductSPECI      TextProductType = "SPECI"
	TextProductTAF        TextProductType = "TAF"
	TextProductTAFAmended TextProductType = "TAF.AMD"
	TextProductWinds      TextProductType = "WINDS"
	TextProductPIREP      TextProductType = "PIREP"
)

// Key identifies one cache entry - the pure-package equivalent of
// storagelifecycle.CacheProductKey, which is what this package's
// production adapter (main/fisbcachestorage.go) actually constructs from
// a Key via StorageProductType/StorageProductID.
type Key struct {
	Class ProductClass
	// Identity is class-specific:
	//   - ClassText: "<type> <location>" (e.g. "METAR KSEA") - the exact
	//     pair main.WeatherMessage already carries as Type/Location, so
	//     this cache's identity notion matches what this project already
	//     considers "the same report series" for its own live text
	//     stream, without inventing a new notion of station identity.
	//   - ClassNexradTile: a stable string built from the tile's own
	//     quantized geographic bounds and radar type/scale - see
	//     NexradTileIdentity.
	Identity string
}

// StorageProductType/StorageProductID adapt Key into
// storagelifecycle.CacheProductKey's two opaque strings - see
// main/fisbcachestorage.go.
func (k Key) StorageProductType() string { return string(k.Class) }
func (k Key) StorageProductID() string   { return k.Identity }

// TextKey builds the Key for a decoded FIS-B text report, given exactly
// the (Type, Location) pair main.WeatherMessage already extracts.
func TextKey(productType, location string) Key {
	return Key{Class: ClassText, Identity: productType + " " + location}
}
