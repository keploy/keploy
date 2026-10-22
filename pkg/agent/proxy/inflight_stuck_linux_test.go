//go:build linux

package proxy

import (
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent"
)

// readOnce has the connection's handler read what the application sent
// first, so it is past its start, as a handler relaying the connection is.
func readOnce(t *testing.T, app *net.TCPConn, reader net.Conn) {
	t.Helper()
	if _, err := app.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
}

// awaitAppEnd waits, on the proxy's socket's own events, for the application's
// reset (POLLERR) or FIN (POLLRDHUP) to have reached it.
func awaitAppEnd(t *testing.T, c *net.TCPConn) {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var perr error
	if err := rc.Control(func(fd uintptr) {
		for {
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLRDHUP}}
			n, err := unix.Poll(fds, -1)
			if err == unix.EINTR {
				continue
			}
			if err != nil || n > 0 {
				perr = err
				return
			}
		}
	}); err != nil || perr != nil {
		t.Fatalf("poll: %v %v", err, perr)
	}
}

// reset is the application aborting its connection: SO_LINGER 0, then
// close, which sends its reset.
func reset(t *testing.T, app, proxySide *net.TCPConn) {
	t.Helper()
	_ = app.SetLinger(0)
	_ = app.Close()
	awaitAppEnd(t, proxySide)
	if s := readProxySide(proxySide).state; s != tcpClose {
		t.Fatalf("the proxy's socket is in state %d after the reset, want CLOSE", s)
	}
}

func lookOnce(t *testing.T, p *Proxy, want string) agent.StuckConn {
	t.Helper()
	stuck := p.InFlightStuck()
	if len(stuck) != 1 || !strings.Contains(stuck[0].String(), want) || !strings.Contains(stuck[0].String(), "has not read") {
		t.Fatalf("InFlightStuck = %q; want it stuck, %s", stuck, want)
	}
	if !stuck[0].Holds() {
		t.Fatal("its mark does not hold as it is made")
	}
	return stuck[0]
}

func notStuck(t *testing.T, p *Proxy, why string) {
	t.Helper()
	if stuck := p.InFlightStuck(); len(stuck) != 0 {
		t.Fatalf("InFlightStuck = %q %s; want it waited on", stuck, why)
	}
	if pending, _ := p.inflight.State(false); pending != 1 {
		t.Fatalf("a record's pending %d %s, want 1", pending, why)
	}
}

