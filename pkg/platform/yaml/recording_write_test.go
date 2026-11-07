package yaml

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func writeLink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Dir(path), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, path); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
}

func requireOnlyInside(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
		t.Fatalf("%s: %v, want it refused as outside the recording", what, err)
	}
}

// WriteFile and CreateFileF, for a caller that names no recording, keep a
// write inside the folder it names: a file there that links out of it is
// refused, with why, and what it leads to is left as it was, or not created.
func TestAWriteWithNoRecordingStaysInItsFolder(t *testing.T) {
	ctx := context.Background()
	for _, isAppend := range []bool{true, false} {
		base := t.TempDir()
		dir := filepath.Join(base, "dir")
		out := filepath.Join(base, "outside.yaml")
		if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "doc.yaml")
		writeLink(t, out, file)
		requireOnlyInside(t, "WriteFile", WriteFile(ctx, zap.NewNop(), dir, "doc", []byte("new"), isAppend))
		if got, err := os.ReadFile(out); err != nil || string(got) != "KEEP ME\n" {
			t.Fatalf("isAppend %v: the file outside holds %q (%v)", isAppend, got, err)
		}
		if info, err := os.Lstat(file); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("isAppend %v: the link is gone: %v, %v", isAppend, info, err)
		}
	}
	base := t.TempDir()
	dir := filepath.Join(base, "dir")
	out := filepath.Join(base, "nothing.yaml")
	writeLink(t, out, filepath.Join(dir, "doc.yaml"))
	_, err := CreateFileF(ctx, zap.NewNop(), dir, "doc", FormatYAML)
	requireOnlyInside(t, "CreateFileF", err)
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatalf("CreateFileF created the file the link names: %v", err)
	}
}

