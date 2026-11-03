package proxy

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// The bytes on the application's connections that are already the proxy's
// (pairbytes_linux.go has the wait). A test's end waits for every connection
// the application had opened to reach the proxy (WaitProxySettled's first two
// phases), but a call written to a connection opened long before (a pooled,
// keep-alive one) opens no connection: its bytes can still be on their way
// (a loaded host, a held packet) when the end reads, and the call then lands
// in the next test, or after the last one, unseen. So the end also waits, on
// each live connection the kernel paired with one of the application's, until
// the proxy's side of it has received every byte the application had written
// to it.

// pairedConn is a connection of the marked listener that the kernel paired
// with one of the application's, while the proxy holds it.
type pairedConn struct {
	appSeq uint64
	// app is the application's end of the connection (its remote end, as
	// the proxy sees it), proxy the proxy's own.
	app, proxy netip.AddrPort
	conn       *net.TCPConn
	// done is closed once the proxy is done with the connection, its socket
	// closed: from then on its side receives nothing more.
	done chan struct{}
	// lost: bytes the application wrote to it were charged as never
	// reaching the proxy, once.
	lost atomic.Bool
	// atClose is the proxy's side as the proxy closed it (closing), read
	// from its socket while it still could be: after the close nothing
	// reads it (a reset socket is unhashed, sock_diag no longer finds it).
	atClose atomic.Pointer[proxySide]
	// wroteAtClose is what the application had written to the connection
	// as the proxy began closing it, read from its socket before atClose
	// (pairClosing): bytes past it it wrote after keploy's close, which are
	// no capture loss. nil when it could not be read.
	wroteAtClose atomic.Pointer[appWrote]
	// noSession: the proxy serves it with no session (serveWithoutSession):
	// it closes it unanswered, or passes it through, and records and mocks
	// nothing on it by design, so nothing the application wrote to it is a
	// capture loss, and no bytes wait holds it.
	noSession atomic.Bool
	// appDev is the index of the device the application's socket is bound
	// to (0 for none), as the first lookup of the connection to find it
	// read it (lookupApp): nil before one has.
	appDev atomic.Pointer[uint32]
	// proxyDev is the index of the device the proxy's socket is bound to (0
	// for none), read as the proxy began closing it (noteWroteAtClose), for
	// a wait's later reads of its orphan (orphanTookFIN): after the close,
	// nothing reads it from the socket. A close whose side had taken the
	// application's FIN or reset leaves it 0: no wait reads that orphan.
	proxyDev atomic.Uint32
	// cuts are the ends cut short on the connection, oldest first: each a
	// test whose bytes wait ended before the proxy's side had received what
	// the application had written when the wait began (cutShort). Bytes up
	// to its want, past the one before it, are that test's: their loss is
	// its own, never the test open when the loss is counted (lostBy).
	cutMu sync.Mutex
	cuts  []cutEnd
}

// cutEnd is a test's end cut short on a connection: what the application had
// written to it when the end's wait began (want), and the test.
type cutEnd struct {
	want uint64
	test string
}

// cutShort marks pc with test's end, which waited for want and was cut short
// first. An end that names no test marks nothing.
func (pc *pairedConn) cutShort(test string, want uint64) {
	if test == "" {
		return
	}
	pc.cutMu.Lock()
	defer pc.cutMu.Unlock()
	pc.cuts = append(pc.cuts, cutEnd{want: want, test: test})
}

// lostBy is who owns the bytes low..top (counted from 1, as the application
// wrote them to pc) that were lost: each test whose end was cut short with
// some of them written before its wait and after the wait of the cut end
// before it (owners), and the test open as they are counted (open) when
// some were written after the last such wait, or when no end was cut short.
// A wait's want only grows (the application's count of what it wrote does),
// so the cuts split the bytes in order; an end whose want is no more than
// the one before it wrote nothing more, and owns none.
func (pc *pairedConn) lostBy(low, top uint64) (owners []string, open bool) {
	low = max(low, 1)
	top = max(top, low)
	pc.cutMu.Lock()
	defer pc.cutMu.Unlock()
	var prev uint64
	for _, c := range pc.cuts {
		if c.want > prev && low <= c.want && top > prev {
			owners = append(owners, c.test)
		}
		prev = max(prev, c.want)
	}
	return owners, top > prev
}

// appWrote is what the application had written to a connection, in bytes and
// a FIN (noteWroteAtClose), and whether that count surely holds its FIN (fin):
// a plaintext close does not wait for the FIN alone (finAloneOnItsWay), and
// one byte short of a count that holds the FIN is that FIN. false says only
// that the count may not hold it.
type appWrote struct {
	want uint64
	fin  bool
}

// proxySide is what the wait reads of the proxy's side of a connection.
type proxySide struct {
	received uint64 // tcpi_bytes_received
	state    uint8  // tcpi_state
	// err is why the socket could not be read.
	err error
}

// holds says whether conn is a paired connection the proxy holds.
func (s *pairedConns) holds(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.byConn[conn]
	return ok
}

// closing notes, once the proxy begins closing conn, what its side had
// received: the wait's answer for it from then on. It returns conn's paired
// connection (nil for one it does not keep, or whose close was noted) and how
// many bytes its socket holds unread, read after its side: the close tosses
// them with a reset (tcp_close), bytes the application wrote that its side
// counts received and the proxy never read. first, when set, runs before its
// side is read.
func (s *pairedConns) closing(conn net.Conn, first func(*pairedConn)) (*pairedConn, int, error) {
	s.mu.Lock()
	pc := s.byConn[conn]
	s.mu.Unlock()
	if pc == nil || pc.atClose.Load() != nil {
		return nil, 0, nil
	}
	if first != nil {
		first(pc)
	}
	ps := readProxySide(pc.conn)
	pc.atClose.Store(&ps)
	unread, err := readUnread(pc.conn)
	return pc, unread, err
}

