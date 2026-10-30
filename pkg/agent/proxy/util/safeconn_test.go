package util

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeConn is a minimal net.Conn for testing SafeConn behavior.
type fakeConn struct {
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
	closed   bool
}

func newFakeConn(data string) *fakeConn {
	return &fakeConn{
		readBuf:  bytes.NewBufferString(data),
		writeBuf: &bytes.Buffer{},
	}
}

func (f *fakeConn) Read(p []byte) (int, error)         { return f.readBuf.Read(p) }
func (f *fakeConn) Write(p []byte) (int, error)        { return f.writeBuf.Write(p) }
func (f *fakeConn) Close() error                       { f.closed = true; return nil }
func (f *fakeConn) LocalAddr() net.Addr                { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234} }
func (f *fakeConn) RemoteAddr() net.Addr               { return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5678} }
func (f *fakeConn) SetDeadline(_ time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(_ time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(_ time.Time) error { return nil }

func TestSafeConnReadPassesThrough(t *testing.T) {
	fc := newFakeConn("hello world")
	sc := NewSafeConn(fc, zap.NewNop())

	buf := make([]byte, 5)
	n, err := sc.Read(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(buf[:n]))
	}
}

func TestSafeConnWritePassesThrough(t *testing.T) {
	fc := newFakeConn("")
	sc := NewSafeConn(fc, zap.NewNop())

	_, err := sc.Write([]byte("data"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fc.writeBuf.String() != "data" {
		t.Fatalf("expected 'data' in underlying write buf, got %q", fc.writeBuf.String())
	}
}

func TestSafeConnCloseIsNoOp(t *testing.T) {
	fc := newFakeConn("")
	sc := NewSafeConn(fc, zap.NewNop())

	err := sc.Close()
	if err != nil {
		t.Fatalf("Close should return nil, got: %v", err)
	}
	if fc.closed {
		t.Fatal("underlying connection should NOT be closed by SafeConn.Close()")
	}
}

func TestSafeConnDeadlinesAreNoOps(t *testing.T) {
	fc := newFakeConn("")
	sc := NewSafeConn(fc, zap.NewNop())

	if err := sc.SetDeadline(time.Now()); err != nil {
		t.Fatalf("SetDeadline should return nil: %v", err)
	}
	if err := sc.SetReadDeadline(time.Now()); err != nil {
		t.Fatalf("SetReadDeadline should return nil: %v", err)
	}
	if err := sc.SetWriteDeadline(time.Now()); err != nil {
		t.Fatalf("SetWriteDeadline should return nil: %v", err)
	}
}

func TestSafeConnWithReader(t *testing.T) {
	fc := newFakeConn("underlying")
	prefix := bytes.NewReader([]byte("prefix-"))
	sc := NewSafeConnWithReader(fc, io.MultiReader(prefix, fc), zap.NewNop())

	buf := make([]byte, 20)
	n, _ := sc.Read(buf)
	if !bytes.HasPrefix(buf[:n], []byte("prefix-")) {
		t.Fatalf("expected to read prefix first, got %q", string(buf[:n]))
	}
}

func TestSafeConnUnwrap(t *testing.T) {
	fc := newFakeConn("")
	sc := NewSafeConn(fc, zap.NewNop())

	if sc.Unwrap() != fc {
		t.Fatal("Unwrap should return the original connection")
	}
}

func TestSafeConnAddrs(t *testing.T) {
	fc := newFakeConn("")
	sc := NewSafeConn(fc, zap.NewNop())

	if sc.LocalAddr().String() != fc.LocalAddr().String() {
		t.Fatal("LocalAddr mismatch")
	}
	if sc.RemoteAddr().String() != fc.RemoteAddr().String() {
		t.Fatal("RemoteAddr mismatch")
	}
}

// A half-close through a wrapper answers for the conn it wraps: done where the
// leaf can (a TCP conn's FIN), and ErrCannotHalfClose, not nil, where it
// cannot, however the wrappers nest. A caller that keeps the other direction
// open for the peer's answer to the FIN must know the peer got one.
func TestCloseWriteIfPossibleAnswersForTheLeaf(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	tcp, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer := <-accepted
	t.Cleanup(func() { _ = tcp.Close(); _ = peer.Close() })
	pipe, other := net.Pipe()
	t.Cleanup(func() { _ = pipe.Close(); _ = other.Close() })
	leaf := struct{ net.Conn }{pipe} // no CloseWrite

	wrapped := func(c net.Conn) net.Conn {
		s := NewSafeConn(c, zap.NewNop())
		return &Conn{Conn: s, Reader: s, Logger: zap.NewNop()}
	}
	for _, c := range []net.Conn{leaf, &Conn{Conn: leaf, Reader: leaf}, NewSafeConn(leaf, zap.NewNop()), wrapped(leaf)} {
		if err := CloseWriteIfPossible(c); !errors.Is(err, ErrCannotHalfClose) || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("CloseWriteIfPossible(%T over a conn with no CloseWrite) = %v, want ErrCannotHalfClose", c, err)
		}
	}
	if err := CloseWriteIfPossible(wrapped(tcp)); err != nil {
		t.Fatalf("CloseWriteIfPossible(a TCP conn, wrapped) = %v, want nil", err)
	}
	if n, err := peer.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("the peer read (%d, %v), want the FIN (0, io.EOF)", n, err)
	}

	// A wrapper with no CloseWrite of its own (one that only filters reads)
	// is looked through to the conn it wraps, by NetConn or Unwrap, however
	// deep it sits: the leaf answers. Red with the walk gone: a TCP leaf
	// under such a wrapper says ErrCannotHalfClose, and its peer is closed
	// on where it could have been half-closed.
	inConn := func(c net.Conn) net.Conn { return &Conn{Conn: c, Reader: c, Logger: zap.NewNop()} }
	for _, w := range []struct {
		name string
		wrap func(net.Conn) net.Conn
	}{
		{"NetConn", func(c net.Conn) net.Conn { return netConnOnly{c} }},
		{"Unwrap", func(c net.Conn) net.Conn { return unwrapOnly{c} }},
		{"NetConn in a Conn", func(c net.Conn) net.Conn { return inConn(netConnOnly{c}) }},
		{"Unwrap in a SafeConn in a Conn", func(c net.Conn) net.Conn { return wrapped(unwrapOnly{c}) }},
		{"NetConn under Unwrap in a Conn", func(c net.Conn) net.Conn { return inConn(unwrapOnly{netConnOnly{c}}) }},
	} {
		if err := CloseWriteIfPossible(w.wrap(leaf)); !errors.Is(err, ErrCannotHalfClose) {
			t.Errorf("CloseWriteIfPossible(%s over a conn with no CloseWrite) = %v, want ErrCannotHalfClose", w.name, err)
		}
		tcp, peer := tcpPair(t, ln)
		t.Cleanup(func() { _ = tcp.Close(); _ = peer.Close() })
		if err := CloseWriteIfPossible(w.wrap(tcp)); err != nil {
			t.Errorf("CloseWriteIfPossible(%s over TCP) = %v, want nil: the leaf can half-close", w.name, err)
			continue
		}
		if n, err := peer.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			t.Errorf("%s: the peer read (%d, %v), want the FIN (0, io.EOF)", w.name, n, err)
		}
	}
	// A wrapper that names itself beneath it ends the walk, no hang.
	if err := CloseWriteIfPossible(selfWrapped{leaf}); !errors.Is(err, ErrCannotHalfClose) {
		t.Errorf("CloseWriteIfPossible(a wrapper that is its own NetConn) = %v, want ErrCannotHalfClose", err)
	}
}

