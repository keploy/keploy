package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.uber.org/zap"
)

// countedXNU is a client socket whose TCP_INFO is answered from a script (in
// turn, the last repeats), with the count of the relay's writes begun from the
// script's first answer, and said bytes written through it since.
func countedXNU(t *testing.T, said uint64, infos ...darwinTCPInfo) *tcpClientSocket {
	t.Helper()
	s := (&scriptedXNU{infos: infos}).socket()
	if !s.beginCount() {
		t.Fatal("the count of writes did not begin on a socket that answers")
	}
	s.writes.said = said
	return s
}

// On macOS a look counts what the client acknowledged from the bytes written
// through the count less those the send buffer holds, from what it held as the
// count began: exactly, whatever snd_nxt does, as when the client's closed
// window pulls it back, and once the connection is reset. written is every
// byte written, from the same start.
func TestXNUCountsWhatTheClientAcknowledgedFromTheBytesWrittenToIt(t *testing.T) {
	s := countedXNU(t, 1000,
		// As the count begins: 100 bytes held, written before the relay's.
		darwinTCPInfo{state: tcpsEstablished, sndNxt: 5000, txunacked: 100, sbbytes: 100},
		// 1000 written since, 600 held: 500 acknowledged.
		darwinTCPInfo{state: tcpsCloseWait, sndNxt: 6100, txunacked: 600, sbbytes: 600},
		// The client's window closed and snd_nxt went back: 300 held, so
		// 800 acknowledged.
		darwinTCPInfo{state: tcpsCloseWait, sndNxt: 5800, lossRecovery: true, txunacked: 300, sbbytes: 300},
		// The client reset: 300 still held, while snd_nxt says less.
		darwinTCPInfo{state: tcpsClosed, sndNxt: 5100, lossRecovery: true, txunacked: 1000, sbbytes: 300},
	)
	if l := s.look(); l != (clientLook{said: clientFinned, acked: 500}) {
		t.Fatalf("first look: %+v, want the FIN in, 500 acknowledged", l)
	}
	if n, ok := s.written(); !ok || n != 1100 {
		t.Fatalf("written: %d %v, want 1100 (100 held as the count began, 1000 written since)", n, ok)
	}
	if l := s.look(); l != (clientLook{said: clientFinned, acked: 800}) {
		t.Fatalf("a look with snd_nxt gone back: %+v, want the FIN in, 800 acknowledged", l)
	}
	if l := s.look(); l != (clientLook{said: clientClosed, acked: 800}) {
		t.Fatalf("a look once reset: %+v, want closed, 800 acknowledged", l)
	}
}

// While a Write runs, or once one failed, the count is a range: from the
// bytes the Writes said they wrote (lo, here 800 acknowledged) to those and
// the bytes they may have written as well (hi, 1000). A look that finds the
// connection open counts lo. One that finds it closed counts the least that is
// no less than what the client acknowledged: hi, snd_nxt, or the count of
// sequence numbers when it is exact, never less than lo.
func TestXNUCountsARangeWhileAWriteRunsOrOnceOneFailed(t *testing.T) {
	begin := darwinTCPInfo{state: tcpsEstablished, sndNxt: 5000, txunacked: 100, sbbytes: 100}
	for _, row := range []struct {
		name string
		info darwinTCPInfo
		want clientLook
	}{
		{"open: what was said", darwinTCPInfo{state: tcpsCloseWait, sndNxt: 6200, txunacked: 300, sbbytes: 300},
			clientLook{said: clientFinned, acked: 800}},
		{"closed: snd_nxt bounds it", darwinTCPInfo{state: tcpsClosed, sndNxt: 5750, lossRecovery: true, sbbytes: 300},
			clientLook{said: clientClosed, acked: 850}},
		{"time-wait: snd_nxt bounds it", darwinTCPInfo{state: tcpsTimeWait, sndNxt: 5750, lossRecovery: true, sbbytes: 300},
			clientLook{said: clientClosed, acked: 850}},
		{"closed: hi bounds it", darwinTCPInfo{state: tcpsClosed, sndNxt: 6200, lossRecovery: true, sbbytes: 300},
			clientLook{said: clientClosed, acked: 1000}},
		{"closed: the exact count of sequence numbers", darwinTCPInfo{state: tcpsClosed, sndNxt: 5850, txunacked: 120, sbbytes: 300},
			clientLook{said: clientClosed, acked: 830}},
		{"closed: never less than lo", darwinTCPInfo{state: tcpsClosed, sndNxt: 5850, txunacked: 300, sbbytes: 300},
			clientLook{said: clientClosed, acked: 800}},
		{"closed: an exact count of sequence numbers above hi", darwinTCPInfo{state: tcpsClosed, sndNxt: 6200, sbbytes: 300},
			clientLook{said: clientClosed, acked: 1000}},
	} {
		for _, way := range []string{"running", "failed"} {
			t.Run(row.name+", a Write "+way, func(t *testing.T) {
				s := countedXNU(t, 1000, begin, row.info)
				if way == "running" {
					s.writes.running = 200
				} else {
					s.writes.unsure = 200
				}
				if l := s.look(); l != row.want {
					t.Fatalf("look: %+v, want %+v", l, row.want)
				}
				if n, ok := s.written(); !ok || n != 1300 {
					t.Fatalf("written: %d %v, want 1300, no fewer than may have been written", n, ok)
				}
			})
		}
	}
}

