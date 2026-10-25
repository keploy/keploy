//go:build windows

package tls

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// runFileLocking: LockFileEx locks belong to the handle, and Windows drops
// them when the process ends, however it ends.
const runFileLocking = true

// runFileLockOffset is where the lock sits: far past the end of any run file.
// A Windows byte-range lock is mandatory, so one over the file's bytes would
// stop the app reading them; one past its end takes nothing from a reader.
const runFileLockOffset = 0x7fff_ffff_0000_0000

// privateDirName is the name of this user's private dir for run files in the
// temp dir: keploy-<the user's SID>. The temp dir is already the user's own
// (%LOCALAPPDATA%\Temp) unless keploy runs as a service account.
func privateDirName() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return "keploy-" + u.User.Sid.String(), nil
}

// checkPrivateDirOwner returns why the dir at path is not this user's: its
// owner is neither this user nor an identity that already controls the user
// (Administrators, which an elevated run's new files belong to, or SYSTEM).
// The dir takes its ACL from the temp dir it is made in, so who may write in
// it is not checked here.
func checkPrivateDirOwner(path string, _ os.FileInfo) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("its owner cannot be read: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("its owner cannot be read: %v", err)
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if owner.Equals(u.User.Sid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) || owner.IsWellKnown(windows.WinLocalSystemSid) {
		return nil
	}
	return fmt.Errorf("it is owned by %s, not by this user (%s) or an administrator", owner, u.User.Sid)
}

// setPrivateDirMode: the dir takes the temp dir's ACL; a Unix mode means
// nothing here.
func setPrivateDirMode(string) error { return nil }

// openRunFileLock opens the run file at path for its lock: read-only, sharing
// read, write and delete, so it takes nothing from an app that reads the file
// and the file can be removed while it is open, and never through a symlink.
func openRunFileLock(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("%s: %w", path, errNotRegular)
		}
		return nil, err
	}
	return f, nil
}

// isSymlinkRefused: openRunFileLock opens a symlink itself, and refuses it
// as errNotRegular.
func isSymlinkRefused(error) bool { return false }

// isInUse reports whether a remove failed because a handle that refuses
// delete has the file open: the creating run's own, in the instant before its
// lock.
func isInUse(err error) bool { return errors.Is(err, windows.ERROR_SHARING_VIOLATION) }

// tryLockRunFile takes the exclusive lock on f without waiting: false when
// another handle of the file, in this process or another, holds it.
func tryLockRunFile(f *os.File) (bool, error) {
	ol := windows.Overlapped{Offset: uint32(runFileLockOffset & 0xffff_ffff), OffsetHigh: uint32(runFileLockOffset >> 32)}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}
