package util

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// awaitTaken returns what ch gives, or fails the test at a quarter of its
// deadline: no clock of the test's own decides anything.
func awaitTaken[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	var expired <-chan time.Time
	if dl, ok := t.Deadline(); ok {
		timer := time.NewTimer(time.Until(dl) / 4)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case v := <-ch:
		return v
	case <-expired:
		t.Fatalf("%s never happened", what)
		panic("unreachable")
	}
}

// A message whose length is a multiple of 1 KiB, with nothing after it, is
// handed on whole: a relay reader used to read it 1 KiB at a time and wait for
// a short read, which the peer, waiting for its answer, never sends.
func TestReadBuffConnHandsOnAMessageWithNoFurtherBytes(t *testing.T) {
	for _, size := range []int{1024, 2048, 4096} {
		t.Run(byteCount(size), func(t *testing.T) {
			peer, conn := net.Pipe()
			t.Cleanup(func() { _ = peer.Close(); _ = conn.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			bufCh := make(chan []byte)
			errCh := make(chan error, 1)
			go ReadBuffConn(ctx, zap.NewNop(), conn, bufCh, errCh, false)

			msg := bytes.Repeat([]byte("m"), size)
			// The peer writes the message and sends nothing more: the pipe's
			// Write returns once the reader has read all of it.
			go func() { _, _ = peer.Write(msg) }()
			var got []byte
			for len(got) < size {
				select {
				case err := <-errCh:
					t.Fatalf("the reader ended with %v after %d of %d bytes", err, len(got), size)
				default:
				}
				got = append(got, awaitTaken(t, bufCh, "the reader handing on the message's bytes")...)
			}
			if !bytes.Equal(got, msg) {
				t.Fatalf("handed on %d bytes, want the %d-byte message", len(got), size)
			}
		})
	}
}

func byteCount(n int) string {
	return string(rune('0'+n/1024)) + "KiB"
}

// endlessConn is a peer that streams total bytes as fast as they are read. It
// counts what the relay has read of it and not yet handed on, and refuses to
// stream past heldCap of that, so a reader that holds the stream fails here
// instead of growing a buffer of all of it.
type endlessConn struct {
	net.Conn
	left    int64
	held    atomic.Int64
	maxHeld atomic.Int64
}

const heldCap = 1 << 20

var errHeldPastCap = errors.New("the relay held more than heldCap of the stream")

func (c *endlessConn) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > c.left {
		n = c.left
	}
	held := c.held.Add(n)
	if held > heldCap {
		return 0, errHeldPastCap
	}
	for {
		m := c.maxHeld.Load()
		if held <= m || c.maxHeld.CompareAndSwap(m, held) {
			break
		}
	}
	c.left -= n
	return int(n), nil
}

// A peer streaming 1 GB keeps what the relay holds of it under a small fixed
// bound: each read is handed on as it comes, at most WaitReadSize, and nothing
// accumulates. A relay reader used to append 1 KiB reads until a short one,
// which a fast streamer never gives, and one held 69 MB.
func TestReadBuffConnHoldsAStreamInBoundedChunks(t *testing.T) {
	const total = 1 << 30
	conn := &endlessConn{left: total}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bufCh := make(chan []byte)
	errCh := make(chan error, 1)
	go ReadBuffConn(ctx, zap.NewNop(), conn, bufCh, errCh, false)

	var taken int64
	for taken < total {
		select {
		case buf := <-bufCh:
			if len(buf) > WaitReadSize {
				t.Fatalf("a hand-off of %d bytes, past WaitReadSize (%d)", len(buf), WaitReadSize)
			}
			conn.held.Add(-int64(len(buf)))
			taken += int64(len(buf))
		case err := <-errCh:
			t.Fatalf("the reader ended with %v after %d of %d bytes", err, taken, int64(total))
		}
	}
	if err := awaitTaken(t, errCh, "the reader's EOF"); err != io.EOF {
		t.Fatalf("the reader ended with %v, want io.EOF", err)
	}
	// One chunk handed on and not yet counted taken, and the next one read.
	if got := conn.maxHeld.Load(); got > 2*WaitReadSize {
		t.Fatalf("the relay held up to %d bytes of the stream, want at most %d", got, 2*WaitReadSize)
	}
}

// sizeTellingConn answers each Read with one byte and tells the size of the
// buffer the Read was given; once done is closed, a Read is answered EOF.
type sizeTellingConn struct {
	net.Conn
	sizes chan int
	done  <-chan struct{}
}

func (c *sizeTellingConn) Read(p []byte) (int, error) {
	select {
	case c.sizes <- len(p):
	case <-c.done:
		return 0, io.EOF
	}
	p[0] = 'x'
	return 1, nil
}

// Each path reads with its own size, and a read holds its buffer for as long
// as it waits. The reads that can wait for hours on an idle connection (a
// pass-through's readers, ReadBuffConn; an HTTP keep-alive connection's wait
// for its next request) take WaitReadSize, the old loop's 1 KiB: at
// ReadChunkSize, 10k idle pass-throughs held about 600 MiB more. ReadBytes,
// whose callers read a message on, and ReadBytesUntilEOF, generic replay's,
// which matches pieces recorded at the relay's read size, take ReadChunkSize.
func TestEachReadPathReadsWithItsSize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, tc := range []struct {
		name string
		read func(io.Reader) ([]byte, error)
		want int
	}{
		{"ReadBytes", func(r io.Reader) ([]byte, error) { return ReadBytes(ctx, zap.NewNop(), r) }, ReadChunkSize},
		{"ReadBytesUntilEOF", func(r io.Reader) ([]byte, error) { return ReadBytesUntilEOF(ctx, zap.NewNop(), r) }, ReadChunkSize},
		{"ReadBytesWaiting", func(r io.Reader) ([]byte, error) { return ReadBytesWaiting(ctx, zap.NewNop(), r) }, WaitReadSize},
	} {
		conn := &sizeTellingConn{sizes: make(chan int, 1)}
		if got, err := tc.read(conn); err != nil || string(got) != "x" {
			t.Fatalf("%s = (%q, %v), want the byte read", tc.name, got, err)
		}
		if got := <-conn.sizes; got != tc.want {
			t.Errorf("%s read with a %d-byte buffer, want %d", tc.name, got, tc.want)
		}
	}

	conn := &sizeTellingConn{sizes: make(chan int), done: ctx.Done()}
	bufCh := make(chan []byte, 1)
	go ReadBuffConn(ctx, zap.NewNop(), conn, bufCh, make(chan error, 1), false)
	for i := 0; i < 3; i++ {
		if got := awaitTaken(t, conn.sizes, "a read of ReadBuffConn"); got != WaitReadSize {
			t.Fatalf("ReadBuffConn's read %d had a %d-byte buffer, want WaitReadSize (%d)", i, got, WaitReadSize)
		}
		<-bufCh
	}
}

