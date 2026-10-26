package yaml

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// A test set that is a directory junction -- the usual Windows link, which
// needs no administrator rights -- is refused like a symbolic link: the
// listing reports a junction as irregular, not as a link or a folder, so it
// used to be passed over without a word, and keploy record could write into
// the set it points to.
func TestReadSessionIndices_AJunctionedTestSetIsRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	for _, d := range []string{filepath.Join(root, "test-set-0", "tests"), filepath.Join(base, "elsewhere", "tests")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "test-set-1")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, filepath.Join(base, "elsewhere")).CombinedOutput(); err != nil {
		t.Skipf("cannot make a directory junction here (%v): %s", err, out)
	}
	ctx := context.Background()
	for name, list := range map[string]func() ([]string, error){
		"ReadSessionIndicesF":   func() ([]string, error) { return ReadSessionIndicesF(ctx, root, zap.NewNop(), ModeDir, FormatYAML) },
		"ReadSessionIndicesAny": func() ([]string, error) { return ReadSessionIndicesAny(ctx, root, zap.NewNop(), ModeDir) },
	} {
		ids, err := list()
		if err == nil || !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), "replace the link with a real folder") {
			t.Errorf("%s of a keploy folder holding a junctioned test set = %q, %v; want an error naming %s and saying to replace it with a real folder", name, ids, err, link)
		}
	}
}
