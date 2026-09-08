package storagelifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

func testNamespace() Namespace {
	return Namespace{ID: "cache", Root: "/data/cache", Criticality: CriticalityCache, ItemKind: ItemKindFile}
}

func sequentialSuffix() TempSuffixFunc {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("suffix%d", n)
	}
}

func writeBytes(b []byte) WriteFunc {
	return func(w io.Writer) error {
		_, err := w.Write(b)
		return err
	}
}

func TestAtomicWrite_TargetAbsent(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	path, err := w.Write(context.Background(), AtomicWriteOptions{
		Namespace: testNamespace(),
		Name:      "product.json",
		Mode:      0o644,
		Write:     writeBytes([]byte(`{"ok":true}`)),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/data/cache/product.json" {
		t.Errorf("unexpected path: %s", path)
	}
	if string(fs.content(path)) != `{"ok":true}` {
		t.Errorf("unexpected content: %s", fs.content(path))
	}
	if fs.exists("/data/cache/.slctmp-product.json.suffix1") {
		t.Error("expected the temp file to no longer exist after a successful rename")
	}
}

func TestAtomicWrite_TargetPresentIsReplaced(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	fs.putFile("/data/cache/product.json", []byte("old"), 0o644, time.Now())
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	path, err := w.Write(context.Background(), AtomicWriteOptions{
		Namespace: testNamespace(), Name: "product.json", Mode: 0o644, Write: writeBytes([]byte("new")),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(fs.content(path)) != "new" {
		t.Errorf("expected the destination replaced, got %q", fs.content(path))
	}
}

func TestAtomicWrite_NoPartialDestinationOnWriteFailure(t *testing.T) {
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	_, err := w.Write(context.Background(), AtomicWriteOptions{
		Namespace: testNamespace(), Name: "product.json", Mode: 0o644,
		Write: func(wr io.Writer) error { return errors.New("boom") },
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if fs.exists("/data/cache/product.json") {
		t.Error("destination must never exist after a write failure")
	}
	if fs.exists("/data/cache/.slctmp-product.json.suffix1") {
		t.Error("expected the temp file to be cleaned up after a write failure")
	}
}

func faultFS(t *testing.T) (*fakeFS, Namespace) {
	t.Helper()
	fs := newFakeFS()
	fs.mkdir("/data/cache")
	return fs, testNamespace()
}

func TestAtomicWrite_FaultAtCreate(t *testing.T) {
	fs, ns := faultFS(t)
	fs.failCreate["/data/cache/.slctmp-product.json.suffix1"] = errors.New("disk full")
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "product.json", Mode: 0o644, Write: writeBytes([]byte("x"))})
	if err == nil {
		t.Fatal("expected an error")
	}
	if fs.exists("/data/cache/product.json") {
		t.Error("destination must not exist")
	}
}

func TestAtomicWrite_FaultAtRename(t *testing.T) {
	fs, ns := faultFS(t)
	fs.failRename["/data/cache/.slctmp-product.json.suffix1"] = errors.New("cross-device link")
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "product.json", Mode: 0o644, Write: writeBytes([]byte("x"))})
	if err == nil {
		t.Fatal("expected an error")
	}
	if fs.exists("/data/cache/product.json") {
		t.Error("destination must not exist when rename fails")
	}
	if fs.exists("/data/cache/.slctmp-product.json.suffix1") {
		t.Error("a failed rename is a recoverable failure - the owned temp file must still be cleaned up, per this package's mission requirement to remove only the owned temp file on recoverable failure")
	}
}

func TestAtomicWrite_FaultAtDirectorySyncStillReportsPathWritten(t *testing.T) {
	fs, ns := faultFS(t)
	fs.failSyncDir["/data/cache"] = errors.New("io error")
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	path, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "product.json", Mode: 0o644, Write: writeBytes([]byte("x"))})
	if err == nil {
		t.Fatal("expected an error reported for the directory-sync failure")
	}
	if path != "/data/cache/product.json" {
		t.Errorf("expected the final path still reported (the rename DID succeed), got %q", path)
	}
	if string(fs.content(path)) != "x" {
		t.Error("the content must actually be at the destination despite the directory-sync error")
	}
}

func TestAtomicWrite_FaultAtHandleWrite(t *testing.T) {
	fs, ns := faultFS(t)
	fs.nextHandleFault = handleFault{write: errors.New("write error")}
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "product.json", Mode: 0o644, Write: writeBytes([]byte("x"))})
	if err == nil {
		t.Fatal("expected an error")
	}
	if fs.exists("/data/cache/product.json") {
		t.Error("destination must not exist")
	}
}

func TestAtomicWrite_FaultAtChmod(t *testing.T) {
	fs, ns := faultFS(t)
	fs.nextHandleFault = handleFault{chmod: errors.New("chmod error")}
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "product.json", Mode: 0o644, Write: writeBytes([]byte("x"))})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestAtomicWrite_FaultAtSync(t *testing.T) {
	fs, ns := faultFS(t)
	fs.nextHandleFault = handleFault{sync: errors.New("sync error")}
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "product.json", Mode: 0o644, Write: writeBytes([]byte("x"))})
	if err == nil {
		t.Fatal("expected an error")
	}
	if fs.exists("/data/cache/product.json") {
		t.Error("destination must not exist")
	}
}

func TestAtomicWrite_FaultAtClose(t *testing.T) {
	fs, ns := faultFS(t)
	fs.nextHandleFault = handleFault{close: errors.New("close error")}
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "product.json", Mode: 0o644, Write: writeBytes([]byte("x"))})
	if err == nil {
		t.Fatal("expected an error")
	}
	if fs.exists("/data/cache/product.json") {
		t.Error("destination must not exist")
	}
}

