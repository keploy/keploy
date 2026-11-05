package relay

import (
	"context"
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
// Only a client connection that is a TCP socket on Linux can be asked
// (clientSocketOf). Elsewhere only a failed write's error is handed on.

// clientErrorSocket is a clientSocket that can say what error its connection
// holds (SO_ERROR), taking it as it says it: nil when it holds none, or cannot
// say.
type clientErrorSocket interface {
	clientSocket
	pendingError() error
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
	s, ok := r.client.(clientErrorSocket)
	if !ok {
		return
	}
	if r.clientEOF.Load() {
		r.clientAnswered(ctx)
	}
	if err := s.pendingError(); err != nil {
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
