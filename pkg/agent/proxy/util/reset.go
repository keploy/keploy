package util

import (
	"errors"
	"io"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/neterr"
)

// ResetOnClose makes c's socket send a reset rather than a FIN when it is
// closed (SO_LINGER 0), looking beneath the wrappers the proxy puts on its
// connections (SetLingerBeneath). It does nothing to a connection with no
// TCP socket beneath it.
//
// It does not close c. Whoever owns the socket closes it, and that close is
// what sends the reset; SafeConn's own Close is a no-op on purpose.
func ResetOnClose(c net.Conn) { _ = SetLingerBeneath(c, 0) }

// errNoLinger is SetLingerBeneath's answer for a connection with no socket
// that takes SO_LINGER beneath it.
var errNoLinger = errors.New("no TCP socket beneath the connection to set SO_LINGER on")

// SetLingerBeneath sets SO_LINGER to sec on the TCP socket beneath c. It
// walks beneath connseq.Upstream, SafeConn and any connection with a
// NetConn method (a tls.Conn, and the proxy's own wrappers that expose the
// connection beneath them, such as trackedConn), and stops at the first
// connection that sets the linger itself. A wrapper that other walkers of
// connection wrappers must not see beneath (treeRealConn, liveReader)
// exposes SetLinger instead of NetConn, so that only the linger goes
// through.
func SetLingerBeneath(c net.Conn, sec int) error {
	// Bounded, so that a cycle of NetConn wrappers cannot hang it. A SetLinger
	// method that walks on starts a walk of its own; none leads back to its
	// own wrapper.
	for i := 0; i < 8 && c != nil; i++ {
		switch v := c.(type) {
		case interface{ SetLinger(sec int) error }:
			return v.SetLinger(sec)
		case *connseq.Upstream:
			c = v.Conn
		case *SafeConn:
			c = v.Unwrap()
		case interface{ NetConn() net.Conn }:
			c = v.NetConn()
		default:
			return errNoLinger
		}
	}
	return errNoLinger
}

// ReadRecorder is a reader that keeps the error its last Read returned.
//
// io.Copy returns a failed read of its source and a failed write to its
// destination the same way. A relay needs to know which: a reset READ means
// the source's peer is gone in both directions, while a failed write says
// nothing about the source.
type ReadRecorder struct {
	R   io.Reader
	Err error
}

func (r *ReadRecorder) Read(p []byte) (int, error) {
	n, err := r.R.Read(p)
	r.Err = err
	return n, err
}

// PeerGone reports whether err says a connection's peer is gone in both
// directions: it reset (neterr.IsConnReset), or it vanished without a reset
// and the kernel gave up on it (neterr.IsPeerVanished: ETIMEDOUT,
// EHOSTUNREACH and their kin).
//
// A relay passes both on to the other peer as a reset. For a vanished peer
// that is not the error the other peer would see with no proxy in between:
// its own socket would fail with the vanished peer's ETIMEDOUT or
// EHOSTUNREACH, while through the proxy it reads ECONNRESET. Its socket is
// the proxy's, which is alive, and a reset is the one end the proxy can give
// it that says the stream was cut. The alternatives are worse: a FIN calls a
// cut stream complete, and saying nothing leaves the other peer waiting on a
// connection that is dead (until its own timeout, if it has one), with the
// relay waiting on it too.
func PeerGone(err error) bool { return neterr.IsConnReset(err) || neterr.IsPeerVanished(err) }

// PassOnReset passes the end of a copy's source peer, when it reset or
// vanished (PeerGone), on to dst, the connection the copy wrote to, as a
// reset, and reports whether it did. readErr is the error the copy's source
// returned last (ReadRecorder.Err).
//
// A peer that is gone is gone in both directions, so this is how a relay
// ends on the event rather than on a clock:
//   - the other direction writes to that peer, so nothing it reads can be
//     delivered any more, and the relay may stop waiting for it at once;
//   - dst learns of the end the way it would with no proxy in between, as
//     a reset (or, for a vanished peer, the closest to it: see PeerGone):
//     its close sends a reset, not the FIN that would call a truncated
//     stream complete. Without that a destination that waits for the
//     client's next request (an SMTP server after its reply, a database
//     between queries) is never told, and the relay waits out its idle
//     window on it.
//
// SO_LINGER 0 drops whatever dst's socket has not transmitted yet when it
// is closed. That includes bytes the resetting peer did send, which the
// copy read and wrote to dst, if dst's socket still holds them. A socket
// holds written bytes untransmitted while its peer's receive window is full
// (a receiver that is not reading) or, off loopback, while its congestion
// window is. With no proxy in between, a full window would hold the same
// bytes in the resetting peer's own send queue, and its reset would drop
// them there: the receiver gets what fits its receive queue either way.
// So passing the reset on loses nothing a reset would not; only the socket
// that held the bytes differs.
func PassOnReset(dst net.Conn, readErr error) bool {
	if !PeerGone(readErr) {
		return false
	}
	ResetOnClose(dst)
	return true
}