// CopyDir copies no file through a link: a source that links out of its
// folder is refused, and so is a target that does, which keeps what it held.
func TestCopyDirFollowsNoLinkOutOfAFolder(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(base, "dst")
	secret := filepath.Join(base, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET-OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLink(t, secret, filepath.Join(src, "linked.yaml"))
	requireOnlyInside(t, "CopyDir from a link", CopyDir(src, src, dst, false, zap.NewNop()))
	if _, err := os.Stat(filepath.Join(dst, "linked.yaml")); !os.IsNotExist(err) {
		t.Fatalf("CopyDir copied the file outside its folder: %v", err)
	}

	src2 := filepath.Join(base, "src2")
	if err := os.MkdirAll(src2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src2, "doc.yaml"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst2 := filepath.Join(base, "dst2")
	kept := filepath.Join(base, "kept.txt")
	if err := os.WriteFile(kept, []byte("KEEP ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLink(t, kept, filepath.Join(dst2, "doc.yaml"))
	requireOnlyInside(t, "CopyDir onto a link", CopyDir(src2, src2, dst2, false, zap.NewNop()))
	if got, err := os.ReadFile(kept); err != nil || string(got) != "KEEP ME\n" {
		t.Fatalf("the file outside holds %q (%v)", got, err)
	}
}

// A folder CopyDir copies is read only inside the checkout it comes with: the
// copied folder itself, a link out of the checkout, is refused, and nothing
// it leads to is copied. A folder link that stays inside the checkout, and a
// file link into another of its folders, are copied.
func TestCopyDirCopiesNoFolderThatLinksOutOfItsCheckout(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "id_rsa"), []byte("SECRET-OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(base, "VirtualCPR", "svc")
	set := filepath.Join(checkout, "keploy", "test-set-0")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	// keploy/test-set-0/tests -> ../../../../outside
	if err := os.Symlink(filepath.Join("..", "..", "..", "..", "outside"), filepath.Join(set, "tests")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(set, "tests", "id_rsa")); err != nil || string(b) != "SECRET-OUTSIDE" {
		t.Fatalf("the link does not reach the file outside: %q, %v", b, err)
	}
	dst := filepath.Join(base, "Download", "Tests", "svc")
	requireOnlyInside(t, "CopyDir of a folder that links out", CopyDir(checkout, filepath.Join(set, "tests"), dst, false, zap.NewNop()))
	if _, err := os.Stat(filepath.Join(dst, "id_rsa")); !os.IsNotExist(err) {
		t.Fatalf("CopyDir copied a file from outside the checkout: %v", err)
	}

	// Inside the checkout, links keep working.
	real := filepath.Join(checkout, "keploy", "test-set-1", "tests")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "test-1.yaml"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	set2 := filepath.Join(checkout, "keploy", "test-set-2")
	if err := os.MkdirAll(set2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "test-set-1", "tests"), filepath.Join(set2, "tests")); err != nil {
		t.Fatal(err)
	}
	dst2 := filepath.Join(base, "Download", "Tests", "svc2")
	if err := CopyDir(checkout, filepath.Join(set2, "tests"), dst2, false, zap.NewNop()); err != nil {
		t.Fatalf("CopyDir of a folder that links inside its checkout: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dst2, "test-1.yaml")); err != nil || string(b) != "inside" {
		t.Fatalf("the copy holds %q (%v), want the file inside", b, err)
	}
}

// Each change to a recording's file goes through a path, or a link, only as
// far as root: refused past it, and done inside it, a link inside included.
func TestChangesInARecording(t *testing.T) {
	setup := func(t *testing.T) (root, out, viaFile, viaDir string) {
		t.Helper()
		base := t.TempDir()
		root = filepath.Join(base, "keploy")
		if err := os.MkdirAll(filepath.Join(root, "set"), 0o755); err != nil {
			t.Fatal(err)
		}
		out = filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "file"), []byte("KEEP ME"), 0o640); err != nil {
			t.Fatal(err)
		}
		viaFile = filepath.Join(root, "set", "linked")
		writeLink(t, filepath.Join(out, "file"), viaFile)
		viaDir = filepath.Join(root, "dir")
		writeLink(t, out, viaDir)
		return root, out, viaFile, viaDir
	}
	unchanged := func(t *testing.T, what, out string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(out, "file"))
		if err != nil || string(got) != "KEEP ME" {
			t.Fatalf("%s: the file outside holds %q (%v)", what, got, err)
		}
		info, err := os.Stat(filepath.Join(out, "file"))
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("%s: the file outside is %v (%v)", what, info, err)
		}
		entries, err := os.ReadDir(out)
		if err != nil || len(entries) != 1 {
			t.Fatalf("%s: outside holds %v (%v), want only its file", what, entries, err)
		}
	}
	for _, c := range []struct {
		name string
		call func(root, viaFile, viaDir string) error
	}{
		{"OpenFile through a link", func(root, viaFile, _ string) error {
			_, err := OpenFileInRecording(root, viaFile, os.O_WRONLY|os.O_APPEND, 0)
			return err
		}},
		{"OpenFile in a folder link", func(root, _, viaDir string) error {
			_, err := OpenFileInRecording(root, filepath.Join(viaDir, "new"), os.O_WRONLY|os.O_CREATE, 0o644)
			return err
		}},
		{"Stat through a link", func(root, viaFile, _ string) error {
			_, err := StatInRecording(root, viaFile)
			return err
		}},
		{"WriteFile through a link", func(root, viaFile, _ string) error {
			return WriteFileInRecording(root, viaFile, []byte("new"), 0o644)
		}},
		{"MkdirAll under a folder link", func(root, _, viaDir string) error {
			return MkdirAllInRecording(root, filepath.Join(viaDir, "a", "b"), 0o755)
		}},
		{"Chmod through a link", func(root, viaFile, _ string) error {
			return ChmodInRecording(root, viaFile, 0o600)
		}},
		{"Remove in a folder link", func(root, _, viaDir string) error {
			return RemoveInRecording(root, filepath.Join(viaDir, "file"))
		}},
		{"RemoveAll in a folder link", func(root, _, viaDir string) error {
			return RemoveAllInRecording(root, filepath.Join(viaDir, "file"))
		}},
		{"Rename onto a link", func(root, viaFile, _ string) error {
			src := filepath.Join(root, "set", "src")
			if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
				return err
			}
			return RenameInRecording(root, src, viaFile)
		}},
		{"Rename from a link", func(root, viaFile, _ string) error {
			return RenameInRecording(root, viaFile, filepath.Join(root, "set", "moved"))
		}},
		{"Rename into a folder link", func(root, _, viaDir string) error {
			src := filepath.Join(root, "set", "src")
			if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
				return err
			}
			return RenameInRecording(root, src, filepath.Join(viaDir, "moved"))
		}},
		{"ReplaceFile onto a link", func(root, viaFile, _ string) error {
			src := filepath.Join(root, "set", "src")
			if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
				return err
			}
			return ReplaceFileInRecording(zap.NewNop(), root, src, viaFile)
		}},
		{"ReplaceFile into a folder link", func(root, _, viaDir string) error {
			src := filepath.Join(root, "set", "src")
			if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
				return err
			}
			err := ReplaceFileInRecording(zap.NewNop(), root, src, filepath.Join(viaDir, "file"))
			// Refused before a rename is tried: its fallback would make a
			// file beside the target, outside the recording, to move it to.
			if err != nil && strings.Contains(err.Error(), "aside") {
				return errors.New("refused by the fallback, not before the rename")
			}
			return err
		}},
		{"ReplaceFile from a link", func(root, viaFile, _ string) error {
			return ReplaceFileInRecording(zap.NewNop(), root, viaFile, filepath.Join(root, "set", "moved"))
		}},
		{"CreateTemp in a folder link", func(root, _, viaDir string) error {
			_, err := CreateTempInRecording(root, viaDir, "x.*.tmp", 0o600)
			return err
		}},
		{"DeleteRecordingFileF in a folder link", func(root, _, viaDir string) error {
			return DeleteRecordingFileF(zap.NewNop(), root, viaDir, "file", Format("none"))
		}},
		{"DeleteRecordingDir in a folder link", func(root, _, viaDir string) error {
			return DeleteRecordingDir(zap.NewNop(), root, filepath.Join(viaDir, "file"))
		}},
		{"a path that climbs out", func(root, _, _ string) error {
			return WriteFileInRecording(root, filepath.Join(root, "..", "outside", "file"), []byte("new"), 0o644)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, out, viaFile, viaDir := setup(t)
			requireOnlyInside(t, c.name, c.call(root, viaFile, viaDir))
			unchanged(t, c.name, out)
		})
	}
}

