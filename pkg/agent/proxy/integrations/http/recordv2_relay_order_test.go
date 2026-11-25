//go:build linux || darwin || windows

package http

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// These tests record through the proxy's relay on real sockets, and nothing in
// them runs in lockstep: the destination's bytes and the client's wait in the
// kernel until the relay's two forwarders read them, in whatever order the
// scheduler runs them. Which of the two a forwarder reads first decides
// nothing: a chunk is numbered by what the destination had sent the proxy when
// the client's chunk was read (connseq.Upstream).

// relayedConn is an application and a destination connected through the
// proxy's relay, with recordV2 recording the relay's capture.
type relayedConn struct {
	app, dest net.Conn
	src, dst  net.Conn // the proxy's ends
	mocks     chan *models.Mock
	spans     *spanLog
	mgr       *syncMock.SyncMockManager
	core      zapcore.Core
	logs      *observer.ObservedLogs
	done      chan error
	// wrapSrc, when set, is the proxy's end of the client connection as
	// the relay is handed it, over src; wrapDst, the destination's, over
	// the connseq.Upstream the relay reads dst through.
	wrapSrc, wrapDst func(net.Conn) net.Conn
	// sess is the session recordV2 records; relayDone is closed once the
	// relay has ended.
	sess      *supervisor.Session
	relayDone chan struct{}
	// stopRelay cancels the relay's context, as the recording's stop does.
	stopRelay context.CancelFunc
	// grace is the relay's HalfCloseGrace (0: its default).
	grace time.Duration
	// stopBegun is whether the recording's stop has begun as the relay reads
	// an end (Session.MarkPeerEndedBy, EndAtStop): set, as keploy record's
	// client says it is stopping before it stops the application.
	stopBegun atomic.Bool
	// clientEnd is the end that records no fault (a supervisor.NoFaultEnd)
	// the relay reads the client's end as before the stop has begun, as the
	// proxy's peerEndMarker reads it of the hooks: EndAtExit where they saw
	// the client's process exit on its own, EndUnjudged where they cannot
	// say how it released the connection (the macOS and Windows hooks).
	clientEnd atomic.Uint32
	// endHook, when set, runs as the relay reads a side's end, on that side's
	// forwarder, and marks it on the session by calling mark: it orders the
	// two ends' marks as a busy host's scheduler can.
	endHook func(dir fakeconn.Direction, mark func())
}

// loopback returns the two ends of a loopback TCP connection.
func loopback(t *testing.T) (dialed, accepted net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	got := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		got <- c
	}()
	if dialed, err = net.Dial("tcp", l.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if accepted = <-got; accepted == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = dialed.Close(); _ = accepted.Close() })
	return dialed, accepted
}

// newRelayedConn connects an application and a destination through the
// relay; start runs the relay and recordV2.
func newRelayedConn(t *testing.T) *relayedConn {
	t.Helper()
	app, src := loopback(t)
	// The count from the accept, as the proxy puts it on every client it
	// accepts (relay.CountClientWrites): Windows' reset of the client reads
	// what was written to it from it.
	src = relay.CountClientWrites(src)
	dst, dest := loopback(t)
	core, logs := observer.New(zapcore.DebugLevel)
	return &relayedConn{app: app, dest: dest, src: src, dst: dst, mocks: make(chan *models.Mock, 8), spans: &spanLog{},
		mgr: syncMock.New(nil), core: core, logs: logs, done: make(chan error, 1)}
}

func (c *relayedConn) start(t *testing.T) {
	t.Helper()
	c.startRelay(t)
	c.startParser(t)
}

