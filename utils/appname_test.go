package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in dir, with no user or system config of the machine's.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo makes a git repository in a folder named name, with one commit,
// and the folders a/b inside it.
func newRepo(t *testing.T, name string) string {
	t.Helper()
	top := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(top, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, top, "init", "-q")
	gitIn(t, top, "commit", "-q", "--allow-empty", "-m", "first")
	return top
}

func wantAppName(t *testing.T, dir, name string, from AppNameSource) {
	t.Helper()
	gotName, gotFrom := AppNameFor(dir)
	if gotName != name || gotFrom != from {
		t.Errorf("AppNameFor(%s) = %q from %s, want %q from %s", dir, gotName, gotFrom, name, from)
	}
}

func TestAppNameForOutsideAnyRepository(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tmp.Xa81bq")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").CombinedOutput(); err == nil {
		t.Fatalf("the test's temporary folder %s is inside the git repository %s; set TMPDIR outside any repository", dir, out)
	}
	wantAppName(t, dir, "tmp.Xa81bq", AppNameFromDir)
}

// A repository with no remote is named after its folder, from the folder
// itself and from any folder inside it, a mktemp one included.
func TestAppNameForARepositoryWithNoRemote(t *testing.T) {
	top := newRepo(t, "orders")
	wantAppName(t, top, "orders", AppNameFromRepo)
	wantAppName(t, filepath.Join(top, "a", "b"), "orders", AppNameFromRepo)
	tmp, err := os.MkdirTemp(top, "tmp.")
	if err != nil {
		t.Fatal(err)
	}
	wantAppName(t, tmp, "orders", AppNameFromRepo)
}

// The origin remote names the repository, whatever its folder is called,
// with or without .git on its URL, and from a folder inside it.
func TestAppNameForARepositoryWithAnOriginRemote(t *testing.T) {
	for _, url := range []string{
		"https://github.com/acme/orders-api.git",
		"https://github.com/acme/orders-api",
		"git@github.com:acme/orders-api.git",
		"git@github.com:acme/orders-api",
		"ssh://git@example.com:2222/acme/orders-api.git/",
	} {
		top := newRepo(t, "checkout")
		gitIn(t, top, "remote", "add", "origin", url)
		gitIn(t, top, "remote", "add", "upstream", "https://github.com/other/upstream-name.git")
		if got := repoNameFromURL(gitIn(t, top, "remote", "get-url", "origin")); got != "orders-api" {
			t.Fatalf("git remote get-url origin names %q for %s", got, url)
		}
		wantAppName(t, top, "orders-api", AppNameFromRemote)
		wantAppName(t, filepath.Join(top, "a", "b"), "orders-api", AppNameFromRemote)
	}
}

// Every linked worktree of a repository names the app as the repository
// does: by its remote, or with none by the folder of the repository it was
// added from, not by the worktree's own folder.
func TestAppNameForALinkedWorktree(t *testing.T) {
	top := newRepo(t, "orders")
	wt := filepath.Join(filepath.Dir(top), "orders-wt-feature")
	gitIn(t, top, "worktree", "add", "-q", wt)
	wantAppName(t, wt, "orders", AppNameFromRepo)
	wantAppName(t, filepath.Join(wt, "a"), "orders", AppNameFromRepo)
	gitIn(t, top, "remote", "add", "origin", "https://github.com/acme/orders-api.git")
	wantAppName(t, wt, "orders-api", AppNameFromRemote)

	// A worktree whose repository is bare: the bare folder without .git.
	bare := filepath.Join(t.TempDir(), "billing.git")
	gitIn(t, filepath.Dir(bare), "clone", "-q", "--bare", top, bare)
	gitIn(t, bare, "remote", "remove", "origin")
	wt2 := filepath.Join(t.TempDir(), "billing-wt")
	gitIn(t, bare, "worktree", "add", "-q", wt2)
	wantAppName(t, wt2, "billing", AppNameFromRepo)
}

func TestRepoNameFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/orders-api.git":       "orders-api",
		"https://github.com/acme/orders-api/":          "orders-api",
		"git@github.com:acme/orders-api.git":           "orders-api",
		"git@github.com:orders-api":                    "orders-api",
		"/srv/git/orders-api.git/":                     "orders-api",
		"/srv/git/orders-api/.git":                     "orders-api",
		`C:\src\orders-api`:                            "orders-api",
		"https://dev.azure.com/acme/p/_git/orders-api": "orders-api",
		"  https://github.com/acme/orders-api.git \n":  "orders-api",
		"/orders-api":                                  "orders-api",
		"":                                             "",
		".git":                                         "",
	} {
		if got := repoNameFromURL(in); got != want {
			t.Errorf("repoNameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// originURL reads remote.origin.url as git does, in the forms git writes and
// accepts, and nothing else.
func TestOriginURL(t *testing.T) {
	for _, c := range []struct{ name, config, want string }{
		{"as git writes it", "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = https://github.com/acme/a.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n", "https://github.com/acme/a.git"},
		{"another remote first", "[remote \"upstream\"]\n\turl = https://github.com/acme/up.git\n[remote \"origin\"]\n\turl = https://github.com/acme/a.git\n", "https://github.com/acme/a.git"},
		{"the first of several", "[remote \"origin\"]\n\turl = https://github.com/acme/a.git\n\turl = https://github.com/acme/b.git\n", "https://github.com/acme/a.git"},
		{"quoted, with a comment", "[remote \"origin\"]\n\turl = \"https://github.com/acme/a;b.git\" ; a mirror\n", "https://github.com/acme/a;b.git"},
		{"a comment after it", "[remote \"origin\"]\n\turl = https://github.com/acme/a.git # ours\n", "https://github.com/acme/a.git"},
		{"key and section in any case, a tab in the header", "[REMOTE\t\"origin\"]\n\tURL=https://github.com/acme/a.git\n", "https://github.com/acme/a.git"},
		{"the older dotted header", "[remote.Origin]\n\turl = https://github.com/acme/a.git\n", "https://github.com/acme/a.git"},
		{"on the header's line", "[remote \"origin\"] url = https://github.com/acme/a.git\n", "https://github.com/acme/a.git"},
		{"continued on the next line", "[remote \"origin\"]\n\turl = https://github.com/\\\nacme/a.git\n", "https://github.com/acme/a.git"},
		{"a commented-out line", "[remote \"origin\"]\n#\turl = https://github.com/acme/old.git\n\turl = https://github.com/acme/a.git\n", "https://github.com/acme/a.git"},
		{"a comment ending in a backslash", "# see C:\\\n[remote \"origin\"]\n\turl = https://github.com/acme/a.git\n", "https://github.com/acme/a.git"},
		{"a remote named Origin is another remote", "[remote \"Origin\"]\n\turl = https://github.com/acme/a.git\n", ""},
		{"a remote named origin2", "[remote \"origin2\"]\n\turl = https://github.com/acme/a.git\n", ""},
		{"a url outside origin", "[remote \"upstream\"]\n\turl = https://github.com/acme/up.git\n[branch \"main\"]\n\turl = x\n", ""},
		{"an unclosed header ends origin", "[remote \"origin\"]\n[broken\n\turl = https://github.com/acme/a.git\n", ""},
		{"a url with no value, then one", "[remote \"origin\"]\n\turl\n\turl = https://github.com/acme/a.git\n", "https://github.com/acme/a.git"},
		{"another key first", "[remote \"origin\"]\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n\turl = https://github.com/acme/a.git\n", "https://github.com/acme/a.git"},
		{"an unquoted subsection is not git's", "[remote origin]\n\turl = https://github.com/acme/a.git\n", ""},
		{"escaped backslashes", "[remote \"origin\"]\n\turl = C:\\\\src\\\\orders-api\n", `C:\src\orders-api`},
		{"an escaped backslash at the end", "[remote \"origin\"]\n\turl = /srv/orders-api\\\\\n\tfetch = x\n", `/srv/orders-api\`},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, []byte(c.config), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := originURL(path); got != c.want {
				t.Errorf("originURL = %q, want %q", got, c.want)
			}
		})
	}
}

// A .git file that names no git directory still marks the repository's
// folder: the app is named after it, and no other file is taken for its
// config (here a config in the folder itself, naming another remote).
func TestAppNameForAnUnreadableGitFile(t *testing.T) {
	for _, content := range []string{".\n", "gitdir:   \n", "not a gitdir line\n"} {
		top := filepath.Join(t.TempDir(), "orders")
		if err := os.MkdirAll(filepath.Join(top, "a"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(top, ".git"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(top, "config"), []byte("[remote \"origin\"]\n\turl = https://github.com/acme/elsewhere.git\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		wantAppName(t, filepath.Join(top, "a"), "orders", AppNameFromRepo)
	}
}

// A repository whose git directory is elsewhere (git init
// --separate-git-dir) is named after its own folder, not the git directory's.
func TestAppNameForASeparateGitDir(t *testing.T) {
	top := filepath.Join(t.TempDir(), "orders")
	gitIn(t, t.TempDir(), "init", "-q", "--separate-git-dir", filepath.Join(t.TempDir(), "store.git"), top)
	wantAppName(t, top, "orders", AppNameFromRepo)
}

// DefaultAppName names the app from the working directory.
func TestDefaultAppNameIsTheWorkingDirectorysRepository(t *testing.T) {
	top := newRepo(t, "checkout")
	gitIn(t, top, "remote", "add", "origin", "https://github.com/acme/orders-api.git")
	t.Chdir(filepath.Join(top, "a", "b"))
	name, from, err := DefaultAppName()
	if err != nil || name != "orders-api" || from != AppNameFromRemote {
		t.Fatalf("DefaultAppName() = %q, %s, %v, want orders-api from %s", name, from, err, AppNameFromRemote)
	}
}
