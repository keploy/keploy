//go:build linux

package util

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// The ends filter passes a destroy message of a named socket, whichever half
// of its cookie differs from another's, and drops every other one: it reads
// the cookie where the message has it, in this host's order.
func TestTheEndsFilterPassesTheNamedSocketsOnly(t *testing.T) {
	named := []uint64{0x0000000100000002, 0xdeadbeef00c0ffee}
	raw := tcpEndsFilter(named)
	insns := make([]bpf.Instruction, len(raw))
	for i, r := range raw {
		insns[i] = bpf.RawInstruction{Op: r.Code, Jt: r.Jt, Jf: r.Jf, K: r.K}.Disassemble()
	}
	vm, err := bpf.NewVM(insns)
	if err != nil {
		t.Fatal(err)
	}
	msg := func(cookie uint64) []byte {
		b := make([]byte, unix.NLMSG_HDRLEN+listenDiagMsgLen)
		binary.NativeEndian.PutUint64(b[tcpEndsCookieAt:], cookie)
		return b
	}
	for _, c := range []struct {
		cookie uint64
		pass   bool
	}{
		{named[0], true},
		{named[1], true},
		{0x0000000100000003, false}, // its low half differs
		{0x0000000200000002, false}, // its high half
		{0, false},
	} {
		n, err := vm.Run(msg(c.cookie))
		if err != nil {
			t.Fatal(err)
		}
		if (n != 0) != c.pass {
			t.Fatalf("cookie %#x: the filter kept %d bytes, want it passed %v", c.cookie, n, c.pass)
		}
	}
}

// A TCP connection that closes is read after it has gone from the socket
// table: its end says what it carried up to its close, by the segments that
// carried data (here one each way), which its handshake and FINs do not move,
// so an idle connection that closes ends as it was. Another socket's end is
// not kept once the watch names its sockets, whether a filter on the socket
// drops it or (more sockets named than a filter holds) the watch does, and an
// end is answered once.
func TestATCPConnectionsEndSaysWhatItCarried(t *testing.T) {
	t.Run("filtered", func(t *testing.T) { endsSayWhatTheyCarried(t, 0) })
	t.Run("past the filter's room", func(t *testing.T) { endsSayWhatTheyCarried(t, tcpEndsMaxFilter) })
}

// endsSayWhatTheyCarried is TestATCPConnectionsEndSaysWhatItCarried with pad
// more (unused) cookies named.
func endsSayWhatTheyCarried(t *testing.T, pad int) {
	w, err := WatchTCPEnds()
	if err != nil {
		t.Fatalf("watch the TCP sockets' ends: %v", err)
	}
	defer w.Close()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	dial := func() (net.Conn, net.Conn, uint64) {
		t.Helper()
		c, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		s, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		rc, err := c.(*net.TCPConn).SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var cookie uint64
		var cerr error
		_ = rc.Control(func(fd uintptr) { cookie, cerr = unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_COOKIE) })
		if cerr != nil {
			t.Fatal(cerr)
		}
		return c, s, cookie
	}
	busy, busyEnd, busyCookie := dial()
	idle, idleEnd, idleCookie := dial()
	other, otherEnd, otherCookie := dial()
	named := []uint64{busyCookie, idleCookie}
	for i := 0; i < pad; i++ {
		named = append(named, 1<<62+uint64(i))
	}
	w.Only(named)
	live, err := TCPSockets(1 << TCPEstablished)
	if err != nil {
		t.Fatal(err)
	}
	before := map[uint64]TCPSocket{}
	for _, s := range live {
		before[s.Cookie] = s
	}
	for _, ck := range []uint64{busyCookie, idleCookie} {
		if s, ok := before[ck]; !ok || !s.DataKnown {
			t.Fatalf("the connection %d is not in the socket table with its counts: %+v", ck, live)
		}
	}

	if _, err := io.WriteString(busy, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(busyEnd, make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(busyEnd, "1 row"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(busy, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	for _, x := range []net.Conn{busyEnd, busy, idleEnd, idle, other, otherEnd} {
		_ = x.Close()
	}
	got := w.Ended([]uint64{busyCookie, idleCookie}, 5*time.Second)
	if e, ok := got[busyCookie]; !ok || !e.DataKnown || e.DataSegs != before[busyCookie].DataSegs+2 {
		t.Fatalf("the busy connection's end %+v (%v), want %d segments with data: the 2 it carried since", e, ok, before[busyCookie].DataSegs+2)
	}
	if e, ok := got[idleCookie]; !ok || !e.DataKnown || e.DataSegs != before[idleCookie].DataSegs {
		t.Fatalf("the idle connection's end %+v (%v), want %d segments with data: none since", e, ok, before[idleCookie].DataSegs)
	}
	if got := w.Ended([]uint64{otherCookie}, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("an end of a socket the watch does not name was kept: %+v", got)
	}
	if again := w.Ended([]uint64{busyCookie}, 10*time.Millisecond); len(again) != 0 {
		t.Fatalf("an end answered twice: %+v", again)
	}
}
