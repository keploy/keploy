package tls

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
)

// recordHeaderLen is a TLS record's header: its content type, version and
// length (RFC 8446 §5.1).
const recordHeaderLen = 5

// recordConn is the client's connection under keploy's TLS server
// (HandleTLSConnection): it hands crypto/tls the client's bytes no further
// than the end of the TLS record they belong to, until the first byte the
// client sends after the handshake. crypto/tls reads a record whole, header
// then body, and asks for no record it does not need, so when the handshake
// ends it has read exactly the handshake's records: any byte the client sent
// after its Finished -- data, or its close_notify -- is still in the socket,
// and counted as it is read (after). crypto/tls itself cannot say: it answers a
// close_notify and a bare FIN at a record's end alike, with io.EOF, and may
// have read either in the same read as the client's Finished.
type recordConn struct {
	net.Conn
	// left is how much of the current record is still to be handed over,
	// its header included once that is read; hdr is the part of a header
	// read so far.
	left int
	hdr  []byte
	// done: the handshake has ended (handshakeDone). after counts the bytes
	// handed over from then on; once there is one, the reads go straight
	// through (pass).
	done  atomic.Bool
	after atomic.Uint64
	pass  bool
	// silent, when set (ClientEvents.SilentHangUp), is told, once, of the
	// client's hang-up as the first read after the handshake ends with
	// nothing read since: the end of the stream, or a reset, never keploy's
	// own close of the connection.
	silent     func(error)
	silentOnce sync.Once
	// release, when set, releases the hold on the client's hang-up
	// (HangUpHolder), once: at the first byte after the handshake, which
	// leaves no silent hang-up to tell; at the first read after it that
	// fails with nothing read, once silent is told of it if it is the
	// client's; or at the close.
	release     func()
	releaseOnce sync.Once
}

func newRecordConn(conn net.Conn) *recordConn {
	return &recordConn{Conn: conn, hdr: make([]byte, 0, recordHeaderLen)}
}

// Read hands over no more than what is left of the current record (a header
// first, up to its five bytes), until the first byte after the handshake.
// crypto/tls reads from one goroutine at a time.
func (c *recordConn) Read(b []byte) (int, error) {
	n, err := c.read(b)
	if n == 0 && err != nil && c.done.Load() && c.after.Load() == 0 {
		if c.silent != nil && IsClientGiveUp(err) && !errors.Is(err, net.ErrClosed) {
			c.silentOnce.Do(func() { c.silent(err) })
		}
		c.releaseHangUp()
	}
	return n, err
}

// Close releases the hold on the client's hang-up (release), then closes
// the connection beneath.
func (c *recordConn) Close() error {
	c.releaseHangUp()
	return c.Conn.Close()
}

// releaseHangUp releases the hold on the client's hang-up, once.
func (c *recordConn) releaseHangUp() {
	if c.release != nil {
		c.releaseOnce.Do(c.release)
	}
}

func (c *recordConn) read(b []byte) (int, error) {
	if c.pass || len(b) == 0 {
		n, err := c.Conn.Read(b)
		c.count(n)
		return n, err
	}
	if c.left == 0 {
		// At a record's start: its header, which says how long it is.
		n, err := c.Conn.Read(b[:min(len(b), recordHeaderLen-len(c.hdr))])
		c.hdr = append(c.hdr, b[:n]...)
		if len(c.hdr) == recordHeaderLen {
			c.left = int(c.hdr[3])<<8 | int(c.hdr[4])
			c.hdr = c.hdr[:0]
		}
		c.count(n)
		return n, err
	}
	n, err := c.Conn.Read(b[:min(len(b), c.left)])
	c.left -= n
	c.count(n)
	return n, err
}

// count notes n bytes handed over: after the handshake, the first ends the
// record-at-a-time reads.
func (c *recordConn) count(n int) {
	if n > 0 && c.done.Load() {
		c.after.Add(uint64(n))
		c.pass = true
		c.releaseHangUp()
	}
}

// handshakeDone marks the handshake's end: what is handed over from now on
// is the client's after it.
func (c *recordConn) handshakeDone() { c.done.Store(true) }

// NetConn is the connection beneath, for what looks for the socket under a
// connection (connseq, realdeps, PassTLSLayer).
func (c *recordConn) NetConn() net.Conn { return c.Conn }

// CloseWrite half-closes the connection beneath, as the conn it wraps would.
func (c *recordConn) CloseWrite() error { return util.CloseWriteIfPossible(c.Conn) }

// SilentSinceHandshake reports whether the client of conn -- a connection
// HandleTLSConnection served, or one wrapping it -- has sent nothing since
// its TLS handshake ended: no data and no close_notify. A client that then
// hangs up (IsClientGiveUp) left without a word once it had keploy's
// certificate: curl, libcurl and Node.js under TLS 1.2 reject a certificate
// they do not trust so, after the handshake, as a client that gave up looks.
// False for a connection HandleTLSConnection did not serve.
func SilentSinceHandshake(conn net.Conn) bool {
	c := conn
	for i := 0; c != nil && i < 16; i++ { // a wrapper chain, never a cycle
		if rc, ok := c.(*recordConn); ok {
			return rc.done.Load() && rc.after.Load() == 0
		}
		switch u := c.(type) {
		case interface{ NetConn() net.Conn }:
			c = u.NetConn()
		case interface{ Unwrap() net.Conn }:
			c = u.Unwrap()
		default:
			return false
		}
	}
	return false
}
