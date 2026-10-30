//go:build linux

package util

import (
	"errors"
	"net"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// WatchPeerEnd watches the socket under conn for its peer's end, without
// taking anything from it, while nothing else reads conn: a proxy that
// forwards an answer to a request the peer sent whole reads only the other
// side, and would not otherwise learn that the peer went. onEnd is called
// once, from the watch's own goroutine, when what the socket holds for its
// reader is the peer's FIN (reset false) or a reset or other failure (reset
// true). Bytes the peer sends (its next request) end the watch without
// onEnd: they stay for conn's next read. A socket not to be found under conn
// (NetConn, at most 8 deep) is not watched.
//
// stop ends the watch and returns once it has, and onEnd with it if it was
// called, and reports whether onEnd was called. It may be called more than
// once. Nothing may read conn until stop has returned: the watch holds the
// socket's read side (a read waits for it). stop wakes the watch by conn's
// read deadline and then clears that deadline, so the caller must not hold
// one of its own across a watch.
func WatchPeerEnd(conn net.Conn, onEnd func(reset bool)) (stop func() (ended bool)) {
	rc, ok := rawSocket(conn)
	if !ok || onEnd == nil {
		return func() bool { return false }
	}
	done := make(chan struct{})
	var ended bool // written by the watch before done is closed
	go func() {
		defer close(done)
		var b [1]byte
		var n int
		var rerr error
		err := rc.Read(func(fd uintptr) bool {
			n, _, rerr = unix.Recvfrom(int(fd), b[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
			// Nothing yet: wait for the socket to become readable.
			return !errors.Is(rerr, unix.EAGAIN) && !errors.Is(rerr, unix.EINTR)
		})
		if err != nil || (rerr == nil && n > 0) {
			// Stopped (the deadline woke the wait), closed under the
			// watch, or the peer spoke: no end to tell.
			return
		}
		ended = true
		onEnd(rerr != nil)
	}()
	var once sync.Once
	return func() bool {
		once.Do(func() {
			_ = conn.SetReadDeadline(aLongTimeAgo)
			<-done
			_ = conn.SetReadDeadline(time.Time{})
		})
		return ended
	}
}
