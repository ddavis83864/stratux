// Package assets carries the committed, validated production splash
// bitmaps for the runtime: the boot splash (plain ARS logo) and the
// final shutdown splash (the same logo plus a prominent "Safe to remove
// power" message - see issue #43 and docs/epaper-shutdown-splash.md).
// Each asset's approved source artwork and preview PNG are deliberately
// NOT embedded - only the 15000-byte bitmap the driver consumes is.
//
// Regenerate with `go run ./epaper/splash/cmd/splashgen [-name ars-shutdown]`;
// see docs/epaper-boot-splash.md and docs/epaper-shutdown-splash.md.
package assets

import _ "embed"

//go:embed ars-splash-400x300.bin
var production []byte

//go:embed ars-shutdown-400x300.bin
var shutdownProduction []byte

// Bitmap returns a copy of the production boot-splash bitmap: 15000
// bytes (splash.BitmapLen), 1 bit/pixel, MSB-first, row-major, 1 =
// white, at the 4.2in V2 panel's native rotation-0 orientation. The
// caller may modify the copy.
func Bitmap() []byte {
	out := make([]byte, len(production))
	copy(out, production)
	return out
}

// ShutdownBitmap returns a copy of the production final-shutdown-splash
// bitmap: the same panel format as Bitmap, but the artwork is the ARS
// logo plus a prominent "Safe to remove power" message - the single,
// final retained image an orderly power-off leaves on the panel. The
// caller may modify the copy.
func ShutdownBitmap() []byte {
	out := make([]byte, len(shutdownProduction))
	copy(out, shutdownProduction)
	return out
}
