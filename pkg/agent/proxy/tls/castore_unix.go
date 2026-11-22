//go:build !windows

package tls

import (
	"fmt"
	"os"
	"syscall"
)

// writeKeyFileSecurely writes the CA private key to path with 0600, refusing to
// follow a symlink and refusing to overwrite an existing file. Under keploy's
// process-wide umask 0 (main.go) a plain create would be world-readable, so the
// mode is set explicitly and re-asserted with Chmod in case an ancestor umask
// still applied.
func writeKeyFileSecurely(path string, pem []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("failed to create CA key file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("failed to set CA key file perms: %w", err)
	}
	if _, err := f.Write(pem); err != nil {
		return fmt.Errorf("failed to write CA key: %w", err)
	}
	return nil
}

// verifyKeyFileSecure reports nil only when path is a regular file owned by this
// process's effective uid, with no group/other permission bits, and is not a
// symlink (checkOwned). A persisted key that fails any of these is not trusted and is
// regenerated. os.IsNotExist on the returned error means "no persisted key yet".
func verifyKeyFileSecure(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkOwned("CA key", path, fi, false, 0o077, os.Geteuid(), false)
}

// checkOwned reports nil only when fi, what is at path (its os.Lstat, or the
// Stat of a file opened without following a link), is no symlink, is a
// directory when dir is true and a regular file otherwise, has none of the
// permission bits deny, and is owned by the user euid, or by root when orRoot
// is true. A file or directory that fails any of these is one another user
// may have made, or may change, so keploy does not trust it. what names path
// in the error.
func checkOwned(what, path string, fi os.FileInfo, dir bool, deny os.FileMode, euid int, orRoot bool) error {
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s %s is a symlink", what, path)
	case dir && !fi.IsDir():
		return fmt.Errorf("%s %s is not a directory", what, path)
	case !dir && !fi.Mode().IsRegular():
		return fmt.Errorf("%s %s is not a regular file", what, path)
	case fi.Mode().Perm()&deny != 0:
		return fmt.Errorf("%s %s has mode %o; it must have none of the bits %o", what, path, fi.Mode().Perm(), deny)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if ok && int(st.Uid) != euid && (!orRoot || st.Uid != 0) {
		return fmt.Errorf("%s %s is owned by uid %d, not by this run's user (uid %d)", what, path, st.Uid, euid)
	}
	return nil
}
