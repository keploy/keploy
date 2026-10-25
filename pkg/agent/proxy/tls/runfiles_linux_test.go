package tls

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// runFileTargetOpens watches path for opens with inotify: an open queues its
// event before the open returns, so once the code under test has returned,
// opened says whether it opened path, with no wait.
type runFileTargetOpens struct{ fd int }

func watchRunFileTarget(t *testing.T, path string) *runFileTargetOpens {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, path, unix.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	return &runFileTargetOpens{fd: fd}
}

func (w *runFileTargetOpens) opened(t *testing.T) bool {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := unix.Read(w.fd, buf)
	if errors.Is(err, unix.EAGAIN) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// TestRunFiles_TheLockNeverOpensThroughALink: a symlink at a run file's name,
// to a file elsewhere (a device, as a root sweep could be pointed at, would be
// armed by the open alone). Opening it for its lock, as a sweep or a new
// run's lock does, refuses the link itself (ELOOP) and never opens its target:
// a check after the open would come too late.
func TestRunFiles_TheLockNeverOpensThroughALink(t *testing.T) {
	dir := runFilesTempDir(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "keploy-ca-run-7.crt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	w := watchRunFileTarget(t, target)
	f, err := openRunFileLock(link)
	if err == nil {
		_ = f.Close()
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Errorf("opening a link for its lock gave %v, want ELOOP", err)
	}
	if removed, err := removeIfDead(link); removed || !errors.Is(err, syscall.ELOOP) {
		t.Errorf("a sweep's take of a link gave %v, %v; want not removed, ELOOP", removed, err)
	}
	if w.opened(t) {
		t.Errorf("the link's target %s was opened", target)
	}
	if !exists(t, link) || !exists(t, target) {
		t.Error("the link or its target is gone")
	}
}

// TestRunFiles_AMountAtThePrivateNameIsNeverUsed: a mount point at the
// private dir's name (a FUSE mount another user made there reports whatever
// owner and mode it likes) is not this user's private dir, however its owner
// and mode look. /proc stands in for it: a dir no one may write, on another
// filesystem than its parent.
func TestRunFiles_AMountAtThePrivateNameIsNeverUsed(t *testing.T) {
	fi, err := os.Lstat("/proc")
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	orig := runFilesOwner
	runFilesOwner = func(p string) int {
		if p == "/proc" {
			return int(st.Uid)
		}
		return orig(p)
	}
	t.Cleanup(func() { runFilesOwner = orig })
	why := checkPrivateDir("/proc")
	if why == nil || !strings.Contains(why.Error(), "mount point") {
		t.Errorf("a mount point passed as a private dir: %v", why)
	}
}
