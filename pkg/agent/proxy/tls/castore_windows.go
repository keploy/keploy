//go:build windows

package tls

import (
	"fmt"
	"os"
)

// writeKeyFileSecurely writes the CA private key to path with O_EXCL so it never
// overwrites an existing key. On Windows the file inherits the ACL of its parent
// directory, which lives under the per-user %LOCALAPPDATA%\Keploy\ca tree and is
// therefore already user-scoped; the 0600 request maps to a read/write file for
// the owner. O_NOFOLLOW has no Windows equivalent and is omitted.
//
// NOTE: the Windows per-user persistence path is not exercised in CI here (no
// Windows host), and whether reusing a persisted CA avoids a fresh certutil
// ROOT-store prompt is unverified. It must be validated on a real Windows host
// before relying on the "prompt at most once" behaviour.
func writeKeyFileSecurely(path string, pem []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("failed to create CA key file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(pem); err != nil {
		return fmt.Errorf("failed to write CA key: %w", err)
	}
	return nil
}

// verifyKeyFileSecure confirms the persisted key exists and is a regular file.
// Ownership/ACL enforcement on NTFS is left to the per-user directory location;
// os.IsNotExist on the returned error means "no persisted key yet".
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
	return nil
}