// startRelay starts the relay alone; recordV2 starts later (startParser), as
// a parser running behind its capture reads what it was handed later.
func (c *relayedConn) startRelay(t *testing.T) {
	t.Helper()
	// The relay carries the connection from its first byte: its session is
	// never one the capture joined mid-way (JoinedMidConnection). It ends the
	// destination's stream where the client stopped taking it, and says so on
	// the session, as it says the client ended its side before the
	// destination's end reached it, as the proxy wires them
	// (recordViaSupervisor).
	c.sess = &supervisor.Session{
		Mocks: c.mocks, Mgr: c.mgr, Orphans: c.spans,
		Logger: zap.New(zapcore.NewTee(zaptest.NewLogger(t).Core(), c.core)), ClientConnID: "relayed-1",
		Ctx: context.Background(),
	}
	src := c.src
	if c.wrapSrc != nil {
		src = c.wrapSrc(src)
	}
	var dst net.Conn = connseq.NewUpstream(c.dst)
	if c.wrapDst != nil {
		dst = c.wrapDst(dst)
	}
	mark := func(dir fakeconn.Direction, reset bool) {
		why := supervisor.EndMayBeFault
		if c.stopBegun.Load() {
			why = supervisor.EndAtStop
		} else if dir == fakeconn.FromClient {
			why = supervisor.NoFaultEnd(c.clientEnd.Load())
		}
		c.sess.MarkPeerEndedBy(dir, reset, why)
	}
	r := relay.New(relay.Config{Logger: zap.NewNop(), HalfCloseGrace: c.grace, ClientStopped: c.sess.MarkEndedWhereClientStopped, PeerEnded: func(dir fakeconn.Direction, reset bool) {
		if c.endHook == nil {
			mark(dir, reset)
			return
		}
		c.endHook(dir, func() { mark(dir, reset) })
	}, ClientEndedBeforeDest: c.sess.MarkClientEndedBeforeDest}, src, dst)
	c.sess.ClientStream, c.sess.DestStream = r.ClientStream(), r.DestStream()
	ctx, cancel := context.WithCancel(context.Background())
	c.stopRelay = cancel
	c.relayDone = make(chan struct{})
	relayDone := c.relayDone
	go func() { defer close(relayDone); _ = r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = c.app.Close()
		_ = c.dest.Close()
		<-relayDone
	})
}

// startParser starts recordV2 on the relay's capture (startRelay).
func (c *relayedConn) startParser(t *testing.T) {
	t.Helper()
	go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(context.Background(), c.sess) }()
}

// received is the socket c's count of the bytes it has received, read or not.
func received(t *testing.T, c net.Conn) uint64 {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got uint64
	if err := rc.Control(func(fd uintptr) {
		if _, n, ok := tcpInfoOf(fd); ok {
			got = n
		}
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// write writes b from w and waits until the proxy's end of the connection,
// at, has received it: it is at the proxy, read by the relay or not.
func write(t *testing.T, w, at net.Conn, b string) {
	t.Helper()
	want := received(t, at) + uint64(len(b))
	if _, err := w.Write([]byte(b)); err != nil {
		t.Fatal(err)
	}
	for limit := time.Now().Add(5 * time.Second); received(t, at) < want; {
		if time.Now().After(limit) {
			t.Fatalf("the proxy received %d of %d bytes in 5s", received(t, at), want)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// relayed reads len(b) bytes at r, as the relay delivers them, within 10 s:
// fewer bytes than b fail the row instead of hanging the package.
func relayed(t *testing.T, r net.Conn, b string) {
	t.Helper()
	_ = r.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = r.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, len(b))
	if n, err := io.ReadFull(r, buf); err != nil || string(buf) != b {
		t.Fatalf("relayed %q, %v; want %q", buf[:n], err, b)
	}
}

// end closes both peers and returns what recordV2 recorded.
func (c *relayedConn) end(t *testing.T) []*models.Mock {
	t.Helper()
	mocks, err := c.endErr(t)
	if err != nil {
		t.Fatalf("recordV2 = %v", err)
	}
	return mocks
}

// endErr is end for a connection recordV2 may stop on: it returns the mocks
// recorded and what recordV2 returned.
func (c *relayedConn) endErr(t *testing.T) ([]*models.Mock, error) {
	t.Helper()
	_ = c.app.Close()
	_ = c.dest.Close()
	var err error
	select {
	case err = <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("recordV2 did not return within 10s of the connection's end")
	}
	var mocks []*models.Mock
	for {
		select {
		case m := <-c.mocks:
			mocks = append(mocks, m)
		default:
			return mocks, err
		}
	}
}

// closeUnread closes the application's side without reading what the relay
// wrote it (a FIN, or a reset when that is unread in its socket), and waits
// until the relay has read the end, so the client's end is the connection's
// first.
func closeUnread(t *testing.T, c *relayedConn) {
	t.Helper()
	_ = c.app.Close()
	awaitPeerEnd(t, c, fakeconn.FromClient)
}
