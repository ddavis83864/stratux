package fisbcache

import "fmt"

// nexradGridDegrees quantizes a NEXRAD tile's own reported LatNorth/
// LonWest into a stable identity key. uatparse.NEXRADBlock's geographic
// bounds are recomputed from a numeric block number every time a tile is
// received (see uatparse/nexrad.go's block_location) and are exact
// floating-point values, not a named tile ID - two receptions of the
// truly same tile reliably reproduce the identical LatNorth/LonWest/
// Height/Width (block_location is a pure function of the block number),
// so no quantization/rounding is actually needed for identity to be
// stable; this constant exists only as an explicit, documented defense
// against an unforeseen floating-point formatting difference, not
// because real-world jitter has been observed.
const nexradGridDegrees = 0.0001

// NexradTileIdentity builds the Key.Identity for one NEXRAD tile from
// its own reported geographic bounds and radar type/scale - the only
// identity information uatparse.NEXRADBlock actually carries (see
// docs/fisb-weather-cache.md's product-policy table). Two receptions of
// the same physical tile (same radar type, scale, and geographic
// rectangle) must produce an identical string so the second reception is
// treated as a supersession of the first, not a second entry.
func NexradTileIdentity(radarType uint32, scale int, latNorth, lonWest, height, width float64) string {
	q := func(v float64) float64 {
		return float64(int64(v/nexradGridDegrees+0.5)) * nexradGridDegrees
	}
	return fmt.Sprintf("radar=%d;scale=%d;lat=%.4f;lon=%.4f;h=%.4f;w=%.4f",
		radarType, scale, q(latNorth), q(lonWest), q(height), q(width))
}

// NexradKey builds the full Key for one received NEXRAD tile.
func NexradKey(radarType uint32, scale int, latNorth, lonWest, height, width float64) Key {
	return Key{Class: ClassNexradTile, Identity: NexradTileIdentity(radarType, scale, latNorth, lonWest, height, width)}
}
