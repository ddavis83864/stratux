package storagelifecycle

import (
	"context"
	"fmt"
	"io"
	"os"
)

// tempFilePrefix marks every temporary file this package's AtomicWriter
// ever creates. It is deliberately distinctive (not a bare ".tmp" suffix,
// which could collide with some other subsystem's own temp-file
// convention) so recovery.go can recognize - and ONLY act on - files this
// package itself is provably responsible for. See recovery.go's doc
// comment for why "provably owned" matters as much as it does.
const tempFilePrefix = ".slctmp-"

// TempSuffixFunc returns a fresh, unique string to disambiguate one
// AtomicWriter call's temp file from any other concurrent one writing the
// same final name. The real implementation is cryptographically random;
// tests inject a deterministic sequence instead - see NewAtomicWriter.
type TempSuffixFunc func() string

// tempFileName returns the temp filename AtomicWriter uses for finalName
// with the given suffix - shared with recovery.go so both sides agree
// exactly on the naming convention.
func tempFileName(finalName, suffix string) string {
	return tempFilePrefix + finalName + "." + suffix
}

// ownedTempFinalName reports whether tempName is one of this package's
// own temp files, and if so, the final name it was written toward.
func ownedTempFinalName(tempName string) (finalName string, ok bool) {
	if len(tempName) <= len(tempFilePrefix) || tempName[:len(tempFilePrefix)] != tempFilePrefix {
		return "", false
	}
	rest := tempName[len(tempFilePrefix):]
	// rest is "<finalName>.<suffix>" - the suffix never itself contains a
	// '.', so the LAST '.' in rest separates them; finalName itself may
	// legitimately contain dots (e.g. "diagnostic-....json").
	lastDot := -1
	for i := len(rest) - 1; i >= 0; i-- {
		if rest[i] == '.' {
			lastDot = i
			break
		}
	}
	if lastDot <= 0 || lastDot == len(rest)-1 {
		return "", false
	}
	return rest[:lastDot], true
}

// WriteFunc streams a new file's content to w. It must not retain w
// beyond the call.
type WriteFunc func(w io.Writer) error

// ValidateFunc inspects the fully-written temp file at tempPath (already
// flushed and closed) before AtomicWriter commits it by renaming it into
// place. A nil ValidateFunc skips this step - callers writing a format
// this package cannot itself parse (JSON, a specific binary layout, …)
// should always supply one; skipping it is a caller decision, not this
// package's default.
type ValidateFunc func(tempPath string) error

// AtomicWriteOptions describes one call to AtomicWriter.Write.
type AtomicWriteOptions struct {
	// Namespace is the registered namespace Name is written into -
	// required, and never derived from HTTP input (see Namespace's own
	// doc comment).
	Namespace Namespace
	// Name is the final, single-segment filename within Namespace.Root -
	// validated the same way SafeJoin validates any other item name.
	Name string
	Mode os.FileMode
	// Write supplies the new file's content. Required.
	Write WriteFunc
	// Validate, if non-nil, is run against the completed temp file before
	// it is committed - see ValidateFunc's doc comment.
	Validate ValidateFunc
}

// AtomicWriter is the one reusable way this package's callers (and,
// eventually, automatic recording and the FIS-B cache - see contracts.go)
// replace or create a file in a registered namespace without ever
// exposing a reader to a partially-written result.
type AtomicWriter struct {
	FS         FS
	TempSuffix TempSuffixFunc
}

// NewAtomicWriter returns an AtomicWriter using fs and a cryptographically
// random temp-suffix generator. Tests construct an AtomicWriter literal
// directly with a deterministic TempSuffix instead.
func NewAtomicWriter(fs FS) *AtomicWriter {
	return &AtomicWriter{FS: fs, TempSuffix: randomTempSuffix}
}

// Write performs the full validated-temp-file-then-rename sequence
// documented in docs/storage-lifecycle.md's atomic-write section:
//
//  1. Validate opts.Namespace/opts.Name via SafeJoin.
//  2. Create a uniquely-named, package-owned temp file in the same
//     directory, exclusively (never overwriting or following anything
//     that might already exist at that name).
//  3. Stream content through opts.Write.
//  4. Apply opts.Mode explicitly (never relies on the process umask).
//  5. Sync the temp file's own content, then close it.
//  6. Run opts.Validate against the closed temp file, if supplied.
//  7. Rename the temp file onto the final name (same directory, so this
//     is atomic on the same filesystem - AtomicWriter never renames
//     across a mount boundary because it never creates the temp file
//     anywhere but Namespace.Root).
//  8. Sync the containing directory, so the rename itself is durable.
//
// On any failure, Write removes only the temp file it created (never the
// final destination, which - if the failure happened before the rename -
// was never touched at all) and returns a non-nil error. ctx is checked
// before writing and again before the rename, so a cancelled context
// aborts without ever committing a partial result; ctx is not threaded
// into the Write/Validate callbacks themselves (those run to completion
// once started, matching this package's small, synchronous scope).
func (w *AtomicWriter) Write(ctx context.Context, opts AtomicWriteOptions) (finalPath string, err error) {
	finalPath, joinErr := SafeJoin(opts.Namespace.Root, opts.Name)
	if joinErr != nil {
		return "", fmt.Errorf("storagelifecycle: %w", joinErr)
	}
	if opts.Write == nil {
		return "", fmt.Errorf("storagelifecycle: AtomicWriteOptions.Write is required")
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("storagelifecycle: write cancelled before starting: %w", err)
	}

	tempName := tempFileName(opts.Name, w.TempSuffix())
	tempPath, joinErr := SafeJoin(opts.Namespace.Root, tempName)
	if joinErr != nil {
		return "", fmt.Errorf("storagelifecycle: %w", joinErr)
	}

	f, err := w.FS.CreateExclusive(tempPath, opts.Mode)
	if err != nil {
		return "", fmt.Errorf("storagelifecycle: create temp file: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Best-effort: a cleanup failure here does not mask the
			// original error, and never touches anything but the exact
			// temp path this call itself just created.
			_ = w.FS.Remove(tempPath)
		}
	}()

	if err := opts.Write(f); err != nil {
		f.Close()
		return "", fmt.Errorf("storagelifecycle: write: %w", err)
	}
	if err := f.Chmod(opts.Mode); err != nil {
		f.Close()
		return "", fmt.Errorf("storagelifecycle: chmod: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", fmt.Errorf("storagelifecycle: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("storagelifecycle: close: %w", err)
	}

	if opts.Validate != nil {
		if err := opts.Validate(tempPath); err != nil {
			return "", fmt.Errorf("storagelifecycle: validate: %w", err)
		}
	}

	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("storagelifecycle: write cancelled before commit: %w", err)
	}

	if err := w.FS.Rename(tempPath, finalPath); err != nil {
		return "", fmt.Errorf("storagelifecycle: rename: %w", err)
	}
	committed = true // the temp path no longer exists under that name - nothing left for the deferred cleanup to remove

	if err := w.FS.SyncDir(opts.Namespace.Root); err != nil {
		// The rename already succeeded - finalPath holds the new content
		// either way. A directory-sync failure means that fact is not
		// yet guaranteed durable against a crash, which is worth
		// reporting, but it is not a reason to report finalPath as
		// unwritten (it is written) or to attempt to undo the rename.
		return finalPath, fmt.Errorf("storagelifecycle: directory sync: %w", err)
	}
	return finalPath, nil
}