// A record's wait for the calls in flight waits while a connection holds bytes
// the application sent that its handler has not taken. One whose handler does
// not read them (held in its write to a destination that never reads), and
// whose application's end is gone, raises no event and holds them for good: the
// record's test end waited until go test's own timeout killed the test binary
// (an upload the application aborted, behind a dependency that stopped
// reading). InFlightStuck tells it apart: its handler has read from it before
// and is not reading, and its application reset it, closed it, or its socket is
// gone. It is marked once, and no record's wait waits on it while the mark
// holds; a read that takes a byte of it ends the mark. One whose application
// may still send, whose handler is reading, or whose handler never read (it is
// still to start relaying it) is not stuck.
func TestARecordsWaitTellsBytesNoReaderWillTake(t *testing.T) {
	t.Run("the application reset it", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		readOnce(t, app, reader)
		queue(t, app)
		reset(t, app, proxySide)
		if pending, _ := p.inflight.State(false); pending != 1 {
			t.Fatalf("pending %d before it is looked at, want 1: its bytes are untaken", pending)
		}
		mark := lookOnce(t, p, "it was reset")
		if pending, _ := p.inflight.State(false); pending != 0 {
			t.Fatalf("a record's wait still waits on it (pending %d)", pending)
		}
		if got := p.inflight.PendingProgress(false); len(got) != 0 {
			t.Fatalf("the watchdog still weighs it: %v", got)
		}
		if stuck := p.InFlightStuck(); len(stuck) != 0 {
			t.Fatalf("said again: %q; want once", stuck)
		}
		// A replay's wait is its own: the mark is no record's there.
		if pending, _ := p.inflight.State(true); pending != 1 {
			t.Fatalf("a replay's pending %d, want 1", pending)
		}
		// Its handler takes a byte after all (the socket still gives what
		// came before the reset): the mark ends, and it is work again,
		// until a look finds it stuck once more.
		if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		if mark.Holds() {
			t.Fatal("its mark still holds after a read took a byte of it")
		}
		if pending, _ := p.inflight.State(false); pending != 1 {
			t.Fatalf("a record's pending %d after the read, want 1", pending)
		}
		again := lookOnce(t, p, "it was reset")
		if mark.Holds() || !again.Holds() {
			t.Fatalf("the old mark holds %v, the new %v; want only the new", mark.Holds(), again.Holds())
		}
	})
	t.Run("the application closed it", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		readOnce(t, app, reader)
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		_ = app.CloseWrite()
		awaitAppEnd(t, proxySide)
		lookOnce(t, p, "it closed")
	})
	t.Run("the application's socket is gone", func(t *testing.T) {
		p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		readOnce(t, app, reader)
		queue(t, app)
		// Its end as sock_diag would no longer find it, the application's
		// sockets seen from here: a reset the proxy's socket refused, or a
		// socket its kernel gave up.
		p.paired.mu.Lock()
		for _, pc := range p.paired.byConn {
			pc.app = netip.MustParseAddrPort("127.0.0.1:1")
		}
		p.paired.mu.Unlock()
		p.paired.visible.Store(true)
		lookOnce(t, p, "its socket is gone")
	})
	t.Run("the application may still send", func(t *testing.T) {
		p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		readOnce(t, app, reader)
		queue(t, app)
		notStuck(t, p, "with its socket not seen")
		// sock_diag finds its socket where it finds the application's.
		p.paired.visible.Store(true)
		notStuck(t, p, "with its socket found")
		// Nor where the application's sockets cannot be seen: gone from
		// here is no sign.
		p.paired.mu.Lock()
		for _, pc := range p.paired.byConn {
			pc.app = netip.MustParseAddrPort("127.0.0.1:1")
		}
		p.paired.mu.Unlock()
		p.paired.visible.Store(false)
		notStuck(t, p, "with no application socket ever seen")
	})
	t.Run("its handler is reading", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		readOnce(t, app, reader)
		queue(t, app)
		reset(t, app, proxySide)
		tc := reader.(*trackedConn)
		tc.count(&tc.reading, 1)
		defer tc.count(&tc.reading, -1)
		notStuck(t, p, "with its handler reading")
	})
	t.Run("its handler never read it", func(t *testing.T) {
		p, app, proxySide, _ := heldPair(t, "tcp4", "127.0.0.1:0")
		queue(t, app)
		reset(t, app, proxySide)
		notStuck(t, p, "with its handler still to start")
	})
	t.Run("two at once", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		readOnce(t, app, reader)
		queue(t, app)
		reset(t, app, proxySide)
		app2, proxySide2, reader2 := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
		readOnce(t, app2, reader2)
		queue(t, app2)
		reset(t, app2, proxySide2)
		stuck := p.InFlightStuck()
		if len(stuck) != 2 || stuck[0].String() > stuck[1].String() {
			t.Fatalf("InFlightStuck = %q; want both, in order", stuck)
		}
	})
	t.Run("its handler is done with it", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		readOnce(t, app, reader)
		queue(t, app)
		reset(t, app, proxySide)
		p.inflight.forget(reader.(*trackedConn))
		if stuck := p.InFlightStuck(); len(stuck) != 0 {
			t.Fatalf("InFlightStuck = %q for a connection its handler is done with", stuck)
		}
	})
	t.Run("beside one that holds nothing", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		readOnce(t, app, reader)
		queue(t, app)
		reset(t, app, proxySide)
		// Its handler took all its application sent before the reset.
		idleApp, idleSide, idleReader := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
		readOnce(t, idleApp, idleReader)
		reset(t, idleApp, idleSide)
		lookOnce(t, p, "it was reset")
		if pending, _ := p.inflight.State(false); pending != 0 {
			t.Fatalf("pending %d, want 0", pending)
		}
	})
}
