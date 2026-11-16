//go:build linux || darwin

package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
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
	select {
	case s := <-run.streams:
		if s != want {
			t.Fatalf("the destination's stream carried %q, want %q", s, want)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the destination's stream did not end")
	}
	select {
	case <-run.done:
	case <-time.After(15 * time.Second):
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
	case reset && (len(run.told) != 1 || !errors.Is(run.told[0], syscall.EPIPE) && !errors.Is(run.told[0], syscall.ECONNRESET)):
		t.Fatalf("ClientEndError was told %v, want once, the client's reset (EPIPE or ECONNRESET)", run.told)
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
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if got, err := io.ReadAll(server); err != nil || string(got) != "req" {
		t.Fatalf("the destination read %q, %v, want the request and the FIN", got, err)
	}
}

// A half-closed client whose stack took the answer (so it is teed), and whose
// application then closes its socket on it unread: the reset comes after the
// relay read the client's FIN, so PeerEnded says FIN, and the relay hands the
// reset the socket holds on as it ends. With more from the destination after
// the reset, the relay's Write of it fails and takes the error off the socket:
// that Write's error is handed on. A client that read the answer and closed
// is told of no error.
func TestAClientThatResetsAfterItsFINHasTheResetHandedOn(t *testing.T) {
	t.Parallel()
	const answer = "the answer, which the half-closed client's stack takes"
	for _, tc := range []struct {
		name        string
		read, after bool
	}{
		{name: "closed unread, nothing after"},
		{name: "closed unread, more after", after: true},
		{name: "read, then closed", read: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			run := runClientEnd(t, &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, proxyServer)
			halfClosedRequest(t, app, server)
			if _, err := server.Write([]byte(answer)); err != nil {
				t.Fatal(err)
			}
			for limit := time.Now().Add(5 * time.Second); bytesAcked(t, proxyClient) < uint64(len(answer)); {
				if time.Now().After(limit) {
					t.Fatal("the client's stack did not take the answer in 5s")
				}
				time.Sleep(50 * time.Microsecond)
			}
			if tc.read {
				_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, err := io.ReadFull(app, make([]byte, len(answer))); err != nil {
					t.Fatal(err)
				}
			}
			_ = app.Close()
			if !tc.read {
				awaitTCPState(t, proxyClient, stateClosed, "the client resets the answer it closed on unread")
			}
			if tc.after {
				if _, err := server.Write([]byte("more")); err != nil {
					t.Fatal(err)
				}
			}
			_ = server.Close()
			run.ended(t, answer, !tc.read)
		})
	}
}

// A closed client whose FIN reaches the proxy's socket between the relay's
// look and its Write of the destination's answer (a client that gave up on a
// call at its own deadline): the answer is teed, and the client resets it,
// after the FIN the relay reads as the client's end. The destination closes
// once it has the FIN, so the relay can end before the reset is in: it waits
// for the client's answer to every byte it wrote, and hands the reset on.
func TestAClosedClientThatResetsTheAnswerItsFINRacedHasTheResetHandedOn(t *testing.T) {
	t.Parallel()
	for round := 0; round < 20; round++ {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		release := make(chan struct{})
		close(release)
		c := &finInFlightConn{Conn: &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, release: release}
		c.hook = func() {
			_ = app.Close()
			awaitTCPState(t, proxyClient, stateCloseWait, "the closed client's FIN is in")
		}
		run := runClientEnd(t, c, proxyServer)
		if _, err := app.Write([]byte("req")); err != nil {
			t.Fatal(err)
		}
		_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(server, make([]byte, 3)); err != nil {
			t.Fatalf("round %d: the destination did not read the request: %v", round, err)
		}
		if _, err := server.Write([]byte("inflight")); err != nil {
			t.Fatal(err)
		}
		if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
			t.Fatalf("round %d: the destination read %d bytes, %v, want the forwarded FIN", round, len(rest), err)
		}
		_ = server.Close()
		run.ended(t, "inflight", true)
	}
}

// The same closed client, with the relay's read of the client's end held
// until the FIN and the client's reset of the answer are both in: Linux's
// read returns the FIN and leaves the reset on the socket; macOS's returns
// the reset, and the relay tells from the socket that the FIN came first. On
// both the relay reads the FIN as the client's end, forwards it, and hands the
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
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
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

