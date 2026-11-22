//go:build windows

package tls

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/windows"
)

// reparseAt makes link a reparse point to target: a symbolic link where this
// token may make one (an elevated run, or Developer Mode), else a junction,
// which any user may make, to target made a directory. It reports whether it
// made a symbolic link.
func reparseAt(t *testing.T, link, target string) (symlink bool) {
	t.Helper()
	if os.Symlink(target, link) == nil {
		return true
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
	}
	return false
}

// mkdirWithSDDL makes dir with the security sddl gives it.
func mkdirWithSDDL(t *testing.T, dir, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SDDL %q: %v", sddl, err)
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err := windows.CreateDirectory(p, &sa); err != nil {
		t.Fatalf("CreateDirectory %s with %q: %v", dir, sddl, err)
	}
}

func runUserSID(t *testing.T) string {
	t.Helper()
	user, _, err := runUser()
	if err != nil {
		t.Fatal(err)
	}
	return user.String()
}

// The runs directory, or any directory above it, that is a symbolic link or a
// junction is refused, never followed: nothing is made where it points.
func TestCARunLockDirThroughAReparsePointIsRefused(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	reparseAt(t, link, real)
	for _, dir := range []string{link, filepath.Join(link, "ca-runs")} {
		err := makeCARunsDir(dir)
		if err == nil || !strings.Contains(err.Error(), "reparse point") {
			t.Errorf("makeCARunsDir(%s) = %v, want it refused as a reparse point", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(real, "ca-runs")); !os.IsNotExist(err) {
		t.Errorf("a directory was made where the junction points (%v), want none", err)
	}
}

// A lock file that is a symbolic link or a junction is refused, never
// followed: a run takes no lock through it, creates nothing where it points,
// and says it changes the stores without the lock.
func TestStoreLockThroughALinkIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	if err := makeCARunsDir(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere", "planted.lock")
	symlink := reparseAt(t, filepath.Join(dir, storeLockName), target)
	if f, err := openLockFile(filepath.Join(dir, storeLockName), os.O_RDONLY); err == nil {
		_ = f.Close()
		t.Fatal("openLockFile opened a lock file that is a link, want it refused")
	} else if !strings.Contains(err.Error(), "reparse point") {
		t.Errorf("openLockFile = %v, want it refused as a reparse point", err)
	}
	old := caRunsDir
	caRunsDir = dir
	t.Cleanup(func() { caRunsDir = old; storeLockWarned.Store(false) })
	storeLockWarned.Store(false)
	core, logs := observer.New(zapcore.WarnLevel)
	unlock, err := lockStores(context.Background(), zap.New(core))
	if err != nil {
		t.Fatalf("lockStores = %v, want the change to go on without the lock", err)
	}
	unlock()
	if logs.FilterMessageSnippet("could not take the trust stores' lock").Len() != 1 {
		t.Errorf("warnings %v, want one saying the stores change without their lock", logs.All())
	}
	if symlink {
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Errorf("the link's target was made (%v), want nothing made where it points", err)
		}
	}
}

// A run's lock file that is a link counts as a live run's: a sweep leaves its
// entries, and does not open what the link points to.
func TestCARunLockFileThatIsALinkCountsAsLive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	if err := makeCARunsDir(dir); err != nil {
		t.Fatal(err)
	}
	reparseAt(t, filepath.Join(dir, "c0ffee01.4242.lock"), filepath.Join(t.TempDir(), "unlocked.lock"))
	old := caRunsDir
	caRunsDir = dir
	t.Cleanup(func() { caRunsDir = old })
	if !caRunLive("c0ffee01") {
		t.Error("a run whose lock file is a link counts as ended, want live")
	}
}

// Who may own the runs directory, and change it: the administrators or
// SYSTEM, where other users may add files and nothing more, or this run's
// user alone.
func TestCARunLockDirOwnersAndRights(t *testing.T) {
	u := runUserSID(t)
	user, err := windows.StringToSid(u)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, sddl string
		ok         bool
	}{
		{"what a run that is no administrator makes", caRunsDirSDDL(u, false), true},
		{"the administrators', users may add files (as %ProgramData% gives)",
			"O:BAD:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)(A;CI;DCLCRPCR;;;BU)", true},
		{"SYSTEM's", "O:SYD:P(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)", true},
		{"the administrators', this user may change it (an elevated run's, in its own temp directory)",
			"O:BAD:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;FA;;;" + u + ")", true},
		{"this user's, others denied", "O:" + u + "D:P(D;;FA;;;WD)(A;OICI;FA;;;" + u + ")", true},
		{"this user's, others given rights only on what is made in it",
			"O:" + u + "D:P(A;OICI;FA;;;" + u + ")(A;OICIIO;FA;;;WD)", true},
		{"this user's, everyone may change it", "O:" + u + "D:P(A;OICI;FA;;;" + u + ")(A;OICI;FA;;;WD)", false},
		{"this user's, users may add files", "O:" + u + "D:P(A;OICI;FA;;;" + u + ")(A;CI;DCLCRPCR;;;BU)", false},
		{"the administrators', users may delete what is in it", "O:BAD:P(A;OICI;FA;;;BA)(A;CI;0x40;;;BU)", false},
		{"the administrators', users may change its access", "O:BAD:P(A;OICI;FA;;;BA)(A;;WD;;;BU)", false},
		{"another user's", "O:S-1-5-21-1-2-3-1001D:P(A;OICI;FA;;;S-1-5-21-1-2-3-1001)", false},
		{"no access list", "O:" + u + "D:NO_ACCESS_CONTROL", false},
		{"an entry of a kind keploy does not read",
			"O:" + u + "D:P(A;OICI;FA;;;" + u + ")(OA;;CC;bf967aba-0de6-11d0-a285-00aa003049e2;;WD)", false},
	} {
		sd, err := windows.SecurityDescriptorFromString(row.sddl)
		if err != nil {
			t.Fatalf("%s: SDDL %q: %v", row.name, row.sddl, err)
		}
		if err := checkDirSD(sd, `C:\runs`, user); (err == nil) != row.ok {
			t.Errorf("%s (%s): checkDirSD = %v, want ok %v", row.name, row.sddl, err, row.ok)
		}
	}
	if got := caRunsDirSDDL(u, true); got != "O:BA" {
		t.Errorf("an administrator's run makes the directory with %q, want the administrators' (O:BA), access inherited", got)
	}
}

