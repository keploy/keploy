//go:build linux

package relay

import (
	"context"
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
}

// Each chunk written after the client's FIN is judged by its own bytes: an
// acknowledgement of bytes written before it takes none of it. Here a
// half-closed client's FIN comes in with 100 bytes in flight, its stack
// acknowledges them after the relay read the FIN and before the destination
// answers, and the connection then closes with nothing more acknowledged: the
// answer is refused. Judged from what was acknowledged at the look that found
// the FIN, the acknowledgement of the bytes in flight took the answer.
func TestAnAcknowledgementOfBytesWrittenBeforeAChunkTakesNoneOfIt(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	proxyServer, server := loopbackPair(t)
	r := New(Config{Logger: zap.NewNop(), HalfCloseGrace: time.Minute}, proxyClient, connseq.NewUpstream(proxyServer))
	// The client's FIN is in, with 100 of the 200 bytes written acknowledged.
	sock := &scriptedSocket{script: []clientLook{finned(100)}, wrote: 200}
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
	for limit := time.Now().Add(10 * time.Second); r.teeD2C.awaitingLen() == 0; {
		if time.Now().After(limit) {
			t.Fatal("the answer was not set aside")
		}
		time.Sleep(50 * time.Microsecond)
	}
	// The connection closes with nothing more acknowledged.
	sock.set(closed(200))
	_ = server.Close()
	select {
	case s := <-streams:
		if s != "" {
			t.Fatalf("the destination's stream carried %q, want nothing: none of the answer was acknowledged", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destination's stream did not end")
	}
	if chunks, bytes := r.NotTaken(); chunks != 1 || bytes != int64(len(answer)) {
		t.Fatalf("%d chunks (%d bytes) not taken, want the answer's 1 (%d)", chunks, bytes, len(answer))
	}
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

func (l *slowLastLook) written() (uint64, bool) { return l.s.written() }

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
	if r.clientStopped.Load() {
		t.Fatal("stopped; want not: the client never answered")
	}
}

// Each chunk the destination sends after the client's FIN is judged on its
// own: teed once the client's stack acknowledged a byte of it, refused once
// the connection closes with none of it acknowledged. Here a half-closed
// client reads the first answer whole, its stack acknowledges it, and then
// the client closes its socket; the destination sends a second answer, which
// the closed socket resets. The first is teed, the second is not: the client
// never read it. A relay that took the first acknowledgement to mean the
// client reads on from then on recorded the second.
func TestAHalfClosedClientThatReadsThenClosesHasWhatCameAfterRefused(t *testing.T) {
	t.Parallel()
	first, second := strings.Repeat("1", 100), strings.Repeat("2", 100)
	for round := 0; round < 20; round++ {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		src := &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}
		r := New(Config{Logger: zap.NewNop()}, src, connseq.NewUpstream(proxyServer))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = r.Run(ctx) }()
		streams := make(chan string, 1)
		go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()

		if _, err := app.Write([]byte("req")); err != nil {
			t.Fatal(err)
		}
		if err := app.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
		if got, err := io.ReadAll(server); err != nil || string(got) != "req" {
			t.Fatalf("round %d: the destination read %q, %v; want the request and the FIN", round, got, err)
		}
		if _, err := server.Write([]byte(first)); err != nil {
			t.Fatal(err)
		}
		_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
		got := make([]byte, len(first))
		if _, err := io.ReadFull(app, got); err != nil || string(got) != first {
			t.Fatalf("round %d: the half-closed client read %q, %v", round, got, err)
		}
		// The client's stack has acknowledged the first answer.
		for limit := time.Now().Add(5 * time.Second); bytesAcked(t, proxyClient) < uint64(len(first)); {
			if time.Now().After(limit) {
				t.Fatalf("round %d: %d bytes acknowledged after 5s, want %d", round, bytesAcked(t, proxyClient), len(first))
			}
			time.Sleep(20 * time.Microsecond)
		}
		_ = app.Close()
		if _, err := server.Write([]byte(second)); err != nil {
			t.Fatal(err)
		}
		awaitTCPState(t, proxyClient, unix.BPF_TCP_CLOSE, "the closed client resets the second answer")
		_ = server.Close()
		select {
		case s := <-streams:
			if s != first {
				t.Fatalf("round %d: the destination's stream carried %q, want only the first answer, which the client read", round, s)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("round %d: the destination's stream did not end", round)
		}
		if chunks, bytes := r.NotTaken(); chunks == 0 || bytes != int64(len(second)) {
			t.Fatalf("round %d: %d chunks (%d bytes) not taken, want the second answer's %d bytes", round, chunks, bytes, len(second))
		}
		cancel()
		<-done
	}
}

// A closed client whose FIN reaches the proxy's socket between the relay's
// look and its Write of a destination chunk: that chunk is teed, as before
// (the relay could learn of the FIN only once it had handed the chunk on).
// The client resets on it, before the relay has read the FIN, so the look as
// the relay reads the FIN finds the connection closed, not the FIN. The FIN
// came in order (the relay read io.EOF, not a reset), and a chunk written to a
// closed connection reaches no one: the destination's answer to the forwarded
// FIN is refused, the stream ends where the client stopped, and no mock is
// marked incomplete for the answer's failed Write.
func TestAClosedClientThatResetsBeforeTheRelayReadsItsFINHasTheAnswerRefused(t *testing.T) {
	t.Parallel()
	answer := strings.Repeat("A", 64)
	for round := 0; round < 20; round++ {
		app, proxyClient := loopbackPair(t)
		proxyServer, server := loopbackPair(t)
		c := &finInFlightConn{Conn: &proxyutil.Conn{Conn: proxyClient, Reader: proxyClient, Logger: zap.NewNop()}, release: make(chan struct{})}
		c.hook = func() {
			_ = app.Close()
			for limit := time.Now().Add(5 * time.Second); tcpState(t, proxyClient) != unix.BPF_TCP_CLOSE_WAIT && time.Now().Before(limit); {
				time.Sleep(20 * time.Microsecond)
			}
		}
		var mu sync.Mutex
		var marks []string
		var stops atomic.Int32
		r := New(Config{
			Logger:               zap.NewNop(),
			OnMarkMockIncomplete: func(m string) { mu.Lock(); marks = append(marks, m); mu.Unlock() },
			ClientStopped:        func(func() []byte) { stops.Add(1) },
		}, c, connseq.NewUpstream(proxyServer))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = r.Run(ctx) }()
		streams := make(chan string, 1)
		go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()

		if _, err := app.Write([]byte("req")); err != nil {
			t.Fatal(err)
		}
		_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
		if got := make([]byte, 3); func() bool { _, err := io.ReadFull(server, got); return err != nil }() {
			t.Fatalf("round %d: the destination did not read the request", round)
		}
		if _, err := server.Write([]byte("inflight")); err != nil {
			t.Fatal(err)
		}
		awaitTCPState(t, proxyClient, unix.BPF_TCP_CLOSE, "the closed client resets the chunk written as its FIN came in")
		close(c.release)
		if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
			t.Fatalf("round %d: the destination read %d bytes, %v, want the forwarded FIN", round, len(rest), err)
		}
		_, _ = server.Write([]byte(answer))
		_ = server.Close()
		select {
		case s := <-streams:
			if s != "inflight" {
				t.Fatalf("round %d: the destination's stream carried %q, want only the chunk written as the FIN came in", round, s)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("round %d: the destination's stream did not end", round)
		}
		cancel()
		<-done
		mu.Lock()
		if len(marks) != 0 || stops.Load() != 1 {
			t.Fatalf("round %d: marked %q, the relay said the client stopped %d times; want no mark, once", round, marks, stops.Load())
		}
		mu.Unlock()
	}
}

