package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// TestRelay_AResetWriteStillDeliversTheGonePeersReply: a write that fails
// with a reset says the peer it went to is gone, but that peer's socket may
// still hold what it sent before its reset (a server's 413 before it resets
// an upload it will not read). The other forwarder reads that socket, so the
// relay must let it deliver the reply rather than stop it at once. Once the
// write took the reset, the kernel reports the socket's end to the read as
// EOF; from a peer known to be gone that EOF is a reset, so the other peer
// reads the reply and then a reset, not a FIN.
//
// The reset is real: the gone peer writes its reply, then closes with
// SO_LINGER 0, and the row waits for the relay's socket of it to reach
// TCP_CLOSE before the other peer writes, so the relay's write meets that
// reset. What is arranged is only the order: the relay's read of the gone
// peer is held until the relay has acted on the failed write (the
// forwarder that wrote has exited and run has handled it), which is an
// order a loaded host can give. On main the relay's act was to stop the
// reading forwarder and nudge its read, so the held read then timed out and
// the reply was never read.
//
// Both directions: the destination resets (the client's upload meets it),
// and the client resets (the destination's answer meets it, and the request
// the client sent before its reset still reaches the destination).
func TestRelay_AResetWriteStillDeliversTheGonePeersReply(t *testing.T) {
	t.Run("the destination reset", func(t *testing.T) { testAGoneWriteStillDeliversTheReply(t, true) })
	t.Run("the client reset", func(t *testing.T) { testAGoneWriteStillDeliversTheReply(t, false) })
}

func testAGoneWriteStillDeliversTheReply(t *testing.T, destGone bool) {
	clientApp, srcProxy := relayTCPPair(t)
	dstProxy, destSvc := relayTCPPair(t)
	guard := hangGuard(t)
	ended := rowEnded(t)

	// gone is the peer that resets, goneProxy the relay's socket of it,
	// other the peer that is told. writer is the forwarder that writes to
	// the gone peer and meets its reset.
	gone, goneProxy, other := destSvc, dstProxy, clientApp
	writer := fakeconn.FromClient
	if !destGone {
		gone, goneProxy, other = clientApp, srcProxy, destSvc
		writer = fakeconn.FromDest
	}
	acted := make(chan struct{})
	src := &rowConn{Conn: srcProxy, ended: ended}
	dst := &rowConn{Conn: dstProxy, ended: ended}
	if destGone {
		dst.hold = acted
	} else {
		src.hold = acted
	}
	cfg := Config{Logger: zap.NewNop(), afterForwarderExit: func(d fakeconn.Direction) {
		if d == writer {
			close(acted)
		}
	}}
	r := New(cfg, src, connseq.NewUpstream(dst))
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_ = r.Run(context.Background())
		// The caller's close, once the relay has returned.
		_ = srcProxy.Close()
		_ = dstProxy.Close()
	}()

	reply := []byte("413 too large\r\n")
	if _, err := gone.Write(reply); err != nil {
		t.Fatalf("the gone peer writes its reply: %v", err)
	}
	resetAndWait(t, gone, goneProxy, guard)
	if _, err := other.Write([]byte("upload")); err != nil {
		t.Fatalf("the other peer writes: %v", err)
	}

	_ = other.SetReadDeadline(guard)
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(other, got); err != nil || string(got) != string(reply) {
		t.Fatalf("the other peer read %q, %v; want the gone peer's reply %q. The relay stopped the "+
			"forwarder that reads the gone peer when the write to it failed, so what that peer sent "+
			"before its reset was never read", got, err, reply)
	}
	if _, err := other.Read(make([]byte, 16)); !neterr.IsConnReset(err) {
		t.Fatalf("after the reply the other peer read %v, want a reset: the gone peer reset (the "+
			"write took it), so its EOF is not a half-close to pass on as a FIN", err)
	}
	select {
	case <-returned:
	case <-time.After(time.Until(guard)):
		t.Fatal("the relay never returned after the gone peer's end")
	}
}

