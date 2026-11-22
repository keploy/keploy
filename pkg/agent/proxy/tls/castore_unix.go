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
// symlink. A persisted key that fails any of these is not trusted and is
// regenerated. os.IsNotExist on the returned error means "no persisted key yet".
func verifyKeyFileSecure(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("CA key %s is a symlink", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("CA key %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("CA key %s is group/other accessible (%o)", path, fi.Mode().Perm())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("CA key %s is owned by uid %d, not %d", path, st.Uid, os.Geteuid())
	}
	return nil
}
