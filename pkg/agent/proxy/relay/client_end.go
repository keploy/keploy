package relay

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// What the client's socket says once the relay is done with it.
//
// A client that closes its socket (close(2)) with bytes it has not read resets
// the connection, and a client that closed its socket with none resets every
// byte that reaches it after (client_took.go). The relay reads the first as
// the client's end (Config.PeerEnded: its read fails with ECONNRESET). The
// second it does not read, when the client's FIN came first: its read of the
// client's end returned io.EOF, and the reset comes after it, for a chunk the
// destination's forwarder wrote while that FIN was on its way, before a look
// could find it, or for a chunk a half-closed client's stack acknowledged and
// its application then closed its socket on, unread. That chunk was teed, and
// the parser has it, like a chunk the client read.
//
// The reset is still there to read. The kernel keeps a reset's error on the
// socket (sk_err) until a read or a write of the socket takes it, or
// getsockopt(SO_ERROR) does: ECONNRESET, or EPIPE where the client's FIN had
// come first (tcp_reset, for a socket in CLOSE_WAIT). So as the relay ends,
// once its forwarders are done reading the client's socket and writing to it,
// it hands what the socket holds to its owner (Config.ClientEndError), with no
// protocol's rule: the parser judges what a reset after the last write says of
// its last answer. A write to the client that failed took the error itself,
// so that write's error is handed on instead: the client had reset what was
// written before it.
//
// The relay asks once the client's stack has answered for every byte written
// to it (clientAnswered), when the relay read the client's FIN: the closed
// client's reset of the last chunk comes one round trip after that chunk's
// Write, and the relay's end can come before it (the destination's forwarder
// reads the destination's FIN, which answers the client's FIN the relay
// forwarded on). An answer is an acknowledgement of every byte (a half-closed
// client that took them) or the connection's close (a reset). The relay waits
// for it as long as it waits for chunks set aside (awaitClientVerdict),
// HalfCloseGrace, and no longer than the recording's context: a client that
// answers neither by then has the socket read as it is. With a negative
// HalfCloseGrace (the relay stops at the client's FIN) it is not waited
// for.
//
// On macOS the reset can be what the client's forwarder reads instead of the
// FIN that came before it (client_took_darwin.go): the relay reads that FIN
// as the client's end, as on Linux, and hands the reset on as it ends, as the
// one the socket held. And macOS keeps no error for a reset that comes once
// the proxy's own FIN is sent after the client's (LAST_ACK or CLOSING), where
// Linux keeps ECONNRESET: the relay's FIN can go before the closed client's
// reset of the last chunk comes back (the destination answers the client's
// FIN with its own at once). The socket's TCP_INFO says it then
// (client_end_darwin.go), and the relay hands it on as Linux's would be.
//
// Windows keeps no error on the socket for a reset in any state, and its read
// returns the reset where the client's FIN came first, as macOS's does, with
// its own code then (WSAECONNABORTED, not WSAECONNRESET, from that read or the
// socket's next: wintcpinfo.go's resetAfterFIN). Its TCP_INFO says the reset
// from the connection's state and counts instead (wintcpinfo.go), which needs
// to know whether the proxy sent its own FIN: the relay tells the socket when
// it does (noteClientFINSent).
//
// Only a client connection that is a TCP socket on Linux, macOS or Windows can
// be asked (clientSocketOf). Elsewhere only a failed write's error is handed
// on.

// clientErrorSocket is a clientSocket that can say what error its connection
// holds (SO_ERROR), taking it as it says it: nil when it holds none, or cannot
// say. finRead: the relay read the client's FIN (clientEOF), which a reset
// macOS kept no error for comes after.
type clientErrorSocket interface {
	clientSocket
	pendingError(finRead bool) error
}

// resetAfterFINSocket is a clientSocket that can say whether a reset its read
// returned came after the client's FIN (macOS's and Windows'). readErr is the
// error that read returned, which says it on Windows.
type resetAfterFINSocket interface {
	clientSocket
	finBeforeReset(readErr error) bool
}

