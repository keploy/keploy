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
// Those that need neither a closed receive window nor a count Windows cannot
// give are in client_took_socket_any_test.go, and run on Windows too.

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
// Not on Windows: there a socket with no count from its accept says only the
// bytes it sent, not those its send buffer still holds (client_socket_windows.go),
// and client_count_windows_test.go's rows hold the count from the accept.
func TestWrittenCountsEveryByteWrittenToARealSocket(t *testing.T) {
	t.Parallel()
	_, proxyClient := loopbackPair(t)
	s := clientSocketOf(proxyClient, zap.NewNop())
	if s == nil {
		t.Fatal("the client's socket is not asked")
	}
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
	looks := &countedLooks{clientSocket: tcpSocket(t, r.client)}
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
	if s == nil {
		t.Fatal("the socket under the count from its accept is not asked")
	}
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
