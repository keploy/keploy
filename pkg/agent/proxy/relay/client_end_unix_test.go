//go:build linux || darwin

package relay

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

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
		s := tcpSocket(t, clientSocketOf(proxyClient, zap.NewNop()))
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