// What a change refuses says so; what is simply not there says only that, as
// the os call would, and a root that is no directory is refused.
func TestAChangeTellsNotThereFromRefused(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := StatInRecording(root, filepath.Join(root, "absent"))
	if !os.IsNotExist(err) {
		t.Fatalf("StatInRecording of nothing: %v, want what os.IsNotExist tells", err)
	}
	_, err = StatInRecording(filepath.Join(root, "absent-root"), filepath.Join(root, "absent-root", "x"))
	if !os.IsNotExist(err) {
		t.Fatalf("StatInRecording under no root: %v, want what os.IsNotExist tells", err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = StatInRecording(file, filepath.Join(file, "x"))
	requireOnlyInside(t, "a root that is a file", err)
	_, err = StatInRecording(root, "relative")
	requireOnlyInside(t, "a relative path under an absolute root", err)
	if !strings.Contains(err.Error(), `"relative"`) {
		t.Fatalf("the refusal does not name the path: %v", err)
	}
	err = RenameInRecording(root, file, "relative")
	requireOnlyInside(t, "a relative target under an absolute root", err)
	if !strings.Contains(err.Error(), `"relative"`) {
		t.Fatalf("the refusal does not name the path: %v", err)
	}
}

// A link cannot be followed at the final step of a group of files either: one
// that takes the target's place after the group looked at it, as a process
// beside keploy could, is refused at the rename, and left as it is.
func TestALinkThatAppearsBeforeTheRenameIsRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "outside.yaml")
	if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "a.yaml")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	planted := false
	hooks := ReplaceHooks{Step: func() {
		if _, err := os.Lstat(target + ReplaceOldSuffix); planted || err != nil {
			return
		}
		planted = true
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		writeLink(t, out, target)
	}}
	err := ReplaceFilesWith(zap.NewNop(), root, hooks, []File{{Path: target, Data: []byte("new")}})
	if !planted {
		t.Fatal("the group finished before the link could take the target's place")
	}
	requireOnlyInside(t, "ReplaceFilesWith", err)
	if got, rerr := os.ReadFile(out); rerr != nil || string(got) != "KEEP ME\n" {
		t.Fatalf("the file outside holds %q (%v)", got, rerr)
	}
	if info, lerr := os.Lstat(target); lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is gone: %v, %v", info, lerr)
	}
}

