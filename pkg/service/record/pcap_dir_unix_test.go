//go:build !windows

package record

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The folder a record's packet capture is streamed to is the test set's, made
// only inside the project: through a keploy folder the repository carries as
// a link out of the project, it is refused with why, and no folder is made
// where the link leads. One inside the project is made as before.
func TestPcapDirIsMadeOnlyInsideTheProject(t *testing.T) {
	project, outside := t.TempDir(), t.TempDir()
	keployDir := filepath.Join(project, "keploy")
	if err := os.Symlink(outside, keployDir); err != nil {
		t.Fatal(err)
	}
	if dir, err := pcapDir(keployDir, "test-set-0"); err == nil {
		t.Errorf("pcapDir through a keploy folder that links out = %s; want it refused", dir)
	}
	if _, err := os.Lstat(filepath.Join(outside, "test-set-0")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a folder was made where the keploy link leads: %v", err)
	}

	inside := filepath.Join(t.TempDir(), "keploy")
	dir, err := pcapDir(inside, "test-set-0")
	if err != nil || dir != filepath.Join(inside, "test-set-0") {
		t.Fatalf("pcapDir = %s, %v", dir, err)
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		t.Errorf("the test set's folder was not made: %v", err)
	}
}

// The project is found from the keploy folder, never from the set's name: a
// set named keploy, under a keploy folder the repository carries as a link
// out of the project, is refused with why, and nothing is made where the
// link leads. Rooted at the set's folder instead, the set would be taken for
// the keploy folder and the link followed.
// A set folder that is a link out of the keploy folder is refused, said
// plainly (the set, the link and where it leads), and nothing is made there.
func TestPcapDirASetThatLinksOutIsSaidPlainly(t *testing.T) {
	keployDir, outside := filepath.Join(t.TempDir(), "keploy"), t.TempDir()
	if err := os.MkdirAll(keployDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "pcap")
	if err := os.Symlink(target, filepath.Join(keployDir, "test-set-0")); err != nil {
		t.Fatal(err)
	}
	_, err := pcapDir(keployDir, "test-set-0")
	if want := fmt.Sprintf("the test set's folder %q is made only inside %q: it is a link that leads out of it, to %q; replace the link with what it leads to", "test-set-0", keployDir, target); err == nil || err.Error() != want {
		t.Fatalf("pcapDir through a set that links out = %v; want %q", err, want)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a folder was made where the set's link leads: %v", err)
	}
}

func TestPcapDirASetNamedKeployDoesNotMoveTheProject(t *testing.T) {
	project, outside := t.TempDir(), t.TempDir()
	keployDir := filepath.Join(project, "keploy")
	if err := os.Symlink(outside, keployDir); err != nil {
		t.Fatal(err)
	}
	if dir, err := pcapDir(keployDir, "keploy"); err == nil {
		t.Errorf("pcapDir of a set named keploy through a keploy folder that links out = %s; want it refused", dir)
	}
	if _, err := os.Lstat(filepath.Join(outside, "keploy")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a folder was made where the keploy link leads: %v", err)
	}

	inside := filepath.Join(t.TempDir(), "keploy")
	dir, err := pcapDir(inside, "keploy")
	if err != nil || dir != filepath.Join(inside, "keploy") {
		t.Fatalf("pcapDir of a set named keploy inside the project = %s, %v", dir, err)
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		t.Errorf("the set's folder was not made: %v", err)
	}
}

// A set the repository carries as a link out of the keploy folder is refused
// with why, and nothing is made where it leads; one that links inside the
// keploy folder is used as before.
func TestPcapDirASetThatLinksOutIsRefused(t *testing.T) {
	outside := t.TempDir()
	keployDir := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(filepath.Join(keployDir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{outside, "../../" + filepath.Base(outside)} {
		set := "test-set-out"
		_ = os.Remove(filepath.Join(keployDir, set))
		if err := os.Symlink(target, filepath.Join(keployDir, set)); err != nil {
			t.Fatal(err)
		}
		if dir, err := pcapDir(keployDir, set); err == nil {
			t.Errorf("pcapDir of a set linking to %s = %s; want it refused", target, dir)
		}
	}
	if err := os.Symlink("real", filepath.Join(keployDir, "test-set-in")); err != nil {
		t.Fatal(err)
	}
	if _, err := pcapDir(keployDir, "test-set-in"); err != nil {
		t.Errorf("pcapDir of a set linking inside the keploy folder: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("the outside folder changed: %v %v", entries, err)
	}
}
