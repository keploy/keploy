//go:build linux || darwin

package relay

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// gatedConn is the proxy's end of the client connection, as handleConnection
// wraps it (util.Conn: NetConn reaches the socket), whose Reads after the first
// wait for release: the client's FIN can then reach the proxy's socket while
// the relay has not read it yet.
type gatedConn struct {
	*proxyutil.Conn
	reads   atomic.Int32
	release chan struct{}
}

func (g *gatedConn) Read(p []byte) (int, error) {
	if g.reads.Add(1) > 1 {
		<-g.release
	}
	return g.Conn.Read(p)
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

// notTakenLogged is how many bytes the relay said it did not capture because
// the client did not take them: the count on its line (0 without one).
func notTakenLogged(t *testing.T, logs *observer.ObservedLogs) int64 {
	t.Helper()
	lines := logs.FilterMessageSnippet("did not take what the destination sent it after").All()
	switch len(lines) {
	case 0:
		return 0
	case 1:
		n, _ := lines[0].ContextMap()["bytes"].(int64)
		return n
	}
	t.Fatalf("the relay logged %d lines for the bytes the client did not take, want one", len(lines))
	return 0
}

// After the client has sent its FIN, the relay still forwards what the
// destination sends, but tees it to the parser only once the client's TCP
// stack has taken it. A client that only half-closed (shutdown(SHUT_WR)) takes
// it, and reads it: the answer to an EOF-delimited request is captured as
// before. A client that closed its socket (a driver closing a pooled
// connection, an HTTP client giving up at its timeout) reads nothing more: its
// stack answers the bytes with a reset, so they reach no application, and the
// parser does not see them, whatever its protocol. That holds whether the
// relay had read the client's FIN by then or not: here the destination's
// answer comes once the FIN has reached the proxy's socket, and either once
// the relay forwarded the FIN, or while the relay has not read it yet.
func TestAfterTheClientClosed_WhatTheClientDidNotTakeIsNotTeed(t *testing.T) {
	const request, answer = "request", "the answer, after the client's FIN"
	for _, row := range []struct {
		name string
		// halfClose: the client shuts down only its sending side and reads
		// on; otherwise it closes its socket.
		halfClose bool
		// unread: the destination answers while the relay has not read the
		// client's FIN, which has reached the proxy's socket; otherwise once
		// the relay has forwarded the FIN.
		unread bool
	}{
		{name: "closed, the FIN forwarded"},
		{name: "closed, the FIN not read yet", unread: true},
		{name: "half-closed, the FIN forwarded", halfClose: true},
		{name: "half-closed, the FIN not read yet", halfClose: true, unread: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			gate := &gatedConn{Conn: &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, release: make(chan struct{})}
			var src net.Conn = gate.Conn
			if row.unread {
				src = gate
			} else {
				close(gate.release)
			}
			core, logs := observer.New(zapcore.DebugLevel)
			r := New(Config{Logger: zap.New(core)}, src, connseq.NewUpstream(proxyServer))
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = r.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-gate.release:
				default:
					close(gate.release)
				}
				<-done
			})
			streams := make(chan [2]string, 1)
			go func() { streams <- [2]string{streamOf(t, r.ClientStream()), streamOf(t, r.DestStream())} }()

			if _, err := app.Write([]byte(request)); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(request))
			_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(server, got); err != nil {
				t.Fatalf("the destination did not get the request: %v", err)
			}
			var err error
			if row.halfClose {
				err = app.(*net.TCPConn).CloseWrite()
			} else {
				err = app.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if row.unread {
				// The FIN is at the proxy's socket, and the relay's next
				// Read of the client has not returned.
				for limit := time.Now().Add(5 * time.Second); tcpState(t, proxyClient) != stateCloseWait; {
					if time.Now().After(limit) {
						t.Fatalf("the client's FIN did not reach the proxy's socket in 5s (state %d)", tcpState(t, proxyClient))
					}
					time.Sleep(50 * time.Microsecond)
				}
			} else {
				// The relay forwarded the FIN.
				_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
				if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
					t.Fatalf("the destination read %d more bytes, %v, before the FIN", len(rest), err)
				}
			}
			if _, err := server.Write([]byte(answer)); err != nil {
				t.Fatal(err)
			}
			if row.halfClose {
				// Forwarded, and read by the half-closed client.
				_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
				got := make([]byte, len(answer))
				if _, err := io.ReadFull(app, got); err != nil || string(got) != answer {
					t.Fatalf("the half-closed client read %q, %v", got, err)
				}
			} else {
				// Forwarded, and refused: the closed client's stack resets
				// the connection.
				for limit := time.Now().Add(5 * time.Second); tcpState(t, proxyClient) != stateClosed; {
					if time.Now().After(limit) {
						t.Fatalf("the closed client did not reset the connection in 5s (state %d)", tcpState(t, proxyClient))
					}
					time.Sleep(50 * time.Microsecond)
				}
			}
			if row.unread {
				close(gate.release)
			}
			_ = server.Close()
			var s [2]string
			select {
			case s = <-streams:
			case <-time.After(10 * time.Second):
				t.Fatal("the relay's streams did not end once both sides had closed")
			}
			if s[0] != request {
				t.Fatalf("the client's stream carried %q, want %q", s[0], request)
			}
			want, notTaken := answer, int64(0)
			if !row.halfClose {
				want, notTaken = "", int64(len(answer))
			}
			if s[1] != want {
				t.Fatalf("the destination's stream carried %q, want %q", s[1], want)
			}
			if got := notTakenLogged(t, logs); got != notTaken {
				t.Fatalf("the relay logged %d bytes the client did not take, want %d", got, notTaken)
			}
		})
	}
}

