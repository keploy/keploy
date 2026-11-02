//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// countDumps counts the dumps lookups make (lookupDump) until the test ends.
func countDumps(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	was := lookupDump
	lookupDump = func(d *diagSock, local, remote netip.AddrPort) (diagTCP, error) {
		n.Add(1)
		return was(d, local, remote)
	}
	t.Cleanup(func() { lookupDump = was })
	return &n
}

// written10 has the application write 10 bytes that the proxy reads.
func written10(t *testing.T, app *net.TCPConn, reader net.Conn) {
	t.Helper()
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
}

// A close's read of the application's socket dumps (a walk of every bucket of
// the established hash, tens of ms on a large one) only where a dump can give
// its answer, and every close still records the 10 bytes written:
//   - a socket bound to no device is found by the exact lookup;
//   - one bound to the device the proxy's socket is bound to, as in a VRF, by
//     the exact lookup asked with that device;
//   - one bound to another device, at the connection's first lookup, by the
//     dump, which a later lookup of the connection makes no more: it asks for
//     the socket the first found.
func TestACloseDumpsOnlyWhereADumpGivesItsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name                string
		listenDev, appDev   string
		waitFirst           bool
		waitDumps, endDumps int64
	}{
		{"bound to no device", "", "", false, 0, 0},
		{"bound to the proxy's socket's device", "lo", "lo", false, 0, 0},
		{"bound to another device, first looked up as the proxy closes", "", "lo", false, 0, 1},
		{"bound to another device, looked up by a wait before the close", "", "lo", true, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emptyDiagFree(t)
			p := &Proxy{logger: zap.NewNop()}
			app, proxySide, reader := heldPairBound(t, p, 1, "tcp4", "127.0.0.1:0", tc.listenDev, tc.appDev)
			if want := tc.listenDev != ""; (boundDevice(proxySide) != 0) != want {
				t.Fatalf("the proxy's socket bound to device %d; want it bound: %v (this case's premise)", boundDevice(proxySide), want)
			}
			written10(t, app, reader)
			pc := pairedConnOf(t, p, 1)
			dumps := countDumps(t)
			if tc.waitFirst {
				if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
					t.Fatalf("WaitPairedBytes = %v, want settled", err)
				}
			}
			if got := dumps.Swap(0); got != tc.waitDumps {
				t.Fatalf("the wait made %d dumps, want %d", got, tc.waitDumps)
			}
			_ = reader.Close()
			if w := pc.wroteAtClose.Load(); w == nil || w.want != 10 {
				t.Fatalf("the close's record: %+v, want the 10 bytes written", w)
			}
			if got := dumps.Load(); got != tc.endDumps {
				t.Fatalf("the close made %d dumps, want %d", got, tc.endDumps)
			}
		})
	}
}

// Once a lookup of the connection has found the application's socket, a miss
// of the socket it found:
//   - is, for the close's read, no count and no dump: a dump there finds its
//     timewait socket, one read in CLOSE, or none, and none gives a count;
//   - is, for a wait, followed by a dump, as lookup's miss is: the kernel's
//     lockless lookup can miss a socket as it becomes a timewait socket, which
//     the dump finds, and a timewait socket needs no waiting (pairTarget)
//     where one gone does.
func TestAMissOfALearnedSocketDumpsForAWaitOnly(t *testing.T) {
	t.Run("gone, at the close", func(t *testing.T) {
		p := &Proxy{logger: zap.NewNop()}
		app, _, reader := heldPairBound(t, p, 1, "tcp4", "127.0.0.1:0", "", "")
		written10(t, app, reader)
		pc := pairedConnOf(t, p, 1)
		ds, err := openDiag()
		if err != nil {
			t.Fatal(err)
		}
		defer ds.close()
		if _, err := ds.lookupApp(pc, false); err != nil || pc.appDev.Load() == nil {
			t.Fatalf("lookupApp = %v, learned %v; want the socket found and its device learned", err, pc.appDev.Load())
		}
		if err := app.SetLinger(0); err != nil {
			t.Fatal(err)
		}
		_ = app.Close() // reset: gone at once, no timewait socket
		if _, err := ds.exact(pc.app, pc.proxy); !errors.Is(err, errNoSocket) {
			t.Fatalf("the reset socket's exact lookup = %v, want it gone (this case's premise)", err)
		}
		dumps := countDumps(t)
		if w, ok, err := wroteNow(ds, pc); ok || err != nil {
			t.Fatalf("wroteNow of a learned socket gone = %+v, %v, %v; want no count", w, ok, err)
		}
		if got := dumps.Swap(0); got != 0 {
			t.Fatalf("the close's read made %d dumps, want none", got)
		}
		if _, err := ds.lookupApp(pc, false); !errors.Is(err, errNoSocket) {
			t.Fatalf("a wait's lookup of a learned socket gone = %v, want errNoSocket", err)
		}
		if got := dumps.Load(); got != 1 {
			t.Fatalf("a wait's lookup made %d dumps, want 1", got)
		}
	})
	t.Run("missed as a timewait socket, by a wait", func(t *testing.T) {
		p := &Proxy{logger: zap.NewNop()}
		app, proxySide, reader := heldPairBound(t, p, 1, "tcp4", "127.0.0.1:0", "", "")
		written10(t, app, reader)
		pc := pairedConnOf(t, p, 1)
		ds, err := openDiag()
		if err != nil {
			t.Fatal(err)
		}
		defer ds.close()
		if _, err := ds.lookupApp(pc, false); err != nil || pc.appDev.Load() == nil {
			t.Fatalf("lookupApp = %v, learned %v; want the socket found and its device learned", err, pc.appDev.Load())
		}
		_ = app.Close()
		sideIn(t, proxySide, tcpCloseWait)
		_ = reader.Close()
		deadline := time.Now().Add(10 * time.Second)
		for {
			s, err := ds.exact(pc.app, pc.proxy)
			if err == nil && s.state == tcpTimeWait {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the application's socket = state %d, %v; want it in TIME_WAIT (this case's premise)", s.state, err)
			}
			time.Sleep(time.Millisecond)
		}
		was := lookupExactOn
		lookupExactOn = func(*diagSock, uint32, netip.AddrPort, netip.AddrPort) (diagTCP, error) {
			return diagTCP{}, errNoSocket
		}
		t.Cleanup(func() { lookupExactOn = was })
		if tg, err := p.pairTarget(ds, pc); err != nil || tg != nil {
			t.Fatalf("pairTarget of a timewait socket the exact lookup missed = %+v, %v; want nothing to wait for", tg, err)
		}
	})
}

