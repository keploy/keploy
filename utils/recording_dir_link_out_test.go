package utils

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.keploy.io/server/v3/utils/linkout"
)

func symlink(t *testing.T, target, at string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, at); err != nil {
		t.Fatal(err)
	}
}

// A keploy folder, or a folder in it, that is a link os.Root does not follow
// is refused by OpenRecordingDir and MkdirOpenRecordingDir with what any
// refusal of the folder says, then the link and where it leads, not in
// os.Root's words; nothing is made where it leads.
func TestAFolderThatLinksOutIsSaidPlainly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on windows")
	}
	for _, c := range []struct {
		name string
		// layout makes the link in project, beside outside, and says the
		// folder to open, the link's name and the folder it is in, and
		// where it leads.
		layout func(t *testing.T, project, outside string) (dir, link, at, target string)
		inside bool
	}{
		{"a keploy folder by an absolute path out", func(t *testing.T, project, outside string) (string, string, string, string) {
			symlink(t, outside, filepath.Join(project, "keploy"))
			return filepath.Join(project, "keploy"), "keploy", project, outside
		}, false},
		{"a keploy folder that climbs out", func(t *testing.T, project, outside string) (string, string, string, string) {
			symlink(t, filepath.Join("..", "outside"), filepath.Join(project, "keploy"))
			return filepath.Join(project, "keploy"), "keploy", project, outside
		}, false},
		{"a keploy folder by an absolute path into the project", func(t *testing.T, project, _ string) (string, string, string, string) {
			if err := os.MkdirAll(filepath.Join(project, "real"), 0o755); err != nil {
				t.Fatal(err)
			}
			symlink(t, filepath.Join(project, "real"), filepath.Join(project, "keploy"))
			return filepath.Join(project, "keploy"), "keploy", project, filepath.Join(project, "real")
		}, true},
		{"a folder in keploy that climbs out", func(t *testing.T, project, outside string) (string, string, string, string) {
			symlink(t, filepath.Join("..", "..", "outside"), filepath.Join(project, "keploy", "tests"))
			return filepath.Join(project, "keploy", "tests"), "tests", filepath.Join(project, "keploy"), outside
		}, false},
		{"a keploy folder named from the working directory", func(t *testing.T, project, _ string) (string, string, string, string) {
			symlink(t, filepath.Join("..", "outside"), filepath.Join(project, "keploy"))
			t.Chdir(project)
			return "keploy", "keploy", ".", filepath.Join("..", "outside")
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			project, outside := filepath.Join(base, "project"), filepath.Join(base, "outside")
			for _, d := range []string{project, outside} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			dir, link, at, target := c.layout(t, project, outside)
			open := func() error {
				r, err := OpenRecordingDir(dir)
				if err == nil {
					_ = r.Close()
				}
				return err
			}
			mkdir := func() error {
				r, err := MkdirOpenRecordingDir(filepath.Join(dir, "test-set-0"), 0o755)
				if err == nil {
					_ = r.Close()
				}
				return err
			}
			for what, op := range map[string]func() error{"open": open, "make": mkdir} {
				err := op()
				var out *linkout.Error
				if !errors.As(err, &out) {
					t.Fatalf("%s: got %v, want the link named", what, err)
				}
				if out.Link != link || out.Recording != at || out.Target != target || out.Inside != c.inside {
					t.Errorf("%s: got %+v, want the link %q in %q to %q (inside: %v)", what, *out, link, at, target, c.inside)
				}
				msg := err.Error()
				says := fmt.Sprintf("it is a link that leads out of it, to %q; replace the link with what it leads to", target)
				if c.inside {
					says = fmt.Sprintf("it is a link by the absolute path %q, which keploy does not follow; make the link relative", target)
				}
				if want := fmt.Sprintf("the folder %q is opened only inside %q: %s", link, at, says); msg != want {
					t.Errorf("%s: says %q\nwant %q", what, msg, want)
				}
			}
			if ents, err := os.ReadDir(outside); err != nil || len(ents) != 0 {
				t.Errorf("the folder outside holds %v (%v), want nothing made there", ents, err)
			}
			if ents, err := os.ReadDir(filepath.Join(project, "real")); err == nil && len(ents) != 0 {
				t.Errorf("the folder the absolute link names holds %v, want nothing made there", ents)
			}
		})
	}
}
