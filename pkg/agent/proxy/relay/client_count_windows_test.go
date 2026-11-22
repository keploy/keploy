//go:build windows

package relay

import (
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A Windows socket counted from its accept says it was written every byte
// written through the count, from what it had sent as the count began, sent
// or not yet: what a closed window holds in the send buffer is written. A
// Write still running, or one that failed, counts every byte it was given. An
// answer that counts more bytes sent than could have been written through the
// count says a write went around it: written says nothing from then on, a
// look's answer telling it as a written's does. Each count is of the bytes
// sent once: a closed window's probe resent is not a write around the count.
// An answer after a fast retransmit or a timeout is not read for it. With no
// count, written is the bytes sent, each once, as before.
func TestWindowsWrittenIsTheCountFromTheAccept(t *testing.T) {
	var fast uint32
	counted := func(out *uint64, retrans *uint32) (*tcpClientSocket, *writeCount) {
		s := winAnsweringSocket(func() []byte {
			return winAnswer(wtcpInfoSize, winTCPInfo{state: wtcpEstablished, out: *out, retrans: *retrans, fastRetrans: fast})
		})
		a := &countedClient{count: &writeCount{said: 10}, begin: winTCPInfo{out: 6, retrans: 2}}
		s.beginFrom(a)
		return s, a.count
	}
	written := func(t *testing.T, s *tcpClientSocket, want uint64, wantOK bool, what string) {
		t.Helper()
		if n, ok := s.written(); n != want && wantOK || ok != wantOK {
			t.Errorf("%s: written %d, %v; want %d, %v", what, n, ok, want, wantOK)
		}
	}
	out, retrans := uint64(14), uint32(0)
	s, c := counted(&out, &retrans)
	if s.writes != c || s.out0 != 4 {
		t.Fatalf("the count does not begin from the bytes the accept had sent once: %v, from %d", s.writes == c, s.out0)
	}
	written(t, s, 14, true, "every byte sent")
	out = 8
	written(t, s, 14, true, "6 bytes still in the send buffer")
	c.running = 5
	written(t, s, 19, true, "a Write running")
	out = 17
	written(t, s, 19, true, "a running Write's bytes sent in part")
	c.running, c.unsure = 0, 5
	written(t, s, 19, true, "a Write that failed")
	// 19 bytes can have been written through the count: the 4 the accept had
	// sent once, the 10 said and the 5 of the failed Write. The window closes
	// with 18 of them sent; its probe sends the 19th and then that byte again
	// three times (BytesOut 22, BytesRetrans 3), so 19 were sent once.
	out, retrans = 22, 3
	written(t, s, 19, true, "a closed window's probe sent again three times")
	out, retrans, fast = 40, 3, 1
	written(t, s, 19, true, "an answer after a fast retransmit")
	out, retrans, fast = 23, 3, 0
	written(t, s, 0, false, "more bytes sent once than written through the count")
	out, retrans = 20, 0
	written(t, s, 0, false, "a count once gone around, nothing sent again")
	out = 8
	written(t, s, 0, false, "a count once gone around")

	out = 20
	looked, _ := counted(&out, &retrans)
	if l := looked.look(); l.said != clientOpen || l.acked != 20 {
		t.Fatalf("a look at an open socket: %+v", l)
	}
	out = 8
	written(t, looked, 0, false, "after a look that counted more bytes sent than written")

	out, retrans = 8, 0
	none := winAnsweringSocket(func() []byte { return winInfo(wtcpInfoSize, wtcpEstablished, 0, out, retrans, 0) })
	written(t, none, 8, true, "no count: the bytes sent")
	out, retrans = 11, 3
	written(t, none, 8, true, "no count: the bytes sent, a probe's resends once")

	fenced := &tcpClientSocket{}
	fenced.beginFrom(&countedClient{count: &writeCount{}, begin: winTCPInfo{out: 6, fastRetrans: 1}})
	if fenced.out0 != 0 {
		t.Fatalf("an accept whose counts cannot say begins from %d, want 0", fenced.out0)
	}
}

// The proxy counts what is written to an accepted client's socket from its
// accept on Windows too (CountClientWrites): the relay counts from there,
// under a TLS layer too, and the socket is found under the count
// (AcceptedConn). Writes through the count reach the client and are written;
// one straight to the socket, around the count, has written say nothing. On
// a real socket, and a call that fails as the count begins.
func TestWindowsTheCountFromTheAcceptIsTheRelays(t *testing.T) {
	t.Parallel()
	app, proxyClient := loopbackPair(t)
	a, ok := CountClientWrites(proxyClient).(*countedClient)
	if !ok || a.tcp != proxyClient || AcceptedConn(a) != proxyClient {
		t.Fatal("an accepted socket is not counted, or not found under its count")
	}
	var s *tcpClientSocket
	for _, src := range []net.Conn{a, tls.Server(a, &tls.Config{MinVersion: tls.VersionTLS12})} {
		s, _ = clientSocketOf(src, zap.NewNop()).(*tcpClientSocket)
		if s == nil {
			t.Fatalf("the socket under %T is not asked", src)
		}
		countFromAccept(src, s)
		if begun, _ := a.begin.sentOnce(); s.writes != a.count || s.out0 != begun {
			t.Fatalf("the relay under %T does not count from the accept", src)
		}
	}
	if _, err := a.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if n, ok := s.written(); n != s.out0+10 || !ok {
		t.Fatalf("written after 10 bytes through the count: %d, %v", n, ok)
	}
	got := make([]byte, 15)
	_ = app.SetReadDeadline(time.Now().Add(tcpSettle))
	if n, err := io.ReadFull(app, got[:10]); err != nil || string(got[:n]) != "0123456789" {
		t.Fatalf("the client read %q, %v", got[:n], err)
	}
	if _, err := AcceptedConn(a).Write([]byte("abcde")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(app, got[10:]); err != nil {
		t.Fatal(err)
	}
	// The client has read them, so they were sent.
	if n, ok := s.written(); ok {
		t.Fatalf("written after a write around the count: %d, %v; want nothing", n, ok)
	}
	_, gone := loopbackPair(t)
	_ = gone.Close()
	if CountClientWrites(gone) != gone {
		t.Error("a socket that cannot answer as it is accepted is counted")
	}
	var info winTCPInfo
	if answerAtAccept(^uintptr(0), &info) {
		t.Error("a SIO_TCP_INFO call that failed is read as an answer")
	}
}
