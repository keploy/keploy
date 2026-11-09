package mock

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// backupProject makes base/project/keploy and base/outside, and returns the
// keploy folder and the outside folder.
func backupProject(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	project, outside := filepath.Join(base, "project"), filepath.Join(base, "outside")
	for _, d := range []string{project, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(project, "keploy"), outside
}

func writeBackupFile(t *testing.T, path, data string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), perm); err != nil {
		t.Fatal(err)
	}
}

// TestABackupRefusesAKeployFolderThatLinksOut: a keploy folder the
// repository carries as a link out of the project is refused before the
// record backs a set up, for a set that is there where it leads (with an
// old backup beside it, or none) and for a fresh one, and nothing where it
// leads is removed, copied or moved.
func TestABackupRefusesAKeployFolderThatLinksOut(t *testing.T) {
	keployDir, outside := backupProject(t)
	if err := os.Symlink(filepath.Join("..", "outside"), keployDir); err != nil {
		t.Fatal(err)
	}
	writeBackupFile(t, filepath.Join(outside, "e2e", "old"), "theirs", 0o644)
	writeBackupFile(t, filepath.Join(outside, ".e2e.previous", "keepme"), "theirs", 0o644)
	writeBackupFile(t, filepath.Join(outside, "other", "old"), "theirs", 0o644)
	m := &mockService{logger: zap.NewNop(), config: &config.Config{Path: keployDir}}

	for _, name := range []string{"e2e", "other", "fresh"} {
		restore, _, err := m.saveSet(name)
		if err == nil {
			restore(false)
			t.Fatalf("saveSet(%q) must refuse a keploy folder that links out of the project", name)
		}
	}
	if _, _, err := m.existsOnly("e2e"); err == nil {
		t.Fatal("existsOnly must refuse a keploy folder that links out of the project")
	}
	for _, f := range []string{filepath.Join("e2e", "old"), filepath.Join(".e2e.previous", "keepme"), filepath.Join("other", "old")} {
		got, err := os.ReadFile(filepath.Join(outside, f))
		if err != nil || string(got) != "theirs" {
			t.Fatalf("%s where the keploy link leads must be left as it was: %q, %v", f, got, err)
		}
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("nothing may be made where the keploy link leads: %v", entries)
	}
}

// TestABackupRefusesASetThatLinksOut: a set the repository carries as a link
// out of the project is refused with why, before anything where it leads is
// read: no backup is made of it, and the link is left as it is.
func TestABackupRefusesASetThatLinksOut(t *testing.T) {
	keployDir, outside := backupProject(t)
	writeBackupFile(t, filepath.Join(outside, "secrets", "id_rsa"), "KEY", 0o600)
	if err := os.MkdirAll(keployDir, 0o755); err != nil {
		t.Fatal(err)
	}
	set := filepath.Join(keployDir, "e2e")
	if err := os.Symlink(filepath.Join("..", "..", "outside", "secrets"), set); err != nil {
		t.Fatal(err)
	}
	m := &mockService{logger: zap.NewNop(), config: &config.Config{Path: keployDir}}

	restore, _, err := m.saveSet("e2e")
	if err == nil {
		restore(false)
		t.Fatal("saveSet must refuse a set that links out of the project")
	}
	// Said plainly: the set, the link, and where it leads.
	if want := fmt.Sprintf("the recording's file %q is read only inside its recording %q: it is a link that leads out of it, to %q; replace the link with what it leads to", "e2e", keployDir, filepath.Join(outside, "secrets")); !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("saveSet said %q; want it to end %q", err, want)
	}
	if _, _, err := m.existsOnly("e2e"); err == nil {
		t.Fatal("existsOnly must refuse a set that links out of the project")
	}
	if _, err := os.Lstat(filepath.Join(keployDir, ".e2e.previous")); !os.IsNotExist(err) {
		t.Fatalf("no backup may be made of what the set links to: %v", err)
	}
	if fi, err := os.Lstat(set); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the set's link must be left as it is: %v, %v", fi, err)
	}
}