func TestAtomicWrite_FaultAtValidation(t *testing.T) {
	fs, ns := faultFS(t)
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	_, err := w.Write(context.Background(), AtomicWriteOptions{
		Namespace: ns, Name: "product.json", Mode: 0o644, Write: writeBytes([]byte("x")),
		Validate: func(tempPath string) error { return errors.New("invalid content") },
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if fs.exists("/data/cache/product.json") {
		t.Error("destination must not exist when validation fails")
	}
	if fs.exists("/data/cache/.slctmp-product.json.suffix1") {
		t.Error("expected the temp file cleaned up after a validation failure")
	}
}

func TestAtomicWrite_ValidationSeesFullyWrittenContent(t *testing.T) {
	fs, ns := faultFS(t)
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	var seen string
	_, err := w.Write(context.Background(), AtomicWriteOptions{
		Namespace: ns, Name: "product.json", Mode: 0o644, Write: writeBytes([]byte("full-content")),
		Validate: func(tempPath string) error { seen = string(fs.content(tempPath)); return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seen != "full-content" {
		t.Errorf("expected the validator to see the fully written content, got %q", seen)
	}
}

func TestAtomicWrite_NoTargetPathInput(t *testing.T) {
	fs, ns := faultFS(t)
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	for _, name := range []string{"../escape", "/abs", "a/b", ""} {
		_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: name, Mode: 0o644, Write: writeBytes([]byte("x"))})
		if err == nil {
			t.Errorf("expected rejection of unsafe name %q", name)
		}
	}
}

func TestAtomicWrite_MissingWriteFuncRejected(t *testing.T) {
	fs, ns := faultFS(t)
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "x.json", Mode: 0o644})
	if err == nil {
		t.Fatal("expected an error for a nil Write callback")
	}
}

func TestAtomicWrite_CancelledBeforeStart(t *testing.T) {
	fs, ns := faultFS(t)
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.Write(ctx, AtomicWriteOptions{Namespace: ns, Name: "x.json", Mode: 0o644, Write: writeBytes([]byte("x"))})
	if err == nil {
		t.Fatal("expected an error for an already-cancelled context")
	}
	if fs.exists("/data/cache/x.json") || fs.exists("/data/cache/.slctmp-x.json.suffix1") {
		t.Error("nothing should have been created for a pre-cancelled write")
	}
}

func TestAtomicWrite_CancelledBeforeCommitAbortsWithoutRename(t *testing.T) {
	fs, ns := faultFS(t)
	w := &AtomicWriter{FS: fs, TempSuffix: sequentialSuffix()}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := w.Write(ctx, AtomicWriteOptions{
		Namespace: ns, Name: "x.json", Mode: 0o644,
		Write: func(wr io.Writer) error {
			cancel() // cancel partway through, before the commit check
			_, e := wr.Write([]byte("x"))
			return e
		},
	})
	if err == nil {
		t.Fatal("expected an error for a context cancelled before commit")
	}
	if fs.exists("/data/cache/x.json") {
		t.Error("the rename must never happen once the context is cancelled")
	}
}

func TestAtomicWrite_ConcurrentWritersOfDifferentNamesDoNotCollide(t *testing.T) {
	fs, ns := faultFS(t)
	w := &AtomicWriter{FS: fs, TempSuffix: randomTempSuffix}
	done := make(chan error, 2)
	go func() {
		_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "a.json", Mode: 0o644, Write: writeBytes([]byte("A"))})
		done <- err
	}()
	go func() {
		_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "b.json", Mode: 0o644, Write: writeBytes([]byte("B"))})
		done <- err
	}()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if string(fs.content("/data/cache/a.json")) != "A" || string(fs.content("/data/cache/b.json")) != "B" {
		t.Error("both concurrent writes of different names must succeed independently")
	}
}

func TestAtomicWrite_ConcurrentWritersOfSameNameBothSucceedWithRandomSuffixes(t *testing.T) {
	// Both use the real random suffix generator, so their temp files
	// never collide even though they target the same final name - one of
	// the two renames simply lands second and wins, exactly the same
	// "last writer wins" semantics a single-threaded caller doing two
	// sequential writes would already have.
	fs, ns := faultFS(t)
	w := &AtomicWriter{FS: fs, TempSuffix: randomTempSuffix}
	done := make(chan error, 2)
	for _, content := range []string{"first", "second"} {
		c := content
		go func() {
			_, err := w.Write(context.Background(), AtomicWriteOptions{Namespace: ns, Name: "shared.json", Mode: 0o644, Write: writeBytes([]byte(c))})
			done <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	final := string(fs.content("/data/cache/shared.json"))
	if final != "first" && final != "second" {
		t.Errorf("expected the destination to hold one complete writer's content, got %q", final)
	}
}

func TestOwnedTempFinalName_RoundTrips(t *testing.T) {
	name := tempFileName("diagnostic-2026.json", "abc123")
	final, ok := ownedTempFinalName(name)
	if !ok || final != "diagnostic-2026.json" {
		t.Errorf("expected round trip to recover the final name, got final=%q ok=%v", final, ok)
	}
}

func TestOwnedTempFinalName_RejectsUnrelatedNames(t *testing.T) {
	for _, name := range []string{"diagnostic-2026.json", ".tmp-something", "random.tmp", ""} {
		if _, ok := ownedTempFinalName(name); ok {
			t.Errorf("expected %q to not be recognized as an owned temp name", name)
		}
	}
}
