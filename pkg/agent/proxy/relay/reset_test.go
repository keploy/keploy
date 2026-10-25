package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
)

// TestRelay_APauseAfterAWriteResetDoesNotWaitForTheExitedForwarder: after
// a write reset the relay leaves the other forwarder running, with no clock,
// so that it delivers what the gone peer sent before its reset. A pause
// directive (a TLS upgrade, a client-hold release) then waits for both
// forwarders to park. The forwarder whose write failed has returned and can
// never park; it is off the live sockets for good, which is all a park
// promises, so the wait must count it as parked. Without that, it waited for
// a park that never came, and nothing else ended the wait: the relay stayed
// up after both peers had closed. (On main a write error closed the relay's
// stopping signal, which ends the handlers' wait; this row guards the exit
// with no clock that this change makes. It passes the wait a stopping
// channel that never closes, which is why it also fails on main's relay.)
//
// Both orders: the pause is raised after the forwarder has exited, and
// while it is between its failed write and its exit.
//
// No clock decides it: the pause is raised at those points, and the wait
// returns when the other forwarder parks. The guard only turns a hang into
// a failure.
func TestRelay_APauseAfterAWriteResetDoesNotWaitForTheExitedForwarder(t *testing.T) {
	t.Run("the pause comes after the exit", func(t *testing.T) { testAPauseAfterAWriteReset(t, false) })
	t.Run("the pause comes before the exit", func(t *testing.T) { testAPauseAfterAWriteReset(t, true) })
}

func testAPauseAfterAWriteReset(t *testing.T, pauseFirst bool) {
	clientApp, srcProxy := relayTCPPair(t)
	dstProxy, destSvc := relayTCPPair(t)
	guard := hangGuard(t)
	ended := rowEnded(t)

	exited := make(chan struct{})
	failed := make(chan struct{})
	paused := make(chan struct{})
	var failOnce sync.Once
	cfg := Config{Logger: zap.NewNop(), afterForwarderExit: func(d fakeconn.Direction) {
		if d == fakeconn.FromClient {
			close(exited)
		}
	}}
	if pauseFirst {
		// Hold the forwarder between its failed write and its exit until
		// the pause is up.
		cfg.beforeTee = func(d fakeconn.Direction) {
			if d != fakeconn.FromClient {
				return
			}
			failOnce.Do(func() { close(failed) })
			select {
			case <-paused:
			case <-ended:
			}
		}
	}
	r := New(cfg, &rowConn{Conn: srcProxy}, connseq.NewUpstream(&rowConn{Conn: dstProxy, failWith: syscall.ECONNRESET}))
	// A failed row must not leave the parked survivor behind.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_ = r.Run(ctx)
		_ = srcProxy.Close()
		_ = dstProxy.Close()
	}()

	// The upload's write to the destination fails with a (simulated) reset.
	if _, err := clientApp.Write([]byte("upload")); err != nil {
		t.Fatalf("write the upload: %v", err)
	}
	wait := exited
	if pauseFirst {
		wait = failed
	}
	select {
	case <-wait:
	case <-time.After(time.Until(guard)):
		t.Fatal("the forwarder's write never failed")
	}

	// The handler's pause, as handleReleaseClient and handleUpgradeTLS raise it.
	r.beginPause()
	if pauseFirst {
		close(paused)
	}
	parked := make(chan struct{})
	go func() {
		defer close(parked)
		r.waitForwardersParked(ctx, make(chan struct{}))
	}()
	select {
	case <-parked:
	case <-time.After(time.Until(guard)):
		t.Fatal("a pause after a write reset waited for the park of the forwarder that had " +
			"already exited, and nothing ends that wait")
	}
	r.endPause()

	// Both peers close; the relay returns.
	_ = destSvc.Close()
	_ = clientApp.Close()
	select {
	case <-returned:
	case <-time.After(time.Until(guard)):
		t.Fatal("the relay never returned after both peers closed")
	}
}

