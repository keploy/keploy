//go:build linux

package linux

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func GetSelfInodeNumber() (uint64, error) {
	p := filepath.Join("/proc", "self", "ns", "pid")

	f, err := os.Stat(p)
	if err != nil {
		return 0, errors.New("failed to get inode of the keploy process")
	}
	// Dev := (f.Sys().(*syscall.Stat_t)).Dev
	Ino := (f.Sys().(*syscall.Stat_t)).Ino
	if Ino != 0 {
		return Ino, nil
	}
	return 0, nil
}
