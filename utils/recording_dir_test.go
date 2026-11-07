package utils

import (
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

	if _, err := OpenRecordingDir(filepath.Join(project, "missing")); !os.IsNotExist(err) {
		t.Errorf("a keploy folder that is not there: %v, want it not there (os.IsNotExist)", err)
	}
	// The file system's root is in no folder: it is opened as it is.
	if r, err := OpenRecordingDir(filepath.VolumeName(project) + string(filepath.Separator)); err != nil {
		t.Errorf("the file system's root: %v, want it opened", err)
	} else {
		_ = r.Close()
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

// MkdirRecordingDir makes a recording's folders as OpenRecordingDir opens
// them: a link on the way that leads out of the project is refused, and no
// folder, not even an empty one, is made where it leads. A link that stays
// inside is followed.
func TestMkdirRecordingDirMakesNothingOutsideTheProject(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(project, "keploy")); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	if err := MkdirRecordingDir(filepath.Join(project, "keploy"), 0o755); err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("an absolute keploy link out: %v, want it refused", err)
	}
	t.Chdir(project)
	if err := MkdirRecordingDir(filepath.Join("keploy", "test-set-1", "assets"), 0o755); err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("a relative path through a keploy link out: %v, want it refused", err)
	}
	if err := os.Symlink(filepath.Join("..", filepath.Base(outside)), "Download"); err != nil {
		t.Fatal(err)
	}
	if err := MkdirRecordingDir(filepath.Join("Download", "Tests"), 0o755); err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("a relative link that climbs out: %v, want it refused", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("made %d entries where the links lead", len(entries))
	}

	if err := os.MkdirAll("real", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", "inside"); err != nil {
		t.Fatal(err)
	}
	if err := MkdirRecordingDir(filepath.Join("inside", "a", "b"), 0o755); err != nil {
		t.Fatalf("a link that stays inside: %v", err)
	}
	if info, err := os.Stat(filepath.Join(project, "real", "a", "b")); err != nil || !info.IsDir() {
		t.Fatalf("the folder was not made through the link inside: %v", err)
	}
	if err := MkdirRecordingDir(filepath.Join("inside", "a", "b"), 0o755); err != nil {
		t.Fatalf("a folder that is there already: %v", err)
	}
	// A mode as os.Stat gives a folder's (with fs.ModeDir) is taken as its
	// permission bits, as os.MkdirAll takes it.
	if err := MkdirRecordingDir(filepath.Join("inside", "c"), fs.ModeDir|0o750); err != nil {
		t.Fatalf("a folder's mode with its type bits: %v", err)
	}
	if info, err := os.Stat(filepath.Join(project, "real", "c")); err != nil || !info.IsDir() {
		t.Fatalf("the folder was not made from a mode with its type bits: %v", err)
	}
}
