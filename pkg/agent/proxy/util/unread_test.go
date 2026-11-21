package util

import (
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

// unreadUnder is a wrapper that gives the connection beneath it (NetConn) and
// is no syscall.Conn itself, as a TLS connection is.
type unreadUnder struct{ net.Conn }

func (c unreadUnder) NetConn() net.Conn { return c.Conn }

// UnreadBytes counts what the socket under a connection holds unread, itself
// or through a wrapper that gives the connection beneath, on each system the
// proxy runs on, and gives 0 for a connection with no socket under it.
func TestUnreadBytesCountsWhatTheSocketHolds(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skip("UnreadBytes does not ask the socket here")
	}
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
	app, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	proxySide := <-accepted
	if proxySide == nil {
		t.Fatal("the accept failed")
	}
	t.Cleanup(func() { _ = proxySide.Close() })
	awaitUnread := func(c net.Conn, want int64) {
		t.Helper()
		end := time.Now().Add(10 * time.Second)
		for UnreadBytes(c) != want {
			if time.Now().After(end) {
				t.Fatalf("the socket holds %d bytes unread; want %d", UnreadBytes(c), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	awaitUnread(proxySide, 0)
	if _, err := app.Write(make([]byte, 3000)); err != nil {
		t.Fatal(err)
	}
	awaitUnread(proxySide, 3000)
	awaitUnread(unreadUnder{proxySide}, 3000)
	if _, err := io.ReadFull(proxySide, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	awaitUnread(unreadUnder{proxySide}, 2000)
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	if n := UnreadBytes(a); n != 0 {
		t.Fatalf("a connection with no socket under it holds %d bytes unread; want 0", n)
	}
}
