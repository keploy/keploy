//go:build darwin

package proxy

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"runtime"
	"slices"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// darwinPair is a connection as the proxy holds a paired one under the proven
// switch, on macOS: the application's end (app) and the proxy's (proxySide),
// paired as application connection 1, its reads seen through reader
// (inflight.go), its close noted (pairClosing) as the accept loop notes it.
func darwinPair(t *testing.T) (p *Proxy, app, proxySide *net.TCPConn, reader *trackedConn) {
	t.Helper()
	p = &Proxy{logger: zap.NewNop()}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	app = c.(*net.TCPConn)
	t.Cleanup(func() { _ = app.Close() })
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	proxySide = s.(*net.TCPConn)
	t.Cleanup(func() { _ = proxySide.Close() })
	p.paired.add(proxySide, 1)
	reader = p.inflight.track(proxySide)
	reader.beforeClose = func() { p.pairClosing(context.Background(), proxySide, reader) }
	t.Cleanup(func() { p.inflight.forget(reader) })
	return p, app, proxySide, reader
}

// untilUnread returns once conn's socket holds n bytes unread: loopback hands
// them to the proxy's socket on its own input thread, not within the
// application's write.
func untilUnread(t *testing.T, conn *net.TCPConn, n int) {
	t.Helper()
	for {
		got, err := readUnread(conn)
		if err != nil {
			t.Fatalf("SO_NREAD: %v", err)
		}
		if got == n {
			return
		}
		runtime.Gosched()
	}
}

// untilStateLeaves returns the TCP state of conn's socket once it is none of
// from.
func untilStateLeaves(t *testing.T, conn *net.TCPConn, from ...uint8) uint8 {
	t.Helper()
	for {
		ps := readProxySide(conn)
		if ps.err != nil {
			t.Fatalf("tcp_info cannot be read: %v", ps.err)
		}
		state := ps.state
		left := true
		for _, f := range from {
			left = left && state != f
		}
		if left {
			return state
		}
		runtime.Gosched()
	}
}

// The proxy's handler closes a paired connection with bytes the application
// wrote that it never read: the close tosses them, so it charges the loss,
// once; a close with nothing unread charges none, and neither does one the
// proxy serves with no session.
func TestAProxysCloseWithBytesUnreadIsALossOnDarwin(t *testing.T) {
	t.Run("bytes unread", func(t *testing.T) {
		p, app, proxySide, reader := darwinPair(t)
		if _, err := app.Write([]byte("publish 1\npublish 2\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil { // the handler takes the first call only
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 10)
		_ = reader.Close()
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("%d app_bytes_lost charged at the close, want 1: 10 bytes the application wrote were never read, and the close tossed them", l)
		}
		_ = reader.Close()
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("%d app_bytes_lost charged, want 1: once per connection", l)
		}
	})
	t.Run("all read", func(t *testing.T) {
		p, app, _, reader := darwinPair(t)
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
		_ = reader.Close()
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want 0: the proxy read every byte", l)
		}
	})
	t.Run("no session", func(t *testing.T) {
		p, app, proxySide, reader := darwinPair(t)
		p.paired.servedWithoutSession(proxySide)
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 10)
		_ = reader.Close()
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want 0: a connection served with no session captures nothing by design", l)
		}
	})
}

// The count of what the proxy's side received (its sequence numbers) holds
// the application's FIN as one, as Linux's does: the application sends 100
// bytes and its FIN, the proxy's handler reads 99, and one byte, not two, is
// still untaken, so the connection still holds work; once it reads the
// 100th, none is. (Not taking the FIN off read two, then one.)
func TestAnUnreadByteBesideTheFINIsUntakenOnDarwin(t *testing.T) {
	_, app, proxySide, reader := darwinPair(t)
	if _, err := app.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 99)); err != nil {
		t.Fatal(err)
	}
	if s := untilStateLeaves(t, proxySide, xnuTCPEstablished); s != xnuTCPCloseWait {
		t.Fatalf("the proxy's side is in state %d after the application's FIN, want CLOSE_WAIT (%d)", s, xnuTCPCloseWait)
	}
	untilUnread(t, proxySide, 1)
	if n := reader.untaken(reader.view()); n != 1 {
		t.Fatalf("untaken = %d with 1 byte unread beside the FIN, want 1", n)
	}
	if !reader.busyAt(reader.view(), false) {
		t.Fatal("the connection holds no work with a byte the application sent unread")
	}
	if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if n := reader.untaken(reader.view()); n != 0 {
		t.Fatalf("untaken = %d with every byte read, want 0", n)
	}
	if _, err := reader.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read after the last byte: %v, want EOF", err)
	}
	if n := reader.untaken(reader.view()); n != 0 {
		t.Fatalf("untaken = %d after the EOF, want 0", n)
	}
}

