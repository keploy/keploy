package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
)

// resolvedTempDir is a new folder, its links resolved, as keploy names a
// project (keploytest's realPath).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	r, err := filepath.EvalSymlinks(d)
	require.NoError(t, err)
	return r
}

// symlink makes link point at target, or skips the test where the system
// lets this process make no link (Windows without the privilege).
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
}

// The key's folder is below the project, the folder keploy.yml was read
// from, its links resolved as keploytest resolves the folder it claims with
// (macOS's /var is a link to /private/var), and the working folder when the
// run names none. The key is joined as it is written: a key that is a link
// keeps its own name, whether it points inside the project or out of it, as
// the record names a linked folder of the repository, so the folder stays the
// one the test's ID names and never becomes another folder's set; a folder
// the key names that is not there is named the same way. Mutation: leave the
// project unresolved, and the linked project's rows fail; resolve the key's
// folder, and the linked keys' rows fail; read the working folder always,
// and the named rows fail.
func TestKeyFolderIsBelowTheResolvedProject(t *testing.T) {
	target := resolvedTempDir(t)
	outside := resolvedTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(target, "lib"), 0o755))
	link := filepath.Join(t.TempDir(), "project")
	symlink(t, target, link)
	symlink(t, filepath.Join(target, "lib"), filepath.Join(target, "shared"))
	symlink(t, outside, filepath.Join(target, "escape"))
	wd := resolvedTempDir(t)
	t.Chdir(wd)
	named := &Agent{config: &config.Config{ConfigPath: link}}
	require.Equal(t, target, named.keyFolder("."), "a linked project")
	require.Equal(t, filepath.Join(target, "lib"), named.keyFolder("lib"), "a folder in a linked project")
	require.Equal(t, filepath.Join(target, "shared"), named.keyFolder("shared"), "a key that links inside the project keeps its name")
	require.Equal(t, filepath.Join(target, "escape"), named.keyFolder("escape"), "a key that links out of the project keeps its name")
	require.Equal(t, filepath.Join(target, "gone", "x"), named.keyFolder("gone/x"), "a folder that is not there")
	require.Equal(t, filepath.Join(wd, "tests"), (&Agent{config: &config.Config{}}).keyFolder("tests"), "no config folder: the working folder's")
	require.Equal(t, wd, (&Agent{}).keyFolder("."), "no config: the working folder")
}