// pairedConns are the paired connections the proxy holds, under the proven
// switch, from their accept until the proxy is done with them.
type pairedConns struct {
	mu     sync.Mutex
	byConn map[net.Conn]*pairedConn
	// err is why no wait on them can be answered: the listener closed, or
	// this kernel's sock_diag cannot see the application's sockets.
	err error
	// changed is closed, and replaced, at every removal or failure.
	changed chan struct{}
	// visible: an application socket has been seen from the proxy's
	// network namespace (proveAppVisible, pairTarget): one that is not is
	// gone, not hidden.
	visible atomic.Bool
	// progress counts what the bytes waits saw move: a waited-for
	// connection that received more, whose orphan changed state, or that
	// was resolved (PairedBytesProgress).
	progress atomic.Uint64
	// clk is the runnable clock a bytes wait measures its wait for an
	// orphan on (orphanLooks); nil is the process's. A test sets it.
	clk stall.Clock
	// owned are the losses counted on a connection that a test whose end
	// was cut short owned (pairLost), under mu, until the agent takes them
	// (TakeOwnedLosses).
	owned []agent.OwnedLoss
}

// maxOwned bounds the owned losses the proxy keeps untaken.
const maxOwned = agent.MaxOwnedLosses

// own notes a loss of the bytes test wrote to the connection from app, which
// its end could not wait for.
func (s *pairedConns) own(test string, app netip.AddrPort) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bounded while no agent takes them (a run that never reads them):
	// past maxOwned, one entry of no test's, the run's, counts the rest.
	if n := len(s.owned); n >= maxOwned {
		if s.owned[n-1].Test != "" {
			s.owned = append(s.owned, agent.OwnedLoss{Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, What: agent.OwnedLossesPastTheBound}})
		}
		s.owned[len(s.owned)-1].Loss.Count++
		return
	}
	s.owned = append(s.owned, agent.OwnedLoss{Test: test, Loss: models.CaptureLoss{
		Kind:  models.CaptureLossAppBytesLost,
		Count: 1,
		What:  "from " + app.String() + ", written " + agent.WrittenIn(test) + ", where keploy's wait for them was cut short",
	}})
}

// TakeOwnedLosses returns, once, the losses counted on a connection that a
// test whose end was cut short owned (agent.OwnedLossTaker): bytes it
// wrote before that end that were lost later. None is in CaptureLosses'
// counts, so none is charged to the test open when it was counted.
func (p *Proxy) TakeOwnedLosses() []agent.OwnedLoss {
	p.paired.mu.Lock()
	defer p.paired.mu.Unlock()
	out := p.paired.owned
	p.paired.owned = nil
	return out
}

// PairedBytesProgress counts the bytes waits' progress, for the agent's
// watchdog (agent.PairedBytesWaiter): a waited-for connection that received
// more, whose orphan changed state, or that was resolved. It only ever grows.
func (p *Proxy) PairedBytesProgress() uint64 { return p.paired.progress.Load() }

// add keeps an accepted connection the kernel paired with the application's
// connection number appSeq.
func (s *pairedConns) add(conn *net.TCPConn, appSeq uint64) {
	app, ok1 := addrPortOf(conn.RemoteAddr())
	proxy, ok2 := addrPortOf(conn.LocalAddr())
	if !ok1 || !ok2 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byConn == nil {
		s.byConn = map[net.Conn]*pairedConn{}
	}
	s.byConn[conn] = &pairedConn{appSeq: appSeq, app: app, proxy: proxy, conn: conn, done: make(chan struct{})}
}

// remove forgets conn once the proxy is done with it, its socket closed. A
// connection it does not keep is ignored.
func (s *pairedConns) remove(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pc, ok := s.byConn[conn]
	if !ok {
		return
	}
	delete(s.byConn, conn)
	close(pc.done)
	s.wakeLocked()
}

// fail makes every wait, now and from then on, end with err. The first call
// wins.
func (s *pairedConns) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
	s.wakeLocked()
}

func (s *pairedConns) wakeLocked() {
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}

// upTo is every connection held whose application connection is numbered
// 1..n, but for one the proxy serves with no session (noSession), which no
// wait holds, or, when there is one, why they cannot be waited on: a wait that
// holds none has nothing to tell (a kernel whose sock_diag gives no byte
// counts fails only the waits that would read them).
func (s *pairedConns) upTo(n uint64) ([]*pairedConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*pairedConn
	for _, pc := range s.byConn {
		if pc.appSeq <= n && !pc.noSession.Load() {
			out = append(out, pc)
		}
	}
	if len(out) > 0 && s.err != nil {
		return nil, s.err
	}
	return out, nil
}

// servedWithoutSession notes that the proxy serves conn with no session
// (pairedConn.noSession), and wakes the waits: one that took conn before the
// mark is released by it (pairReached), with no read or close of conn's to
// wake it when the proxy passes conn through. A connection it does not keep is
// ignored.
func (s *pairedConns) servedWithoutSession(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pc := s.byConn[conn]; pc != nil {
		pc.noSession.Store(true)
		s.wakeLocked()
	}
}

// changes is closed at the next removal, failure or connection served with no
// session: take it before looking.
func (s *pairedConns) changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.changed
}

func (s *pairedConns) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func addrPortOf(a net.Addr) (netip.AddrPort, bool) {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := ta.AddrPort()
	return ap, ap.IsValid()
}
