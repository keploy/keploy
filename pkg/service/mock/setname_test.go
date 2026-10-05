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
		{Name: "app", Dir: "other", App: true},
		{Name: "d"},
	})
	if len(got) != 2 || got[0] != "e2e" || got[1] != "pay" {
		t.Fatalf("got %v", got)
	}
}
