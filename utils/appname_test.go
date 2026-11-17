package utils

import (
	"os"
	"os/exec"
	"os/user"
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

// A repository with no remote is named after its folder at its top folder,
// and a folder inside it, a mktemp one included, after the repository and
// the folder's path inside it.
func TestAppNameForARepositoryWithNoRemote(t *testing.T) {
	top := newRepo(t, "orders")
	wantAppName(t, top, "orders", AppNameFromRepo)
	wantAppName(t, top+string(filepath.Separator), "orders", AppNameFromRepo)
	wantAppName(t, filepath.Join(top, "a"), "orders-a", AppNameFromRepo.andPath("a"))
	wantAppName(t, filepath.Join(top, "a", "b"), "orders-a-b", AppNameFromRepo.andPath("a/b"))
	tmp, err := os.MkdirTemp(top, "tmp.")
	if err != nil {
		t.Fatal(err)
	}
	wantAppName(t, tmp, "orders-"+filepath.Base(tmp), AppNameFromRepo.andPath(filepath.Base(tmp)))
}

// The origin remote names the repository, whatever its folder is called,
// with or without .git on its URL; a folder inside it adds its path.
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
		wantAppName(t, filepath.Join(top, "a", "b"), "orders-api-a-b", AppNameFromRemote.andPath("a/b"))
	}
}

