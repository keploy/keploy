package relay

import (
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"time"

	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
)

type readTimeProvider interface {
	LastReadTime() time.Time
}

type stashedPayload struct {
	bytes  []byte
	readAt time.Time
}

func (s stashedPayload) len() int {
	return len(s.bytes)
}

func joinStashed(parts []stashedPayload) stashedPayload {
	if len(parts) == 0 {
		return stashedPayload{}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	total := 0
	var firstReadAt time.Time
	for _, p := range parts {
		total += len(p.bytes)
		if firstReadAt.IsZero() && !p.readAt.IsZero() {
			firstReadAt = p.readAt
		}
	}
	if total == 0 {
		return stashedPayload{}
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p.bytes...)
	}
	return stashedPayload{bytes: out, readAt: firstReadAt}
}

func observedReadAt(conn net.Conn, fallback time.Time) time.Time {
	if p, ok := conn.(readTimeProvider); ok {
		if ts := p.LastReadTime(); !ts.IsZero() {
			return ts
		}
	}
	return fallback
}

// readTrackingConn is the conn a TLS layer reads the socket through: it
// stamps each read, and the record the layer hands on next takes the stamp of
// the read that completed it (LastReadTime, through readTimeReportingConn).
//
// The layer holds those bytes from that read until it hands the record on,
// and can hold several records from one read, so the read is also where the
// capture watermark must start to see them (progress.go). layer is the mark
// for that, the teeProgress.layer of the direction the layer feeds (nil: none
// to keep):
//
//   - Before a read, the mark is cleared. A layer reads only once it has
//     handed on every whole record it held, and a record it hands on over the
//     read takes this read's stamp.
//   - After a read that brought bytes, the mark is set to heldUnstamped before
//     the stamp is taken from the clock, then to the stamp. A look that found
//     the mark clear is then older than the stamp.
//   - After a read that brought none, the stamp is cleared, so a record the
//     layer hands on over it is stamped by the forwarder, after the look that
//     saw both marks clear (heldUnstamped).
//
// An inner conn that reports when it read the bytes (prependingConn, for the
// stash a handshake reads first) gives its own stamp instead. The stash holds
// only what the peer sent before the handshake's answer, so a record the layer
// hands on after the handshake never takes that stamp: the handshake reads
// the socket after the stash, and that read stamps what follows.
type readTrackingConn struct {
	net.Conn
	lastReadNano atomic.Int64
	layer        *atomic.Int64
}

func newReadTrackingConn(conn net.Conn, layer *atomic.Int64) *readTrackingConn {
	return &readTrackingConn{Conn: conn, layer: layer}
}

func (c *readTrackingConn) markLayer(v int64) {
	if c.layer != nil {
		c.layer.Store(v)
	}
}

func (c *readTrackingConn) Read(p []byte) (int, error) {
	c.markLayer(0)
	n, err := c.Conn.Read(p)
	if n <= 0 {
		c.lastReadNano.Store(0)
		return n, err
	}
	c.markLayer(heldUnstamped)
	at := observedReadAt(c.Conn, time.Now()).UnixNano()
	c.lastReadNano.Store(at)
	c.markLayer(at)
	return n, err
}

// CloseWrite forwards a half-close, for the same reason
// readTimeReportingConn does: this type also embeds net.Conn as an
// interface, so CloseWrite is not promoted. Both wrappers delegate so a
// half-close survives however they end up nested.
func (c *readTrackingConn) CloseWrite() error {
	cw, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return errNoCloseWrite
	}
	return cw.CloseWrite()
}

// HoldTLSLayer passes the TLS layer served over c to the connection under it
// that the proxy closes with care (pTls.TLSLayerHolder): c exposes no
// NetConn, which other walks of the wrappers would then follow.
func (c *readTrackingConn) HoldTLSLayer(l *tls.Conn) { pTls.PassTLSLayer(c.Conn, l) }

func (c *readTrackingConn) LastReadTime() time.Time {
	n := c.lastReadNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// errNoCloseWrite reports that the conn underneath cannot half-close,
// so the caller must fall back to a full teardown.
var errNoCloseWrite = errors.New("relay: underlying conn does not support CloseWrite")

type readTimeReportingConn struct {
	net.Conn
	source readTimeProvider
}

func newReadTimeReportingConn(conn net.Conn, source readTimeProvider) net.Conn {
	if conn == nil || source == nil {
		return conn
	}
	return &readTimeReportingConn{Conn: conn, source: source}
}

func (c *readTimeReportingConn) LastReadTime() time.Time {
	return c.source.LastReadTime()
}

// CloseWrite forwards a half-close to the conn underneath.
//
// This type is what the TLS upgrade stores into Relay.src / Relay.dst
// (directive_proc.go), and it embeds net.Conn as an INTERFACE — so Go
// promotes only net.Conn's method set, and CloseWrite is not in it.
// Without this method every TLS-upgraded connection silently loses the
// ability to half-close, which is to say the half-close support in
// Relay.run would be dead on exactly the MITM'd traffic keploy exists
// to record.
//
// The same blind spot is already documented one file over, where the
// RealCertHook has to run BEFORE this wrapper because unwrapToTLSConn
// cannot see through it either. Delegating explicitly is deliberately
// narrower than adding NetConn(): it fixes half-close without silently
// changing what every other unwrap in the tree can now see through.
func (c *readTimeReportingConn) CloseWrite() error {
	cw, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return errNoCloseWrite
	}
	return cw.CloseWrite()
}
