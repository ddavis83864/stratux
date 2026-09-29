// splashgen regenerates (or verifies) a production e-paper splash
// bitmap from its owner-approved monochrome source artwork. Two named
// assets share this tool: "ars-splash" (the boot splash, plain logo)
// and "ars-shutdown" (the final retained shutdown splash, logo plus a
// prominent "Safe to remove power" message - see issue #43).
//
//	go run ./epaper/splash/cmd/splashgen                        # regenerate ars-splash
//	go run ./epaper/splash/cmd/splashgen -name ars-shutdown      # regenerate ars-shutdown
//	go run ./epaper/splash/cmd/splashgen -check                  # verify, write nothing
//	go run ./epaper/splash/cmd/splashgen -name ars-shutdown -check
//
// Inputs:  <dir>/source/<name>-source.png   (approved artwork)
// Outputs: <dir>/<name>-400x300.bin         (consumed by the runtime)
//
//	<dir>/<name>-400x300.preview.png  (human review only)
//	<dir>/CHECKSUMS.sha256            (sha256sum -c compatible; shared
//	                                   across every named asset - each
//	                                   run updates only its own two
//	                                   lines, preserving the others')
//
// See docs/epaper-boot-splash.md and docs/epaper-shutdown-splash.md.
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/stratux/stratux/epaper/splash"
)

const sumsName = "CHECKSUMS.sha256"

func main() {
	dir := flag.String("dir", "epaper/splash/assets", "assets directory (run from the repository root, or pass an absolute path)")
	name := flag.String("name", "ars-splash", "asset base name: ars-splash (boot) or ars-shutdown (final shutdown splash)")
	check := flag.Bool("check", false, "verify the committed outputs match a fresh conversion; write nothing")
	flag.Parse()

	if err := run(*dir, *name, *check); err != nil {
		fmt.Fprintln(os.Stderr, "splashgen:", err)
		os.Exit(1)
	}
}

func run(dir, name string, check bool) error {
	sourceRel := filepath.Join("source", name+"-source.png")
	binName := name + "-400x300.bin"
	previewName := name + "-400x300.preview.png"

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
	newLines := checksumLines(sourceRel, srcBytes, binName, bitmap)

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
		for _, line := range newLines {
			if !strings.Contains(string(gotSums), line) {
				return fmt.Errorf("%s is stale or was modified (missing/mismatched line for %s)", sumsName, binName)
			}
		}
		fmt.Printf("OK: %s matches %s (black %.1f%%)\n", binName, sourceRel, 100*stats.BlackFraction())
		return nil
	}

	for fname, data := range map[string][]byte{
		binName:     bitmap,
		previewName: previewBuf.Bytes(),
	} {
		if err := os.WriteFile(filepath.Join(dir, fname), data, 0o644); err != nil {
			return err
		}
	}
	if err := mergeChecksums(filepath.Join(dir, sumsName), sourceRel, binName, newLines); err != nil {
		return err
	}
	fmt.Printf("wrote %s, %s, updated %s (black %.1f%%)\n", binName, previewName, sumsName, 100*stats.BlackFraction())
	return nil
}

// checksumLines returns the two sha256sum-format lines for one named
// asset's source and packed bitmap. The preview PNG is excluded on
// purpose: PNG compression output is not guaranteed stable across Go
// versions, whereas the source bytes and the packed bitmap are.
func checksumLines(sourceRel string, source []byte, binName string, bitmap []byte) []string {
	return []string{
		fmt.Sprintf("%x  %s", sha256.Sum256(source), sourceRel),
		fmt.Sprintf("%x  %s", sha256.Sum256(bitmap), binName),
	}
}

// mergeChecksums replaces this asset's two lines (identified by their
// file path in the second column) in the shared CHECKSUMS.sha256,
// leaving every other asset's lines untouched, then writes the result
// back sorted for a stable diff. A missing file starts a fresh one.
func mergeChecksums(path, sourceRel, binName string, newLines []string) error {
	existing := map[string]string{} // file -> full line
	if raw, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			f := regexp.MustCompile(`\s+`).Split(line, 2)
			if len(f) == 2 {
				existing[f[1]] = line
			}
		}
	}
	for _, line := range newLines {
		f := regexp.MustCompile(`\s+`).Split(line, 2)
		existing[f[1]] = line
	}
	var files []string
	for f := range existing {
		files = append(files, f)
	}
	sort.Strings(files)
	var out strings.Builder
	for _, f := range files {
		out.WriteString(existing[f])
		out.WriteString("\n")
	}
	return os.WriteFile(path, []byte(out.String()), 0o644)
}
