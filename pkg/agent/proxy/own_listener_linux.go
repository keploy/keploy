//go:build linux

package proxy

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// listenerHeldBy reports whether process pid holds a socket listening at
// ip:port (listenInodes): one of its open files is that socket. Read from
// /proc/<pid>/fd, so false wherever the agent cannot read that process's
// files. Only a socket's link names one of the listeners' inode numbers.
func listenerHeldBy(ip net.IP, port uint32, pid int) bool {
	inodes := listenInodes(ip, port)
	fds, _ := filepath.Glob(filepath.Join("/proc", strconv.Itoa(pid), "fd", "*"))
	for _, fd := range fds {
		link, _ := os.Readlink(fd)
		if inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] {
			return true
		}
	}
	return false
}

// sameContainer reports whether processes a and b share one mount namespace:
// one container's. False wherever keploy cannot read either's, and for a
// process it cannot name (0): /proc/0 is no process.
func sameContainer(a, b int) bool {
	ma, errA := mountNSOf(a)
	mb, errB := mountNSOf(b)
	return errA == nil && errB == nil && ma == mb
}

// mountNSOf is process pid's mount namespace, as /proc/<pid>/ns/mnt names
// it; a test swaps it for one keploy cannot read.
var mountNSOf = func(pid int) (string, error) {
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", "mnt"))
}
