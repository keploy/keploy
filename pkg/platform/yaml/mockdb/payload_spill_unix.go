//go:build unix

package mockdb

import "golang.org/x/sys/unix"

// spillRoom reports whether the filesystem of dir can take a spill of need
// bytes and keep a tenth of itself, and at least 1 GiB, free: a spill is a
// cache, and must not be what fills a disk the replay writes its reports
// and mocks to.
func spillRoom(dir string, need int64) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false
	}
	bsize := uint64(st.Bsize) //nolint:gosec,unconvert // a block size is positive; its type differs by system
	avail, total := uint64(st.Bavail)*bsize, uint64(st.Blocks)*bsize
	if need < 0 || uint64(need) > avail {
		return false
	}
	return avail-uint64(need) >= max(1<<30, total/10)
}
