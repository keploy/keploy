//go:build darwin

package proxy

import (
	"golang.org/x/sys/unix"
)

// What a kept connection's socket has received, on macOS, is counted by
// sequence numbers, as the bytes wait counts it (pairbytes_darwin.go): how
// far the next sequence number it expects (tcpi_rcv_nxt) has come since the
// application's first byte. XNU's byte count (tcpi_rxbytes) also counts
// payload that arrived twice (a retransmit of a segment already queued, on
// a host loaded enough that the retransmit timer fires first): a count of
// bytes no read can ever return, which would hold every later settle that
// cuts the connection until its watchdog gave up.

// sockBase is where the counts of a kept connection's socket begin: the
// sequence number of the application's first byte (its irs plus one). It is
// read once, as the proxy keeps the connection and before its handler runs
// (beginCounts), and only read after that.
type sockBase struct {
	first uint32
	// ok: first was read. A connection whose first byte's number cannot be
	// read has no count of what it received (socketCounts).
	ok bool
}

// beginCounts reads where c's counts begin (sockBase): as its handler has
// not run, no read has taken a byte from its socket yet, so the socket holds
// everything it has received in sequence (SO_NREAD) and, once it came, the
// application's FIN, and the next number it expects less those is the first
// byte's. The two are separate reads: they are taken as one only when a
// read of the expected number on each side of SO_NREAD agree (no byte, and
// no FIN, came in between). A socket already reset (CLOSED) may have taken
// the FIN before the reset; it is taken as not, so its counts never hold
// the FIN, and the one a read that returns the stream's end takes off
// (finCounted) is put back by received's floor: never fewer than were
// delivered.
func (c *trackedConn) beginCounts() {
	_ = c.raw.Control(func(fd uintptr) { c.base.first, c.base.ok = xnuFirstSeq(int(fd)) })
}

// xnuFirstSeq is beginCounts's read of socket fd: false when its tcp_info
// or SO_NREAD cannot be read. It reads again for as long as bytes or the
// FIN come in between its reads; what can come is bounded by what the
// socket's receive window lets in while nothing reads it.
func xnuFirstSeq(fd int) (uint32, bool) {
	for {
		r1, s1, err := xnuTCPInfo(fd)
		if err != nil {
			return 0, false
		}
		unread, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_NREAD)
		if err != nil || unread < 0 {
			return 0, false
		}
		r2, s2, err := xnuTCPInfo(fd)
		if err != nil {
			return 0, false
		}
		if r1 != r2 || s1 != s2 {
			continue
		}
		first := r1 - uint32(unread)
		if xnuFINIn(s1) {
			first--
		}
		return first, true
	}
}

// xnuFINIn reports whether a socket in XNU TCP state state has taken its
// peer's FIN in sequence, so that its next expected sequence number counts
// it: CLOSE_WAIT, LAST_ACK, CLOSING and TIME_WAIT are reached only by one.
func xnuFINIn(state uint8) bool {
	switch state {
	case xnuTCPCloseWait, xnuTCPLastAck, xnuTCPClosing, xnuTCPTimeWait:
		return true
	}
	return false
}

// xnuCountsUnknown names, among the connections whose held requests cannot
// be told (trackedConn.unknown), one whose socket's count of what it
// received macOS does not give keploy.
const xnuCountsUnknown = "TCP (macOS does not give keploy the count of what this connection received)"

// socketCounts is what c's socket has received in sequence, the
// application's FIN counted as one once it came, and its TCP state, from one
// copy of its tcp_info. The sequence numbers wrap at 2^32; what the socket
// received is never fewer than c's reads delivered (v), and never 2^31 more
// (no socket holds that much unread), so it is read as its distance past
// what was delivered. ok is false for a socket that cannot be asked (its
// handler closed it). A count that cannot be read from a socket that can be
// (where it began was not read, the counts' layout was not proven, the
// socket's tcp_info fails, or it reads fewer than were delivered) leaves c's
// requests unknown, and holds no settle: it counts as nothing untaken.
func (c *trackedConn) socketCounts(v connView) (received uint64, state uint8, ok bool) {
	var rcvNxt uint32
	var err error
	if cerr := c.raw.Control(func(fd uintptr) { rcvNxt, state, err = xnuTCPInfo(int(fd)) }); cerr != nil {
		return 0, 0, false
	}
	if err != nil {
		c.countsUnknown()
		return 0, 0, false
	}
	past := int32(rcvNxt - c.base.first - uint32(v.delivered))
	if !c.base.ok || past < 0 || xnuTableReadable() != nil {
		c.countsUnknown()
		return v.delivered, state, true
	}
	return v.delivered + uint64(past), state, true
}

// countsUnknown leaves c's requests unknown (xnuCountsUnknown), unless its
// parser already left them so.
func (c *trackedConn) countsUnknown() {
	name := xnuCountsUnknown
	if c.unknown.CompareAndSwap(nil, &name) {
		c.t.changed()
	}
}

// finCounted reports whether the count socketCounts gives holds the
// application's FIN: whether a socket in XNU TCP state state has taken it
// (xnuFINIn). One CLOSED (a reset, or closed as its FIN_WAIT_2, CLOSING or
// LAST_ACK ended) has taken one for sure only when a read returned the end
// of the stream (eof), as on Linux.
func finCounted(state uint8, eof bool) bool {
	if state == xnuTCPClosed {
		return eof
	}
	return xnuFINIn(state)
}

// clientGone reports whether the client of a socket its handler still holds
// has gone (xnuClientGone).
func clientGone(state uint8) bool { return xnuClientGone(state) }
