//go:build linux || darwin || windows

package relay

// The relay over real loopback sockets on every OS keploy records on: the
// reads that end in a peer's reset, and the helpers the socket rows share.

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testdeadline"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
)

// clientEndRun is a relay between a client and a destination over loopback
// sockets, with what it told of the client's end.
type clientEndRun struct {
	r       *Relay
	streams chan string
	done    chan struct{}
	mu      sync.Mutex
	told    []error
	ends    []bool // the client's ends PeerEnded was told of: reset or not
}

func runClientEnd(t *testing.T, client net.Conn, proxyServer net.Conn) *clientEndRun {
	t.Helper()
	run := &clientEndRun{streams: make(chan string, 1), done: make(chan struct{})}
	run.r = New(Config{
		Logger: zap.NewNop(),
		ClientEndError: func(err error) {
			run.mu.Lock()
			run.told = append(run.told, err)
			run.mu.Unlock()
		},
		PeerEnded: func(dir fakeconn.Direction, reset bool) {
			if dir == fakeconn.FromClient {
				run.mu.Lock()
				run.ends = append(run.ends, reset)
				run.mu.Unlock()
			}
		},
	}, client, connseq.NewUpstream(proxyServer))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { defer close(run.done); _ = run.r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-run.done })
	go func() { go streamOf(t, run.r.ClientStream()); run.streams <- streamOf(t, run.r.DestStream()) }()
	return run
}

// ended waits for the destination's stream to end and the relay to return,
// and checks the stream carried want, PeerEnded was told the client sent its
// FIN (no reset), and ClientEndError was told a reset exactly when reset.
func (run *clientEndRun) ended(t *testing.T, want string, reset bool) {
	t.Helper()
	streamExpired, streamStop := testdeadline.Before(t)
	defer streamStop()
	select {
	case s := <-run.streams:
		if s != want {
			t.Fatalf("the destination's stream carried %q, want %q", s, want)
		}
	case <-streamExpired:
		t.Fatal("the destination's stream did not end")
	}
	doneExpired, doneStop := testdeadline.Before(t)
	defer doneStop()
	select {
	case <-run.done:
	case <-doneExpired:
		t.Fatal("the relay did not end")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.ends) != 1 || run.ends[0] {
		t.Fatalf("PeerEnded was told of the client's end %v, want once, its FIN", run.ends)
	}
	switch {
	case !reset && len(run.told) != 0:
		t.Fatalf("ClientEndError was told %v, want nothing", run.told)
	case reset && (len(run.told) != 1 || !errors.Is(run.told[0], syscall.EPIPE) && !neterr.IsPeerReset(run.told[0])):
		// A write's error is handed on as the OS gave it (Windows:
		// WSAECONNABORTED), which the session keeps as a reset.
		t.Fatalf("ClientEndError was told %v, want once, the client's reset (EPIPE or a reset)", run.told)
	}
}

// halfClosedRequest has the client send a request and half-close, and the
// destination read both.
func halfClosedRequest(t *testing.T, app, server net.Conn) {
	t.Helper()
	if _, err := app.Write([]byte("req")); err != nil {
		t.Fatal(err)
	}
	if err := app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	testdeadline.SetRead(t, server)
	if got, err := io.ReadAll(server); err != nil || string(got) != "req" {
		t.Fatalf("the destination read %q, %v, want the request and the FIN", got, err)
	}
}

// A client that resets with no FIN before (it closed with the answer unread)
// is read as a reset, also when the relay reads its end only once the reset
// is in, after a look found the connection open.
func TestAClientThatResetsWithNoFINIsReadAsAResetWhenReadLate(t *testing.T) {
	t.Parallel()
	lateReadOfAResetWithNoFIN(t, func(app net.Conn) {
		awaitUnread(t, app)
		_ = app.Close()
	})
}

// The same for a client that aborts (SO_LINGER 0) with every byte taken: on
// Windows its reset makes the late read fail with WSAECONNRESET, a reset.
func TestAClientThatAbortsWithNoFINIsReadAsAResetWhenReadLate(t *testing.T) {
	t.Parallel()
	lateReadOfAResetWithNoFIN(t, func(app net.Conn) {
		if _, err := io.ReadFull(app, make([]byte, len(lateAnswer))); err != nil {
			t.Fatalf("the client did not read the answer: %v", err)
		}
		if err := app.(*net.TCPConn).SetLinger(0); err != nil {
			t.Fatal(err)
		}
		_ = app.Close()
	})
}

// lateAnswer is the destination's answer in lateReadOfAResetWithNoFIN.
const lateAnswer = "the destination's answer"

