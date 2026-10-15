//go:build linux

package util

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// WatchUntilTaken runs onClosed if the destination closes (or resets) the
// pre-dialled connection before a dial takes it.
//
// The connection is opened when the application's handshake completes, and
// may then wait for the application's first bytes. A destination that closes
// idle connections (a keep-alive or header-read timeout) closes it in that
// time; without keploy the application's own connection would have received
// that close, so onClosed lets the proxy close the application's end too —
// when the application has sent nothing (ReceivedNothing): one that has (a
// TLS ClientHello the proxy answered itself) would have kept the destination
// busy, so its connection stays, and its dial gets a fresh upstream instead
// (take never hands over a connection the destination closed). Bytes the
// destination sends first (a server greeting) are only peeked at: they stay
// for whoever takes the connection, and the watch ends.
func (p *Predialed) WatchUntilTaken(onClosed func()) {
	if p == nil || onClosed == nil {
		return
	}
	// A connection handed back wrapped (to replay bytes already read) is
	// not watched: the destination spoke, and that ends a watch anyway.
	sc, ok := p.conn.(syscall.Conn)
	if !ok {
		p.mu.Lock()
		p.onClosed = onClosed
		p.mu.Unlock()
		return
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return
	}
	p.mu.Lock()
	p.onClosed = onClosed
	if p.taken || p.watchDone != nil {
		p.mu.Unlock()
		return
	}
	done := make(chan struct{})
	p.watchDone = done
	p.mu.Unlock()

	go func() {
		defer close(done)
		defer func() { _ = recover() }() // a watch must never take the agent down
		closed := false
		var b [1]byte
		err := rc.Read(func(fd uintptr) bool {
			n, _, rerr := unix.Recvfrom(int(fd), b[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
			if errors.Is(rerr, unix.EAGAIN) || errors.Is(rerr, unix.EINTR) {
				return false // nothing yet: wait for the socket to become readable
			}
			closed = rerr != nil || n == 0
			return true
		})
		if err != nil || !closed {
			// Taken (the deadline woke the read), closed by CloseIfUnused, or
			// the destination spoke first.
			return
		}
		p.mu.Lock()
		taken := p.taken
		p.mu.Unlock()
		if !taken {
			onClosed()
		}
	}()
}

// closedByPeer reports whether the destination has closed or reset conn,
// without reading anything from it.
func closedByPeer(conn net.Conn) bool {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	closed := false
	var b [1]byte
	_ = rc.Control(func(fd uintptr) {
		n, _, rerr := unix.Recvfrom(int(fd), b[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
		switch {
		case rerr == nil:
			closed = n == 0
		case errors.Is(rerr, unix.EAGAIN), errors.Is(rerr, unix.EINTR):
		default:
			closed = true
		}
	})
	return closed
}

// ReceivedNothing reports whether the application has sent nothing on conn,
// the proxy's end of its connection: the kernel's count of bytes received on
// the socket, which also counts what the proxy has already read.
func ReceivedNothing(conn net.Conn) bool {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	nothing := false
	_ = rc.Control(func(fd uintptr) {
		info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		nothing = err == nil && info.Bytes_received == 0
	})
	return nothing
}

// Waiting reports what the socket under conn holds for its reader, without
// reading it: bytes, or the peer's close. Both are false when nothing waits,
// or when there is no socket to look at. A Go read whose deadline has already
// passed fails without looking at the socket, so a read stopped by a deadline
// before it began cannot say whether anything had arrived; this can.
//
// Wrappers are looked through by NetConn, so data says only that the socket
// holds bytes: a Read of a wrapper that transforms them (a *tls.Conn) may still
// block. firstRead.stop in the proxy reads at once on data, so it relies on its
// conn being bare or a byte-transparent wrapper (util.Conn).
func Waiting(conn net.Conn) (data, closed bool) {
	for i := 0; ; i++ {
		if _, ok := conn.(syscall.Conn); ok {
			break
		}
		if i == 8 || conn == nil { // bounded: a cycle must not hang
			return false, false
		}
		u, ok := conn.(interface{ NetConn() net.Conn })
		if !ok {
			return false, false
		}
		conn = u.NetConn()
	}
	rc, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		return false, false
	}
	var b [1]byte
	_ = rc.Control(func(fd uintptr) {
		n, _, rerr := unix.Recvfrom(int(fd), b[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
		switch {
		case rerr == nil:
			data, closed = n > 0, n == 0
		case errors.Is(rerr, unix.EAGAIN), errors.Is(rerr, unix.EINTR):
		default:
			closed = true
		}
	})
	return data, closed
}
