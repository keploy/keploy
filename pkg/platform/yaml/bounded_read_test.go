package yaml

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/platform/safeyaml"
	"go.uber.org/zap"
)

// within runs f, failing the test if it has not returned within d: a FIFO
// opened as a file or directory blocks the open for good.
func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not return within %s: it blocked", d)
	}
}

// A directory keploy lists -- keploy/reports, a test set -- is in the
// repository, so it can be a FIFO or a file: listing it is refused at once,
// naming it, and a directory that is not there is still no sessions.
func TestListingANonDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "reports")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{"a file": file}
	if runtime.GOOS != "windows" {
		fifo := filepath.Join(dir, "fifo")
		mkfifo(t, fifo)
		paths["a FIFO"] = fifo
	}
	for kind, p := range paths {
		t.Run(kind, func(t *testing.T) {
			within(t, 5*time.Second, func() {
				_, err := ReadDir(p, fs.ModePerm)
				var pe *fs.PathError
				if !errors.Is(err, safeyaml.ErrNotDir) || !errors.As(err, &pe) || pe.Path != p {
					t.Errorf("ReadDir(%s) = %v, want ErrNotDir naming the path", kind, err)
				}
				if _, err := ReadSessionIndices(context.Background(), p, zap.NewNop(), ModeDir); !errors.Is(err, safeyaml.ErrNotDir) {
					t.Errorf("ReadSessionIndices(%s) = %v, want ErrNotDir", kind, err)
				}
				if _, err := ReadSessionIndicesAny(context.Background(), p, zap.NewNop(), ModeFile); !errors.Is(err, safeyaml.ErrNotDir) {
					t.Errorf("ReadSessionIndicesAny(%s) = %v, want ErrNotDir", kind, err)
				}
			})
		})
	}
	got, err := ReadSessionIndices(context.Background(), filepath.Join(dir, "absent"), zap.NewNop(), ModeDir)
	if err != nil || len(got) != 0 {
		t.Errorf("ReadSessionIndices(absent) = %v, %v; want none, no error", got, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "run", "test-run-0"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err = ReadSessionIndices(context.Background(), filepath.Join(dir, "run"), zap.NewNop(), ModeDir)
	if err != nil || len(got) != 1 || got[0] != "test-run-0" {
		t.Errorf("ReadSessionIndices(a directory) = %v, %v; want [test-run-0]", got, err)
	}
}

// Listing a directory closes it. A handle left open locks the directory on
// Windows, where TestListingANonDirectory's TempDir cleanup then fails; here
// it is a descriptor left open each time, which /proc/self/fd counts. The
// collector is off meanwhile, so that no finalizer closes a leaked one first.
// Mutation: drop either Close.
func TestListingClosesTheDirectory(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc/self/fd to count descriptors in")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "test-run-0"), 0o700); err != nil {
		t.Fatal(err)
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	open := func() int {
		fds, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(fds)
	}
	for name, list := range map[string]func() ([]string, error){
		"ReadSessionIndices": func() ([]string, error) {
			return ReadSessionIndices(context.Background(), dir, zap.NewNop(), ModeDir)
		},
		"ReadSessionIndicesAny": func() ([]string, error) {
			return ReadSessionIndicesAny(context.Background(), dir, zap.NewNop(), ModeDir)
		},
	} {
		before := open()
		for i := 0; i < 16; i++ {
			if got, err := list(); err != nil || len(got) != 1 {
				t.Fatalf("%s = %v, %v; want [test-run-0]", name, got, err)
			}
		}
		if after := open(); after != before {
			t.Errorf("%s: %d descriptors open after listing 16 times, %d before", name, after, before)
		}
	}
}

// ReadFileAnyBounded reads the preferred format, else the other; refuses a
// file that is not a regular one (without trying the other format) or is past
// the limit; and is fs.ErrNotExist when there is neither.
func TestReadFileAnyBounded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, err := ReadFileAnyBounded(ctx, dir, "r", FormatYAML, 1<<10); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("neither format = %v, want fs.ErrNotExist", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "r.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, f, err := ReadFileAnyBounded(ctx, dir, "r", FormatYAML, 1<<10)
	if err != nil || f != FormatJSON || string(data) != `{"a":1}` {
		t.Errorf("only r.json = %q, %s, %v; want it read as JSON", data, f, err)
	}
	if _, _, err := ReadFileAnyBounded(ctx, dir, "r", FormatYAML, 4); !safeyaml.IsRefused(err) {
		t.Errorf("past the limit = %v, want a bound", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	mkfifo(t, filepath.Join(dir, "r.yaml"))
	within(t, 5*time.Second, func() {
		if _, _, err := ReadFileAnyBounded(ctx, dir, "r", FormatYAML, 1<<10); !errors.Is(err, safeyaml.ErrNotRegular) {
			t.Errorf("r.yaml a FIFO beside r.json = %v, want ErrNotRegular", err)
		}
	})
}
