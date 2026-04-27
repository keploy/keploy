package relay

import (
	"net"
)

// prependingConn wraps a net.Conn and yields a fixed prefix of bytes
// from Read before falling through to the underlying conn. Writes,
// deadlines, addresses, and Close pass straight through to the wrapped
// conn — only the read side is intercepted.
//
// This exists so handleUpgradeTLS can hand stashed pre-handshake bytes
// to tls.Server.Handshake / tls.Client.HandshakeContext without losing
// them. Background: when the relay raises its pause barrier mid-
// connection, a forwarder that was already inside src.Read can return
// with N bytes from the kernel TCP recv buffer that the post-Read
// pause check then transfers into r.stashedC2D / r.stashedD2C. Those
// bytes are NOT in the kernel buffer anymore, so a naive
// tls.Server(src, cfg).Handshake() reading directly from src misses
// them. With Postgres SSL under sslmode=require and a SUT that
// pipelines its SSLRequest tightly with its TLS ClientHello (lib/pq +
// libpq do this under load), the stashed bytes ARE the start of the
// ClientHello — losing them surfaces as `tls: server did not echo
// the legacy session ID` (peer-to-keploy) or `tls: illegal parameter`
// (keploy-to-peer) and the connection falls through to passthrough,
// taking every query on that connection out of the recording.
//
// The implementation is intentionally tiny: a one-shot prefix buffer
// drained on each Read until empty, then pure pass-through. No
// concurrency safety beyond what net.Conn already requires from a
// single Reader (Go's tls package serialises Reads inside the
// handshake state machine, so a non-locking design is fine here —
// this type is NOT general-purpose, it is specifically for the
// "drain stash into TLS handshake" use case).
type prependingConn struct {
	net.Conn
	prefix []byte
}

// newPrependingConn returns a net.Conn that reads `prefix` first,
// then falls through to `c`. If prefix is empty, c is returned
// unchanged so callers don't pay the wrapper cost on the common
// path where no bytes were stashed.
func newPrependingConn(c net.Conn, prefix []byte) net.Conn {
	if len(prefix) == 0 {
		return c
	}
	return &prependingConn{Conn: c, prefix: prefix}
}

// Read drains the prefix first. Once the prefix is exhausted, future
// Reads delegate to the wrapped conn. We never mix a partial-prefix
// + partial-conn Read in a single call: the contract for net.Conn
// allows a Read to return fewer bytes than the buffer holds, and
// keeping the two streams separate avoids a class of bugs where a
// caller sizes its buffer to a protocol frame and expects a single
// Read to either fully cover the prefix OR fully cover the live
// stream.
func (p *prependingConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}
