//go:build linux

package linux

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func GetSelfInodeNumber() (uint64, uint64, error) {
	p := filepath.Join("/proc", "self", "ns", "pid")

	f, err := os.Stat(p)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to stat %s for keploy pid namespace: %w", p, err)
	}
	st := f.Sys().(*syscall.Stat_t)
	return st.Ino, uint64(st.Dev), nil
}
