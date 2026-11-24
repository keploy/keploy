package log

import (
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has neither O_NOFOLLOW nor O_NONBLOCK. ReadLogFile's identity check
// (os.SameFile against the file this run created) still refuses whatever
// replaced the log at its name.
const (
	oNoFollow = 0
	oNonBlock = 0
)

// openClientLogFile opens path, the log the CLI that started this agent named
// (ClientLogFileFlag), to append to: only a regular file that already exists,
// no link, and the one that name was when it was checked. The agent runs as
// the CLI's user here (no sudo stands between them), so there is no owner to
// check.
func openClientLogFile(path string) (*os.File, error) {
	named, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("the CLI's log: %w", err)
	}
	if !named.Mode().IsRegular() {
		return nil, fmt.Errorf("the CLI's log %s is not a regular file (%v)", path, named.Mode().Type())
	}
	f, err := openFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return nil, fmt.Errorf("the CLI's log: %w", err)
	}
	opened, err := f.Stat()
	if err == nil && (!opened.Mode().IsRegular() || !os.SameFile(named, opened)) {
		err = fmt.Errorf("the CLI's log %s was replaced while it was opened", path)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// giveLogToSudoUser does nothing: there is no sudo, and the log is the
// user's own.
func giveLogToSudoUser(*os.File) error { return nil }

// markLiveLog marks nothing, and reports f marked: Windows has no flock, and
// a live run's log is kept from the pruner by the open itself. Go opens a
// file without FILE_SHARE_DELETE, so another process cannot remove it while
// it is open (removeDeadLog).
func markLiveLog(*os.File) bool { return true }

// logOwnedByRunUser is true for any regular file: keploy on Windows does not
// compare owners. The temp directory is usually the user's own (%TEMP%);
// where it is shared (a service's C:\Windows\Temp, or a TMP a script set),
// another account's dead keploy log can be pruned too, never a live one, which
// Windows will not let another process remove (removeDeadLog).
func logOwnedByRunUser(fi os.FileInfo) bool { return fi.Mode().IsRegular() }

// removeDeadLog removes name, a kept log that was fi when listed, only if no
// live run holds it open. It opens the name for deletion, sharing everything:
// a live run opened its log without FILE_SHARE_DELETE (Go never asks for it),
// so the open is refused while the run has it, and nothing about the file is
// changed. os.Remove would not do: where its first delete fails it clears the
// file's read-only attribute and tries again, and a full log is sealed so
// (logFileRoom), so it would strip a live log's seal. Holding the file, it
// checks it is a regular file no one has written to lately (fi, from a
// listing, names the same path: on Windows its identity is read from the
// path, so the check is against the file at the name now, the one held),
// clears a dead sealed log's read-only attribute, and deletes it through the
// handle.
func removeDeadLog(name string, fi os.FileInfo) error {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fmt.Errorf("%s is a live run's log, or cannot be opened to delete: %w", name, err)
	}
	f := os.NewFile(uintptr(h), name)
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(fi, opened) || !opened.Mode().IsRegular() || time.Since(opened.ModTime()) < pruneQuiet {
		return fmt.Errorf("%s is no longer the log that was listed, or was written to since", name)
	}
	var held windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &held); err != nil {
		return err
	}
	if held.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		if err := setFileAttributes(h, held.FileAttributes&^windows.FILE_ATTRIBUTE_READONLY); err != nil {
			return err
		}
	}
	del := fileDispositionInfo{deleteFile: true}
	if err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&del)), uint32(unsafe.Sizeof(del))); err != nil {
		if held.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
			_ = setFileAttributes(h, held.FileAttributes)
		}
		return err
	}
	return nil
}

// fileBasicInfo is FILE_BASIC_INFO, and fileDispositionInfo
// FILE_DISPOSITION_INFO, for SetFileInformationByHandle.
type fileBasicInfo struct {
	creationTime, lastAccessTime, lastWriteTime, changeTime int64
	fileAttributes                                          uint32
	_                                                       uint32
}

type fileDispositionInfo struct {
	deleteFile bool
}

// setFileAttributes sets the attributes of the file h holds, its times left
// as they are (0); none is FILE_ATTRIBUTE_NORMAL, as 0 there means "unchanged".
func setFileAttributes(h windows.Handle, attrs uint32) error {
	if attrs == 0 {
		attrs = windows.FILE_ATTRIBUTE_NORMAL
	}
	info := fileBasicInfo{creationTime: 0, lastAccessTime: 0, lastWriteTime: 0, changeTime: 0, fileAttributes: attrs}
	return windows.SetFileInformationByHandle(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}

// stampLog sets f's times to t, through its handle: nothing is looked up by
// its name, which another may have changed. A log is opened for writing, which
// includes the right to set its times.
func stampLog(f *os.File, t time.Time) error {
	ft := windows.NsecToFiletime(t.UnixNano())
	return windows.SetFileTime(windows.Handle(f.Fd()), nil, &ft, &ft)
}
