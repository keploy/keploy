package pkg

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/utils"
)

// A file a test case names that is a link out of the recording is refused
// with the link and where it leads said plainly, by both readers.
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
		var out *utils.LinkOutError
		if !errors.As(err, &out) {
			t.Fatalf("%s: got %v, want the link out named", what, err)
		}
		if msg := err.Error(); !strings.Contains(msg, `"set/body.txt" is a link that leads out of the recording`) || !strings.Contains(msg, outside) || strings.Contains(msg, "escapes from parent") {
			t.Errorf("%s: %q", what, msg)
		}
	}
}
