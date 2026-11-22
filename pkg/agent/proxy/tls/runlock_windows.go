//go:build windows

package tls

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// defaultCARunsDir is under %ProgramData%, which every user of the host can
// read and add files to: a run's per-user CA alias sits in a JDK keystore other
// users' runs sweep too, so they must see its lock. A lock file another user
// cannot open counts as live (runLive).
func defaultCARunsDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "Keploy", "ca-runs")
}

// makeCARunsDir makes dir, this host's runs directory, when it is missing, and
// checks it as every lock file's open does (openCARunsDir).
func makeCARunsDir(dir string) error {
	h, err := openCARunsDir(dir, true)
	if err != nil {
		return err
	}
	_ = windows.CloseHandle(h)
	return nil
}

// openLockFile opens path, a lock file in the runs directory, creating it
// when missing, and returns it only when it is a regular file opened through
// no link: the file is opened in the directory openCARunsDir opened, by its
// handle, never by a path another user can change in between, and a reparse
// point (a symbolic link or a junction) in its place is opened itself, not
// followed (FILE_OPEN_REPARSE_POINT), and refused, as is a directory. A link
// there would make the run create or lock a file wherever the link points.
// A file with more than one name (a hard link) is refused too: its name in
// the runs directory may be one another user added to a file kept elsewhere.
// It is opened for reading whatever flag asks: LockFileEx locks a file opened
// for reading, and no run writes a lock file.
func openLockFile(path string, _ int) (*os.File, error) {
	return openLockFileIn(path, windows.FILE_OPEN_IF)
}

// openRunLockToTest opens path, another run's lock file, to test its lock
// (caRunLive): as openLockFile does, but never creating it.
func openRunLockToTest(path string) (*os.File, error) {
	return openLockFileIn(path, windows.FILE_OPEN)
}

func openLockFileIn(path string, disposition uint32) (*os.File, error) {
	dir, err := openCARunsDir(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(dir) }()
	// The share mode os.OpenFile gives: a run's lock file cannot be deleted
	// while the run holds it open.
	h, err := openAt(dir, filepath.Base(path), windows.GENERIC_READ|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, disposition, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("the lock file %s cannot be opened: %w", path, err)
	}
	fi, err := notAReparsePoint(h, path, false)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	if fi.NumberOfLinks != 1 {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("the lock file %s has %d names (a hard link), which keploy does not open", path, fi.NumberOfLinks)
	}
	return os.NewFile(uintptr(h), path), nil
}

// openCARunsDir opens dir, the runs directory, making it when create is set
// and it is missing, as no other user can redirect or change it, by
// construction:
//   - every part of its path is opened from the handle of the part above it,
//     from the volume's root, and none may be a reparse point (a symbolic
//     link or a junction): a part another user made into a link cannot send
//     the run, or the files it creates there, elsewhere;
//   - the directory is an administrator's (made by an elevated run, an
//     installer or an administrator), where every other user may at most add
//     files, as %ProgramData% lets them, and none may delete, rename or
//     re-permission another's; or it is this run's user's own, which no other
//     user can change at all (dirOwnersOnly).
//
// A directory it makes is made so: an elevated run's is the
// administrators', its access inherited from the directory above it; any
// other run's is this user's alone (caRunsDirSDDL).
func openCARunsDir(dir string, create bool) (windows.Handle, error) {
	user, admin, err := runUser()
	if err != nil {
		return 0, err
	}
	return openCARunsDirAs(dir, create, user, admin)
}

