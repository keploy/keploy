package replay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

func backupLink(t *testing.T, target, path string) {
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

// foundInTree says whether a file under dir holds what.
func foundInTree(t *testing.T, dir, what string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return err
		}
		b, err := os.ReadFile(p)
		found = found || strings.Contains(string(b), what)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// The backup of a test set stays inside the recording: a file or a folder of
// the set that links out is refused, with why, and nothing of what it leads
// to is copied into the backup. A backup folder that links out is refused
// too, and gets nothing.
func TestABackupStaysInsideTheRecording(t *testing.T) {
	const secret = "SECRET-OUTSIDE"
	setup := func(t *testing.T) (base, root string, r *Replayer) {
		t.Helper()
		base = t.TempDir()
		root = filepath.Join(base, "keploy")
		if err := os.MkdirAll(filepath.Join(root, "set"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "set", "mocks.yaml"), []byte("inside"), 0o644); err != nil {
			t.Fatal(err)
		}
		return base, root, &Replayer{logger: zap.NewNop(), config: &config.Config{Path: root}}
	}
	refused := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
			t.Fatalf("createBackup: %v, want it refused as outside the recording", err)
		}
	}
	t.Run("a file links out", func(t *testing.T) {
		base, root, r := setup(t)
		out := filepath.Join(base, "outside.txt")
		if err := os.WriteFile(out, []byte(secret), 0o644); err != nil {
			t.Fatal(err)
		}
		backupLink(t, out, filepath.Join(root, "set", "linked.yaml"))
		refused(t, r.createBackup("set"))
		if foundInTree(t, root, secret) {
			t.Fatal("the backup copied the file outside the recording into it")
		}
	})
	t.Run("a folder links out", func(t *testing.T) {
		base, root, r := setup(t)
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "key"), []byte(secret), 0o644); err != nil {
			t.Fatal(err)
		}
		backupLink(t, out, filepath.Join(root, "set", "linked"))
		refused(t, r.createBackup("set"))
		if foundInTree(t, root, secret) {
			t.Fatal("the backup copied the folder outside the recording into it")
		}
	})
	t.Run("a folder with nothing in it links out", func(t *testing.T) {
		base, root, r := setup(t)
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		backupLink(t, out, filepath.Join(root, "set", "linked"))
		refused(t, r.createBackup("set"))
	})
	t.Run("the backup folder links out", func(t *testing.T) {
		base, root, r := setup(t)
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		backupLink(t, out, filepath.Join(root, "set", ".backup"))
		refused(t, r.createBackup("set"))
		if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
			t.Fatalf("the backup left %v (%v) outside the recording", entries, err)
		}
	})
	t.Run("a link inside the recording is copied", func(t *testing.T) {
		_, root, r := setup(t)
		backupLink(t, filepath.Join(root, "set", "mocks.yaml"), filepath.Join(root, "set", "linked.yaml"))
		if err := r.createBackup("set"); err != nil {
			t.Fatal(err)
		}
	})
}