// keploy half-closes a connection, then its client sends its FIN: the socket
// the handler still holds is in TIME_WAIT on macOS (Linux shows TCP_CLOSE),
// and the client has gone, so a hang-up held on it is seen without waiting
// for a read to return the stream's end.
func TestAClientsFINAfterKeploysHalfCloseIsAHangUpOnDarwin(t *testing.T) {
	_, app, proxySide, reader := darwinPair(t)
	release := reader.HoldHangUp()
	defer release()
	if reader.hungUp(reader.view()) {
		t.Fatal("hung up before anyone closed anything")
	}
	if err := reader.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the application's read after keploy's half-close: %v, want EOF", err)
	}
	if reader.hungUp(reader.view()) {
		t.Fatal("hung up on keploy's own half-close, the client not yet done")
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if s := untilStateLeaves(t, proxySide, xnuTCPFinWait1, xnuTCPFinWait2); s != xnuTCPTimeWait {
		t.Fatalf("the proxy's side is in state %d after the client's FIN, want TIME_WAIT (%d)", s, xnuTCPTimeWait)
	}
	if !reader.hungUp(reader.view()) {
		t.Fatal("the client's FIN after keploy's half-close (TIME_WAIT) is not seen as its hang-up")
	}
}

// XNU's struct tcp_info: where its received byte counts lie, 64-bit counts
// packed at 4 bytes after tcpi_snd_sbbytes (64), as measured on macOS 26
// arm64 (tcpi_rxbytes read as TCP_CONNECTION_INFO's tcpi_rxbytes does).
const (
	xnuTCPInfoOffRxbytes    = 108 // tcpi_rxbytes
	xnuTCPInfoOffRxdupbytes = 116 // tcpi_rxduplicatebytes
)

// onTCPInfo makes every read of a socket's tcp_info, until the row ends, go
// through spoil, which changes what macOS gave (n bytes of buf).
func onTCPInfo(t *testing.T, spoil func(t *testing.T, buf []byte, n uint32)) {
	t.Helper()
	// The counts' layout is proven first, from what macOS gives: a proof
	// that read the spoiled copies would leave the counts unknown for
	// another reason than the row's.
	if err := xnuTableReadable(); err != nil {
		t.Fatalf("the counts' proof: %v", err)
	}
	read := readTCPInfo
	readTCPInfo = func(fd int, buf []byte) (uint32, error) {
		n, err := read(fd, buf)
		if err == nil {
			spoil(t, buf, n)
		}
		return n, err
	}
	t.Cleanup(func() { readTCPInfo = read })
}

// A segment the proxy's side received twice (a spurious retransmit, its
// first copy still queued on a loaded host) is counted twice in XNU's byte
// count, tcpi_rxbytes, and in tcpi_rxduplicatebytes: bytes no read will
// ever return. What the settle counts as received is its sequence numbers,
// which such a segment does not move: once the handler has read every byte
// the application sent, nothing is untaken and the connection holds no
// work. (A count from tcpi_rxbytes would leave the 100 duplicate bytes
// untaken for good, and hold every settle that cuts the connection until
// its watchdog gave up.)
func TestASegmentReceivedTwiceHoldsNoSettleOnDarwin(t *testing.T) {
	_, app, _, reader := darwinPair(t)
	const dup = 100
	onTCPInfo(t, func(t *testing.T, buf []byte, n uint32) {
		if n < xnuTCPInfoOffRxdupbytes+8 {
			t.Errorf("tcp_info gave %d bytes, too few to hold tcpi_rxduplicatebytes", n)
			return
		}
		binary.LittleEndian.PutUint64(buf[xnuTCPInfoOffRxbytes:], binary.LittleEndian.Uint64(buf[xnuTCPInfoOffRxbytes:])+dup)
		binary.LittleEndian.PutUint64(buf[xnuTCPInfoOffRxdupbytes:], binary.LittleEndian.Uint64(buf[xnuTCPInfoOffRxdupbytes:])+dup)
	})
	if _, err := app.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if n := reader.untaken(reader.view()); n != 0 {
		t.Fatalf("untaken = %d with every byte read and %d received twice, want 0", n, dup)
	}
	if reader.busyAt(reader.view(), false) {
		t.Fatal("the connection holds work with every byte the application sent read")
	}
	if u := reader.t.Unknown(true); len(u) != 0 {
		t.Fatalf("the connection's requests are unknown (%v) with its counts read", u)
	}
}

