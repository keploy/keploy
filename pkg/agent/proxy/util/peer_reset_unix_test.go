//go:build linux || darwin

package util

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// awaited runs AwaitPeerReset on conn and returns a channel closed once it
// has returned.
func awaited(ctx context.Context, conn net.Conn) <-chan struct{} {
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		AwaitPeerReset(ctx, conn)
	}()
	return returned
}

// returnsWithin fails the row where the wait has not returned within 10s:
// the bound only turns a hang into a failure.
func returnsWithin(t *testing.T, returned <-chan struct{}, after string) {
	t.Helper()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatalf("AwaitPeerReset did not return %s", after)
	}
}

// finRead has s read c's FIN, c having half-closed.
func finRead(t *testing.T, c, s *net.TCPConn) {
	t.Helper()
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = s.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := s.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read %d byte(s), %v; want the peer's FIN", n, err)
	}
	_ = s.SetReadDeadline(time.Time{})
}

// A peer that half-closed is waited on past its FIN, its socket still open
// to it: the wait ends at its reset, or at ctx's end, by the read deadline,
// and through a wrapper that gives its conn as on the socket itself. It
// returns at once on a socket whose peer reset it already, and waits on ctx
// alone where no socket can be found under the conn.
func TestAwaitPeerReset(t *testing.T) {
	t.Run("the peer's reset after its FIN", func(t *testing.T) {
		c, s := loopbackPair(t)
		finRead(t, c, s)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		returned := awaited(ctx, &Conn{Conn: s})
		// The peer, still there, takes what is sent it: the wait goes on.
		if _, err := s.Write([]byte("the answer")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, make([]byte, len("the answer"))); err != nil {
			t.Fatal(err)
		}
		select {
		case <-returned:
			t.Fatal("AwaitPeerReset returned at the peer's FIN; want it to wait on to the peer's reset")
		default:
		}
		_ = c.SetLinger(0)
		_ = c.Close()
		returnsWithin(t, returned, "at the peer's reset")
	})
	t.Run("ctx's end", func(t *testing.T) {
		c, s := loopbackPair(t)
		finRead(t, c, s)
		ctx, cancel := context.WithCancel(context.Background())
		returned := awaited(ctx, s)
		cancel()
		returnsWithin(t, returned, "once ctx ended")
	})
	t.Run("a peer that reset already", func(t *testing.T) {
		c, s := loopbackPair(t)
		_ = c.SetLinger(0)
		_ = c.Close()
		eventuallyGone(t, s)
		returnsWithin(t, awaited(context.Background(), s), "on a socket reset already")
	})
	t.Run("no socket under the conn", func(t *testing.T) {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
		ctx, cancel := context.WithCancel(context.Background())
		returned := awaited(ctx, a)
		_ = b.Close()
		select {
		case <-returned:
			t.Fatal("AwaitPeerReset returned with no socket to ask and ctx not ended; want it to wait on ctx")
		default:
		}
		cancel()
		returnsWithin(t, returned, "once ctx ended")
	})
}