// A conn with a CloseWrite of its own is asked before anything beneath it
// is looked at: a *tls.Conn's half-close is its close_notify, a record its
// peer's TLS layer reads as the end, not a FIN on the TCP conn beneath it
// (which crypto/tls also reads as io.EOF at a record's boundary, so the peer
// tells them apart by its own socket: it never reads that socket's EOF).
// Red with the walk taken before a conn's own CloseWrite: a wrapper's own
// CloseWrite (the in-flight tracker's) is skipped the same way.
func TestCloseWriteIfPossibleAsksATLSConnForItsCloseNotify(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	raw, peerRaw := tcpPair(t, ln)
	t.Cleanup(func() { _ = raw.Close(); _ = peerRaw.Close() })
	server := tls.Server(peerRaw, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}, MinVersion: tls.VersionTLS12})
	under := &eofSeen{Conn: raw}
	client := tls.Client(under, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test-only self-signed peer
	handshook := make(chan error, 1)
	go func() { handshook <- server.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-handshook; err != nil {
		t.Fatal(err)
	}
	if err := CloseWriteIfPossible(&Conn{Conn: server, Reader: server, Logger: zap.NewNop()}); err != nil {
		t.Fatalf("CloseWriteIfPossible(a TLS conn in a Conn) = %v, want nil", err)
	}
	if n, err := client.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("the TLS peer read (%d, %v), want the end its close_notify brings, (0, io.EOF)", n, err)
	}
	if under.eof {
		t.Fatal("the TLS peer's socket read a FIN: the half-close went beneath the TLS layer, and no close_notify was sent")
	}
}

// eofSeen is a conn that remembers whether a read of it returned io.EOF.
type eofSeen struct {
	net.Conn
	eof bool
}

func (c *eofSeen) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err == io.EOF {
		c.eof = true
	}
	return n, err
}

// netConnOnly and unwrapOnly wrap a conn with no CloseWrite of their own,
// telling the conn beneath by NetConn or Unwrap; selfWrapped tells itself.
type netConnOnly struct{ net.Conn }

func (c netConnOnly) NetConn() net.Conn { return c.Conn }

type unwrapOnly struct{ net.Conn }

func (c unwrapOnly) Unwrap() net.Conn { return c.Conn }

type selfWrapped struct{ net.Conn }

func (c selfWrapped) NetConn() net.Conn { return c }
