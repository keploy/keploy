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
// bound: each read is handed on as it comes, at most ReadChunkSize, and
// nothing accumulates. A relay reader used to append 1 KiB reads until a short
// one, which a fast streamer never gives, and one held 69 MB.
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
			if len(buf) > ReadChunkSize {
				t.Fatalf("a hand-off of %d bytes, past ReadChunkSize (%d)", len(buf), ReadChunkSize)
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
	if got := conn.maxHeld.Load(); got > 2*ReadChunkSize {
		t.Fatalf("the relay held up to %d bytes of the stream, want at most %d", got, 2*ReadChunkSize)
	}
}
