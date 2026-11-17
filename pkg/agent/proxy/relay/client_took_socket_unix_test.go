//go:build linux || darwin

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

// The rows here drive a real client socket and read what its kernel says, on
// Linux and macOS alike, each in its own numbering (sockstate_*_test.go).

// windowClosed reports whether the peer of the socket under c has closed its
// receive window: every byte c sent is acknowledged, and c has more it may not
// send (windowClosedOf).
func windowClosed(t *testing.T, c net.Conn) bool {
	t.Helper()
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var closed bool
	if err := rc.Control(func(fd uintptr) { closed = windowClosedOf(fd) }); err != nil {
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
	r := New(Config{Logger: zap.NewNop(), HalfCloseGrace: time.Minute}, CountClientWrites(proxyClient), connseq.NewUpstream(proxyServer))
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
	_ = server.SetReadDeadline(time.Now().Add(tcpSettle))
	if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d bytes, %v, before the FIN", len(rest), err)
	}
	const answer = "the answer, after the client's FIN"
	if _, err := server.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
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
	case <-time.After(tcpSettle):
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
	r := New(Config{Logger: zap.NewNop(), HalfCloseGrace: time.Minute}, CountClientWrites(proxyClient), connseq.NewUpstream(proxyServer))
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
	_ = server.SetReadDeadline(time.Now().Add(tcpSettle))
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
	for limit := time.Now().Add(tcpSettle); r.teeD2C.awaitingLen() == 0; {
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
	case <-time.After(tcpSettle):
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
			_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
			readN(t, app, len("first"))
			for limit := time.Now().Add(tcpSettle); r.teeD2C.awaitingLen() != 1; {
				if time.Now().After(limit) {
					t.Fatalf("the first chunk was not set aside in %v", tcpSettle)
				}
				time.Sleep(50 * time.Microsecond)
			}
			if _, err := server.Write([]byte("second")); err != nil {
				t.Fatal(err)
			}
			for limit := time.Now().Add(tcpSettle); r.teeD2C.awaitingLen() != 2; {
				if time.Now().After(limit) {
					t.Fatalf("the second chunk, whose Write failed, was not set aside behind the first in %v", tcpSettle)
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
			case <-time.After(tcpSettle):
				t.Fatal("the destination's stream did not end")
			}
			select {
			case <-done:
			case <-time.After(tcpSettle):
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
		for limit := time.Now().Add(tcpSettle); ; {
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
		s := clientSocketOf(proxyClient, zap.NewNop())
		// The relay's first look comes before it writes (clientFINBefore),
		// and macOS counts from the first look (client_took_darwin.go).
		_ = s.look()
		if _, err := proxyClient.Write(make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
		awaitUnreadBytes(t, app, 100)
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
		awaitUnreadBytes(t, app, 1124)
		for limit := time.Now().Add(tcpSettle); s.look().acked != 1124; {
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
		s := clientSocketOf(proxyClient, zap.NewNop())
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
	r := New(Config{Logger: zap.NewNop(), HalfCloseGrace: 50 * time.Millisecond}, CountClientWrites(proxyClient), connseq.NewUpstream(proxyServer))
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
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	got := make([]byte, len(answer))
	if _, err := io.ReadFull(app, got); err != nil || string(got) != answer {
		t.Fatalf("the client read %q, %v", got, err)
	}
	// The forwarder sets the answer aside right after its Write returns.
	for limit := time.Now().Add(tcpSettle); r.teeD2C.awaitingLen() == 0; {
		if time.Now().After(limit) {
			t.Fatalf("the answer was not set aside for the client's answer in %v", tcpSettle)
		}
		time.Sleep(50 * time.Microsecond)
	}
	if n := r.Teed(fakeconn.FromDest); n != 0 {
		t.Fatalf("%d bytes teed before the client answered, want none", n)
	}
	if err := app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(tcpSettle))
	if rest, err := io.ReadAll(server); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d bytes, %v, before the FIN", len(rest), err)
	}
	_ = server.Close()
	select {
	case s := <-streams:
		if s != answer {
			t.Fatalf("the destination's stream carried %q, want the answer, teed as before at the relay's end", s)
		}
	case <-time.After(tcpSettle):
		t.Fatal("the destination's stream did not end")
	}
	select {
	case <-done:
	case <-time.After(tcpSettle):
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
		_ = server.SetReadDeadline(time.Now().Add(tcpSettle))
		if got, err := io.ReadAll(server); err != nil || string(got) != "req" {
			t.Fatalf("round %d: the destination read %q, %v; want the request and the FIN", round, got, err)
		}
		if _, err := server.Write([]byte(first)); err != nil {
			t.Fatal(err)
		}
		_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
		got := make([]byte, len(first))
		if _, err := io.ReadFull(app, got); err != nil || string(got) != first {
			t.Fatalf("round %d: the half-closed client read %q, %v", round, got, err)
		}
		// The client's stack has acknowledged the first answer.
		for limit := time.Now().Add(tcpSettle); bytesAcked(t, proxyClient) < uint64(len(first)); {
			if time.Now().After(limit) {
				t.Fatalf("round %d: %d bytes acknowledged after %v, want %d", round, bytesAcked(t, proxyClient), tcpSettle, len(first))
			}
			time.Sleep(20 * time.Microsecond)
		}
		_ = app.Close()
		if _, err := server.Write([]byte(second)); err != nil {
			t.Fatal(err)
		}
		awaitTCPState(t, proxyClient, stateClosed, "the closed client resets the second answer")
		_ = server.Close()
		select {
		case s := <-streams:
			if s != first {
				t.Fatalf("round %d: the destination's stream carried %q, want only the first answer, which the client read", round, s)
			}
		case <-time.After(tcpSettle):
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
			for limit := time.Now().Add(tcpSettle); tcpState(t, proxyClient) != stateCloseWait && time.Now().Before(limit); {
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
		_ = server.SetReadDeadline(time.Now().Add(tcpSettle))
		if got := make([]byte, 3); func() bool { _, err := io.ReadFull(server, got); return err != nil }() {
			t.Fatalf("round %d: the destination did not read the request", round)
		}
		if _, err := server.Write([]byte("inflight")); err != nil {
			t.Fatal(err)
		}
		awaitTCPState(t, proxyClient, stateClosed, "the closed client resets the chunk written as its FIN came in")
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
		case <-time.After(tcpSettle):
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

// smallWindow is the receive buffer dialSmallWindow asks for.
const smallWindow = 2048

// dialSmallWindow is a loopback client connection whose client end (app) has
// a small receive buffer, so what the proxy's end writes to it fills it while
// it does not read, and whose proxy's end (proxyClient) has a send buffer
// that holds far more: the proxy's writes return while the client has
// acknowledged little of them. Linux keeps the small buffer asked for before
// connect (smallWindow, read back as 4096); macOS raises it as it connects
// (read back as 326640 on macOS 26) and may grow it after, while the row runs
// (receiveBufferGrownOf), so a row sizes what it sends from the most the
// client's socket can come to hold (receiveBufferBoundOf), never from the
// number asked for.
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
		if err := c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, smallWindow) }); err != nil {
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

// On a real socket, written counts every byte written to it: those the
// client's stack has acknowledged and those it has not yet, from one answer
// on macOS. Here what the proxy wrote fits the client's window, so the count
// is exact whatever the client has acknowledged by the time it is asked.
func TestWrittenCountsEveryByteWrittenToARealSocket(t *testing.T) {
	t.Parallel()
	_, proxyClient := loopbackPair(t)
	s := clientSocketOf(proxyClient, zap.NewNop())
	// The relay's first look comes before it writes (clientFINBefore), and
	// macOS counts from the first look (client_took_darwin.go).
	_ = s.look()
	const n = 64 << 10
	if _, err := proxyClient.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.written(); !ok || got != n {
		t.Fatalf("written %d (ok %v), want the %d bytes written", got, ok, n)
	}
}

// seenWrites counts the bytes written through it to the socket under it (n),
// as a test counts them, apart from any count of keploy's.
type seenWrites struct {
	net.Conn
	n atomic.Uint64
}

func (c *seenWrites) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.n.Add(uint64(n))
	return n, err
}

func (c *seenWrites) NetConn() net.Conn { return c.Conn }

// acked is how many of the bytes written through c the socket's peer has
// acknowledged: those less the bytes the socket still holds (queuedOf), while
// no Write runs.
func (c *seenWrites) acked(t *testing.T) uint64 {
	t.Helper()
	rc, err := c.Conn.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var queued int
	var qerr error
	if err := rc.Control(func(fd uintptr) { queued, qerr = queuedOf(fd) }); err != nil || qerr != nil {
		t.Fatalf("the socket cannot say what it holds: %v %v", err, qerr)
	}
	return c.n.Load() - uint64(queued)
}

// The rows below need the client's receive window closed: the client reads
// nothing while the proxy has more to send. Linux counts what the client
// acknowledged from tcpi_bytes_acked, and macOS from the bytes written to the
// socket less those its send buffer holds (client_took_darwin.go), so both
// counts are exact while the window is closed, and the rows run on both.

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
	// What the destination sends before its FIN is more than the client's
	// receive buffer holds, so it fills it while the client does not read.
	app, proxyClient := dialSmallWindow(t)
	proxyServer, server := loopbackPair(t)
	core, logs := observer.New(zapcore.DebugLevel)
	// The test counts what the relay writes to the client itself (seenWrites),
	// over the count the proxy puts on the socket as it accepts it
	// (CountClientWrites), which macOS reads what the client acknowledged
	// from.
	seen := &seenWrites{Conn: CountClientWrites(proxyClient)}
	r := New(Config{Logger: zap.New(core), HalfCloseGrace: time.Minute}, seen, connseq.NewUpstream(proxyServer))
	looks := &countedLooks{clientSocket: r.client}
	r.client = looks
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	streams := make(chan string, 1)
	go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()

	before := strings.Repeat("b", receiveBufferBoundOf(t, app)+8<<10)
	if _, err := server.Write([]byte(before)); err != nil {
		t.Fatal(err)
	}
	// All of it is written to the client before the client's FIN: the relay
	// may read it in more than one chunk, the first of which can fill the
	// client's receive buffer alone, and a chunk written after the FIN is
	// set aside, not teed.
	for limit := time.Now().Add(tcpSettle); seen.n.Load() != uint64(len(before)); {
		if time.Now().After(limit) {
			t.Fatalf("the relay wrote %d bytes to the client in %v, want the %d sent", seen.n.Load(), tcpSettle, len(before))
		}
		time.Sleep(50 * time.Microsecond)
	}
	// The client's window is closed: its stack has acknowledged all it can
	// hold, and acknowledges no more until it reads.
	for limit := time.Now().Add(tcpSettle); !windowClosed(t, proxyClient); {
		if time.Now().After(limit) {
			t.Fatalf("the client's receive window did not close in %v", tcpSettle)
		}
		time.Sleep(50 * time.Microsecond)
	}
	if err := app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(tcpSettle))
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
	for limit := time.Now().Add(tcpSettle); tcpState(t, proxyClient) != stateLastAck; {
		if time.Now().After(limit) {
			t.Fatalf("the relay did not forward the destination's FIN in %v (state %d)", tcpSettle, tcpState(t, proxyClient))
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
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	got, err := io.ReadAll(app)
	if err != nil || string(got) != before+answer {
		t.Fatalf("the client read %d bytes (%v), want the %d it was sent", len(got), err, len(before)+len(answer))
	}
	select {
	case s := <-streams:
		if s != before+answer {
			t.Fatalf("the destination's stream carried %d bytes, want the %d the client took", len(s), len(before)+len(answer))
		}
	case <-time.After(tcpSettle):
		t.Fatal("the destination's stream did not end")
	}
	if n := notTakenLogged(t, logs); n != 0 {
		t.Fatalf("the relay logged %d bytes the client did not take, want none", n)
	}
}

// On a real socket, written counts the bytes the client's stack has not
// acknowledged yet as well as those it has: here what the proxy wrote is
// more than the client's receive buffer holds, and the rest waits in the
// proxy's send queue while the client, its window full, reads nothing. Once
// the client reads them all, its stack has acknowledged every one of them.
// The bytes are written as the proxy writes them: through the count it puts
// on the socket as it accepts it (CountClientWrites), which the relay's looks
// read from (countFromAccept; on macOS the count the socket answers from,
// begun before the write).
func TestWrittenCountsWhatTheClientHasNotAcknowledgedYet(t *testing.T) {
	t.Parallel()
	app, proxyClient := dialSmallWindow(t)
	client := CountClientWrites(proxyClient)
	s := clientSocketOf(client, zap.NewNop())
	countFromAccept(client, s)
	n := uint64(receiveBufferBoundOf(t, app) + 128<<10)
	if _, err := client.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	for limit := time.Now().Add(tcpSettle); !windowClosed(t, proxyClient); {
		if time.Now().After(limit) {
			t.Fatalf("the client's receive window did not close in %v", tcpSettle)
		}
		time.Sleep(50 * time.Microsecond)
	}
	if l := s.look(); l.said != clientOpen || l.acked >= n {
		t.Fatalf("look: %+v, want open with fewer than the %d bytes acknowledged: the client holds fewer and reads nothing", l, n)
	}
	if got, ok := s.written(); !ok || got != n {
		t.Fatalf("written %d (ok %v), want the %d bytes written", got, ok, n)
	}
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	if _, err := io.ReadFull(app, make([]byte, n)); err != nil {
		t.Fatalf("the client read: %v", err)
	}
	for limit := time.Now().Add(tcpSettle); s.look().acked != n; {
		if time.Now().After(limit) {
			t.Fatalf("the client read all %d bytes and its stack acknowledged %d after %v", n, s.look().acked, tcpSettle)
		}
		time.Sleep(50 * time.Microsecond)
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
//
// Here the client's socket is counted from its accept, as the proxy counts it
// (CountClientWrites): over plain TCP, and under TLS, where every chunk the
// relay writes reaches the socket as a TLS record. On macOS the count is what
// the client acknowledged is read from (client_took_darwin.go); on Linux it is
// the socket itself, which counts what its client acknowledged, and Linux
// runs the row on the socket as the listener gave it too
// (TestAChunkBehindOneThatIsNotAcknowledgedYetIsJudgedFromItsOwnFirstByte).
func TestAChunkBehindOneIsJudgedFromItsOwnFirstByteOnASocketCountedFromItsAccept(t *testing.T) {
	t.Parallel()
	judgedFromItsOwnFirstByte(t, acceptCounts)
}

func TestAChunkBehindOneIsJudgedFromItsOwnFirstByteUnderTLS(t *testing.T) {
	t.Parallel()
	judgedFromItsOwnFirstByte(t, acceptCountsUnderTLS)
}

// How a row's client conn is counted: not at all (the socket as the listener
// gave it), or from the socket's accept (CountClientWrites), over plain TCP or
// under TLS.
type countedBy int

const (
	noAcceptCount countedBy = iota
	acceptCounts
	acceptCountsUnderTLS
)

// clientUnder is the client conn a row hands the relay, counted as by says,
// the app's end to read and write through (a TLS client under TLS), and the
// test's own count of the bytes written to the socket (sock, under any TLS).
func clientUnder(t *testing.T, by countedBy, app, proxyClient net.Conn) (src, appEnd net.Conn, sock *seenWrites) {
	t.Helper()
	if by == noAcceptCount {
		sock = &seenWrites{Conn: proxyClient}
		return sock, app, sock
	}
	sock = &seenWrites{Conn: CountClientWrites(proxyClient)}
	if by == acceptCounts {
		return sock, app, sock
	}
	srv := tls.Server(sock, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}, MinVersion: tls.VersionTLS12})
	cli := tls.Client(app, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) // #nosec G402 -- the test's own self-signed server
	errs := make(chan error, 1)
	go func() { errs <- cli.Handshake() }()
	if err := srv.Handshake(); err != nil {
		t.Fatalf("the proxy's TLS handshake: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("the client's TLS handshake: %v", err)
	}
	return srv, cli, sock
}

// halfClose sends the app's FIN (under TLS, after its close_notify).
func halfClose(t *testing.T, appEnd, app net.Conn) {
	t.Helper()
	if cli, ok := appEnd.(*tls.Conn); ok {
		if err := cli.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
}

func judgedFromItsOwnFirstByte(t *testing.T, by countedBy) {
	app, proxyClient := dialSmallWindow(t)
	first, second := strings.Repeat("1", receiveBufferBoundOf(t, app)+128<<10), strings.Repeat("2", 1<<10)
	proxyServer, server := loopbackPair(t)
	// The test counts what the relay writes to the client (plain), and what
	// the client acknowledged of the bytes written to its socket (seen),
	// itself (seenWrites).
	under, appEnd, seen := clientUnder(t, by, app, proxyClient)
	plain := &seenWrites{Conn: under}
	r := New(Config{Logger: zap.NewNop()}, plain, connseq.NewUpstream(proxyServer))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	streams := make(chan string, 1)
	go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()

	if _, err := appEnd.Write([]byte("req")); err != nil {
		t.Fatal(err)
	}
	halfClose(t, appEnd, app)
	_ = server.SetReadDeadline(time.Now().Add(tcpSettle))
	if got, err := io.ReadAll(server); err != nil || string(got) != "req" {
		t.Fatalf("the destination read %q, %v; want the request and the FIN", got, err)
	}
	writtenAll := func(want uint64, what string) {
		t.Helper()
		for limit := time.Now().Add(tcpSettle); plain.n.Load() != want; {
			if time.Now().After(limit) {
				t.Fatalf("%s: the relay wrote %d bytes to the client in %v, want %d", what, plain.n.Load(), tcpSettle, want)
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
	behind := seen.acked(t)
	if behind >= uint64(len(first)) {
		t.Fatalf("the client acknowledged %d bytes before it read any, want fewer than the first answer's %d", behind, len(first))
	}
	// The acknowledgement can come some time after the read that opened the
	// client's window (macOS's loopback hands segments on through a queue),
	// so once the client has read readCap bytes it reads no more and waits
	// for it. readCap opens the window by more than two of loopback's 16K
	// segments: xnu's receiver sends a window update at once only for that
	// much (tcp_output), and otherwise waits for the proxy's window probe,
	// which xnu's persist timer sends no sooner than 5 s (TCPTV_PERSMIN); a
	// 32K cap opened it by less on 1 run in 70. And it keeps the window short
	// of the second answer, 128K behind the most the client's receive buffer
	// can come to hold (receiveBufferBoundOf): macOS grows it as the client
	// reads, and a first answer sized from what it held as it connected was
	// acknowledged whole on up to 20 runs in 1000.
	const readCap = 64 << 10
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	buf := make([]byte, 512)
	for read, limit := 0, time.Now().Add(tcpSettle); seen.acked(t) <= behind; {
		if time.Now().After(limit) {
			t.Fatalf("the client read %d bytes and its stack acknowledged no more than %d in %v", read, behind, tcpSettle)
		}
		if read >= readCap {
			time.Sleep(50 * time.Microsecond)
			continue
		}
		n, err := appEnd.Read(buf)
		if err != nil {
			t.Fatalf("the client read: %v", err)
		}
		read += n
	}
	// It closes with bytes unread: a reset, with the second answer never
	// acknowledged.
	_ = app.Close()
	awaitTCPState(t, proxyClient, stateClosed, "the client closed with bytes unread")
	if acked := seen.acked(t); acked >= uint64(len(first)) {
		t.Fatalf("the client acknowledged %d bytes, want fewer than the first answer's %d", acked, len(first))
	}
	_ = server.Close()
	var got string
	select {
	case got = <-streams:
		if got == "" || !strings.HasPrefix(first, got) {
			t.Fatalf("the destination's stream carried %d bytes (the second answer's in it: %v), want a part of the first answer, which the client was reading", len(got), strings.Contains(got, "2"))
		}
	case <-time.After(tcpSettle):
		t.Fatal("the destination's stream did not end")
	}
	// Every byte the relay wrote is either in the stream or refused, never
	// both, never neither.
	if chunks, bytes := r.NotTaken(); chunks == 0 || bytes < int64(len(second)) || bytes != int64(len(first)+len(second)-len(got)) {
		t.Fatalf("%d chunks (%d bytes) not taken, want the second answer's %d bytes among them, and every byte of the %d written not in the stream's %d",
			chunks, bytes, len(second), len(first)+len(second), len(got))
	}
}

// Under TLS, the count from the socket's accept counts every byte written to
// the socket, the handshake's and each record's own among them: the bytes the
// client's stack has acknowledged and those it has not yet, while the client
// reads nothing, and all of them once it read the whole answer.
func TestWrittenCountsEveryByteUnderTLSFromTheSocketsAccept(t *testing.T) {
	t.Parallel()
	app, proxyClient := dialSmallWindow(t)
	src, appEnd, sock := clientUnder(t, acceptCountsUnderTLS, app, proxyClient)
	s := clientSocketOf(src, zap.NewNop())
	if s == nil {
		t.Fatal("the socket under the TLS layer and the count from its accept is not asked")
	}
	countFromAccept(src, s)
	n := receiveBufferBoundOf(t, app) + 128<<10
	if _, err := src.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	for limit := time.Now().Add(tcpSettle); !windowClosed(t, proxyClient); {
		if time.Now().After(limit) {
			t.Fatalf("the client's receive window did not close in %v", tcpSettle)
		}
		time.Sleep(50 * time.Microsecond)
	}
	all := sock.n.Load()
	if l := s.look(); l.said != clientOpen || l.acked >= all {
		t.Fatalf("look: %+v, want open with fewer than the %d bytes written acknowledged", l, all)
	}
	if got, ok := s.written(); !ok || got != all {
		t.Fatalf("written %d (ok %v), want the %d bytes written to the socket", got, ok, all)
	}
	_ = appEnd.SetReadDeadline(time.Now().Add(tcpSettle))
	if _, err := io.ReadFull(appEnd, make([]byte, n)); err != nil {
		t.Fatalf("the client read: %v", err)
	}
	for limit := time.Now().Add(tcpSettle); s.look().acked != all; {
		if time.Now().After(limit) {
			t.Fatalf("the client read the whole answer and its stack acknowledged %d of the %d bytes after %v", s.look().acked, all, tcpSettle)
		}
		time.Sleep(50 * time.Microsecond)
	}
}
