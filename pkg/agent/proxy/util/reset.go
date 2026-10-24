package util

import (
	"io"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/neterr"
)

// ResetOnClose makes c's socket send a reset rather than a FIN when it is
// closed (SO_LINGER 0), looking beneath the wrappers the proxy puts on its
// connections. It does nothing to a connection with no TCP socket beneath
// it.
//
// It does not close c. Whoever owns the socket closes it, and that close is
// what sends the reset; SafeConn's own Close is a no-op on purpose.
func ResetOnClose(c net.Conn) {
	for i := 0; i < 8 && c != nil; i++ { // bounded: a cycle must not hang
		switch v := c.(type) {
		case interface{ SetLinger(sec int) error }:
			_ = v.SetLinger(0)
			return
		case *connseq.Upstream:
			c = v.Conn
		case *SafeConn:
			c = v.Unwrap()
		case interface{ NetConn() net.Conn }:
			c = v.NetConn()
		default:
			return
		}
	}
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

// PassOnReset passes a reset by a copy's source peer on to dst, the
// connection the copy wrote to, and reports whether it did. readErr is the
// error the copy's source returned last (ReadRecorder.Err).
//
// A peer that resets is gone in both directions, so this is how a relay
// ends on the event rather than on a clock:
//   - the other direction writes to that peer, so nothing it reads can be
//     delivered any more, and the relay may stop waiting for it at once;
//   - dst learns of the reset the way it would with no proxy in between:
//     its close sends a reset, not the FIN that would call a truncated
//     stream complete. Without that a destination that waits for the
//     client's next request (an SMTP server after its reply, a database
//     between queries) is never told, and the relay waits out its idle
//     window on it.
//
// Bytes the copy wrote to dst that its socket has not sent yet are dropped
// with the reset, as the source's own unsent bytes were when its peer
// reset.
func PassOnReset(dst net.Conn, readErr error) bool {
	if !neterr.IsConnReset(readErr) {
		return false
	}
	ResetOnClose(dst)
	return true
}
