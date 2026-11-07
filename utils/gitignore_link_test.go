package utils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// keploy adds its entries to keploy/.gitignore, a file a cloned repository
// can carry as a link. One that leads out of the folder is refused, with why,
// and what it leads to keeps what it held; one that stays inside is written.
func TestAddToGitIgnoreStaysInsideTheFolder(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "keploy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "outside.txt"), filepath.Join(dir, ".gitignore")); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	err := AddToGitIgnore(zap.NewNop(), dir, "/reports/")
	if err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("AddToGitIgnore: %v, want it refused", err)
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "KEEP ME\n" {
		t.Fatalf("the file outside holds %q (%v)", got, err)
	}

	inside := filepath.Join(base, "inside")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inside, "real"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(inside, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if err := AddToGitIgnore(zap.NewNop(), inside, "/reports/"); err != nil {
		t.Fatalf("a link inside the folder: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(inside, "real")); err != nil || !strings.Contains(string(got), "/reports/") {
		t.Fatalf("the entry was not written through the link: %q (%v)", got, err)
	}
}
