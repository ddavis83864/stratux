// Package assets carries the committed, validated production splash
// bitmap for the runtime. The approved source artwork
// (source/ars-splash-source.png) and the preview PNG are deliberately
// NOT embedded - only the 15000-byte bitmap the driver consumes is.
//
// Regenerate with `go run ./epaper/splash/cmd/splashgen`; see
// docs/epaper-boot-splash.md.
package assets

import _ "embed"

//go:embed ars-splash-400x300.bin
var production []byte

// Bitmap returns a copy of the production splash bitmap: 15000 bytes
// (splash.BitmapLen), 1 bit/pixel, MSB-first, row-major, 1 = white, at
// the 4.2in V2 panel's native rotation-0 orientation. The caller may
// modify the copy.
func Bitmap() []byte {
	out := make([]byte, len(production))
	copy(out, production)
	return out
}