// countedSocket is a client socket whose two questions, TCP_INFO and
// SIOCOUTQ, are answered from a model of the kernel: total bytes written to
// it in all, and acked, how many of them the client's stack had acknowledged
// as each question was asked, in turn (the last repeats). SIOCOUTQ says what
// was written beyond that. asked counts the questions.
type countedSocket struct {
	total uint64
	acked []uint64
	asked int
}

func (c *countedSocket) at() uint64 {
	a := c.acked[min(c.asked, len(c.acked)-1)]
	c.asked++
	return a
}

func (c *countedSocket) socket() *tcpClientSocket {
	s := &tcpClientSocket{
		askInfo: func(_ uintptr, info *unix.TCPInfo) bool {
			*info = unix.TCPInfo{Bytes_acked: c.at()}
			return true
		},
		askUnsent: func(_ uintptr, n *int32) bool {
			*n = int32(c.total - c.at())
			return true
		},
	}
	s.count = s.countAt
	return s
}

// How many bytes written counts, from its two questions: the client's
// acknowledgements, and the bytes written beyond them. Acknowledgements can
// move between the two. While they hold, the sum is every byte written. When
// they move between the questions of one try, it asks again, and a try in
// which they held is exact. When they move on every try, it takes the sum
// with the later count, which is no less than the bytes written: a chunk is
// then judged from no earlier than its first byte (a chunk the client did
// not take is never teed). A question the socket cannot answer says so.
func TestWrittenCountsEveryByteWrittenToTheClientsSocket(t *testing.T) {
	t.Parallel()
	const total = 1000
	count := func(c *countedSocket) (uint64, bool) {
		s := c.socket()
		s.countAt(0)
		return s.wrote, s.wroteOK
	}
	t.Run("acknowledgements hold: acknowledged and not, one try", func(t *testing.T) {
		c := &countedSocket{total: total, acked: []uint64{400}}
		if n, ok := count(c); n != total || !ok || c.asked != 3 {
			t.Fatalf("written %d (ok %v) on %d questions, want %d on one try's 3", n, ok, c.asked, total)
		}
	})
	t.Run("they move once, then hold: the try they held is exact", func(t *testing.T) {
		// The first try: 400 acknowledged, 550 beyond the 450 acknowledged
		// as SIOCOUTQ is asked, 500 acknowledged after: 1050, too many.
		c := &countedSocket{total: total, acked: []uint64{400, 450, 500}}
		if n, ok := count(c); n != total || !ok || c.asked != 6 {
			t.Fatalf("written %d (ok %v) on %d questions, want %d on the second try", n, ok, c.asked, total)
		}
	})
	t.Run("they move on every try: no fewer than were written", func(t *testing.T) {
		acked := make([]uint64, 3*writtenTries)
		for i := range acked {
			acked[i] = uint64(10 * i)
		}
		c := &countedSocket{total: total, acked: acked}
		n, ok := count(c)
		if !ok || n < total || c.asked != 3*writtenTries {
			t.Fatalf("written %d (ok %v) on %d questions, want no fewer than %d after %d tries", n, ok, c.asked, total, writtenTries)
		}
	})
	t.Run("a question the socket cannot answer: it cannot say", func(t *testing.T) {
		for _, failing := range []string{"TCP_INFO", "SIOCOUTQ"} {
			s := (&countedSocket{total: total, acked: []uint64{400}}).socket()
			if failing == "TCP_INFO" {
				s.askInfo = func(uintptr, *unix.TCPInfo) bool { return false }
			} else {
				s.askUnsent = func(uintptr, *int32) bool { return false }
			}
			s.countAt(0)
			if s.wroteOK {
				t.Fatalf("%s failed: written says %d, want it to say it cannot say", failing, s.wrote)
			}
		}
	})
}

