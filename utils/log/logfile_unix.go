//go:build !windows

package log

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// oNoFollow makes an open of the log refuse a symlink at its name, and
// oNonBlock keeps it from waiting forever on a named pipe planted there.
const (
	oNoFollow = syscall.O_NOFOLLOW
	oNonBlock = syscall.O_NONBLOCK
)

// openLogFile opens LogFileName in the working directory for this run to
// append to. A file there that is not safely this run's (see LogFileName) is
// refused -- nil, and why -- and the run logs to the console; a log that
// cannot be opened at all is an error, as it always was.
func openLogFile() (*os.File, string, error) {
	const flags = os.O_APPEND | os.O_CREATE | os.O_WRONLY | oNoFollow | oNonBlock
	// Created by this open, or already there: told by the open itself
	// (O_EXCL), not by a look beforehand that a file appearing or going in
	// between would make wrong.
	f, err := os.OpenFile(LogFileName, flags|os.O_EXCL, logFileMode)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(LogFileName, flags, logFileMode)
	}
	if err != nil {
		switch {
		case errors.Is(err, syscall.ELOOP):
			return nil, "it is a symbolic link", nil
		case errors.Is(err, syscall.ENXIO):
			return nil, "it is a named pipe or a socket", nil
		}
		return nil, "", err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		_ = f.Close()
		return nil, "cannot tell who owns it", nil
	}
	switch {
	case !fi.Mode().IsRegular():
		_ = f.Close()
		return nil, "it is not a regular file", nil
	case uint64(st.Nlink) != 1:
		_ = f.Close()
		return nil, fmt.Sprintf("it has %d hard links, so it is also some other file", st.Nlink), nil
	case !mayKeepLog(st.Uid):
		_ = f.Close()
		return nil, fmt.Sprintf("it belongs to another user (uid %d); remove it to keep a log here", st.Uid), nil
	}
	if created && os.Geteuid() == 0 {
		// Root made it: it is the log of whoever root runs for, so a later run
		// of theirs, without sudo, can write it too.
		if uid, gid, ok := logOwnerForRoot(); ok {
			_ = f.Chown(uid, gid)
		}
	}
	// A log an older keploy left 0777 is narrowed, through the open file,
	// never by name; one already stricter than logFileMode is left as it is.
	if perm := fi.Mode().Perm(); perm&^logFileMode != 0 {
		_ = f.Chmod(perm & logFileMode)
	}
	return f, "", nil
}

// mayKeepLog reports whether a log owned by uid is this run's to write: the
// user running keploy's, the user who ran sudo's, or the directory owner's.
func mayKeepLog(uid uint32) bool {
	if int(uid) == os.Geteuid() {
		return true
	}
	if os.Geteuid() == 0 {
		if sudoUID, _, ok := sudoInvoker(); ok && uid == uint32(sudoUID) {
			return true
		}
	}
	dirUID, _, ok := dirOwner()
	return ok && uid == uint32(dirUID)
}

// logOwnerForRoot is who a log root creates should belong to: the user who
// ran sudo, or else the owner of the directory when that is not root.
func logOwnerForRoot() (uid, gid int, ok bool) {
	if uid, gid, ok := sudoInvoker(); ok {
		return uid, gid, true
	}
	if uid, gid, ok := dirOwner(); ok && uid != 0 {
		return uid, gid, true
	}
	return 0, 0, false
}

// sudoInvoker is the user who ran sudo, from the variables sudo sets.
func sudoInvoker() (uid, gid int, ok bool) {
	u, uerr := strconv.Atoi(os.Getenv("SUDO_UID"))
	g, gerr := strconv.Atoi(os.Getenv("SUDO_GID"))
	if uerr != nil || gerr != nil || u < 0 || g < 0 {
		return 0, 0, false
	}
	return u, g, true
}

// dirOwner is the owner of the working directory.
func dirOwner() (uid, gid int, ok bool) {
	fi, err := os.Stat(".")
	if err != nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
