package mock

import (
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestSetNameKeepsAnInRepoLinkRelative(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, out := filepath.Join(base, "repo"), filepath.Join(base, "shared", "e2e")
	for _, d := range []string{root, out, filepath.Join(root, "real")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(out, filepath.Join(root, "e2e")); err != nil {
		t.Fatal(err)
	}
	if got := setName(filepath.Join(root, "e2e"), root); got != "e2e" {
		t.Fatalf("a link inside the repo keeps its repo name, got %q", got)
	}
	if got := setName(filepath.Join(root, "real"), root); got != "real" {
		t.Fatalf("got %q", got)
	}
}

func TestRanSetsListsEachSetThatBeganOnce(t *testing.T) {
	got := ranSets([]models.ScopeWindow{
		{Name: "a", Dir: "e2e"},
		{Name: "b", Dir: "e2e"},
		{Name: "c", Dir: "pay", Suite: true},
		{Name: "e", Dir: "pay"},
		{Name: "app", Dir: "other", App: true},
		{Name: "s", Dir: "only-suite", Suite: true},
		{Name: "d"},
	})
	if len(got) != 2 || got[0] != "e2e" || got[1] != "pay" {
		t.Fatalf("got %v", got)
	}
}

// linkedProject lays out a project under a linked folder, as macOS's /tmp is
// a link to /private/tmp: it returns the project as the shell names it
// (through the link) and as the kernel does (resolved).
func linkedProject(t *testing.T) (base, viaLink, resolved string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resolved = filepath.Join(base, "private", "proj")
	if err := os.MkdirAll(filepath.Join(resolved, "calls"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "private"), filepath.Join(base, "tmp")); err != nil {
		t.Fatal(err)
	}
	return base, filepath.Join(base, "tmp", "proj"), resolved
}

func TestSetNameNamesATestUnderALinkedFolderRelative(t *testing.T) {
	_, viaLink, resolved := linkedProject(t)
	for _, c := range []struct{ root, dir string }{
		{viaLink, filepath.Join(resolved, "calls")},  // root as the shell names it, the folder resolved
		{viaLink, filepath.Join(viaLink, "calls")},   // both through the link
		{resolved, filepath.Join(viaLink, "calls")},  // a resolved root (git's), the folder through the link
		{resolved, filepath.Join(resolved, "calls")}, // both resolved
	} {
		if got := setName(c.dir, c.root); got != "calls" {
			t.Errorf("setName(%s, root %s) = %q, want calls: an absolute name is refused at commit as not portable", c.dir, c.root, got)
		}
	}
}

// Outside a repository the record's root is its working folder, which Go
// names as the shell does ($PWD, through the link). A test folder that
// arrives resolved is still named relative to it, so the recording is
// portable.
func TestARecordOutsideARepoUnderALinkedFolderNamesItsTestsRelative(t *testing.T) {
	base, viaLink, resolved := linkedProject(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
	t.Chdir(viaLink)
	root := gitTop()
	if root != viaLink && root != resolved {
		t.Fatalf("gitTop() = %s outside a repository, want the working folder %s", root, viaLink)
	}
	if got := setName(filepath.Join(resolved, "calls"), root); got != "calls" {
		t.Fatalf("setName(%s, gitTop() %s) = %q, want calls", filepath.Join(resolved, "calls"), root, got)
	}
}