// A lookup asked with a device finds a socket bound to it or to none, never
// one bound to another, and the reply names the device a socket is bound to.
func TestALookupAskedWithADeviceFindsItsSockets(t *testing.T) {
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		t.Skipf("no lo here: %v", err)
	}
	none := uint32(lo.Index) + 1000 // no device has it
	p := &Proxy{logger: zap.NewNop()}
	_, _, _ = heldPairBound(t, p, 1, "tcp4", "127.0.0.1:0", "", "")
	_, _, _ = heldPairBound(t, p, 2, "tcp4", "127.0.0.1:0", "", "lo")
	free, bound := pairedConnOf(t, p, 1), pairedConnOf(t, p, 2)
	ds, err := openDiag()
	if err != nil {
		t.Fatal(err)
	}
	defer ds.close()
	for _, q := range []struct {
		name  string
		pc    *pairedConn
		dev   uint32
		found bool
	}{
		{"bound to none, asked with none", free, 0, true},
		{"bound to none, asked with lo", free, uint32(lo.Index), true},
		{"bound to none, asked with another device", free, none, true},
		{"bound to lo, asked with none", bound, 0, false},
		{"bound to lo, asked with lo", bound, uint32(lo.Index), true},
		{"bound to lo, asked with another device", bound, none, false},
	} {
		got, err := ds.exactOn(q.dev, q.pc.app, q.pc.proxy)
		if found := err == nil; found != q.found || err != nil && !errors.Is(err, errNoSocket) {
			t.Errorf("exactOn of a socket %s = %v; want found: %v", q.name, err, q.found)
		}
		if want := map[*pairedConn]uint32{free: 0, bound: uint32(lo.Index)}[q.pc]; err == nil && got.dev != want {
			t.Errorf("exactOn of a socket %s = dev %d, want %d", q.name, got.dev, want)
		}
	}
	if b, err := ds.lookup(bound.app, bound.proxy); err != nil || b.dev != uint32(lo.Index) {
		t.Fatalf("lookup's dump of a socket bound to lo = dev %d, %v; want dev %d", b.dev, err, lo.Index)
	}
}

// sockDiagFDs counts this process's descriptors that are NETLINK_SOCK_DIAG
// sockets (protocol 4 in /proc/net/netlink).
func sockDiagFDs(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/proc/net/netlink")
	if err != nil {
		t.Fatal(err)
	}
	diag := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) >= 10 && f[1] == "4" {
			diag[f[9]] = true
		}
	}
	n := 0
	for _, l := range fdLinks(t) {
		if strings.HasPrefix(l, "socket:[") && diag[strings.TrimSuffix(strings.TrimPrefix(l, "socket:["), "]")] {
			n++
		}
	}
	return n
}

// fdLinks is what each of this process's descriptors links to.
func fdLinks(t *testing.T) []string {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	var links []string
	for _, fd := range fds {
		if l, err := os.Readlink("/proc/self/fd/" + fd.Name()); err == nil {
			links = append(links, l)
		}
	}
	return links
}

