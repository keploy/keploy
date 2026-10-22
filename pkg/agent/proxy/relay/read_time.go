package relay

import (
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
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

// observedRead is observedReadAt, and the clock its stamp is on (S12 v16
// §2.2: trust is decided at the producer): the proxy's (fakeconn.ClockProxy)
// when the stamp is this relay's own time.Now, taken as the bytes were read
// (the fallback, or a read time one of the relay's own wrappers took so:
// proxyStamped); unset for a read time another producer stamped, which this
// relay cannot vouch for, so it never routes.
func observedRead(conn net.Conn, fallback time.Time) (time.Time, fakeconn.Clock) {
	clock := fakeconn.ClockUnset
	if proxyStamped(conn) {
		clock = fakeconn.ClockProxy
	}
	// A teller that told no time yet gives the fallback, on the clock the
	// teller's own stamps are on: another producer's bytes, read here late,
	// are not stamped by this relay as they were sent.
	return observedReadAt(conn, fallback), clock
}

// proxyStamped reports whether conn's read time (readTimeProvider) is the
// proxy's time.Now: a conn that tells none, or one of the relay's own
// wrappers whose stamps are its time.Now over a conn that is so in turn
// (readTrackingConn, prependingConn, whose prefix's time a relay stash took,
// and readTimeReportingConn over such a source). Any other teller (a capture
// fed from elsewhere, a FakeConn) is another producer's.
func proxyStamped(conn net.Conn) bool {
	switch c := conn.(type) {
	case *readTrackingConn:
		return proxyStamped(c.Conn)
	case *prependingConn:
		return proxyStamped(c.Conn)
	case *readTimeReportingConn:
		src, ok := c.source.(net.Conn)
		return ok && proxyStamped(src)
	}
	_, tells := conn.(readTimeProvider)
	return !tells
}

type readTrackingConn struct {
	net.Conn
	lastReadNano atomic.Int64
}

func newReadTrackingConn(conn net.Conn) *readTrackingConn {
	return &readTrackingConn{Conn: conn}
}

func (c *readTrackingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.lastReadNano.Store(observedReadAt(c.Conn, time.Now()).UnixNano())
	}
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
