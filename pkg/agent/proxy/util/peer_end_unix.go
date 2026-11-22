//go:build linux || darwin

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
// onEnd: they stay for conn's next read.
//
// watched is false when there is no socket under conn (conn, then each
// NetConn, at most 8 deep, none a syscall.Conn): nothing is watched, and
// stop reports no end. A wrapper on the way must give the conn it wraps
// (NetConn), or the peer's end cannot be watched through it.
//
// stop ends the watch and returns once it has, and onEnd with it if it was
// called, and reports whether onEnd was called. It may be called more than
// once. Nothing may read conn until stop has returned: the watch holds the
// socket's read side (a read waits for it). stop wakes the watch by the read
// deadline of the conn that gives the socket (a wrapper above it may make
// its own a no-op) and then clears that deadline, so the caller must not
// hold one of its own across a watch.
func WatchPeerEnd(conn net.Conn, onEnd func(reset bool)) (stop func() (ended bool), watched bool) {
	stop, _, watched = watchPeerEnd(conn, onEnd)
	return stop, watched
}

// watchPeerEnd is WatchPeerEnd, with done, closed once the watch has ended
// (by stop, or by itself: the peer spoke, or ended), nil when nothing is
// watched.
func watchPeerEnd(conn net.Conn, onEnd func(reset bool)) (stop func() (ended bool), done <-chan struct{}, watched bool) {
	sock, rc, ok := socketOf(conn)
	if !ok || onEnd == nil {
		return noPeerEnd, nil, false
	}
	over := make(chan struct{})
	var ended bool // written by the watch before over is closed
	go func() {
		defer close(over)
		defer func() { _ = recover() }() // a watch must never take the agent down
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
			_ = sock.SetReadDeadline(aLongTimeAgo)
			<-over
			_ = sock.SetReadDeadline(time.Time{})
		})
		return ended
	}, over, true
}

func noPeerEnd() bool { return false }