// A half-closed client that took the answer (its stack acknowledged every
// byte, unread) and then resets, all before the relay reads its FIN: Linux's
// read returns the FIN and leaves the reset on the socket; macOS's returns the
// reset and takes it off the socket, which holds nothing then that TCP_INFO
// would say (every byte acknowledged): the relay keeps the reset its read
// took, reads the FIN as the client's end, and hands that reset on as it ends.
func TestAClientThatTookTheAnswerAndResetsBeforeTheReadHasTheResetItsReadTookHandedOn(t *testing.T) {
	t.Parallel()
	const answer = "the answer"
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
	if err := app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(server, make([]byte, 3)); err != nil {
		t.Fatalf("the destination did not read the request: %v", err)
	}
	awaitTCPState(t, proxyClient, stateCloseWait, "the client's FIN is in")
	if _, err := server.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	awaitUnread(t, app)
	for limit := time.Now().Add(5 * time.Second); bytesAcked(t, proxyClient) < uint64(len(answer)); {
		if time.Now().After(limit) {
			t.Fatal("the client's stack did not acknowledge the answer in 5s")
		}
		time.Sleep(time.Millisecond)
	}
	if err := app.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	awaitTCPState(t, proxyClient, stateClosed, "the client's reset is in")
	close(release)
	if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d bytes, %v, want the forwarded FIN", len(rest), err)
	}
	_ = server.Close()
	run.ended(t, answer, true)
}

// A client that resets with no FIN before (it closed with the answer unread)
// is read as a reset, also when the relay reads its end only once the reset
// is in, after a look found the connection open.
func TestAClientThatResetsWithNoFINIsReadAsAResetWhenReadLate(t *testing.T) {
	t.Parallel()
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
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(server, make([]byte, 3)); err != nil {
		t.Fatalf("the destination did not read the request: %v", err)
	}
	if _, err := server.Write([]byte("an answer the client leaves unread")); err != nil {
		t.Fatal(err)
	}
	awaitUnread(t, app)
	_ = app.Close()
	awaitTCPState(t, proxyClient, stateClosed, "the client closed with the answer unread")
	// The destination stays open until the relay has ended: the client's
	// reset, once read, ends it. A FIN from the destination now would race
	// the late read, and a forwarder that finds the relay torn down by the
	// other one's end returns without reading its own peer's (an end the
	// relay makes is not told: Config.PeerEnded).
	close(release)
	select {
	case <-run.done:
	case <-time.After(15 * time.Second):
		t.Fatal("the relay did not end")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.ends) != 1 || !run.ends[0] {
		t.Fatalf("PeerEnded was told of the client's end %v, want once, a reset", run.ends)
	}
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
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the relay did not end")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(destEnds) != 1 || !destEnds[0] {
		t.Fatalf("PeerEnded was told of the destination's end %v, want once, a reset", destEnds)
	}
}

// The socket under a closed client that the proxy writes a chunk to and then
// half-closes holds the client's reset of that chunk, whether it came before
// the proxy's FIN went (Linux and macOS keep its error) or after (Linux keeps
// ECONNRESET; macOS keeps none, and its TCP_INFO says it): handed on once.
// A client that took the chunk and acknowledged the proxy's FIN closes the
// connection with nothing to hand on.
func TestTheSocketHoldsTheResetOfAChunkAndAFINSentToAClosedClient(t *testing.T) {
	t.Parallel()
	for _, took := range []bool{false, true} {
		app, proxyClient := loopbackPair(t)
		if took {
			if err := app.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
		} else {
			_ = app.Close()
		}
		awaitTCPState(t, proxyClient, stateCloseWait, "the client's FIN is in")
		if _, err := proxyClient.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		// Linux's loopback can have the reset in before this, the
		// connection closed: the FIN cannot go then.
		if err := proxyClient.(*net.TCPConn).CloseWrite(); err != nil && took {
			t.Fatal(err)
		}
		if took {
			_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
			if got, err := io.ReadAll(app); err != nil || string(got) != "x" {
				t.Fatalf("the client read %q, %v, want the chunk and the proxy's FIN", got, err)
			}
		}
		awaitTCPState(t, proxyClient, stateClosed, "the connection is closed")
		s := clientSocketOf(proxyClient, zap.NewNop()).(*tcpClientSocket)
		err := s.pendingError(true)
		switch {
		case took && err != nil:
			t.Fatalf("pendingError = %v for a client that took every byte, want none", err)
		case !took && !errors.Is(err, syscall.ECONNRESET) && !errors.Is(err, syscall.EPIPE):
			t.Fatalf("pendingError = %v for a closed client sent a chunk and a FIN, want its reset", err)
		}
		if err := s.pendingError(true); err != nil {
			t.Fatalf("pendingError asked again = %v, want none: it was given", err)
		}
	}
}
