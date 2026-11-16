//go:build linux

package relay

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// This row models Linux's own count of the bytes written (countAt in
// client_took_linux.go): two questions, TCP_INFO and then SIOCOUTQ, which
// acknowledgements can move between. macOS asks one TCP_INFO, which answers
// both at one instant; its count has its own rows (client_took_darwin_test.go).

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

// The rows below need the client's receive window closed: the client reads
// nothing while the proxy has more to send. macOS's count is not exact while
// it is: in 41 runs of each on a Mac (macOS 26), the relay's written said it
// could not say in 5 of TestWrittenCountsWhatTheClientHasNotAcknowledgedYet,
// the first row's answer was not held for the client's answer in 8, and in
// 31 of the last the row's own count of acknowledged bytes (sent, less
// resent, less unacknowledged) stopped moving while the client read. The
// relay reads a count it cannot vouch for as no answer (client_took_darwin.go),
// so on macOS these chunks are teed as before; Linux's count stays exact, and
// these rows check it. macOS's count of bytes written is checked on a real
// socket by TestWrittenCountsEveryByteWrittenToARealSocket, and its readings
// by the scripted rows of client_took_darwin_test.go.

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
	r := New(Config{Logger: zap.New(core), HalfCloseGrace: time.Minute}, proxyClient, connseq.NewUpstream(proxyServer))
	looks := &countedLooks{clientSocket: r.client}
	r.client = looks
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	streams := make(chan string, 1)
	go func() { go streamOf(t, r.ClientStream()); streams <- streamOf(t, r.DestStream()) }()

	before := strings.Repeat("b", receiveBufferOf(t, app)+8<<10)
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
	for limit := time.Now().Add(5 * time.Second); tcpState(t, proxyClient) != stateLastAck; {
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

// On a real socket, written counts the bytes the client's stack has not
// acknowledged yet as well as those it has: here what the proxy wrote is
// more than the client's receive buffer holds, and the rest waits in the
// proxy's send queue while the client, its window full, reads nothing.
func TestWrittenCountsWhatTheClientHasNotAcknowledgedYet(t *testing.T) {
	t.Parallel()
	app, proxyClient := dialSmallWindow(t)
	s := clientSocketOf(proxyClient, zap.NewNop())
	// The relay's first look comes before it writes (clientFINBefore), and
	// macOS counts from the first look (client_took_darwin.go).
	_ = s.look()
	n := uint64(receiveBufferOf(t, app) + 128<<10)
	if _, err := proxyClient.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	if l := s.look(); l.acked >= n {
		t.Fatalf("the client acknowledged %d of the %d bytes, want fewer: it holds fewer and reads nothing", l.acked, n)
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
	app, proxyClient := dialSmallWindow(t)
	first, second := strings.Repeat("1", receiveBufferOf(t, app)+128<<10), strings.Repeat("2", 1<<10)
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
	awaitTCPState(t, proxyClient, stateClosed, "the client closed with bytes unread")
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
