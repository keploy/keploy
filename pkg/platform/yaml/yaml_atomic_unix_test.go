//go:build unix

package yaml

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"go.uber.org/zap"
)

// A new file WriteFileF puts in place is created as the file itself would
// be: 0o644 less the umask, as when the file was first created empty and the
// whole one renamed over it. The temp file it is written to never gives its
// own mode (0o600) to a new file, nor 0o644 past a stricter umask.
func TestWriteFileF_ANewFileTakesTheUmask(t *testing.T) {
	for _, tc := range []struct {
		umask int
		want  os.FileMode
	}{{0o022, 0o644}, {0o077, 0o600}, {0o027, 0o640}} {
		dir := t.TempDir()
		err := func() error {
			defer syscall.Umask(syscall.Umask(tc.umask))
			return WriteFileF(context.Background(), zap.NewNop(), dir, "new", []byte("doc\n"), false, FormatYAML)
		}()
		if err != nil {
			t.Fatalf("umask %#o: %v", tc.umask, err)
		}
		info, err := os.Stat(filepath.Join(dir, "new."+FormatYAML.FileExtension()))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != tc.want {
			t.Fatalf("umask %#o: a new file's mode is %v, want %v", tc.umask, got, tc.want)
		}
	}
}

// While WriteFileF replaces an existing file, the temp file its content is
// written to is created no more readable than 0o600, and the file keeps its
// own mode: a replaced 0o600 file's content is never in a 0o644 file, mid-write
// or when keploy is killed before the rename. A new file's temp file is
// created as the file itself would be.
func TestWriteFileF_ReplacingAFileWritesNoWiderTempFile(t *testing.T) {
	var seen []os.FileMode
	orig := newReplaceTemp
	t.Cleanup(func() { newReplaceTemp = orig })
	newReplaceTemp = func(dir, fileName string, perm os.FileMode) (*os.File, error) {
		f, err := orig(dir, fileName, perm)
		if err == nil {
			if info, serr := f.Stat(); serr == nil {
				seen = append(seen, info.Mode().Perm())
			}
		}
		return f, err
	}
	defer syscall.Umask(syscall.Umask(0o022))
	dir := t.TempDir()
	target := filepath.Join(dir, "secret."+FormatYAML.FileExtension())
	if err := os.WriteFile(target, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileF(context.Background(), zap.NewNop(), dir, "secret", []byte("second\n"), false, FormatYAML); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileF(context.Background(), zap.NewNop(), dir, "new", []byte("doc\n"), false, FormatYAML); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0]&^0o600 != 0 || seen[1] != 0o644 {
		t.Fatalf("temp files created %v; want one no wider than 0600 replacing the 0600 file, then 0644 for the new one", seen)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("the replaced file is %v, want 0600", got)
	}
}

// A file name holding a path separator is refused, as os.CreateTemp refuses
// it, and nothing is written under the directory it names, even one that
// exists.
func TestWriteFileF_AFileNameWithAPathSeparatorIsRefused(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	err := WriteFileF(context.Background(), zap.NewNop(), dir, "sub/x", []byte("doc\n"), false, FormatYAML)
	if err == nil || !strings.Contains(err.Error(), "path separator") {
		t.Fatalf("err = %v; want the path-separator refusal", err)
	}
	entries, err := os.ReadDir(sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("wrote %v under the directory the name holds", entries)
	}
}
