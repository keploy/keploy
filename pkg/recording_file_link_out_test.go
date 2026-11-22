package pkg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.keploy.io/server/v3/utils/linkout"
)

// A file a test case names that is a link out of the recording is refused
// by both readers with what any refusal of it says, then the link and where
// it leads, said plainly.
func TestARecordingFileALinkOutIsSaidPlainly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on windows")
	}
	base := t.TempDir()
	rec := filepath.Join(base, "project", "keploy")
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rec, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rec, "set", "body.txt")); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join("set", "body.txt")
	_, readErr := ReadRecordingFile(rec, name)
	f, openErr := OpenRecordingFile(rec, name)
	if f != nil {
		_ = f.Close()
	}
	for what, err := range map[string]error{"read": readErr, "open": openErr} {
		var out *linkout.Error
		if !errors.As(err, &out) {
			t.Fatalf("%s: got %v, want the link out named", what, err)
		}
		want := fmt.Sprintf("the recording's file %q is read only inside its recording %q: it is a link that leads out of it, to %q; replace the link with what it leads to", name, rec, outside)
		if msg := err.Error(); msg != want {
			t.Errorf("%s: says %q\nwant %q", what, msg, want)
		}
	}
}