// A client that half-closed and took what was written to it after its FIN has
// shown it reads on: everything the destination sends it after is teed as it
// is written, while the client goes on taking it.
func TestAfterTheClientHalfClosed_EveryAnswerItTakesIsTeed(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	core, logs := observer.New(zapcore.DebugLevel)
	r := New(Config{Logger: zap.New(core)}, proxyClient, connseq.NewUpstream(proxyServer))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	streams := make(chan string, 1)
	go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()
	if err := app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d bytes, %v, before the FIN", len(rest), err)
	}
	var want string
	for _, answer := range []string{"the first answer", "the second", "and the third"} {
		if _, err := server.Write([]byte(answer)); err != nil {
			t.Fatal(err)
		}
		_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
		got := make([]byte, len(answer))
		if _, err := io.ReadFull(app, got); err != nil || string(got) != answer {
			t.Fatalf("the half-closed client read %q, %v", got, err)
		}
		want += answer
	}
	_ = server.Close()
	select {
	case got := <-streams:
		if got != want {
			t.Fatalf("the destination's stream carried %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destination's stream did not end once both sides had closed")
	}
	if n := notTakenLogged(t, logs); n != 0 {
		t.Fatalf("the relay logged %d bytes the client did not take, want none", n)
	}
}

// awaitTCPState waits until the TCP state of the socket under c is want.
func awaitTCPState(t *testing.T, c net.Conn, want uint8, what string) {
	t.Helper()
	for limit := time.Now().Add(5 * time.Second); tcpState(t, c) != want; {
		if time.Now().After(limit) {
			t.Fatalf("%s: the socket's state is %d after 5s, want %d", what, tcpState(t, c), want)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// heldConn is the proxy's end of the client connection whose Reads wait for
// release: the relay does not learn of the client's FIN or reset from its own
// Read, and so does not stop for it, until the test says.
type heldConn struct {
	net.Conn
	release chan struct{}
}

func (h *heldConn) Read(p []byte) (int, error) {
	<-h.release
	return h.Conn.Read(p)
}

func (h *heldConn) NetConn() net.Conn { return h.Conn }

// resetBeforeWriteReturns is the proxy's end of the client connection whose
// Write returns only once the connection has been reset (or after 5s): a reset
// the client sends for what it was written is in before the relay can look at
// the socket again, every time, as it is by chance on a busy host, when the
// client runs between the relay's write and its next step.
type resetBeforeWriteReturns struct {
	net.Conn
}

func (c *resetBeforeWriteReturns) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil {
		for limit := time.Now().Add(5 * time.Second); !stateIs(c.Conn, stateClosed) && time.Now().Before(limit); {
			time.Sleep(50 * time.Microsecond)
		}
	}
	return n, err
}

func (c *resetBeforeWriteReturns) NetConn() net.Conn { return c.Conn }

// A client that sends no FIN and resets the connection has closed with bytes
// it had not read: its stack resets instead of sending a FIN (an abortive
// close). What it was written before is teed as it always was, the chunk the
// reset comes back for included: the client read part of it. Here it reads the
// head of a 20 KiB answer and closes with the rest unread. The relay's look
// before each write found no FIN, so nothing is refused, whenever the reset
// comes in: in its own time, or every time before the relay looks again.
func TestAClientThatResetsWithNoFINBeforeKeepsWhatItWasWritten(t *testing.T) {
	head := "HTTP/1.1 200 OK\r\nContent-Length: 20480\r\n\r\n"
	answer := head + strings.Repeat("x", 20480)
	for _, row := range []struct {
		name string
		// wrap is the proxy's end of the client connection as the relay is
		// handed it.
		wrap func(net.Conn) net.Conn
	}{
		{name: "the reset in its own time", wrap: func(c net.Conn) net.Conn { return c }},
		{name: "the reset in before the relay looks again", wrap: func(c net.Conn) net.Conn { return &resetBeforeWriteReturns{Conn: c} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			for round := 0; round < 20; round++ {
				app, proxyClient := loopbackPair(t)
				proxyServer, server := loopbackPair(t)
				core, logs := observer.New(zapcore.DebugLevel)
				var stops atomic.Int32
				r := New(Config{Logger: zap.New(core), ClientStopped: func(func() []byte) { stops.Add(1) }},
					row.wrap(proxyClient), connseq.NewUpstream(proxyServer))
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { defer close(done); _ = r.Run(ctx) }()
				streams := make(chan string, 1)
				go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()
				if _, err := server.Write([]byte(answer)); err != nil {
					t.Fatal(err)
				}
				_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
				got := make([]byte, len(head))
				if _, err := io.ReadFull(app, got); err != nil || string(got) != head {
					t.Fatalf("round %d: the client read %q, %v", round, got, err)
				}
				// The rest of the answer is in the client's socket, or on its
				// way: closing with it unread resets the connection.
				awaitUnread(t, app)
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
				awaitTCPState(t, proxyClient, stateClosed, "the client closed with bytes unread")
				_ = server.Close()
				var s string
				select {
				case s = <-streams:
				case <-time.After(10 * time.Second):
					t.Fatalf("round %d: the destination's stream did not end", round)
				}
				cancel()
				<-done
				if len(s) < len(head) || !strings.HasPrefix(answer, s) {
					t.Fatalf("round %d: the destination's stream carried %d bytes (a prefix of the answer: %v), want at least the head the client read",
						round, len(s), strings.HasPrefix(answer, s))
				}
				if n := notTakenLogged(t, logs); n != 0 || stops.Load() != 0 {
					t.Fatalf("round %d: the relay logged %d bytes not taken and said the client stopped %d times, want neither: the client sent no FIN",
						round, n, stops.Load())
				}
			}
		})
	}
}

// A chunk the client refused is no part of the parser's stream, so no mock is
// marked incomplete for it, its Write failed or not: here the client closed
// (FIN), refused the first answer with a reset, and the second answer's Write
// fails on the reset connection; or, with the client's socket scripted, the
// second answer's Write comes up short (short_write) behind the refused first.
// A mark there would leave out the next mock
// the parser emits, one it may have captured whole before the client stopped.
// A chunk that is teed and whose Write fails still marks it (write_error), as
// before: here a client that reset with no FIN (an abortive close), so its
// answer is teed.
func TestAChunkTheClientRefusedMarksNoMockIncomplete(t *testing.T) {
	t.Run("refused, then a Write that fails: no mark", func(t *testing.T) {
		t.Parallel()
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		core, logs := observer.New(zapcore.DebugLevel)
		var marks atomic.Pointer[[]string]
		marks.Store(&[]string{})
		var stops atomic.Int32
		r := New(Config{
			Logger:               zap.New(core),
			ClientStopped:        func(func() []byte) { stops.Add(1) },
			OnMarkMockIncomplete: func(reason string) { m := append(append([]string(nil), *marks.Load()...), reason); marks.Store(&m) },
		}, proxyClient, connseq.NewUpstream(proxyServer))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = r.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		streams := make(chan string, 1)
		go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
		if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
			t.Fatalf("the destination read %d bytes, %v, before the FIN", len(rest), err)
		}
		const first, second = "the first answer, after the FIN", "and a second one"
		if _, err := server.Write([]byte(first)); err != nil {
			t.Fatal(err)
		}
		for limit := time.Now().Add(5 * time.Second); stops.Load() == 0; {
			if time.Now().After(limit) {
				t.Fatal("the relay did not say the client stopped taking the first answer")
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := server.Write([]byte(second)); err != nil {
			t.Fatal(err)
		}
		_ = server.Close()
		select {
		case s := <-streams:
			if s != "" {
				t.Fatalf("the destination's stream carried %q, want nothing", s)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the destination's stream did not end")
		}
		cancel()
		<-done
		if m := *marks.Load(); len(m) != 0 || stops.Load() != 1 {
			t.Fatalf("marked incomplete %q and said the client stopped %d times, want no mark and once", m, stops.Load())
		}
		if n := notTakenLogged(t, logs); n != int64(len(first)+len(second)) {
			t.Fatalf("the relay logged %d bytes not taken, want both answers', %d", n, len(first)+len(second))
		}
	})
	t.Run("refused, then a Write that comes up short: no mark", func(t *testing.T) {
		t.Parallel()
		_, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		var marks atomic.Pointer[[]string]
		marks.Store(&[]string{})
		var stops atomic.Int32
		r := New(Config{
			Logger:               zap.NewNop(),
			HalfCloseGrace:       time.Minute,
			ClientStopped:        func(func() []byte) { stops.Add(1) },
			OnMarkMockIncomplete: func(reason string) { m := append(append([]string(nil), *marks.Load()...), reason); marks.Store(&m) },
		}, &writesThenFails{Conn: proxyClient, short: true}, connseq.NewUpstream(proxyServer))
		// The client's FIN is in, as far as the relay can tell, and the
		// connection then closes with nothing acknowledged since: the first
		// answer is refused, and the second, whose Write comes up one byte
		// short, is refused behind it.
		r.client = &scriptedSocket{script: []clientLook{finned(0), closed(0)}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = r.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		streams := make(chan string, 1)
		go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()
		const first, second = "the first answer, after the FIN", "and a second one"
		if _, err := server.Write([]byte(first)); err != nil {
			t.Fatal(err)
		}
		for limit := time.Now().Add(5 * time.Second); stops.Load() == 0; {
			if time.Now().After(limit) {
				t.Fatal("the relay did not say the client stopped taking the first answer")
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := server.Write([]byte(second)); err != nil {
			t.Fatal(err)
		}
		select {
		case s := <-streams:
			if s != "" {
				t.Fatalf("the destination's stream carried %q, want nothing", s)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the destination's stream did not end after the short write")
		}
		_ = server.Close()
		cancel()
		<-done
		if m := *marks.Load(); len(m) != 0 || stops.Load() != 1 {
			t.Fatalf("marked incomplete %q and said the client stopped %d times, want no mark and once", m, stops.Load())
		}
		if chunks, bytes := r.NotTaken(); chunks != 2 || bytes != int64(len(first)+len(second)) {
			t.Fatalf("%d chunks (%d bytes) not taken, want both answers (%d bytes)", chunks, bytes, len(first)+len(second))
		}
	})
	t.Run("teed, its Write failed: marked write_error", func(t *testing.T) {
		t.Parallel()
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		var marks atomic.Pointer[[]string]
		marks.Store(&[]string{})
		var stops atomic.Int32
		held := &heldConn{Conn: proxyClient, release: make(chan struct{})}
		r := New(Config{
			Logger:               zap.NewNop(),
			ClientStopped:        func(func() []byte) { stops.Add(1) },
			OnMarkMockIncomplete: func(reason string) { m := append(append([]string(nil), *marks.Load()...), reason); marks.Store(&m) },
		}, held, connseq.NewUpstream(proxyServer))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = r.Run(ctx) }()
		t.Cleanup(func() {
			cancel()
			select {
			case <-held.release:
			default:
				close(held.release)
			}
			<-done
		})
		streams := make(chan string, 1)
		go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()
		// An abortive close: a reset, no FIN.
		if err := app.(*net.TCPConn).SetLinger(0); err != nil {
			t.Fatal(err)
		}
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		awaitTCPState(t, proxyClient, stateClosed, "the client reset")
		const answer = "an answer to a client that reset"
		if _, err := server.Write([]byte(answer)); err != nil {
			t.Fatal(err)
		}
		for limit := time.Now().Add(5 * time.Second); len(*marks.Load()) == 0; {
			if time.Now().After(limit) {
				t.Fatal("no mock was marked incomplete for the answer whose Write failed")
			}
			time.Sleep(time.Millisecond)
		}
		close(held.release)
		_ = server.Close()
		select {
		case s := <-streams:
			if s != answer {
				t.Fatalf("the destination's stream carried %q, want the answer, teed as before", s)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the destination's stream did not end")
		}
		cancel()
		<-done
		if m := *marks.Load(); len(m) != 1 || m[0] != "write_error" || stops.Load() != 0 {
			t.Fatalf("marked incomplete %q and said the client stopped %d times, want write_error and never", m, stops.Load())
		}
	})
}

// The relay asks the TCP socket under the client conn it is handed, through the
// wrappers the proxy puts over it, which say what they wrap (NetConn): a conn
// that is not a TCP socket is not asked, and a socket keploy has closed under
// the relay cannot say what the client did.
func TestClientSocketOfFindsTheSocketUnderTheProxysWrappers(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	defer func() { _ = app.Close() }()
	wrapped := &proxyutil.Conn{Logger: zap.NewNop()}
	inner := &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}
	tlsConn := tls.Server(inner, &tls.Config{})
	wrapped.Conn, wrapped.Reader = tlsConn, tlsConn
	s := clientSocketOf(wrapped)
	if s == nil {
		t.Fatal("no socket found under util.Conn > tls.Conn > util.Conn > TCPConn")
	}
	if l := s.look(); l.said != clientOpen {
		t.Fatalf("the socket of a client that has sent no FIN says %d, want open", l.said)
	}
	pipeA, pipeB := net.Pipe()
	defer func() { _ = pipeA.Close(); _ = pipeB.Close() }()
	if clientSocketOf(pipeA) != nil {
		t.Fatal("a pipe is asked as a TCP socket")
	}
	_ = proxyClient.Close()
	if l := s.look(); l.said != clientUnknown || l.acked != 0 {
		t.Fatalf("a socket keploy closed says %d (%d acknowledged), want that it cannot say", l.said, l.acked)
	}
}

// A client conn with no TCP socket under it to ask is named in the relay's
// debug line by its type alone: the conn's value is what the client sent
// through it (a CONNECT's headers among them), which is never logged.
func TestAClientConnWithNoSocketIsNamedByItsTypeAlone(t *testing.T) {
	t.Parallel()
	pipeA, pipeB := net.Pipe()
	proxyServer, server := loopbackPair(t)
	defer func() { _ = pipeA.Close(); _ = pipeB.Close(); _ = proxyServer.Close(); _ = server.Close() }()
	core, logs := observer.New(zapcore.DebugLevel)
	r := New(Config{Logger: zap.New(core)}, pipeA, connseq.NewUpstream(proxyServer))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	cancel()
	<-done
	got := logs.FilterMessageSnippet("has no TCP socket under it to ask").AllUntimed()
	if len(got) != 1 {
		t.Fatalf("%d lines say the client conn has no socket to ask, want 1", len(got))
	}
	if c, want := got[0].ContextMap()["conn"], fmt.Sprintf("%T", pipeA); c != want {
		t.Fatalf("the client conn is named %q, want its type %q", c, want)
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

func (c *writesThenFails) Write(p []byte) (int, error) {
	if c.writes.Add(1) == 1 {
		return c.Conn.Write(p)
	}
	if c.short {
		return c.Conn.Write(p[:len(p)-1])
	}
	return 0, syscall.ECONNRESET
}