// clientFINBeforeReset reports whether the reset the client's forwarder read
// (readErr) came after the client's FIN, which it then reads as the client's
// end; false where the socket cannot say (Linux returns the FIN first, so its
// sockets are never asked).
func (r *Relay) clientFINBeforeReset(readErr error) bool {
	s, ok := r.client.(resetAfterFINSocket)
	return ok && s.finBeforeReset(readErr)
}

// finSentSocket is a clientSocket the relay tells when it sends the client its
// own TCP FIN (Windows': its socket cannot say whether that FIN went).
type finSentSocket interface {
	clientSocket
	// closedBeforeFIN is asked just before the relay half-closes the
	// client's socket: whether the connection has closed already, so a
	// half-close would send no FIN, though it succeeds. The relay then
	// takes the connection as over, as a failed half-close says on Linux
	// and macOS, and does not half-close it.
	closedBeforeFIN() bool
	proxyFINSent()
}

// clientClosedBeforeFIN is asked before the relay half-closes the client
// conn: whether the client's socket says the connection has closed already,
// so no FIN can go: the connection is over (errClientClosedBeforeFIN), and
// its socket is not told of a FIN (noteClientFINSent).
func (r *Relay) clientClosedBeforeFIN() bool {
	s, ok := r.client.(finSentSocket)
	return ok && s.closedBeforeFIN()
}

// noteClientFINSent tells the client's socket that the relay half-closed the
// client conn c, when that sent a TCP FIN: c is the TCP socket, or wraps it
// (NetConn) with no TLS layer between, whose CloseWrite sends a close_notify
// alert and no FIN.
func (r *Relay) noteClientFINSent(c net.Conn) {
	s, ok := r.client.(finSentSocket)
	if !ok {
		return
	}
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		switch c.(type) {
		case *net.TCPConn:
			s.proxyFINSent()
			return
		case *tls.Conn:
			return
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return
		}
		c = u.NetConn()
	}
}

// noteClientWriteFailed keeps err, the error a write to the client failed with
// (the first one), for clientEnd.
func (r *Relay) noteClientWriteFailed(dir fakeconn.Direction, err error) {
	if dir != fakeconn.FromDest || err == nil {
		return
	}
	r.clientWriteErr.CompareAndSwap(nil, &err)
}

// clientEnd tells Config.ClientEndError what error the client's socket gave
// after the relay's last read of it and last write to it, if it gave one. run
// calls it once both forwarders have returned and every chunk set aside is
// decided (awaitClientVerdict), before the streams end and before the relay
// itself closes or resets the client's socket.
func (r *Relay) clientEnd(ctx context.Context) {
	if r.cfg.ClientEndError == nil {
		return
	}
	if p := r.clientWriteErr.Load(); p != nil {
		r.cfg.ClientEndError(*p)
		return
	}
	if p := r.clientResetRead.Load(); p != nil {
		// The read took the reset off the socket: it holds none now.
		r.cfg.ClientEndError(*p)
		return
	}
	s, ok := r.client.(clientErrorSocket)
	if !ok {
		return
	}
	finRead := r.clientEOF.Load()
	if finRead {
		r.clientAnswered(ctx)
	}
	if err := s.pendingError(finRead); err != nil {
		r.cfg.ClientEndError(err)
	}
}

// clientAnswered returns once the client's stack has answered for every byte
// written to its socket (an acknowledgement of all of them, or the
// connection's close), the socket cannot say, HalfCloseGrace has passed, or ctx
// is done. It looks at resolve's pace.
func (r *Relay) clientAnswered(ctx context.Context) {
	bound := time.NewTimer(r.cfg.HalfCloseGrace) // a negative one has passed
	defer bound.Stop()
	wait := firstLookAgain
	again := time.NewTimer(wait)
	defer again.Stop()
	for {
		// written before look: every byte counted written that the look
		// finds acknowledged was acknowledged by then.
		n, ok := r.client.written()
		l := r.client.look()
		if !ok || l.said == clientClosed || l.said == clientUnknown || l.acked >= n {
			return
		}
		again.Reset(wait)
		select {
		case <-again.C:
			wait = min(2*wait, lastLookAgain)
		case <-bound.C:
			return
		case <-ctx.Done():
			return
		}
	}
}
