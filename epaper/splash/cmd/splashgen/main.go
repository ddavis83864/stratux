// splashgen regenerates (or verifies) the production e-paper splash
// bitmap from the owner-approved monochrome ARS artwork.
//
//	go run ./epaper/splash/cmd/splashgen          # regenerate
//	go run ./epaper/splash/cmd/splashgen -check   # verify, write nothing
//
// Inputs:  <dir>/source/ars-splash-source.png   (approved artwork)
// Outputs: <dir>/ars-splash-400x300.bin         (consumed by the runtime)
//
//	<dir>/ars-splash-400x300.preview.png  (human review only)
//	<dir>/CHECKSUMS.sha256                (sha256sum -c compatible)
//
// See docs/epaper-boot-splash.md.
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"image/png"
	"os"
	"path/filepath"

	"github.com/stratux/stratux/epaper/splash"
)

const (
	sourceRel   = "source/ars-splash-source.png"
	binName     = "ars-splash-400x300.bin"
	previewName = "ars-splash-400x300.preview.png"
	sumsName    = "CHECKSUMS.sha256"
)

func main() {
	dir := flag.String("dir", "epaper/splash/assets", "assets directory (run from the repository root, or pass an absolute path)")
	check := flag.Bool("check", false, "verify the committed outputs match a fresh conversion; write nothing")
	flag.Parse()

	if err := run(*dir, *check); err != nil {
		fmt.Fprintln(os.Stderr, "splashgen:", err)
		os.Exit(1)
	}
}

func run(dir string, check bool) error {
	srcBytes, err := os.ReadFile(filepath.Join(dir, sourceRel))
	if err != nil {
		return err
	}
	src, err := png.Decode(bytes.NewReader(srcBytes))
	if err != nil {
		return fmt.Errorf("decode source: %w", err)
	}
	bitmap, err := splash.Convert(src)
	if err != nil {
		return err
	}
	stats, err := splash.Validate(bitmap)
	if err != nil {
		return err
	}

	var previewBuf bytes.Buffer
	if err := png.Encode(&previewBuf, splash.Preview(bitmap)); err != nil {
		return err
	}
	sums := checksums(srcBytes, bitmap)

	if check {
		got, err := os.ReadFile(filepath.Join(dir, binName))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, bitmap) {
			return fmt.Errorf("%s does not match a fresh conversion of %s", binName, sourceRel)
		}
		gotSums, err := os.ReadFile(filepath.Join(dir, sumsName))
		if err != nil {
			return err
		}
		if !bytes.Equal(gotSums, sums) {
			return fmt.Errorf("%s is stale or was modified", sumsName)
		}
		fmt.Printf("OK: %s matches %s (black %.1f%%)\n%s", binName, sourceRel, 100*stats.BlackFraction(), sums)
		return nil
	}

	for name, data := range map[string][]byte{
		binName:     bitmap,
		previewName: previewBuf.Bytes(),
		sumsName:    sums,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("wrote %s, %s, %s (black %.1f%%)\n%s", binName, previewName, sumsName, 100*stats.BlackFraction(), sums)
	return nil
}

// checksums returns sha256sum-format lines for the approved source and
// the production bitmap. The preview PNG is excluded on purpose: PNG
// compression output is not guaranteed stable across Go versions,
// whereas the source bytes and the packed bitmap are.
func checksums(source, bitmap []byte) []byte {
	return []byte(fmt.Sprintf("%x  %s\n%x  %s\n", sha256.Sum256(source), sourceRel, sha256.Sum256(bitmap), binName))
}