// A send buffer that holds more than could have been written through the
// count says a write went around it: the socket says nothing from then on,
// and written cannot say. One that holds every byte written, none of them
// acknowledged, is counted.
func TestXNUASendBufferHoldingMoreThanWasCountedSaysNothing(t *testing.T) {
	for _, row := range []struct {
		name  string
		infos []darwinTCPInfo
	}{
		{"more than was written", []darwinTCPInfo{{state: tcpsCloseWait, sbbytes: 150}}},
		{"more than the acknowledged bytes leave", []darwinTCPInfo{
			{state: tcpsCloseWait, sbbytes: 20}, {state: tcpsCloseWait, sbbytes: 50},
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			infos := append([]darwinTCPInfo{
				{state: tcpsEstablished},
				{state: tcpsCloseWait, sbbytes: 100},
			}, row.infos...)
			// Then an answer that alone would count: all acknowledged.
			s := countedXNU(t, 100, append(infos, darwinTCPInfo{state: tcpsCloseWait})...)
			if l := s.look(); l != (clientLook{said: clientFinned}) {
				t.Fatalf("every byte written held: %+v, want the FIN in, none acknowledged", l)
			}
			for range row.infos {
				_ = s.look()
			}
			if s.got.said != clientUnknown {
				t.Fatalf("a send buffer holding more than was counted: %+v, want unknown", s.got)
			}
			if l := s.look(); l.said != clientUnknown {
				t.Fatalf("a later look: %+v, want unknown still", l)
			}
			if _, ok := s.written(); ok {
				t.Fatal("written says a count once a write went around it, want it cannot say")
			}
		})
	}
}

// The count of writes is taken only where it holds: on a socket clientSocketOf
// found with no TLS layer between it and the conn the relay writes to (TLS
// writes other bytes than it is given), that answers as the count begins.
// Elsewhere the relay writes to its conn as it is.
func TestTheRelaysWritesAreCountedOnlyWhereTheCountHolds(t *testing.T) {
	t.Parallel()
	_, proxyClient := loopbackPair(t)
	if c, ok := countWrites(proxyClient, clientSocketOf(proxyClient, zap.NewNop())).(*writeCountingConn); !ok || c.Conn != proxyClient {
		t.Fatal("a TCP socket's writes are not counted")
	}
	under := &seenWrites{Conn: proxyClient}
	if _, ok := countWrites(under, clientSocketOf(under, zap.NewNop())).(*writeCountingConn); !ok {
		t.Fatal("the writes to a conn that says what is under it (NetConn) are not counted")
	}
	tlsLayer := tls.Server(proxyClient, &tls.Config{MinVersion: tls.VersionTLS12})
	if s := clientSocketOf(tlsLayer, zap.NewNop()); s == nil || countWrites(tlsLayer, s) != net.Conn(tlsLayer) {
		t.Fatal("the writes to a TLS layer are counted, or its socket is not asked")
	}
	overTLS := &seenWrites{Conn: tlsLayer}
	if s := clientSocketOf(overTLS, zap.NewNop()); s == nil || countWrites(overTLS, s) != net.Conn(overTLS) {
		t.Fatal("the writes to a conn over a TLS layer are counted, or its socket is not asked")
	}
	silent := &tcpClientSocket{rc: noFD{}, askInfo: func(uintptr, *[tcpInfoBuf]byte, *darwinTCPInfo) bool { return false }}
	if countWrites(proxyClient, silent) != proxyClient || silent.writes != nil {
		t.Fatal("the writes to a socket that cannot answer as the count begins are counted")
	}
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("countWrites with no socket to ask panicked: %v", p)
			}
		}()
		if countWrites(proxyClient, nil) != proxyClient {
			t.Error("the writes to a conn with no socket asked are counted")
		}
		if countWrites(proxyClient, &countedLooks{}) != proxyClient {
			t.Error("the writes are counted for a client socket that is no TCP socket's")
		}
	}()
}

