//go:build linux

package utils

import (
	"sync"

	"golang.org/x/sys/unix"
)

// ownPidfds is the pidfds keploy opens to watch a process: the interrupt's
// waits on each process its signal reached (ExitOf), its reads of whether a
// process has exited (groupExitedIn), and the adopter's (pkg/platform/http:
// a reap's zombies, a stop's daemons). None is a sign that its process is a
// child keploy started, which os/exec holds a pidfd on: the adopter tells
// the two apart by the pidfds this process holds but these (WithOwnPidfds),
// and took a daemon an interrupt watched for keploy's own, so it neither
// reaped it nor left it to the stop of the daemons. Changed, and read, under
// its lock: an fd leaves it before it is closed, so its number, given again
// to a pidfd of os/exec's, is never skipped.
var ownPidfds = struct {
	sync.Mutex
	m map[int]bool
}{m: map[int]bool{}}

// OpenOwnPidfd opens a pidfd on pid to watch it (ownPidfds).
func OpenOwnPidfd(pid int) (int, error) {
	ownPidfds.Lock()
	defer ownPidfds.Unlock()
	fd, err := unix.PidfdOpen(pid, 0)
	if err == nil {
		ownPidfds.m[fd] = true
	}
	return fd, err
}

// CloseOwnPidfd closes a pidfd OpenOwnPidfd opened.
func CloseOwnPidfd(fd int) {
	ownPidfds.Lock()
	defer ownPidfds.Unlock()
	delete(ownPidfds.m, fd)
	_ = unix.Close(fd)
}

// WithOwnPidfds calls read with the fds of the pidfds keploy opened to watch
// a process (OpenOwnPidfd), held still: none is opened or closed until read
// returns. read must not keep the map, nor open or close one itself.
func WithOwnPidfds(read func(own map[int]bool)) {
	ownPidfds.Lock()
	defer ownPidfds.Unlock()
	read(ownPidfds.m)
}