// The bytes a read returns with its error are handed on before the error: a
// relay that takes a side's error first ends without that side's last bytes
// (a TLS peer's last record, read together with its close_notify, comes back
// with io.EOF). Both channels are unbuffered and the test takes from
// whichever is ready, so the reader is ready on one only, the one it hands on
// to first. No clock decides it.
func TestReadBuffConnHandsOnTheBytesThatComeWithItsEnd(t *testing.T) {
	peer, c := net.Pipe()
	t.Cleanup(func() { _ = peer.Close(); _ = c.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bufCh := make(chan []byte)
	errCh := make(chan error)
	go ReadBuffConn(ctx, zap.NewNop(), &lastBytesConn{Conn: c, last: []byte("x")}, bufCh, errCh, false)

	var expired <-chan time.Time
	if dl, ok := t.Deadline(); ok {
		timer := time.NewTimer(time.Until(dl) / 4)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case b := <-bufCh:
		if string(b) != "x" {
			t.Fatalf("the reader handed on %q, want the bytes that came with its end, %q", b, "x")
		}
	case err := <-errCh:
		t.Fatalf("the reader handed on its end (%v) before the bytes that came with it", err)
	case <-expired:
		t.Fatal("the reader handing on its read never happened")
	}
	if err := awaitTaken(t, errCh, "the reader handing on its end"); err != io.EOF {
		t.Fatalf("the reader ended with %v, want io.EOF", err)
	}
}

// lastBytesConn's one read returns its last bytes with io.EOF.
type lastBytesConn struct {
	net.Conn
	last []byte
}

func (c *lastBytesConn) Read(p []byte) (int, error) {
	n := copy(p, c.last)
	c.last = c.last[n:]
	return n, io.EOF
}