// The count says what each Write said it wrote, every byte of a Write still
// running, and what a failed Write did not say it wrote. It half-closes the
// conn under it.
func TestTheCountOfWritesSaysWhatMayHaveBeenWritten(t *testing.T) {
	t.Parallel()
	t.Run("a Write that ends", func(t *testing.T) {
		_, proxyClient := loopbackPair(t)
		c := &writeCountingConn{Conn: proxyClient, count: &writeCount{}}
		if n, err := c.Write(make([]byte, 1000)); n != 1000 || err != nil {
			t.Fatalf("Write: %d %v", n, err)
		}
		if got := c.count.now(); got != (tally{said: 1000}) {
			t.Fatalf("count: %+v, want 1000 said, none maybe", got)
		}
	})
	t.Run("a Write that runs until the client reads", func(t *testing.T) {
		app, proxyClient := dialSmallWindow(t)
		c := &writeCountingConn{Conn: proxyClient, count: &writeCount{}}
		n := 4 << 20 // more than both buffers hold
		wrote := make(chan error, 1)
		go func() { _, err := c.Write(make([]byte, n)); wrote <- err }()
		for limit := time.Now().Add(tcpSettle); !windowClosed(t, proxyClient); {
			if time.Now().After(limit) {
				t.Fatalf("the client's receive window did not close in %v", tcpSettle)
			}
			time.Sleep(50 * time.Microsecond)
		}
		if got := c.count.now(); got != (tally{maybe: uint64(n)}) {
			t.Fatalf("count while the Write runs: %+v, want none said, all %d maybe", got, n)
		}
		_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
		if _, err := io.ReadFull(app, make([]byte, n)); err != nil {
			t.Fatalf("the client read: %v", err)
		}
		if err := <-wrote; err != nil {
			t.Fatal(err)
		}
		if got := c.count.now(); got != (tally{said: uint64(n)}) {
			t.Fatalf("count once the Write ended: %+v, want all %d said", got, n)
		}
	})
	t.Run("a Write that fails", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		c := &writeCountingConn{Conn: proxyClient, count: &writeCount{}}
		if err := app.(*net.TCPConn).SetLinger(0); err != nil {
			t.Fatal(err)
		}
		_ = app.Close()
		awaitTCPState(t, proxyClient, tcpsClosed, "the client reset")
		n, err := c.Write(make([]byte, 100))
		if err == nil {
			t.Fatal("a Write to a reset connection did not fail")
		}
		if got := c.count.now(); got != (tally{said: uint64(n), maybe: uint64(100 - n)}) {
			t.Fatalf("count: %+v after a failed Write that said %d, want those said and the rest maybe", got, n)
		}
	})
	t.Run("CloseWrite", func(t *testing.T) {
		app, proxyClient := loopbackPair(t)
		c := &writeCountingConn{Conn: proxyClient, count: &writeCount{}}
		if err := c.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
		if got, err := io.ReadAll(app); err != nil || len(got) != 0 {
			t.Fatalf("the client read %d bytes, %v; want the FIN", len(got), err)
		}
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
		if err := (&writeCountingConn{Conn: a, count: &writeCount{}}).CloseWrite(); !errors.Is(err, errNoCloseWrite) {
			t.Fatalf("CloseWrite of a conn that cannot half-close: %v, want errNoCloseWrite", err)
		}
	})
}