// dialSmallWindow is a loopback client connection whose client end (app) has
// a small receive buffer, so what the proxy's end writes to it fills it while
// it does not read, and whose proxy's end (proxyClient) has a send buffer
// that holds far more: the proxy's writes return while the client has
// acknowledged little of them.
func dialSmallWindow(t *testing.T) (app, proxyClient net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); accepted <- c }()
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 2048) }); err != nil {
			return err
		}
		return serr
	}}
	app, err = d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if proxyClient = <-accepted; proxyClient == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = app.Close(); _ = proxyClient.Close() })
	if err := proxyClient.(*net.TCPConn).SetWriteBuffer(1 << 20); err != nil {
		t.Fatal(err)
	}
	return app, proxyClient
}

// writtenTo is how many bytes were written to the socket under c in all, as
// the test asks it, apart from the relay's own count: the bytes its peer
// acknowledged (TCP_INFO) and those beyond (SIOCOUTQ), on a try in which the
// acknowledgements did not move.
func writtenTo(t *testing.T, c net.Conn) uint64 {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	if err := rc.Control(func(fd uintptr) {
		for try := 0; ; try++ {
			if try == 1000 {
				t.Error("the acknowledgements moved on each of 1000 tries")
				return
			}
			before, ierr := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
			unsent, uerr := unix.IoctlGetInt(int(fd), unix.SIOCOUTQ)
			after, aerr := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
			if ierr != nil || uerr != nil || aerr != nil {
				t.Errorf("TCP_INFO: %v, %v; SIOCOUTQ: %v", ierr, aerr, uerr)
				return
			}
			if before.Bytes_acked == after.Bytes_acked {
				n = after.Bytes_acked + uint64(unsent)
				return
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// On a real socket, written counts the bytes the client's stack has not
// acknowledged yet as well as those it has: here most of what the proxy
// wrote waits in its send queue while the client, its window full, reads
// nothing.
func TestWrittenCountsWhatTheClientHasNotAcknowledgedYet(t *testing.T) {
	t.Parallel()
	_, proxyClient := dialSmallWindow(t)
	s := clientSocketOf(proxyClient)
	const n = 128 << 10
	if _, err := proxyClient.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	if l := s.look(); l.acked >= n {
		t.Fatalf("the client acknowledged %d of the %d bytes, want fewer: its window is small and it reads nothing", l.acked, n)
	}
	if got, ok := s.written(); !ok || got != n {
		t.Fatalf("written %d (ok %v), want the %d bytes written", got, ok, n)
	}
}

// A chunk written after the client's FIN, while the client has not yet
// acknowledged all of the one before it, is judged from where it starts: after
// every byte written before it, acknowledged or not. Here a half-closed client
// with a small window reads part of a long answer and closes its socket with
// the rest unread, and with a second answer behind it that it never read. The
// client's acknowledgements of the first answer move on after the second is
// written, and never reach the second. The part of the first answer the
// client took is teed; the second answer is refused. A count of only the
// acknowledged bytes judged the second answer from inside the first and, once
// the client acknowledged more of the first, recorded the second.
func TestAChunkBehindOneThatIsNotAcknowledgedYetIsJudgedFromItsOwnFirstByte(t *testing.T) {
	t.Parallel()
	first, second := strings.Repeat("1", 48<<10), strings.Repeat("2", 1<<10)
	app, proxyClient := dialSmallWindow(t)
	proxyServer, server := loopbackPair(t)
	r := New(Config{Logger: zap.NewNop()}, proxyClient, connseq.NewUpstream(proxyServer))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	streams := make(chan string, 1)
	go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()

	if _, err := app.Write([]byte("req")); err != nil {
		t.Fatal(err)
	}
	if err := app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if got, err := io.ReadAll(server); err != nil || string(got) != "req" {
		t.Fatalf("the destination read %q, %v; want the request and the FIN", got, err)
	}
	writtenAll := func(want uint64, what string) {
		t.Helper()
		for limit := time.Now().Add(5 * time.Second); writtenTo(t, proxyClient) != want; {
			if time.Now().After(limit) {
				t.Fatalf("%s: the relay wrote %d bytes to the client in 5s, want %d", what, writtenTo(t, proxyClient), want)
			}
			time.Sleep(50 * time.Microsecond)
		}
	}
	if _, err := server.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	writtenAll(uint64(len(first)), "the first answer")
	if _, err := server.Write([]byte(second)); err != nil {
		t.Fatal(err)
	}
	writtenAll(uint64(len(first)+len(second)), "the second answer")
	// The client reads part of the first answer, until its stack has
	// acknowledged more than it had once the second was written.
	behind := bytesAcked(t, proxyClient)
	if behind >= uint64(len(first)) {
		t.Fatalf("the client acknowledged %d bytes before it read any, want fewer than the first answer's %d", behind, len(first))
	}
	_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	for read := 0; bytesAcked(t, proxyClient) <= behind; {
		if read >= len(first)/2 {
			t.Fatalf("the client read %d bytes and its stack acknowledged no more than %d", read, behind)
		}
		n, err := app.Read(buf)
		if err != nil {
			t.Fatalf("the client read: %v", err)
		}
		read += n
	}
	// It closes with bytes unread: a reset, with the second answer never
	// acknowledged.
	_ = app.Close()
	awaitTCPState(t, proxyClient, unix.BPF_TCP_CLOSE, "the client closed with bytes unread")
	if acked := bytesAcked(t, proxyClient); acked >= uint64(len(first)) {
		t.Fatalf("the client acknowledged %d bytes, want fewer than the first answer's %d", acked, len(first))
	}
	_ = server.Close()
	select {
	case got := <-streams:
		if got == "" || !strings.HasPrefix(first, got) {
			t.Fatalf("the destination's stream carried %d bytes (the second answer's in it: %v), want a part of the first answer, which the client was reading", len(got), strings.Contains(got, "2"))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the destination's stream did not end")
	}
	if chunks, bytes := r.NotTaken(); chunks == 0 || bytes < int64(len(second)) {
		t.Fatalf("%d chunks (%d bytes) not taken, want the second answer's %d bytes among them", chunks, bytes, len(second))
	}
}
