//go:build unix

package record

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
)

// A keploy folder whose test sets cannot be listed must stop the recording.
// It used to read as empty, so the next test set was named test-set-0 and
// the recording was written into the user's existing test-set-0.
func TestGetNextTestSetID_FailsWhenTheTestSetsCannotBeListed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, root string)
	}{
		{"permission denied", func(t *testing.T, root string) {
			seedRecordedTestSet(t, root)
			if err := os.Chmod(root, 0o300); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
			if f, err := os.Open(root); err == nil {
				_, rerr := f.Readdirnames(1)
				_ = f.Close()
				if rerr == nil || errors.Is(rerr, io.EOF) {
					t.Skipf("this process (euid %d) can still list a mode-0300 folder, so it cannot be made unlistable here; the symlink-loop case covers the same path", os.Geteuid())
				}
			}
		}},
		{"symlink loop", func(t *testing.T, root string) {
			if err := os.Symlink(root, root); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "keploy")
			tc.prepare(t, root)
			r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, testDB: testdb.New(zap.NewNop(), root)}
			id, err := r.GetNextTestSetID(context.Background())
			if err == nil {
				t.Fatalf("GetNextTestSetID = %q, nil; want an error (the test sets could not be listed)", id)
			}
			if id != "" {
				t.Fatalf("GetNextTestSetID failed but still named a test set %q", id)
			}
		})
	}
}

// The first recording, with no keploy folder yet, still gets test-set-0, and
// one after it the next free id.
func TestGetNextTestSetID_AMissingFolderStartsAtTestSet0(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keploy")
	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, testDB: testdb.New(zap.NewNop(), root)}
	if id, err := r.GetNextTestSetID(context.Background()); err != nil || id != "test-set-0" {
		t.Fatalf("GetNextTestSetID on a missing folder = %q, %v; want test-set-0", id, err)
	}
	seedRecordedTestSet(t, root)
	if id, err := r.GetNextTestSetID(context.Background()); err != nil || id != "test-set-1" {
		t.Fatalf("GetNextTestSetID after test-set-0 = %q, %v; want test-set-1", id, err)
	}
}

func seedRecordedTestSet(t *testing.T, root string) {
	t.Helper()
	tests := filepath.Join(root, "test-set-0", "tests")
	if err := os.MkdirAll(tests, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tests, "test-1.yaml"), []byte("kind: Http\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
