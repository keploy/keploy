//go:build linux || darwin

package relay

import (
	"net"
	"syscall"
	"testing"
)

// receiveBufferBoundOf is the most bytes the receive buffer of the socket
// under c, asked for smallWindow bytes before it connected (dialSmallWindow),
// can come to hold from now on, however its kernel sizes it as the row runs
// (receiveBufferGrownOf): no fewer than its peer may send it unread.
func receiveBufferBoundOf(t *testing.T, c net.Conn) int {
	t.Helper()
	held := receiveBufferOf(t, c)
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	bound := 0
	var gerr error
	if err := rc.Control(func(fd uintptr) { bound, gerr = receiveBufferGrownOf(fd, held) }); err != nil || gerr != nil {
		t.Fatalf("the socket cannot say how far its receive buffer grows: %v %v", err, gerr)
	}
	return bound
}

// receiveBufferOf is the receive buffer of the socket under c as it reads it
// back (SO_RCVBUF): no fewer bytes than its peer may send it unread now.
func receiveBufferOf(t *testing.T, c net.Conn) int {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := rc.Control(func(fd uintptr) {
		var gerr error
		if n, gerr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF); gerr != nil {
			t.Error(gerr)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return n
}