// openCARunsDirAs is openCARunsDir for a run of user, an administrator's when
// admin is set.
func openCARunsDirAs(dir string, create bool, user *windows.SID, admin bool) (windows.Handle, error) {
	var err error
	var sd *windows.SECURITY_DESCRIPTOR
	if create {
		if sd, err = windows.SecurityDescriptorFromString(caRunsDirSDDL(user.String(), admin)); err != nil {
			return 0, err
		}
	}
	dir = filepath.Clean(dir)
	vol := filepath.VolumeName(dir)
	if vol == "" || !filepath.IsAbs(dir) {
		return 0, fmt.Errorf("the directory of keploy runs' locks %s is not an absolute path", dir)
	}
	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(root, windows.FILE_TRAVERSE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, fmt.Errorf("the volume of %s cannot be opened: %w", dir, err)
	}
	parts := strings.FieldsFunc(dir[len(vol):], func(r rune) bool { return r == '\\' || r == '/' })
	for i, part := range parts {
		last := i == len(parts)-1
		access := uint32(windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
		disposition, partSD := uint32(windows.FILE_OPEN), (*windows.SECURITY_DESCRIPTOR)(nil)
		if last {
			access |= windows.FILE_LIST_DIRECTORY | windows.READ_CONTROL
		}
		if create {
			disposition = windows.FILE_OPEN_IF
			if last {
				partSD = sd
			}
		}
		next, err := openAt(h, part, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			disposition, windows.FILE_DIRECTORY_FILE, partSD)
		_ = windows.CloseHandle(h)
		at := vol + `\` + strings.Join(parts[:i+1], `\`)
		if err != nil {
			return 0, fmt.Errorf("%s cannot be opened as a directory: %w", at, err)
		}
		h = next
		if _, err := notAReparsePoint(h, at, true); err != nil {
			_ = windows.CloseHandle(h)
			return 0, err
		}
	}
	if err := dirOwnersOnly(h, dir, user); err != nil {
		_ = windows.CloseHandle(h)
		return 0, err
	}
	return h, nil
}

// openAt opens name in the directory dir, by dir's handle, with
// FILE_OPEN_REPARSE_POINT: a reparse point there is opened itself, never
// followed. sd is the security a file it creates gets (nil: inherited).
func openAt(dir windows.Handle, name string, access, share, disposition, options uint32, sd *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	uname, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: uname, Attributes: windows.OBJ_CASE_INSENSITIVE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access, &oa, &iosb, nil, windows.FILE_ATTRIBUTE_NORMAL, share, disposition,
		options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return 0, err
	}
	return h, nil
}

// notAReparsePoint checks, on h, the handle of what was opened at path, that
// it is no reparse point, and a directory when dir is set, a file otherwise.
// It returns what it read of h, for the caller's own checks.
func notAReparsePoint(h windows.Handle, path string, dir bool) (windows.ByHandleFileInformation, error) {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return fi, fmt.Errorf("%s cannot be asked what it is: %w", path, err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fi, fmt.Errorf("%s is a symbolic link or a junction (a reparse point), which keploy does not follow", path)
	}
	if isDir := fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0; isDir != dir {
		if dir {
			return fi, fmt.Errorf("%s is not a directory", path)
		}
		return fi, fmt.Errorf("the lock file %s is not a regular file", path)
	}
	return fi, nil
}

// runUser is this run's user, and whether it runs as an administrator (its
// token holds the Administrators group enabled: elevated).
func runUser() (user *windows.SID, admin bool, err error) {
	t := windows.GetCurrentProcessToken()
	tu, err := t.GetTokenUser()
	if err != nil {
		return nil, false, fmt.Errorf("this run's user cannot be read: %w", err)
	}
	ba, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, false, err
	}
	admin, err = t.IsMember(ba)
	if err != nil {
		return nil, false, err
	}
	return tu.User.Sid, admin, nil
}

// caRunsDirSDDL is the security a runs directory a run makes gets: an
// administrator's run makes it the administrators' (O:BA), its access
// inherited from the directory above it, as %ProgramData% gives every user
// the right to add files and none to delete another's; any other run makes it
// its user's alone (and SYSTEM's and the administrators'), inheriting nothing.
func caRunsDirSDDL(user string, admin bool) string {
	if admin {
		return "O:BA"
	}
	return fmt.Sprintf("O:%sD:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", user, user)
}

// dirChangeRights are the rights on a directory that let a user delete,
// rename or replace what is in it, or change who may: no user but its owner,
// SYSTEM and the administrators may hold one. dirAddRights let a user add a
// file or a directory: in a directory an administrator owns, any user may
// (%ProgramData% gives them), in one a user owns, only its owner.
const (
	dirChangeRights = windows.DELETE | fileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_WRITE | windows.GENERIC_ALL
	dirAddRights = fileAddFile | fileAddSubdirectory

	// The directory rights winnt.h names that x/sys/windows does not.
	fileAddFile         = 0x2
	fileAddSubdirectory = 0x4
	fileDeleteChild     = 0x40
)

// dirOwnersOnly checks the directory h (at path) is one no other user than
// user, the run's, can change (openCARunsDir): owned by the administrators or
// SYSTEM, or by user, with no allow entry that gives another user a right in
// dirChangeRights, nor, in one user owns, in dirAddRights. An entry of a kind
// it does not read refuses the directory.
func dirOwnersOnly(h windows.Handle, path string, user *windows.SID) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("the access of %s cannot be read: %w", path, err)
	}
	return checkDirSD(sd, path, user)
}

func checkDirSD(sd *windows.SECURITY_DESCRIPTOR, path string, user *windows.SID) error {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("the owner of %s cannot be read: %v", path, err)
	}
	byAdmin := isAdminSID(owner)
	if !byAdmin && !owner.Equals(user) {
		return fmt.Errorf("%s is owned by %s, neither this run's user nor the administrators", path, owner)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("%s has no access list, so every user can change it", path)
	}
	denied := windows.ACCESS_MASK(dirChangeRights)
	if !byAdmin {
		denied |= dirAddRights
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("the access list of %s cannot be read: %w", path, err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return fmt.Errorf("the access list of %s has an entry of a kind keploy does not read (%d)", path, ace.Header.AceType)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue // for what is made in it, not for the directory
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(owner) || sid.Equals(user) || isAdminSID(sid) {
			continue
		}
		if ace.Mask&denied != 0 {
			return fmt.Errorf("%s lets %s change what is in it (rights %#x)", path, sid, ace.Mask&denied)
		}
	}
	return nil
}

// isAdminSID reports whether sid is the administrators' or SYSTEM's.
func isAdminSID(sid *windows.SID) bool {
	return sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) || sid.IsWellKnown(windows.WinLocalSystemSid)
}

// tryLockFile takes an exclusive lock on f's first byte if no one holds one:
// true when it did, false when someone holds it, and an error when
// LockFileEx failed otherwise.
func tryLockFile(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
