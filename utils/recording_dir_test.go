package utils

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	for _, dir := range []string{
		"keploy-up", "./keploy-abs", filepath.Join("keploy", "linked"), filepath.Join("real", "linked"),
		// Leading ".." say where the project is: the folders after them are
		// still opened one inside the other.
		filepath.Join("..", "project", "keploy-up"), filepath.Join("..", "project", "keploy-abs"),
	} {
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

// MkdirOpenRecordingDir makes a recording's folder and returns it open, in
// one pass: what is written through the folder it returns lands in the folder
// made inside the project, one that was there already is opened as it is, and
// a link on the way that leads out is refused, with no folder returned and
// nothing made where it leads.
func TestMkdirOpenRecordingDirOpensTheFolderItMakes(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()
	t.Chdir(project)

	writeThrough := func(dir, name string) error {
		r, err := MkdirOpenRecordingDir(dir, 0o755)
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		return r.WriteFile(name, []byte("x"), 0o644)
	}
	if err := writeThrough(filepath.Join("keploy", "test-set-1", "assets"), "f"); err != nil {
		t.Fatalf("a new folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "keploy", "test-set-1", "assets", "f")); err != nil {
		t.Fatalf("the file was not written in the folder made: %v", err)
	}
	if err := writeThrough(filepath.Join("keploy", "test-set-1", "assets"), "g"); err != nil {
		t.Fatalf("a folder that is there already: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "keploy", "test-set-1", "assets", "g")); err != nil {
		t.Fatalf("the file was not written in the folder that was there: %v", err)
	}
	if err := writeThrough(filepath.Join(project, "keploy"), "h"); err != nil {
		t.Fatalf("an absolute keploy folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "keploy", "h")); err != nil {
		t.Fatalf("the file was not written in the absolute keploy folder: %v", err)
	}

	if err := os.Symlink(filepath.Join("..", filepath.Base(outside)), "Download"); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	if r, err := MkdirOpenRecordingDir(filepath.Join("Download", "Tests"), 0o755); err == nil || r != nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("a relative link that climbs out: root %v, %v; want no root and a refusal", r, err)
	}
	linked := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linked, "keploy")); err != nil {
		t.Fatal(err)
	}
	if r, err := MkdirOpenRecordingDir(filepath.Join(linked, "keploy"), 0o755); err == nil || r != nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("an absolute keploy link out: root %v, %v; want no root and a refusal", r, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("made %d entries where the links lead", len(entries))
	}
}

// An absolute folder in a recording's keploy folder, such as the
// <project>/keploy/tests a user passes, is opened from the project, the
// folder its last keploy folder is in, and then one folder at a time: a
// keploy folder, or a folder in it, that the repository carries as a link out
// is refused, with why, and nothing is read or made where it leads. A link
// that stays inside is followed, and the folders above the project, even one
// named keploy, are opened as they are.
func TestAnAbsoluteFolderInKeployIsOpenedFromTheProject(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	write := func(dir, body string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, at string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, at); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
	}
	write(filepath.Join(outside, "tests"), "SECRET-OUTSIDE")
	// keploy links out of its project by "..", and by an absolute target.
	link(filepath.Join("..", "outside"), filepath.Join(base, "up", "keploy"))
	link(outside, filepath.Join(base, "abs", "keploy"))
	// A folder in keploy links out of keploy.
	link(filepath.Join("..", "..", "outside"), filepath.Join(base, "deep", "keploy", "a"))
	// keploy links to a folder inside its project.
	write(filepath.Join(base, "in", "store", "tests"), "inside")
	link("store", filepath.Join(base, "in", "keploy"))
	// A project reached through an absolute link in a folder named keploy
	// above it: only the last keploy folder says where the project is.
	write(filepath.Join(base, "realproj", "keploy", "tests"), "plain")
	link(filepath.Join(base, "realproj"), filepath.Join(base, "keploy", "proj"))

	for _, dir := range []string{filepath.Join(base, "up", "keploy", "tests"), filepath.Join(base, "deep", "keploy", "a", "tests")} {
		if b, err := os.ReadFile(filepath.Join(dir, "f.yaml")); err != nil || string(b) != "SECRET-OUTSIDE" {
			t.Fatalf("%s does not reach the file outside: %q, %v", dir, b, err)
		}
	}
	for _, dir := range []string{
		filepath.Join(base, "up", "keploy", "tests"),
		filepath.Join(base, "abs", "keploy", "tests"),
		filepath.Join(base, "deep", "keploy", "a", "tests"),
	} {
		if got, err := openReads(dir, "f.yaml"); err == nil || !strings.Contains(err.Error(), "is opened only inside") {
			t.Errorf("%s: read %q, %v; want it refused as leading out of the project", dir, got, err)
		}
		if err := MkdirRecordingDir(filepath.Join(dir, "new"), 0o755); err == nil || !strings.Contains(err.Error(), "only inside") {
			t.Errorf("%s: made %v; want it refused as leading out of the project", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "tests", "new")); !os.IsNotExist(err) {
		t.Errorf("a folder was made where the links lead: %v", err)
	}
	for dir, want := range map[string]string{
		filepath.Join(base, "in", "keploy", "tests"):                            "inside",
		filepath.Join(base, "keploy", "proj", "keploy", "tests"):                "plain",
		filepath.Join(base, "keploy", "proj", "keploy", "tests", "..", "tests"): "plain",
	} {
		if got, err := openReads(dir, "f.yaml"); err != nil || got != want {
			t.Errorf("%s: read %q, %v; want %q", dir, got, err, want)
		}
	}
	// The keploy folder itself is its own last keploy folder.
	if got, err := openReads(filepath.Join(base, "keploy", "proj", "keploy"), filepath.Join("tests", "f.yaml")); err != nil || got != "plain" {
		t.Errorf("a keploy folder below a folder named keploy: read %q, %v; want %q", got, err, "plain")
	}
}

// A folder that cannot be made says which and why: the error names the
// folder and the permission, and is not taken for a folder that is not there.
func TestMkdirRecordingDirSaysWhyAFolderCannotBeMade(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder this user cannot write in")
	}
	keploy := filepath.Join(t.TempDir(), "keploy")
	if err := os.Mkdir(keploy, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(keploy, 0o755) })
	for _, dir := range []string{filepath.Join(keploy, "test-set-0"), filepath.Join(keploy, "test-set-0", "assets")} {
		r, err := MkdirOpenRecordingDir(dir, 0o755)
		if err == nil {
			_ = r.Close()
			t.Fatalf("%s: made in a read-only folder", dir)
		}
		if !strings.Contains(err.Error(), `the folder "test-set-0" is made only inside`) || !errors.Is(err, fs.ErrPermission) || os.IsNotExist(err) {
			t.Errorf("%s: %v; want it to name the folder and the permission", dir, err)
		}
	}
}