// socketLink is what a descriptor of the socket fd links to.
func socketLink(t *testing.T, fd int) string {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	return "socket:[" + strconv.FormatUint(st.Ino, 10) + "]"
}

// Many closes at once, each holding its own sock_diag socket (a barrier in
// the lookup): none waits on the list, every record is right, the list ends
// full (diagFreeMax), and no other sock_diag descriptor is left open.
func TestConcurrentClosesNeitherWaitNorLeak(t *testing.T) {
	emptyDiagFree(t)
	const n = 24
	p := &Proxy{logger: zap.NewNop()}
	readers := make([]net.Conn, n)
	pcs := make([]*pairedConn, n)
	for i := range readers {
		app, _, reader := heldPairOn(t, p, uint64(i+1), "tcp4", "127.0.0.1:0")
		written10(t, app, reader)
		readers[i] = reader
		pcs[i] = pairedConnOf(t, p, uint64(i+1))
	}
	base := sockDiagFDs(t)
	var arrived atomic.Int32
	gate := make(chan struct{})
	was := lookupWritten
	lookupWritten = func(ds *diagSock, c *pairedConn, atClose bool) (diagTCP, error) {
		if arrived.Add(1) == n {
			close(gate)
		}
		<-gate
		return was(ds, c, atClose)
	}
	t.Cleanup(func() { lookupWritten = was })
	done := make(chan struct{}, n)
	for _, r := range readers {
		go func(r net.Conn) { _ = r.Close(); done <- struct{}{} }(r)
	}
	timeout := time.After(60 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatalf("%d of %d concurrent closes ended in 60 s; the rest wait (%d reached the lookup)", i, n, arrived.Load())
		}
	}
	for i, pc := range pcs {
		if w := pc.wroteAtClose.Load(); w == nil || w.want != 10 {
			t.Fatalf("close %d's record: %+v, want the 10 bytes written", i, w)
		}
	}
	if l := len(diagFree); l != diagFreeMax {
		t.Fatalf("the free list holds %d after %d concurrent closes, want %d", l, n, diagFreeMax)
	}
	if got := sockDiagFDs(t) - base; got != len(diagFree) {
		t.Fatalf("%d sock_diag descriptors open beyond the %d on the free list", got-len(diagFree), len(diagFree))
	}
}

// A listed socket holding an unread reply to an earlier request, made before
// the application wrote, changes no answer: the close's lookup skips it by its
// sequence number. A reply it took would count fewer bytes than were written.
func TestAStaleReplyOnAListedSocketChangesNoAnswer(t *testing.T) {
	emptyDiagFree(t)
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	pc := pairedConnOf(t, p, 1)
	ds, err := openDiag()
	if err != nil {
		t.Fatal(err)
	}
	ds.seq++
	req := diagRequest(ds.seq, unix.NLM_F_REQUEST, unix.AF_INET, ^uint32(0), 0, pc.app, pc.proxy)
	if err := unix.Sendto(ds.fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		t.Fatal(err)
	}
	written10(t, app, reader)
	diagFree <- ds
	_ = reader.Close()
	if w := pc.wroteAtClose.Load(); w == nil || w.want != 10 {
		t.Fatalf("the close's record with a stale reply queued on the listed socket: %+v, want the 10 bytes written", w)
	}
}

// A lookup that fails learns no device: the connection's next lookup finds
// the socket as a first one does, so a close after it still counts what the
// application had written. A device learned from a failed lookup (none)
// would make the close's read of a socket bound to a device miss it, and the
// bytes the application writes after keploy's close would then count as a
// loss.
func TestAFailedLookupLearnsNoDevice(t *testing.T) {
	emptyDiagFree(t)
	p := &Proxy{logger: zap.NewNop()}
	app, _, reader := heldPairBound(t, p, 1, "tcp4", "127.0.0.1:0", "", "lo")
	pc := pairedConnOf(t, p, 1)
	bad := &diagSock{fd: -1, buf: make([]byte, diagRecvBufLen)}
	if _, err := bad.lookupApp(pc, false); err == nil || errors.Is(err, errNoSocket) {
		t.Fatalf("lookupApp on a closed sock_diag descriptor = %v, want it to fail (this row's premise)", err)
	}
	if dev := pc.appDev.Load(); dev != nil {
		t.Fatalf("a failed lookup learned device %d, want none", *dev)
	}
	written10(t, app, reader)
	_ = reader.Close()
	if w := pc.wroteAtClose.Load(); w == nil || w.want != 10 {
		t.Fatalf("the close's record after a failed lookup: %+v, want the 10 bytes written", w)
	}
}