// A count that reads fewer bytes received than the handler's reads were
// given cannot be right: the connection's requests are unknown, and it
// holds no settle (nothing untaken), never one held until its watchdog
// gives up.
func TestACountBehindTheReadsIsUnknownOnDarwin(t *testing.T) {
	_, app, proxySide, reader := darwinPair(t)
	if _, err := app.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 60)); err != nil {
		t.Fatal(err)
	}
	untilUnread(t, proxySide, 40)
	onTCPInfo(t, func(_ *testing.T, buf []byte, _ uint32) {
		binary.LittleEndian.PutUint32(buf[xnuTCPInfoOffRcvNxt:], binary.LittleEndian.Uint32(buf[xnuTCPInfoOffRcvNxt:])-70)
	})
	if n := reader.untaken(reader.view()); n != 0 {
		t.Fatalf("untaken = %d from a count behind the reads, want 0 (unknown)", n)
	}
	if u := reader.t.Unknown(true); !slices.Contains(u, xnuCountsUnknown) {
		t.Fatalf("the connection's requests are %v from a count behind the reads, want unknown (%q)", u, xnuCountsUnknown)
	}
}

// A connection whose first byte's number was not read as it was kept has no
// count of what it received: its requests are unknown, and its unread bytes
// hold no settle.
func TestAConnectionWithNoFirstNumberIsUnknownOnDarwin(t *testing.T) {
	_, app, proxySide, reader := darwinPair(t)
	if !reader.base.ok {
		t.Fatal("the first byte's number of a connection kept before its handler ran was not read")
	}
	reader.base.ok = false
	if _, err := app.Write(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	untilUnread(t, proxySide, 10)
	if n := reader.untaken(reader.view()); n != 0 {
		t.Fatalf("untaken = %d with no first number, want 0 (unknown)", n)
	}
	if u := reader.t.Unknown(true); !slices.Contains(u, xnuCountsUnknown) {
		t.Fatalf("the connection's requests are %v with no first number, want unknown (%q)", u, xnuCountsUnknown)
	}
}

// Bytes the application sent before the proxy kept the connection, and its
// FIN, are counted from the first byte: the proxy keeps it with 5 bytes and
// the FIN already in, and they are untaken until its handler reads them.
func TestBytesBeforeTheConnectionIsKeptAreCountedOnDarwin(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	app := c.(*net.TCPConn)
	t.Cleanup(func() { _ = app.Close() })
	if _, err := app.Write(make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	proxySide := s.(*net.TCPConn)
	t.Cleanup(func() { _ = proxySide.Close() })
	untilStateLeaves(t, proxySide, xnuTCPEstablished)
	untilUnread(t, proxySide, 5)
	reader := p.inflight.track(proxySide)
	t.Cleanup(func() { p.inflight.forget(reader) })
	if n := reader.untaken(reader.view()); n != 5 {
		t.Fatalf("untaken = %d with 5 bytes and the FIN in before the connection was kept, want 5", n)
	}
	if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	if n := reader.untaken(reader.view()); n != 0 {
		t.Fatalf("untaken = %d with every byte read, want 0", n)
	}
	if u := p.inflight.Unknown(true); len(u) != 0 {
		t.Fatalf("the connection's requests are unknown (%v) with its counts read", u)
	}
}