// A runs directory every user can change takes no lock, and the run says the
// stores change without it; one this run's user alone can change takes it.
func TestStoreLockInADirectoryOthersCanChange(t *testing.T) {
	u := runUserSID(t)
	for _, row := range []struct {
		name, sddl string
		locked     bool
	}{
		{"everyone may change it", "O:" + u + "D:P(A;OICI;FA;;;" + u + ")(A;OICI;FA;;;WD)", false},
		{"this user's alone", caRunsDirSDDL(u, false), true},
	} {
		dir := filepath.Join(t.TempDir(), "runs")
		mkdirWithSDDL(t, dir, row.sddl)
		old := caRunsDir
		caRunsDir = dir
		storeLockWarned.Store(false)
		core, logs := observer.New(zapcore.WarnLevel)
		unlock, err := lockStores(context.Background(), zap.New(core))
		caRunsDir = old
		if err != nil {
			t.Fatalf("%s: lockStores = %v", row.name, err)
		}
		_, statErr := os.Stat(filepath.Join(dir, storeLockName))
		unlock()
		warned := logs.FilterMessageSnippet("could not take the trust stores' lock").Len() == 1
		if warned == row.locked || (statErr == nil) != row.locked {
			t.Errorf("%s: warned %v, lock file made %v (%v); want the lock taken %v", row.name, warned, statErr == nil, statErr, row.locked)
		}
	}
	storeLockWarned.Store(false)
}

// A directory in a lock file's place is refused, a lock file another run
// removed is not made again by a sweep's look at it, and a runs directory
// named by no absolute path is not used.
func TestCARunLockFileMustBeARegularFileInAnAbsoluteDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	if err := makeCARunsDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, storeLockName), 0o755); err != nil {
		t.Fatal(err)
	}
	if f, err := openLockFile(filepath.Join(dir, storeLockName), os.O_RDONLY); err == nil {
		_ = f.Close()
		t.Error("openLockFile opened a directory as the lock file, want it refused")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("openLockFile = %v, want it refused as no regular file", err)
	}
	gone := filepath.Join(dir, "c0ffee01.4242.lock")
	if f, err := openRunLockToTest(gone); err == nil {
		_ = f.Close()
		t.Error("openRunLockToTest opened a lock file that is not there, want an error")
	}
	if _, err := os.Lstat(gone); !os.IsNotExist(err) {
		t.Errorf("a sweep's look made the lock file it tested (%v), want nothing made", err)
	}
	for _, rel := range []string{`runs\ca-runs`, `C:runs\ca-runs`} {
		if err := makeCARunsDir(rel); err == nil || !strings.Contains(err.Error(), "not an absolute path") {
			t.Errorf("makeCARunsDir(%s) = %v, want it refused as no absolute path", rel, err)
		}
	}
}

// A run that is no administrator makes the runs directory its user's alone,
// and only that directory,
// whatever the directory above it would give: here every user may change
// what is made in it. An administrator's run makes it the administrators',
// with what the directory above gives, which here lets every user change it,
// so it is refused.
func TestCARunLockDirAWorldWritableParentGives(t *testing.T) {
	u := runUserSID(t)
	user, err := windows.StringToSid(u)
	if err != nil {
		t.Fatal(err)
	}
	for _, admin := range []bool{false, true} {
		parent := filepath.Join(t.TempDir(), "open")
		mkdirWithSDDL(t, parent, "O:"+u+"D:P(A;OICI;FA;;;"+u+")(A;OICI;FA;;;WD)")
		mid := filepath.Join(parent, "mid")
		h, err := openCARunsDirAs(filepath.Join(mid, "runs"), true, user, admin)
		if err == nil {
			_ = windows.CloseHandle(h)
		}
		if (err == nil) == admin {
			t.Errorf("an administrator's run %v: openCARunsDirAs = %v, want it made and used %v", admin, err, !admin)
		}
		// Only the runs directory is made so: a directory above it that a
		// run makes inherits its access, as os.MkdirAll's does.
		sd, err := windows.GetNamedSecurityInfo(mid, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if ctl, _, err := sd.Control(); err != nil || ctl&windows.SE_DACL_PROTECTED != 0 {
			t.Errorf("an administrator's run %v: the directory above the runs directory was made with its own access (%v), want it inherited", admin, err)
		}
	}
}