// TestRelay_AResetWriteRacingTheGonePeersEOFStillResetsTheOtherPeer: the
// write takes the destination's reset, and the other forwarder reads the
// destination's queued reply and then its EOF before the failed write is
// recorded. That EOF is then taken for a half-close, and the client reads a
// FIN after the reply (see onForwarderExit in run); but once the failed
// write is recorded, the client's socket must still be set to reset when the
// caller closes it, so the reset is passed on. Without that, the client's
// connection ended on a FIN alone, as if the destination had finished.
//
// No clock decides it: the reading forwarder is let go once the write has
// failed, and the writing forwarder is held, after its write failed and
// before it returns, until the reading one has half-closed the client.
func TestRelay_AResetWriteRacingTheGonePeersEOFStillResetsTheOtherPeer(t *testing.T) {
	clientApp, srcProxy := relayTCPPair(t)
	dstProxy, destSvc := relayTCPPair(t)
	guard := hangGuard(t)
	ended := rowEnded(t)

	failed := make(chan struct{})
	halfClosed := make(chan struct{})
	var halfOnce sync.Once
	src := &rowConn{Conn: srcProxy, ended: ended,
		onCloseWrite: func() { halfOnce.Do(func() { close(halfClosed) }) }}
	dst := &rowConn{Conn: dstProxy, ended: ended, hold: failed,
		onWriteErr: func(error) { close(failed) }}
	cfg := Config{Logger: zap.NewNop(), beforeTee: func(d fakeconn.Direction) {
		if d != fakeconn.FromClient {
			return
		}
		select {
		case <-failed:
		default:
			return
		}
		select {
		case <-halfClosed:
		case <-ended:
		}
	}}
	r := New(cfg, src, connseq.NewUpstream(dst))
	returned := make(chan struct{})
	var linger *unix.Linger
	go func() {
		defer close(returned)
		_ = r.Run(context.Background())
		linger = soLinger(t, srcProxy)
		_ = srcProxy.Close()
		_ = dstProxy.Close()
	}()

	reply := []byte("413 too large\r\n")
	if _, err := destSvc.Write(reply); err != nil {
		t.Fatalf("the destination writes its reply: %v", err)
	}
	resetAndWait(t, destSvc, dstProxy, guard)
	if _, err := clientApp.Write([]byte("upload")); err != nil {
		t.Fatalf("the client writes the upload: %v", err)
	}

	_ = clientApp.SetReadDeadline(guard)
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(clientApp, got); err != nil || string(got) != string(reply) {
		t.Fatalf("the client read %q, %v; want the destination's reply %q", got, err, reply)
	}
	select {
	case <-returned:
	case <-time.After(time.Until(guard)):
		t.Fatal("the relay never returned after the destination's end")
	}
	if linger == nil || linger.Onoff == 0 || linger.Linger != 0 {
		t.Fatalf("after the relay returned the client's socket has SO_LINGER %+v, want on with 0: "+
			"the destination reset, so the caller's close must pass the reset on, not end the "+
			"client's connection on the FIN the racing EOF forwarded", linger)
	}
}