// What a look counts for certain never goes back, though a Write that starts
// after it appends bytes it has not said yet; nor is it ever less than none.
// A Write that ends while the socket answers may have appended its bytes
// before the answer or after: an open look counts them not acknowledged, a
// closed one acknowledged.
func TestXNUCountsWhatIsCertainAcrossWritesThatRun(t *testing.T) {
	t.Run("a Write that starts", func(t *testing.T) {
		s := countedXNU(t, 0,
			darwinTCPInfo{state: tcpsEstablished},
			// 500 running, 300 of them appended, none said.
			darwinTCPInfo{state: tcpsCloseWait, sbbytes: 300},
			// The Write said 500; 200 held: 300 acknowledged.
			darwinTCPInfo{state: tcpsCloseWait, sbbytes: 200},
			// Another 500 running, all appended: 700 held.
			darwinTCPInfo{state: tcpsCloseWait, sbbytes: 700},
		)
		s.writes.running = 500
		if l := s.look(); l != (clientLook{said: clientFinned}) {
			t.Fatalf("a Write running with nothing said: %+v, want none acknowledged", l)
		}
		s.writes.running, s.writes.said = 0, 500
		if l := s.look(); l != (clientLook{said: clientFinned, acked: 300}) {
			t.Fatalf("once it said 500: %+v, want 300 acknowledged", l)
		}
		s.writes.running = 500
		if l := s.look(); l != (clientLook{said: clientFinned, acked: 300}) {
			t.Fatalf("another Write running: %+v, want the 300 counted before still", l)
		}
	})
	for _, row := range []struct {
		state uint8
		want  clientLook
	}{
		{tcpsCloseWait, clientLook{said: clientFinned, acked: 800}},
		{tcpsClosed, clientLook{said: clientClosed, acked: 1000}},
	} {
		t.Run(fmt.Sprintf("a Write that ends as the socket answers, state %d", row.state), func(t *testing.T) {
			s := countedXNU(t, 1000,
				darwinTCPInfo{state: tcpsEstablished, sndNxt: 5000, txunacked: 100, sbbytes: 100},
				darwinTCPInfo{state: row.state, sndNxt: 6400, lossRecovery: true, sbbytes: 300},
			)
			ask := s.askInfo
			s.askInfo = func(fd uintptr, buf *[tcpInfoBuf]byte, info *darwinTCPInfo) bool {
				s.writes.said += 200
				return ask(fd, buf, info)
			}
			if l := s.look(); l != row.want {
				t.Fatalf("look: %+v, want %+v", l, row.want)
			}
		})
	}
}

// The proxy counts what is written to an accepted client's socket from its
// accept (CountClientWrites): the relay counts from there, under a TLS layer
// and a wrapper over it too, puts no count of its own over the conn, and the
// socket is found under the count (AcceptedConn). Writes through the count
// reach the socket and are counted, and it half-closes the socket. A socket
// that cannot answer as it is accepted, and a conn that is not the accepted
// socket, are not counted.
func TestTheCountFromTheAcceptIsTheRelaysUnderTLSToo(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	a, ok := CountClientWrites(proxyClient).(*countedClient)
	if !ok || a.tcp != proxyClient || AcceptedConn(a) != proxyClient || AcceptedConn(proxyClient) != proxyClient {
		t.Fatal("an accepted socket is not counted, or not found under its count")
	}
	tlsLayer := tls.Server(a, &tls.Config{MinVersion: tls.VersionTLS12})
	for _, src := range []net.Conn{a, tlsLayer, &seenWrites{Conn: tlsLayer}} {
		s, _ := clientSocketOf(src, zap.NewNop()).(*tcpClientSocket)
		if s == nil {
			t.Fatalf("the socket under %T is not asked", src)
		}
		if c := countWrites(src, s); c != src || s.writes != a.count || s.unsent0 != uint64(a.begin.sbbytes) || !s.seen {
			t.Errorf("countWrites(%T) gave %T, counting from the accept: %v", src, c, s.writes == a.count)
		}
	}
	if _, err := a.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if w := a.count.now(); w.said != 10 || w.maybe != 0 {
		t.Fatalf("the count after a write of 10 bytes: %+v", w)
	}
	var c net.Conn = a
	if !halfCloseWrite(&c) {
		t.Fatal("the count did not half-close the socket")
	}
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	if got, err := io.ReadAll(app); err != nil || string(got) != "0123456789" {
		t.Fatalf("the client read %q, %v: want the bytes written, then the FIN", got, err)
	}
	if rc, err := a.SyscallConn(); err != nil || rc == nil {
		t.Fatalf("the count does not give the socket's own SyscallConn: %v", err)
	}
	_, gone := loopbackPair(t)
	_ = gone.Close()
	if CountClientWrites(gone) != gone {
		t.Error("a socket that cannot answer as it is accepted is counted")
	}
	// A conn that is not the accepted socket, a nil socket (whose SyscallConn
	// dereferences it) and a socket with no descriptor (whose SyscallConn
	// fails) are not counted, and never asked: a nil RawConn's Control
	// panics.
	for what, c := range map[string]net.Conn{
		"a conn that is not the accepted socket": &seenWrites{Conn: proxyClient},
		"a nil socket":                           (*net.TCPConn)(nil),
		"a socket with no descriptor":            &net.TCPConn{},
	} {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("counting %s panicked: %v", what, p)
				}
			}()
			if CountClientWrites(c) != c {
				t.Errorf("%s is counted", what)
			}
		}()
	}
	// What the send buffer held as the count began is the count's start.
	held := &countedClient{tcp: proxyClient.(*net.TCPConn), count: &writeCount{}, begin: darwinTCPInfo{sbbytes: 7}}
	held.Conn = held.tcp
	if s, _ := clientSocketOf(held, zap.NewNop()).(*tcpClientSocket); s == nil || countWrites(held, s) != net.Conn(held) || s.unsent0 != 7 {
		t.Error("the count does not start from what the send buffer held as it began")
	}
}

