package fisbcache

import "time"

// ProductPolicy is one product's explicit, product-specific freshness
// policy - see docs/fisb-weather-cache.md's product-freshness-policy
// table for the full evidence/rationale behind every value below. There
// is deliberately no single universal TTL: a PIREP and a TAF have wildly
// different real-world currency expectations, and treating them alike
// would either discard a still-useful TAF far too early or, worse,
// present a stale PIREP as if it were still representative.
type ProductPolicy struct {
	known bool
	// FreshLimit/StaleLimit/ExpireLimit are cumulative age thresholds
	// (see Freshness): at or below FreshLimit is CACHED_FRESH, above
	// that up to StaleLimit is CACHED_AGING, above that up to
	// ExpireLimit is STALE (retained, never presented as current), and
	// beyond ExpireLimit is EXPIRED (eviction-eligible).
	FreshLimit, StaleLimit, ExpireLimit time.Duration
}

// textPolicies maps a TextProductType (main.WeatherMessage's own Type
// token) to its ProductPolicy. Values are a conservative, documented
// policy choice based on each product's well-established FAA/NWS
// issuance cadence and operational currency expectations - NOT measured
// from anything this specific codebase tracks (it tracks no issuance
// cadence today - see docs/fisb-weather-cache.md). Every limit is
// deliberately set toward the conservative (shorter) side of that
// product's real-world cadence, per this feature's own "expire products
// conservatively" requirement - never used to imply guaranteed real-time
// accuracy.
var textPolicies = map[TextProductType]ProductPolicy{
	// METAR/SPECI: routinely issued hourly, with SPECI issued between
	// hourly reports for significant changes - conventionally treated as
	// "current" for roughly one reporting cycle.
	TextProductMETAR: {known: true, FreshLimit: 15 * time.Minute, StaleLimit: 75 * time.Minute, ExpireLimit: 3 * time.Hour},
	TextProductSPECI: {known: true, FreshLimit: 15 * time.Minute, StaleLimit: 75 * time.Minute, ExpireLimit: 3 * time.Hour},
	// TAF/TAF.AMD: issued roughly every 6 hours, amended as needed, each
	// covering a 24-30 hour validity period - "fresh" here means recently
	// issued/amended, not that the forecast's own validity has ended.
	TextProductTAF:        {known: true, FreshLimit: 3 * time.Hour, StaleLimit: 8 * time.Hour, ExpireLimit: 30 * time.Hour},
	TextProductTAFAmended: {known: true, FreshLimit: 3 * time.Hour, StaleLimit: 8 * time.Hour, ExpireLimit: 30 * time.Hour},
	// Winds/temperatures aloft: issued a few times daily, each covering
	// several hours of forecast validity.
	TextProductWinds: {known: true, FreshLimit: 3 * time.Hour, StaleLimit: 9 * time.Hour, ExpireLimit: 18 * time.Hour},
	// PIREP: an irregular, ad hoc point-in-time pilot report with no
	// fixed issuance cadence and no forecast validity at all - the most
	// perishable text product this cache ever admits, so it is expired
	// far sooner than any of the above.
	TextProductPIREP: {known: true, FreshLimit: 20 * time.Minute, StaleLimit: 60 * time.Minute, ExpireLimit: 2 * time.Hour},
}

// nexradPolicy is the single policy applied to every NEXRAD tile,
// regardless of radar type/scale - FIS-B NEXRAD imagery is broadcast on
// an update cadence of a few minutes, and radar returns themselves move
// and change quickly, so this is intentionally the most conservative
// (shortest) policy of any product this cache admits.
var nexradPolicy = ProductPolicy{known: true, FreshLimit: 10 * time.Minute, StaleLimit: 20 * time.Minute, ExpireLimit: 45 * time.Minute}

// PolicyFor returns k's ProductPolicy - the zero value (known: false)
// for any class/identity this package has no explicit policy for, which
// Freshness treats as FreshnessUnsupported rather than guessing a
// default TTL.
func PolicyFor(k Key) ProductPolicy {
	switch k.Class {
	case ClassNexradTile:
		return nexradPolicy
	case ClassText:
		// k.Identity is "<type> <location>" (see TextKey) - only the
		// leading type token selects a policy; location never affects
		// freshness.
		for i := 0; i < len(k.Identity); i++ {
			if k.Identity[i] == ' ' {
				return textPolicyFor(k.Identity[:i])
			}
		}
		return textPolicyFor(k.Identity)
	default:
		return ProductPolicy{}
	}
}

func textPolicyFor(productType string) ProductPolicy {
	return textPolicies[productType]
}
