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
				file, link = filepath.Join(report, "x"), fmt.Sprintf("it leads through %q, a link", filepath.ToSlash(report))
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

// A change refused at a link out for another cause, a folder keploy may not
// change or a name that is there already, is said with that cause, after
// what any refusal of it says, never as a link that leads out: os.Root's
// Remove and an O_EXCL create do not follow the link, so it is not why.
func TestAnotherCauseAtALinkOutIsSaidAsItIs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on windows")
	}
	for _, c := range []struct {
		name, folder, cause string
		notRoot             bool
		op                  func(rec, link string) error
	}{
		{"a remove in a folder keploy may not change", "d", "permission denied", true, func(rec, link string) error {
			return RemoveInRecording(rec, link)
		}},
		{"an O_EXCL create", "w", "file exists", false, func(rec, link string) error {
			f, err := OpenFileInRecording(rec, link, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
			if err == nil {
				_ = f.Close()
			}
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.notRoot && os.Geteuid() == 0 {
				t.Skip("root may change a folder whatever its mode")
			}
			base := t.TempDir()
			rec := filepath.Join(base, "project", "keploy")
			outside := filepath.Join(base, "outside.txt")
			if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o600); err != nil {
				t.Fatal(err)
			}
			folder := filepath.Join(rec, c.folder)
			if err := os.MkdirAll(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(folder, "l.yaml")
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if c.notRoot {
				if err := os.Chmod(folder, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(folder, 0o755) })
			}
			err := c.op(rec, link)
			if err == nil {
				t.Fatal("not refused")
			}
			want := fmt.Sprintf("the recording's file %q is changed only inside its recording %q: ", filepath.Join(c.folder, "l.yaml"), rec)
			msg := err.Error()
			if !strings.HasPrefix(msg, want) || !strings.HasSuffix(msg, c.cause) {
				t.Errorf("says %q\nwant %q, then os.Root's error, ending %q", msg, want, c.cause)
			}
			var out *linkout.Error
			if errors.As(err, &out) || strings.Contains(msg, "leads out") {
				t.Errorf("says %q: the link out is not why", msg)
			}
			if b, _ := os.ReadFile(outside); string(b) != "OUTSIDE" {
				t.Errorf("the file outside now holds %q", b)
			}
			if _, err := os.Lstat(link); err != nil {
				t.Errorf("the link is gone: %v", err)
			}
		})
	}
}