// What stays inside the recording goes on as the os call would: a link
// inside is followed, a first recording makes its root, and an empty root is
// the working directory.
func TestChangesInsideARecordingGoOn(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	if err := MkdirAllInRecording(root, filepath.Join(root, "set", "tests"), 0o755); err != nil {
		t.Fatalf("a first recording: %v", err)
	}
	if err := MkdirAllInRecording(root, root, 0o755); err != nil {
		t.Fatalf("the root itself: %v", err)
	}
	real := filepath.Join(root, "set", "real")
	if err := WriteFileInRecording(root, real, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "set", "link")
	writeLink(t, real, link)
	f, err := OpenFileInRecording(root, link, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("a link inside: %v", err)
	}
	if _, err := f.WriteString("two"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(real); err != nil || string(got) != "onetwo" {
		t.Fatalf("through the link: %q (%v)", got, err)
	}
	if err := ChmodInRecording(root, link, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := StatInRecording(root, link); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("a link inside: %v (%v)", info, err)
	}
	src := filepath.Join(root, "set", "src")
	if err := WriteFileInRecording(root, src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFileInRecording(zap.NewNop(), root, src, link); err != nil {
		t.Fatalf("a replace of a link inside: %v", err)
	}
	if got, err := os.ReadFile(link); err != nil || string(got) != "new" {
		t.Fatalf("replaced: %q (%v)", got, err)
	}
	tmp, err := CreateTempInRecording(root, filepath.Join(root, "set"), "a.*.tmp", 0o640)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tmp.Close() }()
	name := filepath.Base(tmp.Name())
	if !strings.HasPrefix(name, "a.") || !strings.HasSuffix(name, ".tmp") || len(name) <= len("a..tmp") {
		t.Fatalf("the temp file is named %q", name)
	}
	if info, err := tmp.Stat(); err != nil || info.Mode().Perm()&^0o640 != 0 {
		t.Fatalf("the temp file: %v (%v)", info, err)
	}
	noStar, err := CreateTempInRecording(root, filepath.Join(root, "set"), "plain", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = noStar.Close() }()
	if n := filepath.Base(noStar.Name()); !strings.HasPrefix(n, "plain") || n == "plain" {
		t.Fatalf("a pattern with no star made %q", n)
	}
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if f, err := CreateTempInRecording(root, root, "a/b.*", 0o600); err == nil {
		_ = f.Close()
		t.Fatal("a pattern holding a separator was taken")
	}
	starFirst, err := CreateTempInRecording(root, filepath.Join(root, "set"), "*.tmp", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = starFirst.Close() }()
	if n := filepath.Base(starFirst.Name()); !strings.HasSuffix(n, ".tmp") || strings.Contains(n, "*") {
		t.Fatalf("a star that comes first made %q", n)
	}
	lastStar, err := CreateTempInRecording(root, filepath.Join(root, "set"), "x*y*z", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lastStar.Close() }()
	if n := filepath.Base(lastStar.Name()); !strings.HasPrefix(n, "x*y") || !strings.HasSuffix(n, "z") {
		t.Fatalf("the random part is not at the last star: %q", n)
	}
	if err := RemoveInRecording(root, src); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Remove of nothing: %v", err)
	}
	if err := DeleteRecordingFileF(zap.NewNop(), root, filepath.Join(root, "set"), "real", Format("none")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("DeleteRecordingFileF of a file of another name: %v", err)
	}
	if err := RemoveAllInRecording(root, filepath.Join(root, "set")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "set")); !os.IsNotExist(err) {
		t.Fatalf("RemoveAll left the folder: %v", err)
	}

	t.Chdir(base)
	if err := MkdirAllInRecording("", "fromcwd", 0o755); err != nil {
		t.Fatalf("an empty root: %v", err)
	}
	if _, err := StatInRecording("", "fromcwd"); err != nil {
		t.Fatalf("an empty root: %v", err)
	}
}

// A temp file's name taken already is passed over for the next one, and names
// that never come free end in an error that says so.
func TestATempNameTakenIsPassedOver(t *testing.T) {
	saved := tempNumber
	t.Cleanup(func() { tempNumber = saved })
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.1.tmp"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	next := []uint32{1, 2}
	tempNumber = func() uint32 {
		n := next[0]
		next = next[1:]
		return n
	}
	f, err := CreateTempInRecording(root, root, "a.*.tmp", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if got := filepath.Base(f.Name()); got != "a.2.tmp" {
		t.Fatalf("the temp file is %q, want the name after the one taken", got)
	}
	tempNumber = func() uint32 { return 1 }
	if _, err := CreateTempInRecording(root, root, "a.*.tmp", 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("no free name: %v, want fs.ErrExist", err)
	}
}
