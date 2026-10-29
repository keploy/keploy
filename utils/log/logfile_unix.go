//go:build !windows

package log

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
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
	f, err := openFile(path, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
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
	gid := -1
	if g, err := strconv.ParseUint(os.Getenv("SUDO_GID"), 10, 32); err == nil {
		gid = int(g)
	}
	if err := chownFile(f, int(uid), gid); err != nil {
		return fmt.Errorf("giving %s to uid %d: %w", f.Name(), uid, err)
	}
	return nil
}
