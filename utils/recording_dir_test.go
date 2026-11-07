package utils

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openReads opens dir with OpenRecordingDir and reads name in it.
func openReads(dir, name string) (string, error) {
	r, err := OpenRecordingDir(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	b, err := r.ReadFile(name)
	return string(b), err
}

// A recording's keploy folder is opened from the project, the folder it is
// in: a keploy folder that is a link out of the project, by ".." or by an
// absolute target, is refused, with why, and nothing under it is read. One
// that links to a folder inside the project is followed. A relative folder is
// held to the working directory one folder at a time, so a folder in the
// recording that links out of the one it is in is refused too.
func TestOpenRecordingDirIsAnchoredAtTheProject(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	project := filepath.Join(base, "project")
	for _, d := range []string{outside, filepath.Join(project, "real", "schema"), filepath.Join(project, "keploy-plain")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for dir, body := range map[string]string{outside: "SECRET-OUTSIDE", filepath.Join(project, "real"): "inside", filepath.Join(project, "keploy-plain"): "plain"} {
		if err := os.WriteFile(filepath.Join(dir, "f.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"keploy-up":       filepath.Join("..", "outside"),
		"keploy-abs":      outside,
		"keploy-in":       "real",
		"keploy-absinner": filepath.Join(project, "real"),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(project, name)); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
	}
	for _, name := range []string{"keploy-up", "keploy-abs"} {
		if b, err := os.ReadFile(filepath.Join(project, name, "f.yaml")); err != nil || string(b) != "SECRET-OUTSIDE" {
			t.Fatalf("%s does not reach the file outside: %q, %v", name, b, err)
		}
	}

	for _, tc := range []struct {
		dir     string
		want    string
		refused bool
	}{
		{dir: filepath.Join(project, "keploy-up"), refused: true},
		{dir: filepath.Join(project, "keploy-abs"), refused: true},
		// An absolute target is refused even when it names a folder inside
		// the project: os.Root cannot tell, and such a link resolves only on
		// one machine.
		{dir: filepath.Join(project, "keploy-absinner"), refused: true},
		{dir: filepath.Join(project, "keploy-in"), want: "inside"},
		{dir: filepath.Join(project, "keploy-plain"), want: "plain"},
		{dir: filepath.Join(project, "keploy-plain") + string(filepath.Separator), want: "plain"},
	} {
		got, err := openReads(tc.dir, "f.yaml")
		if tc.refused {
			if err == nil || !strings.Contains(err.Error(), "is opened only inside") {
				t.Errorf("%s: read %q, %v; want it refused as leading out of the project", tc.dir, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: read %q, %v; want %q", tc.dir, got, err, tc.want)
		}
	}

	if _, err := OpenRecordingDir(filepath.Join(project, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a keploy folder that is not there: %v, want it not there", err)
	}

	// Relative: from the working directory, one folder at a time.
	if err := os.Symlink(filepath.Join("..", "..", "outside"), filepath.Join(project, "real", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(project, "keploy")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	for _, dir := range []string{"keploy-up", "./keploy-abs", filepath.Join("keploy", "linked"), filepath.Join("real", "linked")} {
		if got, err := openReads(dir, "f.yaml"); err == nil || !strings.Contains(err.Error(), "is opened only inside") {
			t.Errorf("%s: read %q, %v; want it refused as leading out of the folder it is in", dir, got, err)
		}
	}
	for dir, want := range map[string]string{
		"keploy":    "inside",
		"./keploy/": "inside",
		"keploy-in": "inside",
		".":         "",
		"":          "",
		filepath.Join("..", "project", "keploy-plain"): "plain",
		filepath.Join("..", "outside"):                 "SECRET-OUTSIDE",
	} {
		if want == "" {
			if r, err := OpenRecordingDir(dir); err != nil {
				t.Errorf("%q: %v, want the working directory", dir, err)
			} else {
				_ = r.Close()
			}
			continue
		}
		if got, err := openReads(dir, "f.yaml"); err != nil || got != want {
			t.Errorf("%s: read %q, %v; want %q", dir, got, err, want)
		}
	}
}
