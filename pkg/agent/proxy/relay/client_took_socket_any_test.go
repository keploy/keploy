//go:build linux || darwin || windows

package relay

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// The rows here drive a real client socket on Linux, macOS and Windows alike,
// and need no closed receive window: the client reads all it is sent, or
// closes. Those that do are in client_took_window_test.go.

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
		if s == nil {
			t.Fatal("the client's socket is not asked")
		}
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
		if s == nil {
			t.Fatal("the client's socket is not asked")
		}
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
