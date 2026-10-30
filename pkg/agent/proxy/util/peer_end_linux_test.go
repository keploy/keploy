//go:build linux

package util

import (
	"io"
	"net"
	"testing"
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
		stop := WatchPeerEnd(c, func(reset bool) { told <- reset })
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
		stop := WatchPeerEnd(c, func(reset bool) { told <- reset })
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
		stop := WatchPeerEnd(c, func(bool) { t.Error("bytes were told as an end") })
		if _, err := peer.Write([]byte("next")); err != nil {
			t.Fatal(err)
		}
		_ = peer.CloseWrite()
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
		stop := WatchPeerEnd(c, func(bool) { t.Error("an end was told with none") })
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
}