// Every linked worktree of a repository names the app as the repository
// does: by its remote, or with none by the folder of the repository it was
// added from, not by the worktree's own folder. A folder inside the worktree
// adds its path from the worktree's top folder.
func TestAppNameForALinkedWorktree(t *testing.T) {
	top := newRepo(t, "orders")
	wt := filepath.Join(filepath.Dir(top), "orders-wt-feature")
	gitIn(t, top, "worktree", "add", "-q", wt)
	wantAppName(t, wt, "orders", AppNameFromRepo)
	wantAppName(t, filepath.Join(wt, "a"), "orders-a", AppNameFromRepo.andPath("a"))
	gitIn(t, top, "remote", "add", "origin", "https://github.com/acme/orders-api.git")
	wantAppName(t, wt, "orders-api", AppNameFromRemote)
	wantAppName(t, filepath.Join(wt, "a", "b"), "orders-api-a-b", AppNameFromRemote.andPath("a/b"))

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
		{"an escaped backslash, then a continuation", "[remote \"origin\"]\n\turl = /srv/orders-api\\\\\\\n-b\n", `/srv/orders-api\-b`},
		{"a comment after a value ending in a backslash", "[remote \"origin\"]\n\tfetch = x # C:\\\n[remote \"upstream\"]\n\turl = https://github.com/acme/up.git\n", ""},
		{"a quoted # is no comment, so its backslash continues", "[remote \"origin\"]\n\turl = \"https://github.com/acme/a#\\\nb.git\"\n", "https://github.com/acme/a#b.git"},
		{"a ; comment after a url ending in a backslash", "[remote \"origin\"]\n\turl = https://github.com/acme/a.git ; C:\\\n\turl = https://github.com/acme/b.git\n", "https://github.com/acme/a.git"},
		{"a ; comment ending in a backslash", "; see C:\\\n[remote \"origin\"]\n\turl = https://github.com/acme/a.git\n", "https://github.com/acme/a.git"},
		{"a header with no closing quote is not git's", "[remote \"origin]\n\turl = https://github.com/acme/a.git\n", ""},
		{"a header with no opening quote is not git's", "[remote origin\"]\n\turl = https://github.com/acme/a.git\n", ""},
		{"a section named origin that is not a remote", "[branch \"origin\"]\n\turl = https://github.com/acme/a.git\n", ""},
		{"a dotted section named origin that is not a remote", "[branch.origin]\n\turl = https://github.com/acme/a.git\n", ""},
		{"another remote in the dotted header", "[remote.upstream]\n\turl = https://github.com/acme/a.git\n", ""},
		{"a section with no subsection", "[remote]\n\turl = https://github.com/acme/a.git\n", ""},
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

// A config is read up to gitFileLimit: an origin past it is not seen, and
// the app is named after the repository's top folder, as with no remote.
func TestAppNameForAConfigPastTheReadLimit(t *testing.T) {
	top := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(filepath.Join(top, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("# padding\n", gitFileLimit/len("# padding\n")+1)
	config := pad + "[remote \"origin\"]\n\turl = https://github.com/acme/far.git\n"
	if err := os.WriteFile(filepath.Join(top, ".git", "config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	wantAppName(t, top, "checkout", AppNameFromRepo)
	// The same origin within the limit names the app.
	if err := os.WriteFile(filepath.Join(top, ".git", "config"), []byte(config[len(pad):]), 0o644); err != nil {
		t.Fatal(err)
	}
	wantAppName(t, top, "far", AppNameFromRemote)
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
		wantAppName(t, filepath.Join(top, "a"), "orders-a", AppNameFromRepo.andPath("a"))
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
	if want := AppNameFromRemote.andPath("a/b"); err != nil || name != "orders-api-a-b" || from != want {
		t.Fatalf("DefaultAppName() = %q, %s, %v, want orders-api-a-b from %s", name, from, err, want)
	}
}

// Dots and spaces in the repository's and the folders' names stay as they
// are; only the path's separators become "-".
func TestAppNameForFoldersWithDotsAndSpaces(t *testing.T) {
	top := newRepo(t, "my shop.v2")
	dir := filepath.Join(top, "svc v1.2", "api.x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	wantAppName(t, dir, "my shop.v2-svc v1.2-api.x", AppNameFromRepo.andPath("svc v1.2/api.x"))
	gitIn(t, top, "remote", "add", "origin", "https://github.com/acme/orders.api.git")
	wantAppName(t, dir, "orders.api-svc v1.2-api.x", AppNameFromRemote.andPath("svc v1.2/api.x"))
}

// setHome makes home the user's home folder for the test, as os.UserHomeDir
// reads it, with no sudo.
func setHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SUDO_USER", "")
}

// newDotfilesHome makes a home folder named name that is a git repository
// (dotfiles kept in ~/.git) with a remote, and the folders proj/a in it.
func newDotfilesHome(t *testing.T, parent, name string) string {
	t.Helper()
	home := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(home, "proj", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, home, "init", "-q")
	gitIn(t, home, "remote", "add", "origin", "https://github.com/someone/dotfiles.git")
	return home
}

// The search for a repository stops at the user's home folder: dotfiles kept
// in ~/.git, or a repository holding the home folder, never name the folders
// under it, nor the home folder itself. A repository under the home folder
// still names its folders.
func TestAppNameStopsAtTheHomeFolder(t *testing.T) {
	home := newDotfilesHome(t, t.TempDir(), "someone")
	setHome(t, home)
	wantAppName(t, filepath.Join(home, "proj", "a"), "a", AppNameFromDir)
	wantAppName(t, filepath.Join(home, "proj"), "proj", AppNameFromDir)
	wantAppName(t, home, "someone", AppNameFromDir)

	shop := filepath.Join(home, "shop")
	if err := os.MkdirAll(filepath.Join(shop, "services", "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, shop, "init", "-q")
	wantAppName(t, filepath.Join(shop, "services", "orders"), "shop-services-orders", AppNameFromRepo.andPath("services/orders"))

	// A repository above the home folder.
	outer := newRepo(t, "outer")
	inner := filepath.Join(outer, "a", "home")
	if err := os.MkdirAll(filepath.Join(inner, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	setHome(t, inner)
	wantAppName(t, filepath.Join(inner, "proj"), "proj", AppNameFromDir)
	// Outside the home folder the search goes on as before.
	wantAppName(t, filepath.Join(outer, "a"), "outer-a", AppNameFromRepo.andPath("a"))
}

// Under sudo, which may set $HOME to root's, the home of the user who ran
// sudo (SUDO_USER) stops the search too; a SUDO_USER that names no user
// stops nothing, and nor does an unset one.
func TestAppNameStopsAtTheSudoUsersHome(t *testing.T) {
	someone := newDotfilesHome(t, t.TempDir(), "someone")
	original := lookupUser
	t.Cleanup(func() { lookupUser = original })
	lookupUser = func(name string) (*user.User, error) {
		if name == "nobody-here" {
			// A user beside the error, which a lookup that fails must not use.
			return &user.User{HomeDir: someone}, user.UnknownUserError(name)
		}
		return &user.User{Username: name, HomeDir: someone}, nil
	}
	dir := filepath.Join(someone, "proj", "a")
	setHome(t, t.TempDir())
	for _, c := range []struct {
		sudoUser, want string
		from           AppNameSource
	}{
		{"someone", "a", AppNameFromDir},
		{"nobody-here", "dotfiles-proj-a", AppNameFromRemote.andPath("proj/a")},
		{"", "dotfiles-proj-a", AppNameFromRemote.andPath("proj/a")},
	} {
		t.Setenv("SUDO_USER", c.sudoUser)
		wantAppName(t, dir, c.want, c.from)
	}
}

// DefaultAppName fails, as GetLastDirectory does, once the working directory
// is gone, rather than name the app after a folder that is not there.
func TestDefaultAppNameInARemovedDirectory(t *testing.T) {
	removeWorkingDirectory(t)
	name, _, err := DefaultAppName()
	wantGone(t, err)
	if name != "" {
		t.Errorf("DefaultAppName() named the app %q in a removed directory", name)
	}
}

// gitConfigValue drops a backslash that ends a value with nothing to escape;
// originURL never passes one, as such a line continues on the next.
func TestGitConfigValue(t *testing.T) {
	for in, want := range map[string]string{
		` https://github.com/acme/a.git `: "https://github.com/acme/a.git",
		`"a b" c`:                         "a b c",
		`a\"b`:                            `a"b`,
		`a\\`:                             `a\`,
		`a\`:                              "a",
		`"a#b";c`:                         "a#b",
		`a\nb`:                            "a\nb",
		`a\tb`:                            "a\tb",
		`a\bb`:                            "a\bb",
		`a\x`:                             "ax",
		"a\t \tb":                         "a   b",
		`"  a "`:                          "  a ",
		`a\t`:                             "a\t",
		`a\ `:                             "a ",
		"a \\":                            "a",
	} {
		if got := gitConfigValue(in); got != want {
			t.Errorf("gitConfigValue(%q) = %q, want %q", in, got, want)
		}
	}
}
