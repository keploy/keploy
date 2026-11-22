package utils

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// keploy keeps its tests in <path>/keploy, and the binary is called keploy:
// download it into a project and run `./keploy record` there, and the folder
// keploy wants IS the binary. The error has to say which it is, and how to get
// out of it -- without telling anyone to overwrite their installed keploy.
func TestKeployPathThatIsAFileSaysWhatToDo(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "keploy")
	if err := os.WriteFile(file, []byte("an old download, or anything"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := EnsureKeployPathIsFolder(file)
	if err == nil {
		t.Fatal("a file where the keploy folder goes was accepted")
	}
	for _, want := range []string{file, "a file, not a folder", "remove or rename that file", "--path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "/usr/local/bin") {
		t.Errorf("a file that is not the running keploy was to be moved into /usr/local/bin, over the installed one: %v", err)
	}
	// The permission check native runs meet next explains it the same way,
	// not with the bare "exists but is not a directory". (A no-op on Windows.)
	if runtime.GOOS != "windows" {
		if _, err := CheckKeployFolderPermissions(zap.NewNop(), file); err == nil || !strings.Contains(err.Error(), "a file, not a folder") {
			t.Errorf("the permission check does not explain a file at the keploy folder's path: %v", err)
		}
	}

	// A folder, or nothing yet, is fine.
	if err := EnsureKeployPathIsFolder(dir); err != nil {
		t.Fatalf("a folder was refused: %v", err)
	}
	if err := EnsureKeployPathIsFolder(filepath.Join(dir, "absent")); err != nil {
		t.Fatalf("a path that does not exist yet was refused: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path on this platform")
	}

	// Another copy of keploy -- an old download, while the installed one runs
	// -- is not the running binary, whatever it looks like: a new file with the
	// same bytes and the exec bit. Only file identity tells them apart, and
	// pointing this one at /usr/local/bin would overwrite the installed keploy.
	other := filepath.Join(t.TempDir(), "keploy")
	copyExecutable(t, exe, other)
	err = EnsureKeployPathIsFolder(other)
	if err == nil || !strings.Contains(err.Error(), "a file, not a folder") || !strings.Contains(err.Error(), "remove or rename that file") {
		t.Errorf("another copy of keploy did not get the advice for a file in the way: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "/usr/local/bin") {
		t.Errorf("another copy of keploy was to be moved into /usr/local/bin, over the installed one: %v", err)
	}

	// ...and when that file is the running binary, it says so, and to move it.
	self := filepath.Join(t.TempDir(), "keploy")
	if err := os.Symlink(exe, self); err != nil {
		t.Skipf("cannot symlink here: %v", err)
	}
	err = EnsureKeployPathIsFolder(self)
	if err == nil || !strings.Contains(err.Error(), "this keploy binary, not a folder") || !strings.Contains(err.Error(), "move the keploy binary out of this folder") {
		t.Fatalf("the running binary at the keploy folder's path was not named: %v", err)
	}

	// A folder reached through a symlink -- a test store kept elsewhere (shared,
	// or mounted) and linked in -- is a folder. The link itself is not.
	linked := filepath.Join(t.TempDir(), "keploy")
	if err := os.Symlink(dir, linked); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := EnsureKeployPathIsFolder(linked); err != nil {
		t.Fatalf("a symlink to a keploy folder was refused: %v", err)
	}
}

// copyExecutable writes a byte-for-byte copy of src to dst, executable: a new
// file, never a link to src.
func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
