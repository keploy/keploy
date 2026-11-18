//go:build linux || darwin || windows

package relay

import (
	"net"
	"syscall"
	"testing"
	"time"
)

// tcpSettle is how long the rows on real sockets wait for the kernel's TCP to
// reach a state: a window closed, bytes acknowledged or arrived, a FIN or a
// reset handed on. Loopback can lose segments: on macOS 26 (8 CPUs), with
// three copies of the closed-window rows at once, every wait that ran out at
// 5 s had the sending socket in loss recovery (TCP_INFO's
// TCPI_FLAG_LOSSRECOVERY), waiting on a retransmission timer of 1.1 to 3.6 s.
// A lost last segment is resent only when that timer fires, and it doubles
// with each loss, so the waits allow several in a row. A row ends as soon as
// its state is reached; only one that never comes waits this long.
const tcpSettle = 30 * time.Second

// awaitUnread waits until the socket c holds a byte its reader has not read
// yet (unreadOf).
func awaitUnread(t *testing.T, c net.Conn) {
	t.Helper()
	awaitUnreadBytes(t, c, 1)
}

// awaitUnreadBytes waits until the socket c holds n bytes its reader has not
// read yet (unreadOf).
func awaitUnreadBytes(t *testing.T, c net.Conn, n int) {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	unread := func() int {
		n := 0
		if err := rc.Control(func(fd uintptr) {
			var ierr error
			if n, ierr = unreadOf(fd); ierr != nil {
				t.Error(ierr)
			}
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for limit := time.Now().Add(tcpSettle); unread() < n; {
		if time.Now().After(limit) {
			t.Fatalf("the socket holds %d unread bytes after %v, want %d", unread(), tcpSettle, n)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// loopbackPair returns the two ends of a loopback TCP connection.
func loopbackPair(t *testing.T) (dialed, accepted net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	got := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		got <- c
	}()
	dialed, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if accepted = <-got; accepted == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = dialed.Close(); _ = accepted.Close() })
	return dialed, accepted
}

// tcpState is the TCP state of the socket under c (tcpStateOf), in the OS's
// own numbering: stateClosed, stateCloseWait and stateLastAck name the ones
// the rows wait for.
func tcpState(t *testing.T, c net.Conn) uint8 {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var state uint8
	if err := rc.Control(func(fd uintptr) {
		var ok bool
		if state, ok = tcpStateOf(fd); !ok {
			t.Error("TCP_INFO: the socket cannot say its state")
		}
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

// stateIs reports whether the TCP state of the socket under c is want, false
// when it cannot be asked. For the relay's goroutines, which must not fail the
// test.
func stateIs(c net.Conn, want uint8) bool {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	var state uint8
	asked := false
	if err := rc.Control(func(fd uintptr) { state, asked = tcpStateOf(fd) }); err != nil {
		return false
	}
	return asked && state == want
}

// bytesAcked is how many of the bytes written to the socket under c its peer's
// stack has acknowledged, in all (ackedOf).
func bytesAcked(t *testing.T, c net.Conn) uint64 {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var acked uint64
	if err := rc.Control(func(fd uintptr) {
		var ok bool
		if acked, ok = ackedOf(fd); !ok {
			t.Error("TCP_INFO: the socket cannot say what its peer acknowledged")
		}
	}); err != nil {
		t.Fatal(err)
	}
	return acked
}

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
		if n, gerr = receiveBufferSizeOf(fd); gerr != nil {
			t.Error(gerr)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return n
}
