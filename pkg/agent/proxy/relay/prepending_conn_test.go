package relay

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// fakeReadConn is a minimal net.Conn whose Read returns a scripted
// sequence of byte chunks (or io.EOF when exhausted). Other methods
// are no-ops sufficient to satisfy the interface — Reads are the
// only thing prependingConn delegates to.
type fakeReadConn struct {
	chunks [][]byte
	closed bool
}

func (f *fakeReadConn) Read(b []byte) (int, error) {
	if f.closed {
		return 0, errors.New("closed")
	}
	if len(f.chunks) == 0 {
		return 0, io.EOF
	}
	c := f.chunks[0]
	n := copy(b, c)
	if n < len(c) {
		f.chunks[0] = c[n:]
	} else {
		f.chunks = f.chunks[1:]
	}
	return n, nil
}

func (f *fakeReadConn) Write(b []byte) (int, error)        { return len(b), nil }
func (f *fakeReadConn) Close() error                       { f.closed = true; return nil }
func (f *fakeReadConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (f *fakeReadConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (f *fakeReadConn) SetDeadline(_ time.Time) error      { return nil }
func (f *fakeReadConn) SetReadDeadline(_ time.Time) error  { return nil }
func (f *fakeReadConn) SetWriteDeadline(_ time.Time) error { return nil }

func TestPrependingConn_EmptyPrefix(t *testing.T) {
	// Empty prefix → wrapper not allocated; raw conn returned. This
	// keeps the common path (no stash) zero-overhead.
	raw := &fakeReadConn{chunks: [][]byte{[]byte("hello")}}
	got := newPrependingConn(raw, nil)
	if got != raw {
		t.Fatalf("newPrependingConn(empty) returned wrapper; want raw conn passthrough")
	}
}

func TestPrependingConn_PrefixOnly(t *testing.T) {
	// Prefix shorter than the read buffer; reading once drains the
	// prefix and returns exactly len(prefix) bytes. Subsequent reads
	// fall through to the live conn.
	raw := &fakeReadConn{chunks: [][]byte{[]byte("LIVE")}}
	c := newPrependingConn(raw, []byte("STASH"))

	buf := make([]byte, 100)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if got := string(buf[:n]); got != "STASH" {
		t.Fatalf("first Read = %q; want %q", got, "STASH")
	}

	n, err = c.Read(buf)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if got := string(buf[:n]); got != "LIVE" {
		t.Fatalf("second Read = %q; want %q", got, "LIVE")
	}
}

func TestPrependingConn_PrefixSpansMultipleReads(t *testing.T) {
	// Prefix longer than the read buffer; multiple Reads are needed
	// to drain it. The contract is "never mix prefix + live in one
	// Read" so each Read returns up to its buffer size from prefix
	// only, then falls through to live on the next call.
	prefix := bytes.Repeat([]byte("X"), 32)
	raw := &fakeReadConn{chunks: [][]byte{[]byte("LIVE")}}
	c := newPrependingConn(raw, prefix)

	buf := make([]byte, 10)
	collected := make([]byte, 0, 32)
	for i := 0; i < 4; i++ {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		collected = append(collected, buf[:n]...)
		if i < 3 && n != 10 {
			t.Fatalf("Read %d = %d bytes; want 10 (still in prefix)", i, n)
		}
	}

	if !bytes.Equal(collected[:32], prefix) {
		t.Fatalf("collected prefix = %q; want %q", collected[:32], prefix)
	}

	// Next Read should be from live stream.
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("post-prefix Read: %v", err)
	}
	if got := string(buf[:n]); got != "LIVE" {
		t.Fatalf("post-prefix Read = %q; want %q", got, "LIVE")
	}
}

func TestPrependingConn_PassThroughMethods(t *testing.T) {
	// Write, Close, addresses, deadlines must delegate to the wrapped
	// conn — only Read is special.
	raw := &fakeReadConn{}
	c := newPrependingConn(raw, []byte("X"))

	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !raw.closed {
		t.Fatal("Close did not propagate to wrapped conn")
	}
}