// TestABackupKeepsALinkInsideTheSetAsALink: a link in the set that stays in
// the recording keeps working: the backup copies it as the link it is, and a
// refused record puts it back so, with a file's execute bit.
func TestABackupKeepsALinkInsideTheSetAsALink(t *testing.T) {
	keployDir, _ := backupProject(t)
	writeBackupFile(t, filepath.Join(keployDir, "shared", "mocks.yaml"), "shared mocks", 0o644)
	set := filepath.Join(keployDir, "e2e")
	writeBackupFile(t, filepath.Join(set, "run.sh"), "#!/bin/sh\n", 0o755)
	target := filepath.Join("..", "shared", "mocks.yaml")
	if err := os.Symlink(target, filepath.Join(set, "mocks.yaml")); err != nil {
		t.Fatal(err)
	}
	m := &mockService{logger: zap.NewNop(), config: &config.Config{Path: keployDir}}

	restore, existed, err := m.saveSet("e2e")
	if err != nil || !existed {
		t.Fatalf("saveSet = %v, %v; want the set backed up", existed, err)
	}
	if err := os.RemoveAll(set); err != nil {
		t.Fatal(err)
	}
	writeBackupFile(t, filepath.Join(set, "mocks.yaml"), "new", 0o644)
	restore(false)

	got, err := os.Readlink(filepath.Join(set, "mocks.yaml"))
	if err != nil || got != target {
		t.Fatalf("the link must be put back as the link it was: %q, %v", got, err)
	}
	data, err := os.ReadFile(filepath.Join(set, "mocks.yaml"))
	if err != nil || string(data) != "shared mocks" {
		t.Fatalf("the link must still lead where it did: %q, %v", data, err)
	}
	fi, err := os.Stat(filepath.Join(set, "run.sh"))
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("a file's execute bit must be kept: %v, %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(keployDir, ".e2e.previous")); !os.IsNotExist(err) {
		t.Fatalf("the backup must be gone once it is put back: %v", err)
	}
}

// TestAPutBackRefusesAKeployFolderSwappedForALinkOut: the put-back after a
// record runs after the repository's own app has run, and checks the keploy
// folder again: one replaced by a link out of the project after the backup
// was made is refused, for a refused record (the set is put back), a kept
// one (the backup is dropped) and a fresh set (what the record left is
// dropped). Nothing where the link leads is removed or moved, and the link
// is left as it is.
func TestAPutBackRefusesAKeployFolderSwappedForALinkOut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fresh bool
		keep  bool
	}{
		{name: "refused record", keep: false},
		{name: "kept record", keep: true},
		{name: "fresh set", fresh: true, keep: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keployDir, outside := backupProject(t)
			if !tc.fresh {
				writeBackupFile(t, filepath.Join(keployDir, "e2e", "old"), "mine", 0o644)
			} else if err := os.MkdirAll(keployDir, 0o755); err != nil {
				t.Fatal(err)
			}
			m := &mockService{logger: zap.NewNop(), config: &config.Config{Path: keployDir}}
			restore, existed, err := m.saveSet("e2e")
			if err != nil || existed == tc.fresh {
				t.Fatalf("saveSet = %v, %v", existed, err)
			}
			// The keploy folder is replaced by a link out while the
			// record runs.
			if err := os.Rename(keployDir, keployDir+".moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "outside"), keployDir); err != nil {
				t.Fatal(err)
			}
			theirs := []string{filepath.Join("e2e", "theirs"), filepath.Join(".e2e.previous", "theirs")}
			for _, f := range theirs {
				writeBackupFile(t, filepath.Join(outside, f), "theirs", 0o644)
			}

			restore(tc.keep)

			for _, f := range theirs {
				got, err := os.ReadFile(filepath.Join(outside, f))
				if err != nil || string(got) != "theirs" {
					t.Fatalf("%s where the keploy link leads must be left as it was: %q, %v", f, got, err)
				}
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatalf("nothing may be made or moved where the keploy link leads: %v", entries)
			}
			if fi, err := os.Lstat(keployDir); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the keploy link must be left as it is: %v, %v", fi, err)
			}
		})
	}
}

// TestAPutBackRefusesABackupSwappedForALinkOut: the backup the put-back moves
// back as the set is checked when it is moved: a backup replaced by a link
// out of the project after it was made is not made the set, so the next
// record does not write where it leads; the link is left where it was.
func TestAPutBackRefusesABackupSwappedForALinkOut(t *testing.T) {
	keployDir, outside := backupProject(t)
	writeBackupFile(t, filepath.Join(keployDir, "e2e", "old"), "mine", 0o644)
	m := &mockService{logger: zap.NewNop(), config: &config.Config{Path: keployDir}}
	restore, existed, err := m.saveSet("e2e")
	if err != nil || !existed {
		t.Fatalf("saveSet = %v, %v", existed, err)
	}
	saved := filepath.Join(keployDir, ".e2e.previous")
	if err := os.RemoveAll(saved); err != nil {
		t.Fatal(err)
	}
	writeBackupFile(t, filepath.Join(outside, "theirs", "f"), "theirs", 0o644)
	if err := os.Symlink(filepath.Join("..", "..", "outside", "theirs"), saved); err != nil {
		t.Fatal(err)
	}

	restore(false)

	if fi, err := os.Lstat(filepath.Join(keployDir, "e2e")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("a backup that links out of the project must not be made the set")
	}
	if fi, err := os.Lstat(saved); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the backup's link must be left where it was: %v, %v", fi, err)
	}
	if got, err := os.ReadFile(filepath.Join(outside, "theirs", "f")); err != nil || string(got) != "theirs" {
		t.Fatalf("what the backup's link leads to must be left as it was: %q, %v", got, err)
	}
}
