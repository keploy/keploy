//go:build !windows

package learn

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// notRoot skips a test of a permission keploy is refused: root is refused
// none.
func notRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root is refused no permission")
	}
}

// readOnly makes dir read-only for the test, writable again at its end.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// The .gitignore ensure says why it could not keep the learned files out of
// git, at the step that failed (reading the .gitignore, creating the keploy
// folder, writing it), writes it fresh in a folder it creates with every
// missing parent, and adds its lines after a last line that ends, never
// with a blank line, nor one before a first.
func TestTheIgnoreSaysWhyItCouldNotBeWritten(t *testing.T) {
	notRoot(t)
	t.Setenv(provenswitch.Env, "1")
	want := strings.Join(learnedIgnore, "\n") + "\n"

	dir := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(filepath.Join(dir, ".gitignore"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLearnedIgnored(dir); err == nil || !strings.Contains(err.Error(), "read "+filepath.Join(dir, ".gitignore")) {
		t.Errorf("a .gitignore that is a folder: %v, want its read's error", err)
	}

	dir = filepath.Join(t.TempDir(), "keploy")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	readOnly(t, dir)
	if err := EnsureLearnedIgnored(dir); err == nil || !strings.Contains(err.Error(), "write ") || strings.Contains(err.Error(), "read back") {
		t.Errorf("a keploy folder it cannot write in: %v, want the write's error", err)
	}

	parent := t.TempDir()
	readOnly(t, parent)
	if err := EnsureLearnedIgnored(filepath.Join(parent, "keploy")); err == nil || !strings.Contains(err.Error(), "create ") {
		t.Errorf("a keploy folder it cannot create: %v, want the create's error", err)
	}

	dir = filepath.Join(t.TempDir(), "a", "b", "keploy")
	if err := EnsureLearnedIgnored(dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err != nil || string(b) != want {
		t.Fatalf("a fresh .gitignore is %q (%v), want %q", b, err, want)
	}

	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("/reports\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLearnedIgnored(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(b) != "/reports\n"+want {
		t.Fatalf(".gitignore is %q, want %q", b, "/reports\n"+want)
	}
}

// writeReplacing writes a file whole or not at all: a write the disk refuses,
// a rename onto a folder and a folder it cannot write in each fail, saying
// which, and leave no temporary file and the file as it was; a link at the
// path is replaced by a file of its own (0644), never written through.
func TestAFileIsWrittenWholeOrNotAtAll(t *testing.T) {
	notRoot(t)
	dir := t.TempDir()
	left := func(t *testing.T) {
		t.Helper()
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				t.Fatalf("a temporary file is left: %s", e.Name())
			}
		}
	}

	onto := filepath.Join(dir, "a-folder")
	if err := os.MkdirAll(filepath.Join(onto, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeReplacing(dir, ".tmp-", onto, []byte("data\n")); err == nil || !strings.Contains(err.Error(), "rename "+onto+" into place") {
		t.Errorf("a rename onto a folder: %v", err)
	}
	left(t)

	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("theirs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeReplacing(dir, ".tmp-", link, []byte("ours\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
		t.Fatalf("the link is now %v (%v), want a file of 0644", info.Mode(), err)
	}
	if b, _ := os.ReadFile(target); string(b) != "theirs\n" {
		t.Fatalf("the link's target was written: %q", b)
	}
	left(t)

	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	readOnly(t, ro)
	if err := writeReplacing(ro, ".tmp-", filepath.Join(ro, "f"), []byte("x")); err == nil || !strings.Contains(err.Error(), "write "+filepath.Join(ro, ".tmp-")) || !errors.Is(err, os.ErrPermission) {
		t.Errorf("a folder it cannot write in: %v, want its open's refusal", err)
	}
}

// writeLearned removes a learned file a set no longer learns, and says so
// when it cannot; a learned file it cannot write, or one whose .gitignore or
// set folder it cannot make, fails the write, and learned.local.yaml is not
// written after a learned.yaml that was not.
func TestALearnedFileThatCannotBeWrittenFailsTheWrite(t *testing.T) {
	notRoot(t)
	keploy, set := localSet(t, "ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	if err := WriteLegible(set); err != nil {
		t.Fatal(err)
	}
	RegisterSetReader(func(string) ([]*models.Mock, error) { return nil, nil })
	readOnly(t, set)
	if err := WriteLegible(set); err == nil {
		t.Error("a learned.yaml it could not remove: no error")
	}
	if _, err := os.Stat(filepath.Join(set, models.LearnedFile)); err != nil {
		t.Fatalf("learned.yaml: %v", err)
	}
	_ = os.Chmod(set, 0o755)

	// learned.yaml is a folder: neither file is written.
	keploy, set = localSet(t, "ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	if err := os.MkdirAll(filepath.Join(set, models.LearnedFile, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteLearned(set, []LocalValue{{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=b2"}}); err == nil {
		t.Error("a learned.yaml it could not write: no error")
	}
	if _, err := os.Stat(filepath.Join(set, models.LearnedLocalFile)); !os.IsNotExist(err) {
		t.Errorf("learned.local.yaml was written after learned.yaml failed (%v)", err)
	}

	// The .gitignore cannot be read: nothing is written.
	keploy, set = localSet(t, "ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	if err := os.Mkdir(filepath.Join(keploy, ".gitignore"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteLegible(set); err == nil || !strings.Contains(err.Error(), "keep the learned files out of git") {
		t.Errorf("a .gitignore it cannot read: %v", err)
	}
	if _, err := os.Stat(filepath.Join(set, models.LearnedFile)); !os.IsNotExist(err) {
		t.Errorf("learned.yaml was written with no ignore (%v)", err)
	}

	// The set's folder cannot be made (the keploy folder, its .gitignore
	// whole, is read-only): the write says so.
	keploy, _ = localSet(t, "ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	if err := EnsureLearnedIgnored(keploy); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(keploy, "test-set-9")
	readOnly(t, keploy)
	if err := WriteLegible(fresh); err == nil || !strings.Contains(err.Error(), "create "+fresh) {
		t.Errorf("a set folder it cannot create: %v", err)
	}
}

// Under sudo every folder and file keploy creates for the learned files is
// the invoking user's (SUDO_UID, SUDO_GID): each missing folder, outermost
// to innermost, and the .gitignore, while a folder that was there keeps its
// owner. One it cannot give the user is written all the same.
func TestTheLearnedFilesAreTheInvokingUsers(t *testing.T) {
	notRoot(t)
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	gids, err := u.GroupIds()
	if err != nil {
		t.Skip(err)
	}
	gid := -1
	for _, g := range gids {
		if g != u.Gid {
			gid, _ = strconv.Atoi(g)
			break
		}
	}
	if gid < 0 {
		t.Skip("the user is in no group but its own, so no file can be given to another")
	}
	t.Setenv(provenswitch.Env, "1")
	t.Setenv("SUDO_USER", u.Username)
	t.Setenv("SUDO_UID", u.Uid)
	t.Setenv("SUDO_GID", strconv.Itoa(gid))
	groupOf := func(p string) int {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return int(info.Sys().(*syscall.Stat_t).Gid)
	}
	root := t.TempDir()
	was := groupOf(root)
	dir := filepath.Join(root, "a", "b", "keploy")
	if err := EnsureLearnedIgnored(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), dir, filepath.Join(dir, ".gitignore")} {
		if g := groupOf(p); g != gid {
			t.Errorf("%s is of group %d, want the invoking user's %d", p, g, gid)
		}
	}
	if g := groupOf(root); g != was {
		t.Errorf("the folder that was there is now of group %d, was %d", g, was)
	}
	// A group the user is not in: the files cannot be given, and are
	// written all the same.
	t.Setenv("SUDO_GID", "0")
	dir = filepath.Join(t.TempDir(), "c", "keploy")
	if err := EnsureLearnedIgnored(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err != nil {
		t.Fatal(err)
	}
}