// TestRelay_AWriteAfterTheGonePeersFINStillDeliversItsReply: a destination
// that writes its reply and closes before the upload reaches it sends a FIN,
// and then resets the upload bytes that arrive afterwards. Linux reports that
// reset to the relay's next write as EPIPE, not ECONNRESET, because its
// socket had read the FIN. The reply and the FIN are still queued in that
// socket, so the reading forwarder must be left to deliver them: the client
// reads the reply, then the FIN (a real one, so not a reset), and its socket
// is reset when the caller closes it.
//
// The reset is real: the first upload write reaches the closed destination,
// which resets it, and the row waits for the relay's socket of it to reach
// TCP_CLOSE before the second write, which takes the EPIPE. The relay's read
// of the destination is held until the relay has acted on the failed write.
func TestRelay_AWriteAfterTheGonePeersFINStillDeliversItsReply(t *testing.T) {
	clientApp, srcProxy := relayTCPPair(t)
	dstProxy, destSvc := relayTCPPair(t)
	guard := hangGuard(t)
	ended := rowEnded(t)

	acted := make(chan struct{})
	writeErr := make(chan error, 1)
	src := &rowConn{Conn: srcProxy, ended: ended}
	dst := &rowConn{Conn: dstProxy, ended: ended, hold: acted, onWriteErr: func(err error) { writeErr <- err }}
	cfg := Config{Logger: zap.NewNop(), afterForwarderExit: func(d fakeconn.Direction) {
		if d == fakeconn.FromClient {
			close(acted)
		}
	}}
	r := New(cfg, src, connseq.NewUpstream(dst))
	returned := make(chan struct{})
	var linger *unix.Linger
	go func() {
		defer close(returned)
		_ = r.Run(context.Background())
		linger = soLinger(t, srcProxy)
		_ = srcProxy.Close()
		_ = dstProxy.Close()
	}()

	reply := []byte("413 too large\r\n")
	if _, err := destSvc.Write(reply); err != nil {
		t.Fatalf("the destination writes its reply: %v", err)
	}
	_ = destSvc.Close()
	if _, err := clientApp.Write([]byte("upload-1")); err != nil {
		t.Fatalf("the client writes the upload: %v", err)
	}
	waitTCPClose(t, dstProxy, guard)
	if _, err := clientApp.Write([]byte("upload-2")); err != nil {
		t.Fatalf("the client writes more of the upload: %v", err)
	}

	var werr error
	select {
	case werr = <-writeErr:
	case <-time.After(time.Until(guard)):
		t.Fatal("the relay's write to the destination never failed")
	}
	if !neterr.IsBrokenPipe(werr) {
		t.Fatalf("the relay's write to the destination failed with %v; the row needs EPIPE", werr)
	}

	_ = clientApp.SetReadDeadline(guard)
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(clientApp, got); err != nil || string(got) != string(reply) {
		t.Fatalf("the client read %q, %v; want the destination's reply %q. The relay stopped the "+
			"forwarder that reads the destination when the write to it failed with EPIPE, so the "+
			"reply queued before its FIN was never read", got, err, reply)
	}
	if _, err := clientApp.Read(make([]byte, 16)); !errors.Is(err, io.EOF) {
		t.Fatalf("after the reply the client read %v, want EOF: the destination closed before "+
			"its reset, so its FIN is passed on as a FIN", err)
	}
	select {
	case <-returned:
	case <-time.After(time.Until(guard)):
		t.Fatal("the relay never returned after the destination's end")
	}
	if linger == nil || linger.Onoff == 0 || linger.Linger != 0 {
		t.Fatalf("the client's socket has SO_LINGER %+v, want on with 0: the destination reset", linger)
	}
}

// resetAndWait closes peer with SO_LINGER 0, so that it sends a reset, and
// waits until the relay's socket of it, proxySide, has taken that reset.
func resetAndWait(t *testing.T, peer, proxySide net.Conn, guard time.Time) {
	t.Helper()
	if err := peer.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatalf("set SO_LINGER 0: %v", err)
	}
	_ = peer.Close()
	waitTCPClose(t, proxySide, guard)
}

// tcpClose is the kernel's TCP_CLOSE state, as TCP_INFO reports it.
const tcpClose = 7

// waitTCPClose waits until c's TCP state is TCP_CLOSE: its peer's reset has
// arrived. It waits on the event: poll(2) with no events asked for reports
// only POLLHUP and POLLERR, which tcp_poll sets once the socket is closed or
// has an error, and not the POLLIN of a queued reply or a FIN. The guard
// only turns a reset that never comes into a failure.
func waitTCPClose(t *testing.T, c net.Conn, guard time.Time) {
	t.Helper()
	raw, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("raw conn: %v", err)
	}
	for {
		var state uint8
		var perr, gerr error
		_ = raw.Control(func(fd uintptr) {
			ms := int(time.Until(guard) / time.Millisecond)
			if ms < 0 {
				ms = 0
			}
			fds := []unix.PollFd{{Fd: int32(fd)}}
			if _, perr = unix.Poll(fds, ms); perr == unix.EINTR {
				perr = nil
			}
			var info *unix.TCPInfo
			info, gerr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
			if gerr == nil {
				state = info.State
			}
		})
		if perr != nil || gerr != nil {
			t.Fatalf("poll %v, TCP_INFO %v", perr, gerr)
		}
		if state == tcpClose {
			return
		}
		if time.Now().After(guard) {
			t.Fatalf("the relay's socket never took its peer's reset (TCP state %d)", state)
		}
	}
}

// soLinger is c's SO_LINGER, or nil when it cannot be read.
func soLinger(t *testing.T, c net.Conn) *unix.Linger {
	raw, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Errorf("raw conn: %v", err)
		return nil
	}
	var l *unix.Linger
	_ = raw.Control(func(fd uintptr) {
		l, err = unix.GetsockoptLinger(int(fd), unix.SOL_SOCKET, unix.SO_LINGER)
	})
	if err != nil {
		t.Errorf("SO_LINGER: %v", os.NewSyscallError("getsockopt", err))
		return nil
	}
	return l
}
