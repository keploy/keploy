//go:build unix

package tls

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"go.uber.org/zap"
)

// TestRunFiles_ThePrivateDirAndTheCAFileAreReadableByAnyUser: whatever the
// umask, the private dir is 0755 and the CA file in it 0644, so an app run as
// another user still reads the CA (a certificate is public), and nobody but
// this user can put anything in the dir.
func TestRunFiles_ThePrivateDirAndTheCAFileAreReadableByAnyUser(t *testing.T) {
	tmp := runFilesTempRoot(t)
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	if err := SetupCaCertEnv(zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	name, _ := privateDirName()
	fi, err := os.Lstat(filepath.Join(tmp, name))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Errorf("the private dir is %v, want a dir of mode 0755", fi.Mode())
	}
	ca := os.Getenv("SSL_CERT_FILE")
	if fi, err := os.Stat(ca); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the CA file %s is %v (%v), want mode 0644", ca, fi, err)
	}
}

// TestRunFiles_ADirAtThePrivateNameAnotherUserCanChangeIsNeverUsed: a link
// (to a dir of this user's, as a planted one would point), a dir its group or
// everyone may write in, and a dir another user owns, at the private dir's
// name: none is used, and none is changed.
func TestRunFiles_ADirAtThePrivateNameAnotherUserCanChangeIsNeverUsed(t *testing.T) {
	for _, c := range []struct {
		name  string
		plant func(t *testing.T, path string)
	}{
		{"a symlink", func(t *testing.T, path string) {
			target := t.TempDir()
			if err := os.Chmod(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if m, _ := filepath.Glob(filepath.Join(target, "*")); len(m) != 0 {
					t.Errorf("the setup made %v in the link's target", m)
				}
			})
		}},
		{"group-writable", func(t *testing.T, path string) { mkdirMode(t, path, 0o775) }},
		{"world-writable", func(t *testing.T, path string) { mkdirMode(t, path, 0o777) }},
		{"sticky world-writable", func(t *testing.T, path string) { mkdirMode(t, path, 0o777|os.ModeSticky) }},
		{"another user's", func(t *testing.T, path string) {
			mkdirMode(t, path, 0o755)
			// As if another uid owned it: this test cannot chown.
			orig := runFilesOwner
			runFilesOwner = func(p string) int {
				if p == path {
					return orig(p) + 1
				}
				return orig(p)
			}
			t.Cleanup(func() { runFilesOwner = orig })
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			setupPastAPlantedPrivateName(t, func(path string) { c.plant(t, path) })
		})
	}
}

func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TestRemoveLegacyTempFiles_LeavesADirAtThePrivateNameThatIsNotOne: `keploy ca
// clean` sweeps and cleans this user's private dir only if it is one. In a
// dir another user can write, or through a link at the name, it removes
// nothing, though run files of dead runs and older names are there.
func TestRemoveLegacyTempFiles_LeavesADirAtThePrivateNameThatIsNotOne(t *testing.T) {
	for _, c := range []struct {
		name  string
		plant func(t *testing.T, path string) string
	}{
		{"world-writable", func(t *testing.T, path string) string { mkdirMode(t, path, 0o777); return path }},
		{"a symlink", func(t *testing.T, path string) string {
			target := t.TempDir()
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			return target
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := runFilesTempRoot(t)
			name, _ := privateDirName()
			in := c.plant(t, filepath.Join(tmp, name))
			files := []string{"keploy-ca-run-5.crt", "keploy-java-truststore-run-6.jks", "keploy-ca-7.crt"}
			for _, f := range files {
				if err := os.WriteFile(filepath.Join(in, f), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			removeLegacyTempFiles(zap.NewNop(), [32]byte{}, true)
			for _, f := range files {
				if !exists(t, filepath.Join(in, f)) {
					t.Errorf("keploy ca clean removed %s in %s, a dir at the private name that is not this user's alone", f, in)
				}
			}
		})
	}
}