// TestRelay_AVanishedPeerIsPassedOnAsAReset: a peer that vanished without a
// reset (its host went down, or the network between dropped it) ends the
// relay's read of it with the error the kernel gives up with, ETIMEDOUT or
// EHOSTUNREACH. That peer is gone in both directions, as after a reset, so
// the other peer is reset too: it reads ECONNRESET (with no proxy its own
// socket would fail with the vanished peer's errno; a reset is the closest
// end the proxy can give it). A FIN instead would tell it the cut stream
// was complete.
//
// No clock decides it: the vanished peer's read fails when the test says
// so, and the other peer's read returns on the FIN or the reset the relay
// gives it (the reset when the caller closes the relay's sockets).
func TestRelay_AVanishedPeerIsPassedOnAsAReset(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ETIMEDOUT, syscall.EHOSTUNREACH} {
		for _, destVanishes := range []bool{true, false} {
			name := "the client vanished: " + errno.Error()
			if destVanishes {
				name = "the destination vanished: " + errno.Error()
			}
			t.Run(name, func(t *testing.T) {
				clientApp, srcProxy := relayTCPPair(t)
				dstProxy, destSvc := relayTCPPair(t)
				gone := make(chan struct{})
				var src, dst net.Conn = srcProxy, dstProxy
				waiting := clientApp
				if destVanishes {
					dst = &vanishedConn{Conn: dstProxy, gone: gone, errno: errno}
				} else {
					src = &vanishedConn{Conn: srcProxy, gone: gone, errno: errno}
					waiting = destSvc
				}
				// A hang guard only, never the verdict's clock: the read
				// below returns on the other peer's FIN or reset.
				guard := hangGuard(t)
				_ = waiting.SetReadDeadline(guard)

				r := New(Config{Logger: zap.NewNop()}, src, connseq.NewUpstream(dst))
				returned := make(chan struct{})
				var runErr error
				go func() {
					defer close(returned)
					runErr = r.Run(context.Background())
					// The caller's close, once the relay has returned.
					_ = srcProxy.Close()
					_ = dstProxy.Close()
				}()
				close(gone)
				_, err := waiting.Read(make([]byte, 1))
				if errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatal("the relay never returned after a peer vanished, so the other peer was never told")
				}
				if !neterr.IsConnReset(err) {
					t.Fatalf("the other peer read %v, want a reset: its peer vanished, which ends the "+
						"connection both ways; a FIN tells it the cut stream was complete", err)
				}
				select {
				case <-returned:
				case <-time.After(time.Until(guard)):
					t.Fatal("the relay never returned after a peer vanished")
				}
				if !neterr.IsPeerVanished(runErr) {
					t.Errorf("the relay returned %v, want the vanished peer's error: it is a genuine "+
						"error, as a reset is, not the benign timeout of the relay's own deadline nudge", runErr)
				}
			})
		}
	}
}

// vanishedConn is a socket whose peer vanished without a reset, as the
// kernel reports it once gone is closed: one read fails with the errno it
// gave up with, and every read after that returns EOF.
type vanishedConn struct {
	net.Conn
	gone     chan struct{}
	errno    syscall.Errno
	reported atomic.Bool
}

func (c *vanishedConn) Read([]byte) (int, error) {
	<-c.gone
	if c.reported.Swap(true) {
		return 0, io.EOF
	}
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", c.errno)}
}

// rowConn is a relay row's socket: a *net.TCPConn with the relay's
// CloseWrite and SetLinger passed through to it (net.Conn embedded as an
// interface promotes only net.Conn's methods, and a row about whether the
// relay forwards a FIN or passes a reset on would pass for the wrong reason
// without them).
//   - hold, when set, holds every Read until it is closed (or the test
//     ends), the order a loaded host can give the read.
//   - failWith, when set, fails every Write with that errno without touching
//     the socket: a SIMULATED gone peer, for the errnos a loopback socket
//     cannot be made to give.
//   - onWriteErr runs once, with the first Write error (simulated or real).
//   - onCloseWrite runs on each CloseWrite.
type rowConn struct {
	net.Conn
	hold         <-chan struct{}
	ended        <-chan struct{}
	failWith     syscall.Errno
	onWriteErr   func(error)
	onCloseWrite func()
	errOnce      sync.Once
}

func (c *rowConn) Read(p []byte) (int, error) {
	if c.hold != nil {
		select {
		case <-c.hold:
		case <-c.ended:
			return 0, errors.New("test: the row ended with this read still held")
		}
	}
	return c.Conn.Read(p)
}

func (c *rowConn) Write(p []byte) (int, error) {
	var n int
	var err error
	if c.failWith != 0 {
		err = &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", c.failWith)}
	} else {
		n, err = c.Conn.Write(p)
	}
	if err != nil && c.onWriteErr != nil {
		c.errOnce.Do(func() { c.onWriteErr(err) })
	}
	return n, err
}

func (c *rowConn) CloseWrite() error {
	if c.onCloseWrite != nil {
		c.onCloseWrite()
	}
	return c.Conn.(*net.TCPConn).CloseWrite()
}

func (c *rowConn) SetLinger(sec int) error { return c.Conn.(*net.TCPConn).SetLinger(sec) }

// rowEnded is closed when the test ends, so a read a row still holds gives
// up and the relay's goroutines return.
func rowEnded(t *testing.T) <-chan struct{} {
	ended := make(chan struct{})
	t.Cleanup(func() { close(ended) })
	return ended
}

// hangGuard is when a row gives up on a wait that should end on an event. It
// turns a hang into a failure and is never a verdict's clock: a row passes
// or fails on what the relay did, not on how long it took.
func hangGuard(t *testing.T) time.Time {
	if dl, ok := t.Deadline(); ok {
		return time.Now().Add(time.Until(dl) / 4)
	}
	// -timeout 0: no deadline, but a hang should still fail the row.
	return time.Now().Add(2 * time.Minute)
}

func relayTCPPair(t *testing.T) (dialed, accepted net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	d, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	a := <-ch
	if a == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = d.Close(); _ = a.Close() })
	return d, a
}
