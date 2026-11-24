//go:build !windows

package log

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// oNoFollow makes ReadLogFile's open refuse a symlink at the log's name, and
// oNonBlock keeps it from waiting forever on a named pipe put there.
const (
	oNoFollow = syscall.O_NOFOLLOW
	oNonBlock = syscall.O_NONBLOCK
)

// openClientLogFile opens path, the log the CLI that started this agent named
// (ClientLogFileFlag), to append to. A root agent writes it, so it takes only
// a file the user who started it could write anyway, each check made once:
//   - the name is a regular file that already exists: nothing is created, and
//     a link, a pipe or a device is not even opened, as opening some devices
//     acts;
//   - it is opened without following a link, which a name swapped since
//     would be;
//   - the file opened is a regular file and belongs to that user
//     (invokingUID).
func openClientLogFile(path string) (*os.File, error) {
	want, err := invokingUID()
	if err != nil {
		return nil, err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("the CLI's log: %w", err)
	}
	if !named.Mode().IsRegular() {
		return nil, fmt.Errorf("the CLI's log %s is not a regular file (%v)", path, named.Mode().Type())
	}
	// Read-write, though the agent only appends: the shared lock that marks
	// the log live (markLiveLog) needs read access where a network file
	// system emulates flock with fcntl's locks.
	f, err := openFile(path, os.O_RDWR|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("the CLI's log: %w", err)
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("the CLI's log %s is not a regular file (%v)", path, fi.Mode().Type())
	}
	if err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok {
			err = fmt.Errorf("the CLI's log %s: no owner to check", path)
		} else if st.Uid != want {
			err = fmt.Errorf("the CLI's log %s belongs to uid %d, not to the user that started this agent (uid %d)", path, st.Uid, want)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// invokingUID is the user that started this agent: the one sudo ran it for
// (SUDO_UID), else the agent's own, which is the CLI's when no sudo stands
// between them.
func invokingUID() (uint32, error) {
	s := os.Getenv("SUDO_UID")
	if s == "" {
		return uint32(os.Getuid()), nil
	}
	uid, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("SUDO_UID %q is not a uid: %w", s, err)
	}
	return uint32(uid), nil
}

// chownFile is (*os.File).Chown, fchown(2) on the descriptor; a test swaps in
// one that records the call.
var chownFile = (*os.File).Chown

// giveLogToSudoUser hands f, a log this run has just created, to the user sudo
// ran keploy for (SUDO_UID, and SUDO_GID's group where it is set), so the log
// the run names is that user's to read, and the native agent, which appends
// only to that user's file (openClientLogFile), appends to it: one run, one
// file. It changes the descriptor's file, never a name, which a link swapped
// in could send elsewhere; mode 0600 stays. Without SUDO_UID the file is
// already the user's, and nothing is changed.
func giveLogToSudoUser(f *os.File) error {
	s := os.Getenv("SUDO_UID")
	if s == "" {
		return nil
	}
	uid, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return fmt.Errorf("SUDO_UID %q is not a uid: %w", s, err)
	}
	// Chown takes an int, which is 32 bits on 386 and arm, and systemd tells
	// distributions to avoid ids from 2^31 up, since tools read uid_t as
	// signed. So keploy hands a log only to a uid below 2^31, the same on
	// every arch. That also refuses 4294967295, which fchown reads as -1,
	// "keep the owner": the log would stay root's with nothing said.
	if uid > math.MaxInt32 {
		return fmt.Errorf("SUDO_UID %d is past the largest uid keploy hands a file to (%d)", uid, math.MaxInt32)
	}
	gid := -1
	if g, err := strconv.ParseUint(os.Getenv("SUDO_GID"), 10, 32); err == nil && g <= math.MaxInt32 {
		gid = int(g)
	}
	if err := chownFile(f, int(uid), gid); err != nil {
		return fmt.Errorf("giving %s to uid %d: %w", f.Name(), uid, err)
	}
	return nil
}

// markLiveLog marks f, a live run's log, as live: a shared flock(2) on its
// descriptor, which goes with the descriptor, when the run closes its log or
// the process ends, killed or not. A log so marked is never pruned
// (removeDeadLog). It reports whether f is marked: a file system that takes
// no flock leaves it unmarked.
func markLiveLog(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil
}

// logOwnedByRunUser says whether fi, a kept log, is a regular file of the user
// keploy runs for (invokingUID): the one sudo ran it for, whose logs a run
// under sudo hands them (giveLogToSudoUser), else keploy's own. A shared temp
// directory holds other users' logs, which are not this run's to prune.
func logOwnedByRunUser(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() {
		return false
	}
	uid, err := invokingUID()
	return err == nil && st.Uid == uid
}

// removeDeadLog removes name, a kept log that was fi when listed, only if no
// live run has it marked (markLiveLog): it opens the name without following a
// link or waiting on a pipe, checks that what it opened is still fi, the
// user's regular file, and not written to lately, and takes an exclusive
// flock, which a live run's shared one refuses, holding it while it removes
// the name, only while the name is still the file it opened (removeIfStill).
// It opens the file to write as well as read where it may: where flock is
// emulated by fcntl locks (NFS), an exclusive lock needs it.
func removeDeadLog(name string, fi os.FileInfo) error {
	const at = syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_NOCTTY
	f, err := os.OpenFile(name, os.O_RDWR|at, 0)
	if errors.Is(err, fs.ErrPermission) {
		// A full log is sealed read-only (logFileRoom). Read-only, the lock
		// works where flock is the kernel's own; where it is emulated by
		// fcntl locks (NFS) it is refused, and the log stays.
		f, err = os.OpenFile(name, os.O_RDONLY|at, 0)
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(fi, opened) || !logOwnedByRunUser(opened) || time.Since(opened.ModTime()) < pruneQuiet {
		return fmt.Errorf("%s is no longer the log that was listed, or was written to since", name)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%s is a live run's log: %w", name, err)
	}
	return removeIfStill(name, opened)
}

// stampLog sets f's times to t, through its descriptor: nothing is looked up
// by its name, which another may have changed.
func stampLog(f *os.File, t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Futimes(int(f.Fd()), []unix.Timeval{tv, tv})
}
