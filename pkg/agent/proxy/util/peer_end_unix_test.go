//go:build linux || darwin

package util

import (
	"io"
	"net"
	"testing"
	"time"
)

// pairForWatch is a connected TCP pair: the peer's end, and the end watched.
func pairForWatch(t *testing.T) (peer, watched *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	p, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := <-accepted
	if c == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = p.Close(); _ = c.Close() })
	return p.(*net.TCPConn), c.(*net.TCPConn)
}

// The watch tells a FIN and a reset apart, says nothing of bytes the peer
// sends (they stay for the next read), and, stopped, leaves the connection
// to read on with no deadline left behind.
func TestWatchPeerEnd(t *testing.T) {
	t.Run("a FIN", func(t *testing.T) {
		peer, c := pairForWatch(t)
		told := make(chan bool, 1)
		stop := watched(t, c, func(reset bool) { told <- reset })
		_ = peer.CloseWrite()
		if reset := <-told; reset {
			t.Fatal("a FIN was told as a reset")
		}
		if !stop() {
			t.Fatal("stop did not report the end it told")
		}
	})
	t.Run("a reset", func(t *testing.T) {
		peer, c := pairForWatch(t)
		told := make(chan bool, 1)
		stop := watched(t, c, func(reset bool) { told <- reset })
		_ = peer.SetLinger(0)
		_ = peer.Close()
		if reset := <-told; !reset {
			t.Fatal("a reset was told as a FIN")
		}
		if !stop() {
			t.Fatal("stop did not report the end it told")
		}
	})
	t.Run("bytes stay for the next read", func(t *testing.T) {
		peer, c := pairForWatch(t)
		stop, done, ok := watchPeerEnd(c, func(bool) { t.Error("bytes were told as an end") })
		if !ok {
			t.Fatal("a TCP conn was not watched")
		}
		if _, err := peer.Write([]byte("next")); err != nil {
			t.Fatal(err)
		}
		_ = peer.CloseWrite()
		// The watch ends by itself on the bytes, before stop: what it
		// looked at is what the next read gets.
		<-done
		if stop() {
			t.Fatal("bytes were reported as an end")
		}
		got, err := io.ReadAll(c)
		if err != nil || string(got) != "next" {
			t.Fatalf("read %q, %v after the watch", got, err)
		}
	})
	t.Run("stopped before anything came", func(t *testing.T) {
		peer, c := pairForWatch(t)
		stop := watched(t, c, func(bool) { t.Error("an end was told with none") })
		if stop() || stop() {
			t.Fatal("a watch with nothing to tell reported an end")
		}
		// The deadline that stopped the watch is gone: a read waits for
		// what the peer sends next.
		go func() { _, _ = peer.Write([]byte("x")); _ = peer.CloseWrite() }()
		got, err := io.ReadAll(c)
		if err != nil || string(got) != "x" {
			t.Fatalf("read %q, %v after the watch was stopped", got, err)
		}
	})
	t.Run("through a wrapper that gives its conn", func(t *testing.T) {
		peer, c := pairForWatch(t)
		told := make(chan bool, 1)
		stop := watched(t, noDeadlines{c}, func(reset bool) { told <- reset })
		_ = peer.CloseWrite()
		if reset := <-told; reset {
			t.Fatal("a FIN was told as a reset")
		}
		if !stop() {
			t.Fatal("stop did not report the end it told")
		}
	})
	t.Run("stopped through a wrapper whose deadlines do nothing", func(t *testing.T) {
		// stop wakes the watch by the socket's own deadline: one set on
		// the wrapper would leave stop waiting for good.
		peer, c := pairForWatch(t)
		stop := watched(t, noDeadlines{c}, func(bool) { t.Error("an end was told with none") })
		if stop() {
			t.Fatal("a watch with nothing to tell reported an end")
		}
		go func() { _, _ = peer.Write([]byte("x")); _ = peer.CloseWrite() }()
		got, err := io.ReadAll(c)
		if err != nil || string(got) != "x" {
			t.Fatalf("read %q, %v after the watch was stopped", got, err)
		}
	})
	t.Run("not through a wrapper that hides its conn", func(t *testing.T) {
		_, c := pairForWatch(t)
		stop, ok := WatchPeerEnd(hidden{c}, func(bool) { t.Error("an end was told with none") })
		if ok {
			t.Fatal("a conn with no socket to be found said it was watched")
		}
		if stop() {
			t.Fatal("an unwatched conn reported an end")
		}
	})
}

// watched starts a watch on conn and fails t if conn was not watched.
func watched(t *testing.T, conn net.Conn, onEnd func(reset bool)) func() bool {
	t.Helper()
	stop, ok := WatchPeerEnd(conn, onEnd)
	if !ok {
		t.Fatalf("%T was not watched", conn)
	}
	return stop
}

// noDeadlines is a wrapper that gives the conn it wraps (NetConn) and whose
// own deadlines do nothing, as some of the proxy's do.
type noDeadlines struct{ net.Conn }

func (c noDeadlines) NetConn() net.Conn               { return c.Conn }
func (c noDeadlines) SetReadDeadline(time.Time) error { return nil }
func (c noDeadlines) SetDeadline(time.Time) error     { return nil }

// hidden is a wrapper that does not give the conn it wraps.
type hidden struct{ net.Conn }
