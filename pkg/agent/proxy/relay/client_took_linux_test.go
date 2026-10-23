//go:build linux

package relay

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"sync"
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
	"golang.org/x/sys/unix"
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

// tcpState is the TCP state of the socket under c (TCP_INFO tcpi_state).
func tcpState(t *testing.T, c net.Conn) uint8 {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var state uint8
	if err := rc.Control(func(fd uintptr) {
		info, gerr := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if gerr != nil {
			t.Error(gerr)
			return
		}
		state = info.State
	}); err != nil {
		t.Fatal(err)
	}
	return state
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
				for limit := time.Now().Add(5 * time.Second); tcpState(t, proxyClient) != unix.BPF_TCP_CLOSE_WAIT; {
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
				for limit := time.Now().Add(5 * time.Second); tcpState(t, proxyClient) != unix.BPF_TCP_CLOSE; {
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

// stateIs reports whether the TCP state of the socket under c is want, false
// when it cannot be asked. For the relay's goroutines, which must not fail the
// test.
func stateIs(c net.Conn, want uint8) bool {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	var state uint8
	if err := rc.Control(func(fd uintptr) {
		if info, gerr := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); gerr == nil {
			state = info.State
		}
	}); err != nil {
		return false
	}
	return state == want
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
		for limit := time.Now().Add(5 * time.Second); !stateIs(c.Conn, unix.BPF_TCP_CLOSE) && time.Now().Before(limit); {
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
				r := New(Config{Logger: zap.New(core), ClientStopped: func() { stops.Add(1) }},
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
				awaitReceived(t, app, uint64(len(head)+1))
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
				awaitTCPState(t, proxyClient, unix.BPF_TCP_CLOSE, "the client closed with bytes unread")
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
			ClientStopped:        func() { stops.Add(1) },
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
			ClientStopped:        func() { stops.Add(1) },
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
			ClientStopped:        func() { stops.Add(1) },
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
		awaitTCPState(t, proxyClient, unix.BPF_TCP_CLOSE, "the client reset")
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

// windowClosed reports whether the peer of the socket under c has closed its
// receive window: every byte c sent is acknowledged, and c has more it may not
// send.
func windowClosed(t *testing.T, c net.Conn) bool {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var closed bool
	if err := rc.Control(func(fd uintptr) {
		if info, gerr := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); gerr == nil {
			closed = info.Unacked == 0 && info.Notsent_bytes > 0
		}
	}); err != nil {
		t.Fatal(err)
	}
	return closed
}

// countedLooks counts the looks taken at a real client socket, each once it
// has returned.
type countedLooks struct {
	clientSocket
	n atomic.Int64
}

func (c *countedLooks) look() clientLook {
	l := c.clientSocket.look()
	c.n.Add(1)
	return l
}

// A client that half-closed and does not read for a while (its receive buffer
// full, so its stack acknowledges nothing) has not answered for the answer the
// destination sends after its FIN: the answer is set aside, not teed, while
// the client reads nothing, and the destination's stream does not end. That
// holds once the relay has forwarded the destination's FIN too, with the
// proxy's socket in LAST_ACK, which reads as the client's FIN in and the
// connection not closed. Once the client reads, its stack acknowledges, the
// answer is teed, and the client gets the whole answer, the parser all of it.
func TestAfterTheClientHalfClosed_AClientNotReadingYetIsWaitedFor(t *testing.T) {
	t.Parallel()
	// The client's receive buffer is small, so what the destination sends
	// before its FIN fills it while it does not read.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); accepted <- c }()
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 2048) }); err != nil {
			return err
		}
		return serr
	}}
	app, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proxyClient := <-accepted
	_ = ln.Close()
	t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close() })
	proxyServer, server := loopbackPair(t)
	core, logs := observer.New(zapcore.DebugLevel)
	r := New(Config{Logger: zap.New(core), HalfCloseGrace: time.Minute}, proxyClient, connseq.NewUpstream(proxyServer))
	looks := &countedLooks{clientSocket: r.client}
	r.client = looks
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	streams := make(chan string, 1)
	go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()

	before := strings.Repeat("b", 8<<10)
	if _, err := server.Write([]byte(before)); err != nil {
		t.Fatal(err)
	}
	// The client's window is closed: its stack has acknowledged all it can
	// hold, and acknowledges no more until it reads.
	for limit := time.Now().Add(5 * time.Second); !windowClosed(t, proxyClient); {
		if time.Now().After(limit) {
			t.Fatal("the client's receive window did not close in 5s")
		}
		time.Sleep(50 * time.Microsecond)
	}
	if err := app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d bytes, %v, before the FIN", len(rest), err)
	}
	const answer = "the answer, after the client's FIN"
	if _, err := server.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	// Set aside, not teed, while the client reads nothing: also once the
	// relay has forwarded the destination's FIN, which puts the proxy's
	// socket in LAST_ACK (the client's FIN in, the proxy's own sent), and the
	// relay has looked at it twice since.
	for limit := time.Now().Add(5 * time.Second); tcpState(t, proxyClient) != unix.BPF_TCP_LAST_ACK; {
		if time.Now().After(limit) {
			t.Fatalf("the relay did not forward the destination's FIN in 5s (state %d)", tcpState(t, proxyClient))
		}
		time.Sleep(50 * time.Microsecond)
	}
	for since := looks.n.Load(); looks.n.Load() < since+2; {
		if r.teeD2C.awaitingLen() == 0 {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	if n := r.teeD2C.awaitingLen(); n == 0 {
		t.Fatal("the answer written after the client's FIN was not set aside for the client's answer, once the relay had sent its FIN")
	}
	if n := r.Teed(fakeconn.FromDest); n != int64(len(before)) {
		t.Fatalf("%d bytes teed while the client acknowledged nothing after its FIN, want the %d sent before it", n, len(before))
	}
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(app)
	if err != nil || string(got) != before+answer {
		t.Fatalf("the client read %d bytes (%v), want the %d it was sent", len(got), err, len(before)+len(answer))
	}
	select {
	case s := <-streams:
		if s != before+answer {
			t.Fatalf("the destination's stream carried %d bytes, want the %d the client took", len(s), len(before)+len(answer))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destination's stream did not end")
	}
	if n := notTakenLogged(t, logs); n != 0 {
		t.Fatalf("the relay logged %d bytes the client did not take, want none", n)
	}
}

// The client's answer for what it is written after its FIN is waited for off
// the forwarding path: the destination's forwarder goes on forwarding, so a
// half-closed client gets the answer and the destination's FIN as soon as they
// come, however long its stack takes to acknowledge them (a delayed
// acknowledgement, some tens of milliseconds, or a retransmission timeout), and
// only what the parser is given waits. Here the socket the relay asks says
// nothing until the test lets it: the client reads the whole answer and its
// end while the answer is still set aside, and once the socket says the client
// read on, the answer is teed and the relay ends.
func TestTheClientsAnswerIsWaitedForOffTheForwardingPath(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	r := New(Config{Logger: zap.NewNop(), HalfCloseGrace: time.Minute}, proxyClient, connseq.NewUpstream(proxyServer))
	sock := &scriptedSocket{script: []clientLook{finned(0)}}
	r.client = sock
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
	const answer = "the answer, after the client's FIN"
	if _, err := server.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	if got, err := io.ReadAll(app); err != nil || string(got) != answer {
		t.Fatalf("the client read %q (%v) while the relay waited for its answer, want the answer and its end", got, err)
	}
	if n := r.teeD2C.awaitingLen(); n == 0 || r.Teed(fakeconn.FromDest) != 0 {
		t.Fatalf("%d chunks set aside, %d bytes teed, before the client's answer; want the answer set aside, nothing teed", n, r.Teed(fakeconn.FromDest))
	}
	sock.set(finned(1))
	select {
	case s := <-streams:
		if s != answer {
			t.Fatalf("the destination's stream carried %q, want the answer", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destination's stream did not end once the client's socket said it read on")
	}
	if !r.clientReadsOn.Load() {
		t.Fatal("the relay did not take the client to read on")
	}
}

// The relay also looks at the client's socket as it reads the client's FIN,
// before it forwards the FIN (clientFINRead). Later acknowledgements are
// counted from what the client's stack had acknowledged at that look. Here a
// half-closed client acknowledges bytes that were in flight at its FIN once
// the relay has read the FIN, and before the destination's next write. That
// shows it reads on, so the chunk written next is teed, even though the
// connection then closes with nothing more acknowledged. A relay that counted
// from its look before that write lost the acknowledgement and refused the
// chunk.
func TestAnAcknowledgementAfterTheClientsFINShowsItReadsOn(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	r := New(Config{Logger: zap.NewNop(), HalfCloseGrace: time.Minute}, proxyClient, connseq.NewUpstream(proxyServer))
	// The client's FIN is in, with 100 bytes acknowledged.
	sock := &scriptedSocket{script: []clientLook{finned(100)}}
	r.client = sock
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
	// The relay has read and forwarded the FIN. Now the bytes in flight at
	// the FIN are acknowledged, before the destination answers.
	sock.set(finned(200))
	const answer = "the answer, after the client's FIN"
	if _, err := server.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len(answer))
	if _, err := io.ReadFull(app, got); err != nil || string(got) != answer {
		t.Fatalf("the half-closed client read %q, %v", got, err)
	}
	// The connection closes with nothing more acknowledged.
	sock.set(closed(200))
	_ = server.Close()
	select {
	case s := <-streams:
		if s != answer {
			t.Fatalf("the destination's stream carried %q, want the answer: the client acknowledged bytes after its FIN", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destination's stream did not end")
	}
	if chunks, bytes := r.NotTaken(); chunks != 0 || !r.clientReadsOn.Load() {
		t.Fatalf("%d chunks (%d bytes) not taken, reads on %v; want none refused, the client reading on", chunks, bytes, r.clientReadsOn.Load())
	}
}

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

// A chunk set aside for the client's answer carries the incomplete-mock mark
// its Write earned (write_error, short_write), and the mark is set once the
// chunk is teed: here the first chunk after the client's FIN is set aside,
// the second one's Write fails or comes up short behind it, and then either
// the client is found to read on, or it never answers and the recording's
// stop makes the next look the last, which tees the chunks as before. Either
// way both chunks are teed and the mark is set once, after them. Nothing
// here waits on a clock: the grace is a minute, and only the client's answer
// or the recording's stop decides the chunks.
func TestAChunkSetAsideCarriesItsWritesMarkOnceTeed(t *testing.T) {
	for _, row := range []struct {
		name    string
		short   bool
		answers bool
		want    string
	}{
		{"write_error, the client reads on", false, true, "write_error"},
		{"short_write, the client reads on", true, true, "short_write"},
		{"write_error, the client never answers before the recording stops", false, false, "write_error"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			app, proxyClient := loopbackPair(t)
			proxyServer, server := loopbackPair(t)
			var mu sync.Mutex
			var marks []string
			r := New(Config{Logger: zap.NewNop(), HalfCloseGrace: time.Minute, OnMarkMockIncomplete: func(reason string) {
				mu.Lock()
				marks = append(marks, reason)
				mu.Unlock()
			}}, &writesThenFails{Conn: proxyClient, short: row.short}, connseq.NewUpstream(proxyServer))
			// The client's FIN is in, as far as the relay can tell.
			sock := &scriptedSocket{script: []clientLook{finned(0)}}
			r.client = sock
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = r.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			streams := make(chan string, 1)
			go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()

			if _, err := server.Write([]byte("first")); err != nil {
				t.Fatal(err)
			}
			_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
			readN(t, app, len("first"))
			for limit := time.Now().Add(5 * time.Second); r.teeD2C.awaitingLen() != 1; {
				if time.Now().After(limit) {
					t.Fatal("the first chunk was not set aside in 5s")
				}
				time.Sleep(50 * time.Microsecond)
			}
			if _, err := server.Write([]byte("second")); err != nil {
				t.Fatal(err)
			}
			for limit := time.Now().Add(5 * time.Second); r.teeD2C.awaitingLen() != 2; {
				if time.Now().After(limit) {
					t.Fatal("the second chunk, whose Write failed, was not set aside behind the first in 5s")
				}
				time.Sleep(50 * time.Microsecond)
			}
			mu.Lock()
			early := append([]string(nil), marks...)
			mu.Unlock()
			if len(early) != 0 {
				t.Fatalf("marked %q while the chunks waited for the client's answer, want nothing yet", early)
			}
			if row.answers {
				sock.set(finned(1))
			} else {
				// The recording stops: resolve's next look is its last.
				cancel()
			}
			_ = server.Close()
			select {
			case s := <-streams:
				if s != "firstsecond" {
					t.Fatalf("the destination's stream carried %q, want both chunks", s)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the destination's stream did not end")
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the relay did not end")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(marks) != 1 || marks[0] != row.want {
				t.Fatalf("marked %q, want [%q] once the chunks were teed", marks, row.want)
			}
		})
	}
}

// readN reads exactly n bytes from c, failing the test if it cannot.
func readN(t *testing.T, c net.Conn, n int) {
	t.Helper()
	if _, err := io.ReadFull(c, make([]byte, n)); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
}

// What the proxy's socket says of its client, the kernel facts the relay's
// rule reads: open before the client's FIN; once its FIN is in, the FIN's own
// acknowledgement of all it had received, and then each byte a half-closed
// client takes, acknowledged; a client that closed its socket acknowledges
// nothing after its FIN, and resets the connection on the next bytes it is
// sent; one that closes with bytes unread resets at once, and what it had
// acknowledged before stays acknowledged.
func TestTheClientsSocketSaysWhatTheClientTook(t *testing.T) {
	t.Parallel()
	lookFor := func(t *testing.T, s clientSocket, want clientSaid, what string) clientLook {
		t.Helper()
		for limit := time.Now().Add(5 * time.Second); ; {
			l := s.look()
			if l.said == want {
				return l
			}
			if time.Now().After(limit) {
				t.Fatalf("%s: the socket says %d, want %d", what, l.said, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	t.Run("half-closed", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		s := clientSocketOf(proxyClient)
		if _, err := proxyClient.Write(make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
		awaitReceived(t, app, 100)
		if l := lookFor(t, s, clientOpen, "before the client's FIN"); l.acked > 100 {
			t.Fatalf("%d bytes acknowledged of the 100 written", l.acked)
		}
		if err := app.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		fin := lookFor(t, s, clientFinned, "after the client's FIN")
		if fin.acked != 100 {
			t.Fatalf("the FIN in, %d bytes acknowledged, want the 100 the client had received", fin.acked)
		}
		if _, err := proxyClient.Write(make([]byte, 1024)); err != nil {
			t.Fatal(err)
		}
		awaitReceived(t, app, 1124)
		for limit := time.Now().Add(5 * time.Second); s.look().acked != 1124; {
			if time.Now().After(limit) {
				t.Fatalf("the half-closed client took the bytes; %d acknowledged, want 1124", s.look().acked)
			}
			time.Sleep(time.Millisecond)
		}
		// It closes with the 1124 bytes unread: a reset, at once.
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		if l := lookFor(t, s, clientClosed, "the client closed with bytes unread"); l.acked != 1124 {
			t.Fatalf("reset, %d acknowledged, want what it had acknowledged before, 1124", l.acked)
		}
	})
	t.Run("closed", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		s := clientSocketOf(proxyClient)
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		fin := lookFor(t, s, clientFinned, "after the client's FIN")
		if _, err := proxyClient.Write(make([]byte, 1024)); err != nil {
			t.Fatal(err)
		}
		if l := lookFor(t, s, clientClosed, "bytes sent to a client that closed its socket"); l.acked != fin.acked {
			t.Fatalf("reset, %d acknowledged, want none after the FIN (%d)", l.acked, fin.acked)
		}
	})
}

// slowLastLook is a scripted socket whose looks, once the relay has made the
// next look its last (lastLook), take a while: a relay that did not wait for
// that look to settle the chunks before it closed its tees would lose them.
type slowLastLook struct {
	s    *scriptedSocket
	last <-chan struct{}
}

func (l *slowLastLook) look() clientLook {
	select {
	case <-l.last:
		time.Sleep(300 * time.Millisecond)
	default:
	}
	return l.s.look()
}

// A half-closed client that answers neither way (its socket says nothing)
// is given HalfCloseGrace once both forwarders have returned; then the last
// look decides, the answer is teed as before, and only then does the relay
// close its tees: the parser's stream carries the answer and ends after it,
// however long that last look takes.
//
// The scripted socket says the client's FIN is in from the first look, so the
// answer is set aside whenever it is written. It is written, and read by the
// client, before the client's real FIN: HalfCloseGrace also bounds how long
// the relay waits on an idle destination after a half-close, and an answer
// written after the FIN would race that bound.
func TestAClientThatNeverAnswersHasItsAnswerTeedBeforeTheRelayEnds(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	r := New(Config{Logger: zap.NewNop(), HalfCloseGrace: 50 * time.Millisecond}, proxyClient, connseq.NewUpstream(proxyServer))
	r.client = &slowLastLook{s: &scriptedSocket{script: []clientLook{finned(0)}}, last: r.lastLook}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	streams := make(chan string, 1)
	go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()
	const answer = "the answer, to a client that never says whether it took it"
	if _, err := server.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len(answer))
	if _, err := io.ReadFull(app, got); err != nil || string(got) != answer {
		t.Fatalf("the client read %q, %v", got, err)
	}
	// The forwarder sets the answer aside right after its Write returns.
	for limit := time.Now().Add(5 * time.Second); r.teeD2C.awaitingLen() == 0; {
		if time.Now().After(limit) {
			t.Fatal("the answer was not set aside for the client's answer in 5s")
		}
		time.Sleep(50 * time.Microsecond)
	}
	if n := r.Teed(fakeconn.FromDest); n != 0 {
		t.Fatalf("%d bytes teed before the client answered, want none", n)
	}
	if err := app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d bytes, %v, before the FIN", len(rest), err)
	}
	_ = server.Close()
	select {
	case s := <-streams:
		if s != answer {
			t.Fatalf("the destination's stream carried %q, want the answer, teed as before at the relay's end", s)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the destination's stream did not end")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not end")
	}
	if r.clientReadsOn.Load() || r.clientStopped.Load() {
		t.Fatalf("reads on %v, stopped %v; want neither: the client never answered", r.clientReadsOn.Load(), r.clientStopped.Load())
	}
}
