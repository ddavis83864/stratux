// Command gensbom generates a CycloneDX 1.5 JSON software bill of
// materials for a Stratux release build, from information already
// present in this checkout - no network access and no new external
// tool dependency (go list, git submodule status, and the Debian
// control template are all this project already has).
//
// Usage: go run ./scripts/gensbom > stratux-<version>.sbom.json
// Must be run from the repository root, with submodules initialized.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

type component struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	PURL    string `json:"purl,omitempty"`
	Scope   string `json:"scope,omitempty"`
}

type sbom struct {
	BOMFormat    string     `json:"bomFormat"`
	SpecVersion  string     `json:"specVersion"`
	SerialNumber string     `json:"serialNumber"`
	Version      int        `json:"version"`
	Metadata     bomMeta    `json:"metadata"`
	Components   []component `json:"components"`
}

type bomMeta struct {
	Timestamp string           `json:"timestamp"`
	Tools     []toolEntry      `json:"tools"`
	Component componentSummary `json:"component"`
}

type toolEntry struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type componentSummary struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

func main() {
	version := os.Getenv("STRATUX_SBOM_VERSION")
	commit := os.Getenv("STRATUX_SBOM_COMMIT")

	var comps []component

	goMods, err := goModules()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gensbom: go modules:", err)
		os.Exit(1)
	}
	comps = append(comps, goMods...)

	comps = append(comps, submoduleComponents()...)
	comps = append(comps, debianComponents()...)

	goVersion := "unknown"
	if out, err := exec.Command("go", "version").Output(); err == nil {
		goVersion = strings.TrimSpace(string(out))
	}

	doc := sbom{
		BOMFormat:    "CycloneDX",
		SpecVersion:  "1.5",
		SerialNumber: "urn:uuid:00000000-0000-0000-0000-000000000000", // placeholder, replaced below
		Version:      1,
		Metadata: bomMeta{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Tools: []toolEntry{
				{Name: "gensbom", Version: "1"},
				{Name: "go", Version: goVersion},
			},
			Component: componentSummary{
				Type:    "application",
				Name:    "stratux",
				Version: version,
			},
		},
		Components: comps,
	}
	doc.SerialNumber = "urn:uuid:" + deterministicUUID(version, commit)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintln(os.Stderr, "gensbom: encode:", err)
		os.Exit(1)
	}
}

// goModules runs `go list -m -json all` (newline-delimited JSON objects,
// not a JSON array - go list's own long-established output shape for
// this flag) and converts every non-main module into a CycloneDX
// "library" component. Deliberately excludes the main module itself
// (reported separately as metadata.component) and any filesystem paths
// (GoMod/Dir fields), which could otherwise leak the build host's own
// directory layout into a published SBOM.
func goModules() ([]component, error) {
	cmd := exec.Command("go", "list", "-m", "-json", "all")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m -json all: %w", err)
	}
	type modInfo struct {
		Path    string
		Version string
		Main    bool
		Replace *modInfo
	}
	var comps []component
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var m modInfo
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("decoding go list output: %w", err)
		}
		if m.Main {
			continue
		}
		v := m.Version
		if m.Replace != nil {
			v = m.Replace.Version
		}
		comps = append(comps, component{
			Type:    "library",
			Name:    m.Path,
			Version: v,
			PURL:    "pkg:golang/" + m.Path + "@" + v,
			Scope:   "required",
		})
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].Name < comps[j].Name })
	return comps, nil
}

// submoduleComponents reads `git submodule status` (this repository's
// own top-level submodules only: dump1090, rtl-ais, ogn-tracker, and
// the image_build/pi-gen image builder - none of them nest further
// submodules this SBOM needs to enumerate) for the already-checked-out
// pinned commit of each, rather than .gitmodules alone, so the SBOM
// records the exact commit actually built, not just the configured
// upstream URL.
func submoduleComponents() []component {
	out, err := exec.Command("git", "submodule", "status").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gensbom: git submodule status:", err)
		return nil
	}
	var comps []component
	sc := bufio.NewScanner(bytes.NewReader(out))
	// Each line: "[+-U ]<sha> <path> [(<describe>)]"
	lineRe := regexp.MustCompile(`^[ +\-U]([0-9a-f]{40}) (\S+)`)
	for sc.Scan() {
		line := sc.Text()
		m := lineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		comps = append(comps, component{
			Type:    "library",
			Name:    m[2],
			Version: m[1],
			Scope:   "required",
		})
	}
	return comps
}

// debianComponents parses the Depends: field of the packaging control
// template (debian/control.dpkg) - the actual runtime native-library
// dependencies of the built .deb, as this project already declares
// them; not resolved to installed versions, since none are pinned by
// this project's own packaging (dpkg/apt resolve them at install time
// on the target device).
func debianComponents() []component {
	data, err := os.ReadFile("debian/control.dpkg")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gensbom: reading debian/control.dpkg:", err)
		return nil
	}
	text := string(data)
	idx := strings.Index(text, "Depends:")
	if idx == -1 {
		return nil
	}
	rest := text[idx+len("Depends:"):]
	// The field continues onto following lines that start with
	// whitespace; stop at the first line that doesn't.
	lines := strings.Split(rest, "\n")
	var fieldLines []string
	fieldLines = append(fieldLines, lines[0])
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
			fieldLines = append(fieldLines, l)
			continue
		}
		break
	}
	joined := strings.Join(fieldLines, " ")
	var comps []component
	for _, dep := range strings.Split(joined, ",") {
		name := strings.TrimSpace(dep)
		if name == "" {
			continue
		}
		comps = append(comps, component{
			Type:  "library",
			Name:  name,
			Scope: "required",
			PURL:  "pkg:deb/debian/" + name,
		})
	}
	return comps
}

// deterministicUUID derives a stable, non-random UUID-shaped string
// from the version/commit being built, so re-running this generator
// against the exact same release produces the exact same
// serialNumber - useful for the reproducibility comparison this
// release process performs, without pulling in a UUID library for one
// field.
func deterministicUUID(version, commit string) string {
	h := fnv64a(version + ":" + commit)
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x",
		uint32(h), uint16(h>>32), uint16(h>>16)&0x0fff, uint16(h)&0x0fff, h&0xffffffffffff)
}

func fnv64a(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
