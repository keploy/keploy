package yaml

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils/linkout"
	"go.uber.org/zap"
)

// A recording's file that is a link out of the recording is refused, by its
// readers and by what changes it, with what any refusal of it says, then the
// link and where it leads, said plainly, not in os.Root's words; what it
// leads to is left as it was.
func TestARecordingFileThatLinksOutIsSaidPlainly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on windows")
	}
	for _, c := range []struct {
		name, verb string
		op         func(rec string) error
	}{
		{"a bounded read", "read", func(rec string) error {
			_, _, err := ReadRecordingFileAnyBounded(context.Background(), rec, filepath.Join(rec, "reports", "test-run-0"), "test-set-0-report", FormatYAML, 1<<20)
			return err
		}},
		{"a read", "read", func(rec string) error {
			_, err := ReadRecordingFileF(context.Background(), zap.NewNop(), rec, filepath.Join(rec, "reports", "test-run-0"), "test-set-0-report", FormatYAML)
			return err
		}},
		{"a write", "changed", func(rec string) error {
			return WriteFileInRecording(rec, filepath.Join(rec, "reports", "test-run-0", "test-set-0-report.yaml"), []byte("new"), 0o644)
		}},
		{"a folder made on the way", "changed", func(rec string) error {
			return MkdirAllInRecording(rec, filepath.Join(rec, "reports", "test-run-0", "test-set-0-report.yaml", "x"), 0o755)
		}},
		{"a rename onto it", "changed", func(rec string) error {
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
			var out *linkout.Error
			if !errors.As(err, &out) {
				t.Fatalf("got %v, want the link out named", err)
			}
			// The folder made on the way is under the link: the link is
			// said as one it leads through.
			report := filepath.Join("reports", "test-run-0", "test-set-0-report.yaml")
			file, link := report, "it is a link"
			if c.name == "a folder made on the way" {
				file, link = filepath.Join(report, "x"), fmt.Sprintf("it leads through %q, a link", report)
			}
			want := fmt.Sprintf("the recording's file %q is %s only inside its recording %q: %s that leads out of it, to %q; replace the link with what it leads to",
				file, c.verb, rec, link, outside)
			// A caller may say what it was doing first.
			if msg := err.Error(); !strings.HasSuffix(msg, want) {
				t.Errorf("says %q\nwant %q", msg, want)
			}
			if b, _ := os.ReadFile(outside); string(b) != "OUTSIDE" {
				t.Errorf("the file outside now holds %q", b)
			}
		})
	}
}
