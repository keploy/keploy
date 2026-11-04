//go:build linux

package http

import (
	"context"
	"io"
	"net"
	"strings"
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
	"golang.org/x/sys/unix"
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
	}, ClientEndedBeforeDest: c.sess.MarkClientEndedBeforeDest, ClientReadAfterFIN: c.sess.MarkClientReadAfterFIN}, src, dst)
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
		if info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
			got = info.Bytes_received
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

// relayed reads len(b) bytes at r, as the relay delivers them, within 10s: a
// relay that never delivers them fails the row here, never by the package's
// timeout. r's read deadline is cleared again after.
func relayed(t *testing.T, r net.Conn, b string) {
	t.Helper()
	buf := make([]byte, len(b))
	_ = r.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = r.SetReadDeadline(time.Time{}) }()
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != b {
		t.Fatalf("relayed %q, %v; want %q", buf, err, b)
	}
}

// end closes both peers and returns what recordV2 recorded.
func (c *relayedConn) end(t *testing.T) []*models.Mock {
	t.Helper()
	_ = c.app.Close()
	_ = c.dest.Close()
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("recordV2 = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recordV2 did not return within 10s of the connection's end")
	}
	var mocks []*models.Mock
	for {
		select {
		case m := <-c.mocks:
			mocks = append(mocks, m)
		default:
			return mocks
		}
	}
}

// droppedLines counts the Debug lines for server bytes dropped with nothing
// left out for them: bytes that answer no request.
func (c *relayedConn) droppedLines() int {
	return c.logs.FilterMessageSnippet("dropped server bytes captured before the request after them").Len()
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

// refused408 checks the call a 408 raced in a round where the client closed
// without reading it (closeUnread), and returns the mocks without it. Either
// the relay wrote the 408 to the client before its close, and the 408 is in
// the capture, captured before the request and dropped with a Debug line: the
// end after it is the server's idle close, and nothing is recorded for the
// request (the client retries it on a new connection). Or it wrote it once the
// client had closed, which the client's stack refused: the 408 is no part of
// the capture (relay: client_took.go), the destination's stream ended where
// the client stopped, and the application closed with nothing read. The call
// is recorded as what it saw, a timeout before any answer, the last mock. One
// of the two, never both nor neither.
func refused408(t *testing.T, round int, c *relayedConn, mocks []*models.Mock, path string) []*models.Mock {
	t.Helper()
	dropped, stopped := c.droppedLines(), c.sess.EndedWhereClientStopped()
	if (dropped == 1) == stopped || dropped > 1 {
		t.Fatalf("round %d: %d Debug lines for the 408, the stream ended where the client stopped: %v; want either the "+
			"line or the stream ended there, not both", round, dropped, stopped)
	}
	if dropped == 1 {
		return mocks
	}
	if len(mocks) == 0 {
		t.Fatalf("round %d: nothing recorded for %s; want the call the application closed on with nothing read, as a "+
			"timeout", round, path)
	}
	m := mocks[len(mocks)-1]
	if m.Spec.HTTPReq == nil || !strings.HasSuffix(m.Spec.HTTPReq.URL, path) {
		t.Fatalf("round %d: the last mock is %v, want the call to %s", round, m.Spec.HTTPReq, path)
	}
	wantFault(t, m, models.Fault{Kind: models.FaultTimeout})
	return mocks[:len(mocks)-1]
}

// rounds is how many connections each test records: on the relay as it
// numbered chunks by the order its reads returned in, the 408 was taken for
// the request's answer in a few rounds of every hundred at the connection's
// start, and in tens mid-connection.
const rounds = 50

// Through the relay, as the proxy records: a server that times an idle
// keep-alive connection out sends a 408, and it reaches the proxy just before
// the client's next request does (the keep-alive idle-close race); the server
// closes without answering the request. The 408 reached the proxy before the
// request, so it is not its answer, and after the request before it, so it
// answers no request at all: no mock is recorded for the request (the client
// retries it on a new connection, where it is recorded), and nothing is left
// out, so no test case in flight is suppressed for it. Before, the request
// was recorded with the 408, a mock that replays a timeout for a request that
// never got one. The 408 marks the request (Session.ServerSpokeUnasked), so
// the end after it is not taken for a fault either.
func TestRecordV2_ThroughTheRelayA408SentBeforeTheNextRequestIsNotItsAnswer(t *testing.T) {
	t.Parallel()
	for i := 0; i < rounds; i++ {
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, jdkRequest(7))
		relayed(t, c.dest, jdkRequest(7))
		write(t, c.dest, c.dst, jdkResponse(7))
		relayed(t, c.app, jdkResponse(7))
		write(t, c.dest, c.dst, idleTimeout) // at the proxy, read by the relay or not
		write(t, c.app, c.src, jdkRequest(8))
		relayed(t, c.dest, jdkRequest(8))
		// In half the rounds the client reads the 408 before it closes, so the
		// parser has it: nothing is recorded for the request. In the others
		// it closes without (refused408).
		read := i%2 == 0
		if read {
			relayed(t, c.app, idleTimeout)
		} else {
			closeUnread(t, c)
		}
		mocks := c.end(t)
		if !read {
			mocks = refused408(t, i, c, mocks, "/orders/8")
		}
		assertPairs(t, mocks, 7)
		if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
			t.Fatalf("round %d: left out %v (%d counted), want nothing: the 408 answers no request", i, s, c.mgr.MocksLeftOut())
		}
	}
}