// foundOn waits until the socket whose ends are local and remote, found by an
// exact lookup asked with the device dev, reads as is says, and returns it.
func foundOn(t *testing.T, dev uint32, local, remote netip.AddrPort, what string, is func(diagTCP) bool) diagTCP {
	t.Helper()
	ds, err := openDiag()
	if err != nil {
		t.Fatal(err)
	}
	defer ds.close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := ds.exactOn(dev, local, remote)
		if err == nil && is(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("the socket %s -> %s asked with device %d = state %d, %v; want it %s (this case's premise)", local, remote, dev, s.state, err, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// missedAskedWithNone fails the row unless an exact lookup asked with no
// device misses the socket whose ends are local and remote: the row's
// premise, that a read asked with none would dump to find it.
func missedAskedWithNone(t *testing.T, local, remote netip.AddrPort) {
	t.Helper()
	ds, err := openDiag()
	if err != nil {
		t.Fatal(err)
	}
	defer ds.close()
	if s, err := ds.exact(local, remote); !errors.Is(err, errNoSocket) {
		t.Fatalf("the exact lookup asked with no device of %s -> %s = state %d, %v; want a miss (this case's premise)", local, remote, s.state, err)
	}
}

// A wait's reads of a closed connection's sockets ask with their devices, so
// a socket bound to one, as in a VRF, is found by the exact lookup, with no
// dump (a walk of every bucket of the established hash, tens of ms on a large
// one, on every look of a wait):
//   - the proxy's orphan (orphanTookFIN), with the device its socket was
//     bound to as its close began, waiting for the application's FIN and
//     then having taken it;
//   - the application's socket (appFINAcknowledged), its FIN acknowledged,
//     with the device the wait's own lookup of it learned.
func TestAClosedConnectionsReadsAskWithTheirDevices(t *testing.T) {
	t.Run("the proxy's orphan", func(t *testing.T) {
		emptyDiagFree(t)
		p := &Proxy{logger: zap.NewNop()}
		app, proxySide, reader := heldPairBound(t, p, 1, "tcp4", "127.0.0.1:0", "lo", "lo")
		dev := boundDevice(proxySide)
		if dev == 0 {
			t.Fatal("the proxy's socket is bound to no device (this case's premise)")
		}
		written10(t, app, reader)
		pc := pairedConnOf(t, p, 1)
		_ = reader.Close()
		foundOn(t, dev, pc.proxy, pc.app, "in FIN_WAIT2", func(s diagTCP) bool { return s.state == tcpFinWait2 })
		missedAskedWithNone(t, pc.proxy, pc.app)
		dumps := countDumps(t)
		if d, took, err := orphanTookFIN(pc); err != nil || took || d.state != tcpFinWait2 {
			t.Fatalf("orphanTookFIN of an orphan waiting for the FIN = state %d, took %v, %v; want FIN_WAIT2, not taken", d.state, took, err)
		}
		if got := dumps.Swap(0); got != 0 {
			t.Fatalf("the read of the orphan waiting for the FIN made %d dumps, want none", got)
		}
		_ = app.Close()
		foundOn(t, dev, pc.proxy, pc.app, "in TIME_WAIT", func(s diagTCP) bool { return s.state == tcpTimeWait })
		if d, took, err := orphanTookFIN(pc); err != nil || !took {
			t.Fatalf("orphanTookFIN of an orphan that took the FIN = state %d, took %v, %v; want it taken", d.state, took, err)
		}
		if got := dumps.Load(); got != 0 {
			t.Fatalf("the read of the orphan that took the FIN made %d dumps, want none", got)
		}
	})
	t.Run("the application's socket", func(t *testing.T) {
		emptyDiagFree(t)
		p := &Proxy{logger: zap.NewNop()}
		app, proxySide, reader := heldPairBound(t, p, 1, "tcp4", "127.0.0.1:0", "lo", "lo")
		written10(t, app, reader)
		pc := pairedConnOf(t, p, 1)
		if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
			t.Fatalf("WaitPairedBytes = %v, want settled", err)
		}
		dev := pc.appDev.Load()
		if dev == nil || *dev == 0 {
			t.Fatalf("the wait learned the application's socket's device %v; want lo's (this case's premise)", dev)
		}
		_ = app.Close()
		sideIn(t, proxySide, tcpCloseWait)
		_ = reader.Close()
		foundOn(t, *dev, pc.app, pc.proxy, "in TIME_WAIT", func(s diagTCP) bool { return s.state == tcpTimeWait })
		missedAskedWithNone(t, pc.app, pc.proxy)
		dumps := countDumps(t)
		if !appFINAcknowledged(pc) {
			t.Fatal("appFINAcknowledged of a socket in TIME_WAIT = false, want true")
		}
		if got := dumps.Load(); got != 0 {
			t.Fatalf("the read of the application's socket made %d dumps, want none", got)
		}
	})
}
