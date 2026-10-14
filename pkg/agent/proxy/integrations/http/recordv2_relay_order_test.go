//go:build linux

package http

import (
	"context"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
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
	r := relay.New(relay.Config{Logger: zap.NewNop()}, c.src, connseq.NewUpstream(c.dst))
	ctx, cancel := context.WithCancel(context.Background())
	relayDone := make(chan struct{})
	go func() { defer close(relayDone); _ = r.Run(ctx) }()
	// The relay carries the connection from its first byte: its session is
	// never one the capture joined mid-way (JoinedMidConnection).
	sess := &supervisor.Session{
		ClientStream: r.ClientStream(), DestStream: r.DestStream(), Mocks: c.mocks, Mgr: c.mgr, Orphans: c.spans,
		Logger: zap.New(zapcore.NewTee(zaptest.NewLogger(t).Core(), c.core)), ClientConnID: "relayed-1",
		Ctx: context.Background(),
	}
	go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(context.Background(), sess) }()
	t.Cleanup(func() {
		cancel()
		_ = c.app.Close()
		_ = c.dest.Close()
		<-relayDone
	})
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

// relayed reads len(b) bytes at r, as the relay delivers them.
func relayed(t *testing.T, r net.Conn, b string) {
	t.Helper()
	buf := make([]byte, len(b))
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
// never got one.
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
		mocks := c.end(t)
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
// connection, where it is recorded) is not suppressed.
func TestRecordV2_ThroughTheRelayA408BeforeTheFirstRequestAnswersNoRequest(t *testing.T) {
	t.Parallel()
	for i := 0; i < rounds; i++ {
		c := newRelayedConn(t)
		write(t, c.dest, c.dst, idleTimeout)
		write(t, c.app, c.src, jdkRequest(7))
		c.start(t)
		relayed(t, c.dest, jdkRequest(7))
		mocks := c.end(t)
		if len(mocks) != 0 {
			t.Fatalf("round %d: recorded %d mock(s), the first %s -> %q; want none: the request got no answer", i,
				len(mocks), mocks[0].Spec.HTTPReq.URL, mocks[0].Spec.HTTPResp.Body)
		}
		if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 || c.droppedLines() != 1 {
			t.Fatalf("round %d: left out %v (%d counted, %d Debug lines); want nothing left out and a Debug line for the 408",
				i, s, c.mgr.MocksLeftOut(), c.droppedLines())
		}
	}
}
