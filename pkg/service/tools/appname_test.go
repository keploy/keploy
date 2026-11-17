package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config/loader"
)

// AddAppName rewrites keploy.yml in place with config.WithAppName's one line:
// the file keeps its mode and every other byte, and a line shorter than the
// one it replaces leaves nothing of that one behind.
func TestAddAppNameWritesInPlace(t *testing.T) {
	for _, c := range []struct{ name, app, doc, want string }{
		{"a line added", "shop", "# mine\npath: x\n", "# mine\nappName: \"shop\"\npath: x\n"},
		{"a longer line replaced by a shorter one", "a", "appName: null\npath: x\n", "appName: \"a\"\npath: x\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keploy.yml")
			if err := os.WriteFile(path, []byte(c.doc), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := AddAppName(path, c.app); err != nil {
				t.Fatalf("AddAppName: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("keploy.yml =\n%q\nwant\n%q", got, c.want)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
				t.Errorf("mode = %v, want the file's own 0600", fi.Mode().Perm())
			}
		})
	}
}

// A keploy.yml that links to a file in its folder (a monorepo layout) is
// written through the link, which stays a link.
func TestAddAppNameWritesThroughALinkInItsFolder(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "shared.yml")
	if err := os.WriteFile(real, []byte("path: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "keploy.yml")
	if err := os.Symlink("shared.yml", link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := AddAppName(link, "shop"); err != nil {
		t.Fatalf("AddAppName: %v", err)
	}
	if got, _ := os.ReadFile(real); string(got) != "appName: \"shop\"\npath: x\n" {
		t.Errorf("the linked file = %q", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("keploy.yml is no longer a link: %v, %v", fi, err)
	}
}

// AddAppName changes nothing it refuses: a link out of keploy.yml's folder, a
// file past keploy's limit, and a file WithAppName refuses.
func TestAddAppNameRefuses(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "elsewhere.yml")
	if err := os.WriteFile(outside, []byte("path: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := "path: x\n# " + strings.Repeat("a", loader.MaxConfigBytes) + "\n"
	for _, c := range []struct {
		name, doc, says string
		link, missing   bool
	}{
		{name: "a link out of its folder", link: true, says: "pointing outside"},
		{name: "a file that is not there", missing: true},
		{name: "past keploy's limit", doc: big, says: "over"},
		{name: "one WithAppName refuses", doc: "appName: orders\n", says: "not an appName line"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keploy.yml")
			check, want := path, c.doc
			if c.link {
				if err := os.Symlink(outside, path); err != nil {
					t.Skipf("no symlinks here: %v", err)
				}
				check, want = outside, "path: x\n"
			} else if c.missing {
				err := AddAppName(path, "shop")
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("AddAppName = %v, want the file's not-exist error", err)
				}
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("AddAppName made the file it refused: %v", err)
				}
				return
			} else if err := os.WriteFile(path, []byte(c.doc), 0o644); err != nil {
				t.Fatal(err)
			}
			err := AddAppName(path, "shop")
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("AddAppName = %v, want a refusal that says %q", err, c.says)
			}
			if got, _ := os.ReadFile(check); string(got) != want {
				t.Errorf("AddAppName changed the file it refused: %.80q", got)
			}
		})
	}
}