// A client whose socket is resending as it resets: xnu flags the socket in
// loss recovery (TCPI_FLAG_LOSSRECOVERY), as it did on the closed connections
// a lost loopback segment left under load, so the count of sequence numbers
// cannot say what the client acknowledged, and a look at it alone would have
// the answer teed. The count of the bytes written to the socket still says
// exactly, so through the relay the part of the answer the client never
// acknowledged is refused, every byte of it, and the rest is in the
// destination's stream. Here the flag is set on the socket's real answers
// once the client's window has closed: the relay's count of its own writes,
// and the count from the accept, over plain TCP and under TLS.
func TestAResendingClientThatResetsHasWhatItNeverAcknowledgedRefused(t *testing.T) {
	t.Parallel()
	for name, by := range map[string]countedBy{
		"the relay's count":              relayCounts,
		"the count from the accept":      acceptCounts,
		"the count from the accept, TLS": acceptCountsUnderTLS,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resendingClientResets(t, by)
		})
	}
}

func resendingClientResets(t *testing.T, by countedBy) {
	app, proxyClient := dialSmallWindow(t)
	answer := strings.Repeat("a", receiveBufferBoundOf(t, app)+128<<10)
	proxyServer, server := loopbackPair(t)
	under, appEnd, _ := clientUnder(t, by, app, proxyClient)
	plain := &seenWrites{Conn: under}
	r := New(Config{Logger: zap.NewNop()}, plain, connseq.NewUpstream(proxyServer))
	s, ok := r.client.(*tcpClientSocket)
	if !ok {
		t.Fatalf("the relay asks %T, want the client's TCP socket", r.client)
	}
	var resending atomic.Bool
	ask := s.askInfo
	s.askInfo = func(fd uintptr, buf *[tcpInfoBuf]byte, info *darwinTCPInfo) bool {
		ok := ask(fd, buf, info)
		if resending.Load() {
			info.lossRecovery = true
		}
		return ok
	}
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
	if _, err := server.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	for limit := time.Now().Add(tcpSettle); plain.n.Load() != uint64(len(answer)); {
		if time.Now().After(limit) {
			t.Fatalf("the relay wrote %d bytes to the client in %v, want the %d sent", plain.n.Load(), tcpSettle, len(answer))
		}
		time.Sleep(50 * time.Microsecond)
	}
	for limit := time.Now().Add(tcpSettle); !windowClosed(t, proxyClient); {
		if time.Now().After(limit) {
			t.Fatalf("the client's receive window did not close in %v", tcpSettle)
		}
		time.Sleep(50 * time.Microsecond)
	}
	resending.Store(true)
	// The client closes with the answer unread: a reset, with the rest of
	// the answer never acknowledged.
	_ = app.Close()
	awaitTCPState(t, proxyClient, stateClosed, "the client closed with bytes unread")
	_ = server.Close()
	var got string
	select {
	case got = <-streams:
		if got == "" || !strings.HasPrefix(answer, got) || len(got) == len(answer) {
			t.Fatalf("the destination's stream carried %d bytes of the %d-byte answer, want the part the client acknowledged, short of the whole", len(got), len(answer))
		}
	case <-time.After(tcpSettle):
		t.Fatal("the destination's stream did not end")
	}
	if chunks, bytes := r.NotTaken(); chunks == 0 || bytes != int64(len(answer)-len(got)) {
		t.Fatalf("%d chunks (%d bytes) not taken, want every byte of the %d written not in the stream's %d",
			chunks, bytes, len(answer), len(got))
	}
}
