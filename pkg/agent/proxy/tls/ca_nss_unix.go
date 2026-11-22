//go:build !windows

package tls

import (
	"errors"
	"os"
	"syscall"
)

// chownToOwnerOf gives path to the owner of ref when needsChown says so. A
// path that does not exist is left alone.
func chownToOwnerOf(ref, path string) error {
	refInfo, err := os.Stat(ref)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	refStat, ok1 := refInfo.Sys().(*syscall.Stat_t)
	st, ok2 := info.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 || !needsChown(int(st.Uid), os.Geteuid(), int(refStat.Uid)) {
		return nil
	}
	return os.Lchown(path, int(refStat.Uid), int(refStat.Gid))
}
