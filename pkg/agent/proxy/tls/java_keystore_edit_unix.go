//go:build !windows

package tls

import (
	"fmt"
	"os"
	"syscall"
)

// keepKeystoreAttributes gives copyPath, keploy's edited copy of the keystore
// whose real file is real (orig, as it was read), what the keystore's readers
// go by besides its content, before the copy is renamed over it: its owner,
// its mode, and its extended attributes (copyExtendedAttributes). A file keploy
// creates is otherwise root's, 0600 (no JVM run as another user could read
// it), and labelled by its directory. One that cannot be given them is not
// put in place.
func keepKeystoreAttributes(copyPath, real string, orig os.FileInfo) error {
	st, ok := orig.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("could not read the owner of the keystore %s", real)
	}
	cfi, err := os.Lstat(copyPath)
	if err != nil {
		return fmt.Errorf("could not stat the edited copy of %s: %w", real, err)
	}
	if cst, ok := cfi.Sys().(*syscall.Stat_t); !ok || cst.Uid != st.Uid || cst.Gid != st.Gid {
		if err := os.Lchown(copyPath, int(st.Uid), int(st.Gid)); err != nil {
			return fmt.Errorf("could not give the edited copy of %s the keystore's owner %d:%d: %w", real, st.Uid, st.Gid, err)
		}
	}
	// After the chown, which clears the set-id bits.
	if err := os.Chmod(copyPath, orig.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
		return fmt.Errorf("could not give the edited copy of %s the keystore's mode %v: %w", real, orig.Mode().Perm(), err)
	}
	if err := copyExtendedAttributes(real, copyPath); err != nil {
		return fmt.Errorf("could not give the edited copy of %s the keystore's extended attributes: %w", real, err)
	}
	return nil
}

// syncDir writes dir's entries (a rename in it) to disk.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// hardLinks is the number of names fi's file has.
func hardLinks(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