// Through the relay, as the proxy records: the proxy dialled the destination
// while it held the application's connect, and the connection idled past the
// server's request timeout before the application's first request. The
// server's 408 and the request are both waiting when the relay starts. The
// relay carries the connection from its first byte, so nothing was sent on it
// before the capture began: the 408 is the server's own, and answers no
// request. It is not the request's answer, and nothing is left out for it, so
// the test case that sent the request (which the application retries on a new
// connection, where it is recorded) is not suppressed. Nor is the end after it
// taken for a fault on the connection's first request: the 408 marks the
// request (Session.ServerSpokeUnasked).
func TestRecordV2_ThroughTheRelayA408BeforeTheFirstRequestAnswersNoRequest(t *testing.T) {
	t.Parallel()
	for i := 0; i < rounds; i++ {
		c := newRelayedConn(t)
		write(t, c.dest, c.dst, idleTimeout)
		write(t, c.app, c.src, jdkRequest(7))
		c.start(t)
		relayed(t, c.dest, jdkRequest(7))
		// In half the rounds the client reads the 408 before it closes, so the
		// parser has it, and says it dropped it: nothing is recorded or left
		// out. In the others it closes without (refused408).
		read := i%2 == 0
		if read {
			relayed(t, c.app, idleTimeout)
		} else {
			closeUnread(t, c)
		}
		mocks := c.end(t)
		if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
			t.Fatalf("round %d: left out %v (%d counted); want nothing left out", i, s, c.mgr.MocksLeftOut())
		}
		if read {
			if dropped, stopped := c.droppedLines(), c.sess.EndedWhereClientStopped(); dropped != 1 || stopped {
				t.Fatalf("round %d: %d Debug lines for the 408, the stream ended where the client stopped: %v; want "+
					"the line: the client read the 408", i, dropped, stopped)
			}
		} else {
			mocks = refused408(t, i, c, mocks, "/orders/7")
		}
		if len(mocks) != 0 {
			t.Fatalf("round %d: recorded %d mock(s), the first %s -> %q (fault %v); want none: the 408 came before the "+
				"request, so the end after it is the server's idle close", i, len(mocks), mocks[0].Spec.HTTPReq.URL,
				mocks[0].Spec.HTTPResp.Body, mocks[0].Spec.Fault)
		}
	}
}
