//go:build linux || darwin || windows

package relay

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// finSentRecorder is a client socket that says nothing but, when closed,
// that the connection closed before the proxy's FIN; it counts the relay's
// words that it sent the client its TCP FIN, and how often it was asked
// before one.
type finSentRecorder struct {
	closed bool
	// inner, when it can say whether the connection closed before the
	// proxy's FIN (finSentSocket: Windows' socket), answers for it.
	inner       clientSocket
	told, asked atomic.Int32
}

func (*finSentRecorder) look() clientLook        { return clientLook{said: clientUnknown} }
func (*finSentRecorder) written() (uint64, bool) { return 0, false }
func (s *finSentRecorder) proxyFINSent()         { s.told.Add(1) }
func (s *finSentRecorder) toldTimes() int32      { return s.told.Load() }
func (s *finSentRecorder) closedBeforeFIN() bool {
	s.asked.Add(1)
	if f, ok := s.inner.(finSentSocket); ok {
		return f.closedBeforeFIN()
	}
	return s.closed
}

// The client's socket is told of the proxy's FIN when the half-close of the
// client conn reached its TCP socket, through the wrappers over it, and not
// when a TLS layer half-closed (a close_notify alert, no FIN), or the conn is
// no TCP socket.
func TestTheClientsSocketIsToldOfTheProxysTCPFINAlone(t *testing.T) {
	t.Parallel()
	_, tcp := loopbackPair(t)
	pipeA, pipeB := net.Pipe()
	t.Cleanup(func() { _ = pipeA.Close(); _ = pipeB.Close() })
	for _, row := range []struct {
		name string
		c    net.Conn
		told int32
	}{
		{"the TCP socket", tcp, 1},
		{"a wrapper over it", &proxyutil.Conn{Conn: tcp, Reader: tcp, Logger: zap.NewNop()}, 1},
		{"a TLS layer over it", tls.Server(tcp, &tls.Config{}), 0},
		{"a pipe", pipeA, 0},
	} {
		s := &finSentRecorder{}
		(&Relay{client: s}).noteClientFINSent(row.c)
		if got := s.toldTimes(); got != row.told {
			t.Errorf("%s: the socket was told of the proxy's FIN %d times, want %d", row.name, got, row.told)
		}
	}
	// A socket that does not ask to be told is not, and the relay goes on.
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("telling a socket that does not ask of the proxy's FIN panicked: %v", p)
			}
		}()
		(&Relay{client: &scriptedSocket{}}).noteClientFINSent(tcp)
	}()
}

// selfWrapping is a wrapper that is its own NetConn: a cycle.
type selfWrapping struct{ net.Conn }

func (c *selfWrapping) NetConn() net.Conn { return c }

// A walk to the client's TCP socket over wrappers that loop back on
// themselves ends, and tells the socket nothing.
func TestTheWalkToTheClientsSocketEndsOnACycleOfWrappers(t *testing.T) {
	t.Parallel()
	s := &finSentRecorder{}
	done := make(chan struct{})
	go func() { defer close(done); (&Relay{client: s}).noteClientFINSent(&selfWrapping{}) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the walk over a cycle of wrappers did not end")
	}
	if got := s.toldTimes(); got != 0 {
		t.Fatalf("the socket was told of the proxy's FIN %d times over a cycle of wrappers, want 0", got)
	}
}

// Through a relay: the client's FIN forwarded to the destination tells the
// client's socket nothing; the destination's FIN forwarded to the client
// tells it once, asked first whether the connection had closed: one that
// had (a client's reset) is taken as over, not half-closed, and told nothing;
// the client reads the end of the stream at the relay's close.
func TestTheRelayTellsTheClientsSocketWhenItForwardsTheDestinationsFIN(t *testing.T) {
	t.Parallel()
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "closed first"}[closed], func(t *testing.T) {
			t.Parallel()
			relayTellsTheDestinationsFIN(t, closed)
		})
	}
}

func relayTellsTheDestinationsFIN(t *testing.T, closed bool) {
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	r := New(Config{Logger: zap.NewNop()}, proxyClient, connseq.NewUpstream(proxyServer))
	s := &finSentRecorder{closed: closed}
	r.client = s
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	go func() { _, _ = io.Copy(io.Discard, r.ClientStream()) }()
	go func() { _, _ = io.Copy(io.Discard, r.DestStream()) }()

	halfClosedRequest(t, app, server)
	if got := s.toldTimes(); got != 0 {
		t.Fatalf("the client's FIN forwarded to the destination told the client's socket of the proxy's FIN %d times", got)
	}
	if got := s.asked.Load(); got != 0 {
		t.Fatalf("the client's FIN forwarded to the destination asked the client's socket %d times", got)
	}
	if _, err := server.Write([]byte("answer")); err != nil {
		t.Fatal(err)
	}
	_ = server.(*net.TCPConn).CloseWrite()
	_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
	if got, err := io.ReadAll(app); err != nil || string(got) != "answer" {
		t.Fatalf("the client read %q, %v, want the answer and the proxy's FIN", got, err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the relay did not end")
	}
	want := int32(1)
	if closed {
		want = 0
	}
	if got := s.toldTimes(); got != want {
		t.Fatalf("the destination's FIN forwarded to the client told its socket %d times, want %d", got, want)
	}
	if got := s.asked.Load(); got != 1 {
		t.Fatalf("the destination's FIN forwarded to the client asked its socket %d times first, want once", got)
	}
}
