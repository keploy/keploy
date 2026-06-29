package yaml

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

// TestWriteFileF_AtomicReplaceNoPartialRead proves the non-append WriteFileF is
// atomic: a concurrent reader of the destination never observes a zero-length or
// mid-document file while it is being rewritten. With the old O_TRUNC + streaming
// write this fails (the reader catches the truncated / half-written file); with
// the temp-file + rename it holds. This is the read-race behind the report-upload
// / reportdb.GetReport / localTestsPassed flakes on overlay / volume-synced
// filesystems under load.
func TestWriteFileF_AtomicReplaceNoPartialRead(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	logger := zap.NewNop()

	docA := []byte(strings.Repeat("aaaaaaaa-line\n", 1500))
	docB := []byte(strings.Repeat("bbbbbbbb-line\n", 3000))

	if err := WriteFileF(ctx, logger, dir, "report", docA, false, FormatYAML); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	target := filepath.Join(dir, "report."+FormatYAML.FileExtension())

	var badRead atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(target)
				if err != nil {
					continue // a momentary ENOENT during rename is fine; partial content is not
				}
				if !bytes.Equal(data, docA) && !bytes.Equal(data, docB) {
					badRead.Add(1)
				}
			}
		}()
	}

	for i := 0; i < 80; i++ {
		doc := docA
		if i%2 == 0 {
			doc = docB
		}
		if err := WriteFileF(ctx, logger, dir, "report", doc, false, FormatYAML); err != nil {
			t.Fatalf("rewrite %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	if n := badRead.Load(); n != 0 {
		t.Fatalf("reader observed %d partial/empty files during atomic rewrite — non-append write is not atomic", n)
	}

	final, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if !bytes.Equal(final, docA) && !bytes.Equal(final, docB) {
		t.Fatalf("final file is neither docA nor docB (len=%d) — atomic replace corrupted content", len(final))
	}
}

// TestWriteFileF_AtomicPreservesContentAndMode checks the happy path: the written
// content round-trips and an existing file's mode is preserved across the replace.
func TestWriteFileF_AtomicPreservesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	logger := zap.NewNop()

	target := filepath.Join(dir, "cfg."+FormatYAML.FileExtension())
	if err := WriteFileF(ctx, logger, dir, "cfg", []byte("first\n"), false, FormatYAML); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := WriteFileF(ctx, logger, dir, "cfg", []byte("second-rewritten\n"), false, FormatYAML); err != nil {
		t.Fatalf("second write: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "second-rewritten\n" {
		t.Fatalf("content = %q, want the rewritten doc", string(data))
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0o640 preserved across atomic replace", info.Mode().Perm())
	}
}