// lateReadOfAResetWithNoFIN has the client send a request, the destination
// answer it, and then the client end the connection with a reset and no FIN
// (end), with the relay's read of the client's end held until the reset is in:
// the relay must tell the client's end once, as a reset.
func lateReadOfAResetWithNoFIN(t *testing.T, end func(app net.Conn)) {
	t.Helper()
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	release := make(chan struct{})
	c := &finInFlightConn{Conn: &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, release: release}
	run := runClientEnd(t, c, proxyServer)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	if _, err := app.Write([]byte("req")); err != nil {
		t.Fatal(err)
	}
	testdeadline.SetRead(t, server)
	if _, err := io.ReadFull(server, make([]byte, 3)); err != nil {
		t.Fatalf("the destination did not read the request: %v", err)
	}
	if _, err := server.Write([]byte(lateAnswer)); err != nil {
		t.Fatal(err)
	}
	end(app)
	awaitTCPState(t, proxyClient, stateClosed, "the client reset with no FIN")
	// The destination stays open until the relay has ended: the client's
	// reset, once read, ends it. A FIN from the destination now would race
	// the late read, and a forwarder that finds the relay torn down by the
	// other one's end returns without reading its own peer's (an end the
	// relay makes is not told: Config.PeerEnded).
	close(release)
	expired, stop := testdeadline.Before(t)
	defer stop()
	select {
	case <-run.done:
	case <-expired:
		t.Fatal("the relay did not end")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.ends) != 1 || !run.ends[0] {
		t.Fatalf("PeerEnded was told of the client's end %v, want once, a reset", run.ends)
	}
}

// A closed client whose FIN races the answer, with the relay's read of the
// client's end held until the FIN and the client's reset of the answer are
// both in: Linux's read returns the FIN and leaves the reset on the socket;
// macOS's returns the reset, and the relay tells from the socket that the FIN
// came first; Windows' returns the reset with WSAECONNABORTED, the code it
// gives a reset that came after the client's FIN (resetAfterFIN). On each
// the relay reads the FIN as the client's end, forwards it, and hands the
// reset on as it ends.
func TestAClosedClientWhoseFINAndResetAreBothInBeforeTheReadHasTheFINRead(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	release := make(chan struct{})
	c := &finInFlightConn{Conn: &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, release: release}
	c.hook = func() {
		_ = app.Close()
		awaitTCPState(t, proxyClient, stateCloseWait, "the closed client's FIN is in")
	}
	run := runClientEnd(t, c, proxyServer)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	if _, err := app.Write([]byte("req")); err != nil {
		t.Fatal(err)
	}
	testdeadline.SetRead(t, server)
	if _, err := io.ReadFull(server, make([]byte, 3)); err != nil {
		t.Fatalf("the destination did not read the request: %v", err)
	}
	if _, err := server.Write([]byte("inflight")); err != nil {
		t.Fatal(err)
	}
	awaitTCPState(t, proxyClient, stateClosed, "the closed client resets the answer")
	close(release)
	if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d bytes, %v, want the forwarded FIN", len(rest), err)
	}
	_ = server.Close()
	run.ended(t, "inflight", true)
}

// A destination that resets the connection after the client's FIN is read
// as the destination's reset: only the client's reset is ever read as its
// FIN.
func TestADestinationsResetAfterTheClientsFINIsAReset(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	var mu sync.Mutex
	var destEnds []bool
	r := New(Config{
		Logger: zap.NewNop(),
		PeerEnded: func(dir fakeconn.Direction, reset bool) {
			if dir == fakeconn.FromDest {
				mu.Lock()
				destEnds = append(destEnds, reset)
				mu.Unlock()
			}
		},
	}, &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, connseq.NewUpstream(proxyServer))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	go func() { go streamOf(t, r.ClientStream()); streamOf(t, r.DestStream()) }()
	halfClosedRequest(t, app, server)
	awaitTCPState(t, proxyClient, stateCloseWait, "the client's FIN is in")
	if err := server.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	expired, stop := testdeadline.Before(t)
	defer stop()
	select {
	case <-done:
	case <-expired:
		t.Fatal("the relay did not end")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(destEnds) != 1 || !destEnds[0] {
		t.Fatalf("PeerEnded was told of the destination's end %v, want once, a reset", destEnds)
	}
}

// streamOf reads fc to its end and returns the bytes it carried.
func streamOf(t *testing.T, fc *fakeconn.FakeConn) string {
	t.Helper()
	var got []byte
	for {
		c, err := fc.ReadChunk()
		if err != nil {
			return string(got)
		}
		got = append(got, c.Bytes...)
	}
}

// awaitTCPState waits until the TCP state of the socket under c is want.
func awaitTCPState(t *testing.T, c net.Conn, want uint8, what string) {
	t.Helper()
	for limit := time.Now().Add(tcpSettle); tcpState(t, c) != want; {
		if time.Now().After(limit) {
			t.Fatalf("%s: the socket's state is %d after %v, want %d", what, tcpState(t, c), tcpSettle, want)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// finInFlightConn is the proxy's end of the client connection whose first
// Write first runs hook (after the relay's look before that Write), and whose
// Reads after the first wait for release.
type finInFlightConn struct {
	*proxyutil.Conn
	reads, writes atomic.Int32
	release       chan struct{}
	hook          func()
}

func (c *finInFlightConn) Read(p []byte) (int, error) {
	if c.reads.Add(1) > 1 {
		<-c.release
	}
	return c.Conn.Read(p)
}

func (c *finInFlightConn) Write(p []byte) (int, error) {
	if c.writes.Add(1) == 1 && c.hook != nil {
		c.hook()
	}
	return c.Conn.Write(p)
}

func (c *finInFlightConn) NetConn() net.Conn { return c.Conn }

// writesThenFails is the proxy's end of the client connection whose Writes
// after the first fail (a client that reset: write_error) or come up one
// byte short (short_write). It has no NetConn, so the relay asks the scripted
// socket the test gives it.
type writesThenFails struct {
	net.Conn
	short  bool
	writes atomic.Int32
}
