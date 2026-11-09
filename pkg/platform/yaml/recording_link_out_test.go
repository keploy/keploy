package yaml

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A recording's file that is a link out of the recording is refused, by its
// readers and by what changes it, with the link and where it leads said
// plainly, not in os.Root's words; what it leads to is left as it was.
func TestARecordingFileThatLinksOutIsSaidPlainly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on windows")
	}
	for _, c := range []struct {
		name string
		op   func(rec string) error
	}{
		{"a bounded read", func(rec string) error {
			_, _, err := ReadRecordingFileAnyBounded(context.Background(), rec, filepath.Join(rec, "reports", "test-run-0"), "test-set-0-report", FormatYAML, 1<<20)
			return err
		}},
		{"a read", func(rec string) error {
			_, err := ReadRecordingFileF(context.Background(), zap.NewNop(), rec, filepath.Join(rec, "reports", "test-run-0"), "test-set-0-report", FormatYAML)
			return err
		}},
		{"a write", func(rec string) error {
			return WriteFileInRecording(rec, filepath.Join(rec, "reports", "test-run-0", "test-set-0-report.yaml"), []byte("new"), 0o644)
		}},
		{"a folder made on the way", func(rec string) error {
			return MkdirAllInRecording(rec, filepath.Join(rec, "reports", "test-run-0", "test-set-0-report.yaml", "x"), 0o755)
		}},
		{"a rename onto it", func(rec string) error {
			from := filepath.Join(rec, "reports", "tmp.yaml")
			if err := os.WriteFile(from, []byte("new"), 0o644); err != nil {
				return err
			}
			return RenameInRecording(rec, from, filepath.Join(rec, "reports", "test-run-0", "test-set-0-report.yaml"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			rec := filepath.Join(base, "project", "keploy")
			outside := filepath.Join(base, "outside.txt")
			if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(rec, "reports", "test-run-0", "test-set-0-report.yaml")
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			err := c.op(rec)
			var out *utils.LinkOutError
			if !errors.As(err, &out) {
				t.Fatalf("got %v, want the link out named", err)
			}
			msg := err.Error()
			for _, want := range []string{`"reports/test-run-0/test-set-0-report.yaml"`, "a link that leads out of the recording", outside} {
				if !strings.Contains(msg, want) {
					t.Errorf("%q does not say %q", msg, want)
				}
			}
			if strings.Contains(msg, "escapes from parent") {
				t.Errorf("%q says os.Root's own words", msg)
			}
			if b, _ := os.ReadFile(outside); string(b) != "OUTSIDE" {
				t.Errorf("the file outside now holds %q", b)
			}
		})
	}
}
