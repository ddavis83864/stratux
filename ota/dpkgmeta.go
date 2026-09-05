// Package ota: dpkg per-package metadata reconciliation.
//
// A file-level rollback (restoring /opt/stratux, the unit files, and the
// udev rules from a tar backup) is not sufficient on its own: dpkg -i also
// records the new package in its own database - the package's stanza in
// /var/lib/dpkg/status, and its control scripts under
// /var/lib/dpkg/info/<pkg>.* - independently of what files end up on disk.
// Restoring files without reconciling that database leaves `dpkg -s
// stratux` reporting whatever state the interrupted/failed install left
// behind (e.g. "install ok unpacked") even though the actual files on disk
// are back to the prior version. That mismatch is a real reliability
// defect: a later `dpkg -i` or `dpkg --configure -a` would reason about the
// package from a false premise.
//
// The fix here is deliberately narrow: back up and restore only the
// "stratux" stanza within /var/lib/dpkg/status (every other package's
// stanza is left untouched, byte for byte) and only the
// /var/lib/dpkg/info/stratux.* control files - never the rest of the dpkg
// database. ExtractStanza/ReplaceStanza below are the pure, tested
// specification of that splice; the actual file I/O runs from
// debian/stratux-pre-start.sh (bare ext4, before the Go daemon exists on
// that boot) via an equivalent python3 script that mirrors this exact
// algorithm - the same bash/Go split already used for the rest of the OTA
// state machine. See docs/ota.md.
package ota

import "bytes"

const stanzaSeparator = "\n\n"

// splitStanzas splits a dpkg status-file-formatted byte slice into its
// blank-line-separated stanzas, dropping any purely-blank leading/trailing
// segments. Each returned slice shares no backing array with status.
func splitStanzas(status []byte) [][]byte {
	trimmed := bytes.Trim(status, "\n")
	if len(trimmed) == 0 {
		return nil
	}
	parts := bytes.Split(trimmed, []byte(stanzaSeparator))
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if len(bytes.TrimSpace(p)) == 0 {
			continue
		}
		cp := make([]byte, len(p))
		copy(cp, p)
		out = append(out, cp)
	}
	return out
}

// stanzaPackage returns the value of the stanza's "Package:" field, if any.
func stanzaPackage(stanza []byte) (string, bool) {
	for _, line := range bytes.Split(stanza, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("Package:")) {
			name := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("Package:")))
			if len(name) == 0 {
				return "", false
			}
			return string(name), true
		}
	}
	return "", false
}

// ExtractStanza returns the exact bytes of pkg's stanza within a dpkg
// status-file-formatted byte slice, and whether it was found. The returned
// bytes are a copy, safe to retain independently of status.
func ExtractStanza(status []byte, pkg string) ([]byte, bool) {
	for _, s := range splitStanzas(status) {
		if name, ok := stanzaPackage(s); ok && name == pkg {
			return s, true
		}
	}
	return nil, false
}

// ReplaceStanza returns status with pkg's stanza replaced by newStanza,
// preserving every other stanza's content and relative order exactly.
//
//   - If pkg has an existing stanza and newStanza is non-nil, it is replaced
//     in place (same position).
//   - If pkg has an existing stanza and newStanza is nil, that stanza is
//     removed (dpkg no longer believes the package is installed at all).
//   - If pkg has no existing stanza and newStanza is non-nil, newStanza is
//     appended.
//   - If pkg has no existing stanza and newStanza is nil, status is
//     returned unchanged (nothing to remove).
func ReplaceStanza(status []byte, pkg string, newStanza []byte) []byte {
	stanzas := splitStanzas(status)
	replaced := false
	out := make([][]byte, 0, len(stanzas)+1)
	for _, s := range stanzas {
		if name, ok := stanzaPackage(s); ok && name == pkg {
			replaced = true
			if newStanza != nil {
				out = append(out, bytes.TrimRight(newStanza, "\n"))
			}
			continue
		}
		out = append(out, s)
	}
	if !replaced && newStanza != nil {
		out = append(out, bytes.TrimRight(newStanza, "\n"))
	}
	if len(out) == 0 {
		return []byte{}
	}
	result := bytes.Join(out, []byte(stanzaSeparator))
	return append(result, '\n')
}
