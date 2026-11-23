//go:build darwin

package proxy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// The bytes wait on darwin. Linux reads, through sock_diag, how much the
// application had written to each paired connection; macOS has no
// sock_diag, so the wait reads macOS's TCP connection table (pcbtable_xnu.go)
// once as it begins: the application's socket, by its ends, gives the
// sequence number after the last thing it wrote (writeSeq: snd_una plus what
// its send buffer holds, plus its FIN), so bytes still in its send buffer,
// held there by a receive window the proxy's starved handler keeps closed,
// or queued on loopback's input thread (XNU's lo_output does not deliver
// synchronously), are waited for, not missed. The proxy's side is then read
// from its own socket at each look (readProxySide: tcpi_rcv_nxt, which only
// in-sequence bytes and the FIN move; tcpi_rxbytes also counts payload that
// arrived twice), on the same events as Linux's: every read the proxy begins
// or ends on a connection it keeps, and every close of a paired one.

// WaitPairedBytes returns once the proxy's side of every connection it holds
// that was paired with an application connection numbered 1..n has received
// every byte the application had written to it when the wait began, as on
// Linux (pairbytes_linux.go says what each outcome means); writes made after
// it began cannot hold it back. It has no deadline of its own. It ends with
// ctx's error, or one wrapping models.ErrProxyConnsUnsupported when it cannot
// tell: the counts' layout is not the one keploy reads (xnuTableReadable),
// the table or a socket cannot be read, or the proxy closed its side of
// a connection whose application end went after the close without its FIN
// reaching the proxy's orphaned socket.
func (p *Proxy) WaitPairedBytes(ctx context.Context, n uint64) error {
	// Counts in a layout the proof did not find are not read: whatever
	// they say, the wait is unknown.
	if err := xnuTableReadable(); err != nil {
		return err
	}
	// What the proxy had moved on each connection as this end began is
	// this wait's to answer for (pairUntold).
	p.paired.markEnd(n, agent.EndWorkerOf(ctx))
	pending, err := p.pairTargets(n)
	if err != nil {
		return err
	}
	return p.waitPairTargets(ctx, pending)
}

// pairTargets reads, for every paired connection the proxy holds whose
// application connection is numbered 1..n, how much the application had
// written to it, from one read of the table.
func (p *Proxy) pairTargets(n uint64) ([]*pairTarget, error) {
	pcs, err := p.paired.upTo(n)
	if err != nil || len(pcs) == 0 {
		return nil, err
	}
	table, err := readXNUTable()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrProxyConnsUnsupported, err)
	}
	var pending []*pairTarget
	var held heldSockets
	for _, pc := range pcs {
		t, err := p.pairTarget(table, pc, &held)
		if err != nil {
			return nil, err
		}
		if t != nil {
			pending = append(pending, t)
		}
	}
	return pending, nil
}

// pairTarget reads, from the application's socket in table, how much it had
// written to pc: nil when there is nothing to wait for. One the table does not
// list, while it has listed none of the application's sockets, is read from
// its holder's descriptor table (h, heldTarget).
func (p *Proxy) pairTarget(table []xnuPCB, pc *pairedConn, h *heldSockets) (*pairTarget, error) {
	if pc.noSession.Load() {
		return nil, nil
	}
	app, ok := findXNUPCB(table, pc.app, pc.proxy)
	if !ok && !p.paired.visible.Load() {
		if t, held, err := p.heldTarget(table, pc, h); held || err != nil {
			return t, err
		}
	}
	if !ok {
		// Gone: closed and its timewait over, or reset. Its FIN, after
		// everything it wrote, or its reset, is what the proxy's side
		// waits for (pairEnded).
		return &pairTarget{pc: pc, gone: true}, nil
	}
	p.paired.visible.Store(true)
	if pr, ok := findXNUPCB(table, pc.proxy, pc.app); ok && pr.irs != app.iss {
		// Two incarnations of one pair of ends: the table's application
		// socket is not the one the proxy holds the other end of.
		return nil, fmt.Errorf("%w: macOS's TCP table lists the application's socket %s with a first sequence number (%d) other than the one the proxy's side of it began from (%d)", models.ErrProxyConnsUnsupported, pc.app, app.iss, pr.irs)
	}
	if app.state == xnuTCPTimeWait || app.state == xnuTCPFinWait2 {
		// Closed, and its FIN acknowledged: the proxy's side received
		// everything it wrote.
		return nil, nil
	}
	return &pairTarget{pc: pc, base: targetBase{isn: app.iss}, want: pc.count(app.writeSeq() - app.iss - 1), finCounted: app.finPending()}, nil
}

// heldTarget is pairTarget for an application socket the table does not list,
// read from its holder's descriptor table (pairedConn.appSocket): held is
// false when no process the agent may read holds it, or the table lists no
// proxy's side of it to count from (both gone: closed). What it had written is
// at most what the proxy's side had received by the read after it, plus what
// its send buffer held, plus its FIN when that is not yet acknowledged and the
// side has not taken it: the wait's want, which it needs no more than, and
// reaches sooner once a read of the socket finds its send buffer empty
// (heldSettled).
func (p *Proxy) heldTarget(table []xnuPCB, pc *pairedConn, h *heldSockets) (t *pairTarget, held bool, err error) {
	pr, listed := findXNUPCB(table, pc.proxy, pc.app)
	if !listed {
		return nil, false, nil
	}
	s, found, err := pc.appSocket(h)
	if err != nil {
		return nil, false, fmt.Errorf("%w: macOS's TCP table does not list the application's end of a connection to the proxy, %s, and the processes holding the application's sockets cannot be read: %v", models.ErrProxyConnsUnsupported, pc.app, err)
	}
	if !found {
		return nil, false, nil
	}
	p.paired.held.Store(true)
	if s.State == xnuTCPTimeWait || s.State == xnuTCPFinWait2 {
		// Closed, and its FIN acknowledged (pairTarget).
		return nil, true, nil
	}
	ps := readHeldProxySide(pc.conn)
	if ps.err != nil {
		return nil, false, fmt.Errorf("%w: read the proxy's socket from %s: %v", models.ErrProxyConnsUnsupported, pc.app, ps.err)
	}
	fin := heldFINPending(s)
	d := ps.rcvNxt - pr.irs - 1 + s.SndCC
	if fin && !peerEnded(ps.state) {
		d++
	}
	return &pairTarget{pc: pc, base: targetBase{isn: pr.irs, held: true}, want: pc.count(d), finCounted: fin}, true, nil
}

// heldSettled says, of a wait's target read from its holder's descriptor
// table (heldTarget), whether a read of the application's socket now finds
// everything it wrote received: its send buffer empty, and its FIN, if it
// sent one, taken by the proxy's side (ps, read before). A reset socket's
// buffer is emptied unsent, so it settles nothing. One its holder closed is
// gone from then on: its FIN, or reset, after everything it wrote, is what
// the proxy's side waits for (pairEnded).
func heldSettled(t *pairTarget, ps proxySide) (bool, error) {
	s, found, err := t.pc.appSocket(&heldSockets{})
	switch {
	case err != nil:
		return false, fmt.Errorf("%w: read the process holding the application's end of a connection to the proxy, %s: %v", models.ErrProxyConnsUnsupported, t.pc.app, err)
	case !found:
		t.gone, t.base.held = true, false
		return false, nil
	}
	return s.State != xnuTCPClosed && s.SndCC == 0 && (!heldFINPending(s) || peerEnded(ps.state)), nil
}

// pairedConnOS is what darwin alone reads of a paired connection: one count of
// what the application wrote (wroteCount), and the process holding the
// application's end for a macOS whose TCP table does not list it (appHolder).
// macOS's table gives no device a socket is bound to (Linux's appDev and
// proxyDev).
type pairedConnOS struct {
	wroteCount
	appHolder
}

// pairedConnsOS is what proves, on darwin, an application socket not found
// gone, not hidden.
type pairedConnsOS struct {
	// visible: an application socket has been listed in macOS's TCP table
	// as the agent reads it (proveAppVisible, pairTarget,
	// noteWroteAtClose): one the table does not list is gone, not hidden.
	// Until one is, an application socket not listed may be one hidden
	// from the agent, and what it wrote is not known.
	visible atomic.Bool
	// held: an application socket the table did not list was read from its
	// holder's descriptor table (pairedConn.appSocket), so one no process
	// the agent may read holds is gone, not hidden.
	held atomic.Bool
}

// wroteCount extends darwin's counts of what the application had written to a
// connection, distances from its first sequence number that wrap at 2^32, to
// one count from its first byte (pairedConn): a cut end's want and a close's
// bytes lost (lostBy) are compared as counts, and past 4 GiB a distance would
// place a loss among the ends cut short before the wrap. Each read of the
// application's socket counts from the last any read found: a step of less
// than 2^31 back is a read made before that one (a wait's and a close's, run
// at once); any other step is ahead, as the application only writes on. So
// the counts hold while the application writes less than 2 GiB between two
// reads of its socket, a wait's or a close's.
type wroteCount struct {
	mu   sync.Mutex
	last uint64
}

// count is d, a distance from the application socket's first sequence number
// (to the one after the last thing it wrote), as a count from the
// connection's first byte. A step back past the first byte is none: it is
// ahead, past the wrap.
func (c *wroteCount) count(d uint32) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if back := uint32(c.last) - d; back < 1<<31 && uint64(back) <= c.last {
		return c.last - uint64(back)
	}
	c.last += uint64(d - uint32(c.last))
	return c.last
}

// targetBase is where, on darwin, the proxy's side's count of what it
// received of a connection begins: the application socket's initial sequence
// number (the proxy's side's irs). want and seen count from the one after it.
// held: want was read from the application socket's holder (heldTarget), the
// most it could have written, so each look reads that socket again
// (heldSettled), and the wait looks on its own while it waits (looks).
type targetBase struct {
	isn  uint32
	held bool
}

// looks says whether a wait on a target that begins from b looks at it on its
// own (orphanLooks): nothing else marks the moment its holder's send buffer
// empties.
func (b targetBase) looks() bool { return b.held }

// received is how much the proxy's side ps received since b, its FIN
// counted: a distance between sequence numbers, which wrap at 2^32.
func (b targetBase) received(ps proxySide) uint32 { return ps.rcvNxt - b.isn - 1 }

// pairReached says whether t needs no more waiting: the proxy's side received
// what the application had written, or it never will (a capture loss).
// Sequence numbers wrap at 2^32, so what the proxy's side received is
// compared to what was written as their distance, not as counts.
func (p *Proxy) pairReached(t *pairTarget) (bool, error) {
	pc := t.pc
	ps, closed := pc.side()
	if pc.noSession.Load() {
		// As on Linux: marked after the wait took it, read after its side.
		return true, nil
	}
	if ps.err != nil {
		return false, fmt.Errorf("%w: read the proxy's socket from %s: %v", models.ErrProxyConnsUnsupported, pc.app, ps.err)
	}
	if t.gone {
		return p.pairEnded(t, ps, closed)
	}
	got := t.base.received(ps)
	if d := int32(got - uint32(t.seen)); d > 0 {
		t.seen = uint64(got)
		p.paired.progress.Add(1)
	}
	short := int32(uint32(t.want) - got)
	if short > 0 && t.base.held {
		if settled, err := heldSettled(t, ps); err != nil || settled {
			return settled, err
		}
		if t.gone {
			return p.pairEnded(t, ps, closed)
		}
	}
	switch {
	case short <= 0:
		return true, nil
	case ps.state == xnuTCPClosed:
		// A reset reached the proxy's side before the proxy closed it: XNU
		// too takes a reset only at the sequence number it expects next
		// (RFC 5961), so every byte the application had sent had arrived
		// (pairbytes_linux.go's pairReached).
		return true, nil
	case closed:
		// Short of what was written, and the proxy closed it: as on Linux
		// (pairbytes_linux.go's pairReached says why), an orphan that took
		// the application's FIN refused nothing; one FIN short with the
		// orphan still waiting for that FIN is looked at again; anything
		// else is a loss.
		//
		// The close may not have reached the kernel yet (the descriptor is
		// marked closing, which is what side reads as closed, before its
		// last reference goes), and the socket is then no orphan. One FIN
		// short, a socket in CLOSE_WAIT took that FIN in sequence, after
		// every byte counted as the close began: nothing was refused. One
		// still ESTABLISHED waits for it as an orphan does. More short than
		// that, the bytes went with the close.
		//
		// What was written is counted up to what the application had
		// written as the close began (noteWroteAtClose), when that is less:
		// bytes it wrote after keploy's close are no capture loss, and are
		// not waited for. Less, it wrote on after that read, so the read
		// held no FIN of its: one short of it is a byte, never the FIN.
		// Both count from the application socket's first sequence number
		// (each read checks it against the proxy's side), so they are
		// compared as a distance, as what was received is.
		want, finCounted := t.want, t.finCounted
		if w := pc.wroteAtClose.Load(); w != nil && int32(uint32(w.want)-uint32(want)) < 0 {
			want, finCounted = w.want, false
		}
		short = int32(uint32(want) - got)
		if short <= 0 {
			return true, nil
		}
		d, took, err := orphanTookFIN(pc, t.base.isn)
		oneShort := finCounted && short == 1
		switch {
		case took:
			return true, nil
		case err == nil && oneShort && d.state == xnuTCPCloseWait:
			return true, nil
		case errors.Is(err, errNoXNUPCB) && oneShort:
			return false, fmt.Errorf("%w: the proxy closed its side of a connection from %s one FIN short of what the application wrote, and its orphaned socket is gone: whether it took that FIN or refused a byte cannot be told", models.ErrProxyConnsUnsupported, pc.app)
		case err != nil && oneShort:
			return false, fmt.Errorf("%w: the proxy closed its side of a connection from %s one FIN short of what the application wrote, and its orphaned socket, which tells that FIN from a byte it refused, cannot be read: %v", models.ErrProxyConnsUnsupported, pc.app, err)
		case err == nil && oneShort && (d.state == xnuTCPEstablished || d.state == xnuTCPFinWait1 || d.state == xnuTCPFinWait2):
			if t.orphan != d.state {
				t.orphan = d.state
				p.paired.progress.Add(1)
			}
			return false, nil
		}
		// The bytes lost run from the first its side did not receive to
		// the last the application wrote before its FIN.
		top := want
		if finCounted {
			top--
		}
		p.pairLost(pc, want-uint64(short)+1, top, "bytes the application wrote to a connection keploy's proxy holds never reached the proxy: the connection was reset, or closed, first, so what they carried was neither recorded nor mocked")
		return true, nil
	}
	return false, nil
}

// orphanEstablished is XNU's ESTABLISHED, the state a bytes wait finds a
// closed connection's socket in while the proxy's close has not reached the
// kernel (pairReached, orphansUnmoved).
const orphanEstablished = xnuTCPEstablished

// errNoXNUPCB: the table lists no such socket.
var errNoXNUPCB = errors.New("macOS's TCP table lists no such socket")

// orphanTookFIN reads the proxy's socket of pc, orphaned when the proxy closed
// its side (the one that began from isn): it took the application's FIN in
// sequence when it is in TIME_WAIT, CLOSING or LAST_ACK (pairbytes_linux.go).
func orphanTookFIN(pc *pairedConn, isn uint32) (xnuPCB, bool, error) {
	table, err := readXNUTable()
	if err != nil {
		return xnuPCB{}, false, err
	}
	d, ok := findXNUPCB(table, pc.proxy, pc.app)
	if !ok || d.irs != isn {
		return d, false, errNoXNUPCB
	}
	return d, d.state == xnuTCPTimeWait || d.state == xnuTCPClosing || d.state == xnuTCPLastAck, nil
}

// pairEnded is pairReached for a connection whose application socket was gone
// when the wait began: the proxy's side waits for its FIN (everything it
// wrote came before) or its reset.
func (p *Proxy) pairEnded(t *pairTarget, ps proxySide, closed bool) (bool, error) {
	pc := t.pc
	switch {
	case ps.state == xnuTCPCloseWait || ps.state == xnuTCPLastAck || ps.state == xnuTCPClosing || ps.state == xnuTCPTimeWait:
		return true, nil
	case ps.state == xnuTCPClosed:
		return true, nil
	case closed:
		if d, err := readXNUTable(); err == nil {
			if o, ok := findXNUPCB(d, pc.proxy, pc.app); ok && (o.state == xnuTCPTimeWait || o.state == xnuTCPClosing || o.state == xnuTCPLastAck) {
				return true, nil
			}
		}
		return false, fmt.Errorf("%w: the application's end of a connection to the proxy, %s, went after the proxy closed its side, and whether bytes it wrote before that close were refused cannot be told", models.ErrProxyConnsUnsupported, pc.app)
	case p.paired.visible.Load() || p.paired.held.Load():
		// The application's sockets are listed to the agent, or read from
		// their holders' descriptor tables, so this one is gone with its
		// FIN or reset on its way (loopback's input thread): its arrival
		// ends the handler's read, and the wait looks again.
		return false, nil
	default:
		return false, fmt.Errorf("%w: macOS's TCP table does not list the application's end of a live connection to the proxy, %s, and has listed none of the application's sockets to keploy's agent: %s", models.ErrProxyConnsUnsupported, pc.app, xnuHidden)
	}
}

// xnuHidden says why an application socket the table does not list may be
// one hidden from the agent, not one gone.
const xnuHidden = "macOS lists to a process only its own sockets in some cases (macOS 27, for a process keploy's CLI starts without root), so what the application wrote cannot be counted"

// tcpStateName names the TCP states a bytes wait or a close reports, by
// XNU's names.
func tcpStateName(s uint8) string { return xnuTCPStateName(s) }

// appSocketClosed reports whether no process holds pc's application socket
// any more (the drain's word to a handler waiting past the application's
// FIN, pairedConn.appClosed): on darwin it is not read, and false leaves such
// a wait to a reset or the run's end, as before.
func (*Proxy) appSocketClosed(*pairedConn) bool { return false }

// PairedBytesErr is why the bytes on the proxy's paired connections cannot be
// counted, nil while they can: macOS's TCP table cannot be read in the layout
// keploy reads (found once, xnuTableReadable), or a failure since makes every
// later wait on a paired connection unknown.
func (p *Proxy) PairedBytesErr() error {
	p.paired.mu.Lock()
	err := p.paired.err
	p.paired.mu.Unlock()
	if err != nil {
		return err
	}
	return xnuTableReadable()
}

// xnuTableReadable proves the counts once (proveXNUCounts): a macOS whose
// table or tcp_info keploy cannot read in the layout it reads leaves every
// wait on a paired connection, and every count of what a kept connection
// received (inflight_darwin.go), unknown from the start, as Linux's canary
// does (checkPairedBytes).
var xnuTableReadable = xnuCountsProven(proveXNUCounts)

// xnuCountsProven runs proof once, its failure an ErrProxyConnsUnsupported:
// xnuTableReadable, and its rows' proof of a moved layout.
func xnuCountsProven(proof func() error) func() error {
	return sync.OnceValue(func() error {
		if err := proof(); err != nil {
			return fmt.Errorf("%w: %v", models.ErrProxyConnsUnsupported, err)
		}
		return nil
	})
}

// checkPairedBytes on Linux proves, on the canary's connection, that
// sock_diag gives the byte counts the wait reads. On darwin sock_diag does
// not exist: the counts the waits read are proven once, on a loopback
// connection of keploy's own (xnuTableReadable), before the proxy says it is
// ready (decideCounted). The canary itself runs as on Linux (accepts.go).
func (p *Proxy) checkPairedBytes(net.Conn, net.Conn) {}

// proveAppVisible notes, until it has once, that macOS's TCP table lists the
// application's sockets to the agent, as Linux's notes they are seen from the
// proxy's network namespace: the accepted connection's application end, just
// paired, is in the table. macOS has one network stack for every process, but
// it does not list every socket to every process (xnuHidden): one the table
// does not list is gone only once one of the application's has been listed.
// Where macOS lists the agent only its own sockets (macOS 27) none ever is,
// so every accept reads the table.
func (p *Proxy) proveAppVisible(conn *net.TCPConn) {
	if p.paired.visible.Load() || xnuTableReadable() != nil {
		return
	}
	app, ok1 := addrPortOf(conn.RemoteAddr())
	pr, ok2 := addrPortOf(conn.LocalAddr())
	if !ok1 || !ok2 {
		return
	}
	table, err := readXNUTable()
	if err != nil {
		return
	}
	if _, ok := findXNUPCB(table, app, pr); ok {
		p.paired.visible.Store(true)
	}
}

// readUnread returns the number of bytes in conn's receive buffer that the
// application wrote but the proxy has not yet read. On macOS, SO_NREAD
// (getsockopt SOL_SOCKET) returns this count — the macOS-specific equivalent
// of Linux's SIOCINQ/FIONREAD. For TCP sockets it returns total unread bytes.
func readUnread(conn *net.TCPConn) (int, error) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		n, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NREAD)
	}); err != nil {
		return 0, err
	}
	return n, gerr
}

// readProxySide reads the proxy's side of a paired connection: the next
// sequence number its socket expects (tcpi_rcv_nxt) and its TCP state, from
// one copy of XNU's struct tcp_info (getsockopt TCP_INFO, xnuTCPInfo).
func readProxySide(conn *net.TCPConn) proxySide {
	rc, err := conn.SyscallConn()
	if err != nil {
		return proxySide{err: err}
	}
	var ps proxySide
	if err := rc.Control(func(fd uintptr) { ps.rcvNxt, ps.state, ps.err = xnuTCPInfo(int(fd)) }); err != nil {
		return proxySide{err: err}
	}
	return ps
}

// readHeldProxySide is how heldTarget, noteHeldAtClose and noteUnlistedAtClose
// read the proxy's side (readProxySide); a test replaces it to make the read
// fail.
var readHeldProxySide = readProxySide

// heldUncounted says why bytes in the send buffer of an application socket
// read from its holder (procsock) cannot be counted.
const heldUncounted = "macOS's TCP table does not list this socket to keploy's agent (macOS 27, for an agent keploy's CLI starts without root), and the read of it from the process holding it gives the bytes its send buffer holds but no sequence numbers: bytes the proxy's side received and had not yet acknowledged cannot be told from bytes still on their way"

// noteWroteAtClose reads, as the proxy begins closing pc, what the application
// had written to it (pairedConn.wroteAtClose), from macOS's TCP table as a
// bytes wait reads it as it begins (pairTarget): the application socket's
// sequence number after the last thing it wrote, its FIN counted once it
// closed (fin: one table read holds both), as a distance from its first. It
// is read before the proxy's side, so a byte it counts that the side has not
// received was written before the close and is still on its way: held in the
// application's send buffer, or queued on loopback's input thread. The close
// then waits for those bytes and charges any data among them (pairClosing),
// as on Linux, rather than dropping a connection with bytes unaccounted.
// What the application writes after it is no capture loss (Linux's
// noteWroteAtClose says why).
//
// It is read even when the proxy's side has taken the application's FIN,
// though nothing is on its way then: unlike Linux's, the side's own count
// (its sequence number) says nothing of where the connection began, and the
// record does, so a loss of bytes the close tosses unread is placed among the
// ends cut short on the connection (pairedConn.lostBy). No record is made
// from counts in a layout the proof did not find (xnuTableReadable: every
// wait is unknown then), or for an application socket the table no longer
// lists with the first sequence number the proxy's side began from (gone:
// reset, its reset the last it sent), as Linux makes none for a socket it
// cannot read: the close then finds nothing on its way. A table that cannot
// be read tells nothing: the tests the connection could have served are left
// without a verdict (closeUnknown).
//
// An application socket not listed is gone only once one of the
// application's has been (paired.visible). Before, it may be hidden from the
// agent (xnuHidden), and is read from its holder (noteHeldAtClose). One no
// process the agent may read holds, another user's or one its holder closed,
// is gone only once the proxy's side shows it took the application's FIN or
// reset, after which nothing it wrote is on its way: a socket whose holder
// closed it lives on in the kernel until its bytes and its FIN are
// acknowledged (FIN_WAIT_1, CLOSING, LAST_ACK), and no holder's read finds
// it. The table shows a FIN taken in the proxy's side's entry; a reset it
// shows by dropping that entry, which the side's own socket then tells
// (noteUnlistedAtClose). The side's own read, as the close begins, may show
// one the table's read did not (noteUnheldAtClose). Before either, what was
// on its way cannot be told: the tests the connection could have served are
// left without a verdict (closeUnknown).
func (p *Proxy) noteWroteAtClose(ctx context.Context, pc *pairedConn) {
	if xnuTableReadable() != nil {
		return
	}
	table, err := readXNUTable()
	if err != nil {
		p.closeUnknown(pc, fmt.Errorf("%w: macOS's TCP table cannot be read as the proxy closes a connection from %s: %v", models.ErrProxyConnsUnsupported, pc.app, err))
		return
	}
	app, ok := findXNUPCB(table, pc.app, pc.proxy)
	if !ok {
		pr, listed := findXNUPCB(table, pc.proxy, pc.app)
		switch {
		case p.paired.visible.Load():
		case listed && p.noteHeldAtClose(pc, pr):
		case listed && peerEnded(pr.state):
		case listed:
			p.noteUnheldAtClose(ctx, pc)
		default:
			p.noteUnlistedAtClose(pc)
		}
		return
	}
	p.paired.visible.Store(true)
	if pr, ok := findXNUPCB(table, pc.proxy, pc.app); !ok || pr.irs != app.iss {
		return
	}
	pc.wroteAtClose.Store(&appWrote{want: pc.count(app.writeSeq() - app.iss - 1), fin: app.finPending(), base: targetBase{isn: app.iss}})
}

// noteUnheldAtClose is noteWroteAtClose for an application socket the table
// does not list and no process the agent may read holds, the proxy's side not
// past the application's FIN or reset as the table was read. Its holder closed
// it (an HTTP client that read the proxy's answer with Connection: close, as
// the answer to a call no mock answers is, and closed at once), or a process
// of another user holds it. A socket its holder closed sends its FIN at once,
// after every byte it wrote, but loopback's input thread hands that FIN to the
// proxy's side some time later, the longer the busier the host: the close
// waits for it (unheldSideAtClose), and a side that takes the FIN or a reset
// has nothing on its way, and no record is made, as for one the table shows
// past it. Otherwise what the application wrote cannot be counted, and bytes
// the open test wrote may be on their way before that FIN, unseen: the close
// is unknown, and the tests the connection could have served, the open test
// of its worker among them whether or not the proxy moved bytes on it, are
// left without a verdict (closeUnknown: the side has not taken the FIN).
// keploy's own stop (ctx) ends the wait and charges nothing.
func (p *Proxy) noteUnheldAtClose(ctx context.Context, pc *pairedConn) {
	ps, why := p.unheldSideAtClose(ctx, pc)
	switch {
	case ctx.Err() != nil:
	case ps.err != nil:
		p.closeUnknown(pc, fmt.Errorf("%w: macOS's TCP table does not list the application's end of a connection to the proxy, %s, as the proxy closes it, no process keploy's agent may read holds it, and the proxy's side of it cannot be read: %v", models.ErrProxyConnsUnsupported, pc.app, ps.err))
	case peerEnded(ps.state):
	default:
		p.closeUnknown(pc, fmt.Errorf("%w: macOS's TCP table does not list the application's end of a connection to the proxy, %s, as the proxy closes it, no process keploy's agent may read holds it, and the proxy's side has not taken its FIN or reset (it is in %s): bytes it wrote may still be on their way, as a socket its holder closed lives on until they are acknowledged (%s); %s", models.ErrProxyConnsUnsupported, pc.app, tcpStateName(ps.state), xnuHidden, why))
	}
}

// unheldSideAtClose reads the proxy's side of pc for noteUnheldAtClose until
// it shows the application's FIN or reset taken, or cannot be read, or the
// close can wait no longer, and says why it stopped short of the FIN (why).
// It waits only while the application's sockets have been read from their
// holders (paired.held): no process keploy's agent may read holding this one
// says its holder closed it, so its FIN is on its way. Before that, a process
// the agent may not read (another user's) may hold it open, as a pool's idle
// socket, and send no FIN: the close does not wait. Nor does it while the
// side holds bytes unread: the close tosses them, a loss it charges whatever
// comes after (pairClosing), and a receive window they filled may keep that
// FIN from ever coming. It looks again on looks of its own, the first a
// stall.Tick on, then twice as far apart, up to
// stall.CheckEvery(orphanFINBudget), as a bytes wait looks at an orphan, and
// stops once keploy could run for orphanFINBudget (stall.Meter): a starved
// agent accrues that budget only as fast as it gets to run, and a socket
// another process keeps open ends the wait at the budget, never holding the
// close for good.
func (p *Proxy) unheldSideAtClose(ctx context.Context, pc *pairedConn) (proxySide, string) {
	ps := readHeldProxySide(pc.conn)
	if ps.err != nil || peerEnded(ps.state) {
		return ps, ""
	}
	if !p.paired.held.Load() {
		return ps, "no application socket has been read from its holder yet, so a process keploy's agent may not read may hold this one open, and the close does not wait for its FIN"
	}
	meter := stall.NewMeter(p.paired.clk, orphanFINBudget)
	defer meter.Stop()
	for every := stall.Tick; ; every = min(2*every, stall.CheckEvery(orphanFINBudget)) {
		// A socket whose count cannot be read fails the side's read too,
		// which ends the wait.
		if n, _ := readUnread(pc.conn); n > 0 {
			return ps, "the proxy's side holds bytes unread, which the close tosses, so it does not wait for the FIN behind them"
		}
		if meter.Expired() {
			return ps, fmt.Sprintf("keploy waited for its FIN while it could run for %v", orphanFINBudget)
		}
		var look <-chan time.Time
		var stop func() bool
		if p.paired.clk == nil {
			look, stop = stall.ProcessClock().After(every)
		} else {
			look, stop = p.paired.clk.After(every)
		}
		select {
		case <-look:
		case <-ctx.Done():
			stop()
			return ps, ""
		}
		if ps = readHeldProxySide(pc.conn); ps.err != nil || peerEnded(ps.state) {
			return ps, ""
		}
	}
}

// noteUnlistedAtClose is noteWroteAtClose for a connection the table lists
// neither end of, with none of the application's sockets proven listed.
// macOS's table drops a reset socket's entry, the proxy's side's among them:
// an application that reset its connection (SO_LINGER 0, or a close with
// bytes unread in its receive buffer, as Go's Transport closes a response
// body read short) leaves no entry for either end. A proxy's side whose own
// socket shows that reset (CLOSED) took it after everything the application
// wrote, as it takes a FIN, so nothing is on its way, and no record is made,
// as for a reset socket its holder shows (noteHeldAtClose). Any other side is
// unknown: the table lists the proxy's own sockets to keploy's agent, so one
// missing in another state is not what a reset leaves, and with no entry
// there is no first sequence number to count a holder's read from. So is one
// whose own socket cannot be read, said with the read's error.
func (p *Proxy) noteUnlistedAtClose(pc *pairedConn) {
	ps := readHeldProxySide(pc.conn)
	switch {
	case ps.err != nil:
		p.closeUnknown(pc, fmt.Errorf("%w: macOS's TCP table lists neither end of a connection to the proxy, %s, as the proxy closes it, and the proxy's side of it cannot be read: %v", models.ErrProxyConnsUnsupported, pc.app, ps.err))
	case ps.state != xnuTCPClosed:
		p.closeUnknown(pc, fmt.Errorf("%w: macOS's TCP table lists neither end of a connection to the proxy, %s, as the proxy closes it, and the proxy's side was not reset (it is in %s): bytes the application wrote may still be on their way, and there is no entry for the proxy's side to count them from (%s)", models.ErrProxyConnsUnsupported, pc.app, tcpStateName(ps.state), xnuHidden))
	}
}

// noteHeldAtClose is noteWroteAtClose for an application socket the table
// does not list, read from its holder's descriptor table (pairedConn.appSocket)
// before the proxy's side (pr, the side's entry in the table; ps, its count):
// done is false when no process the agent may read holds it. Its send buffer
// empty, everything it had written was received by then, which the side's
// count after it holds, its FIN counted as on its way while the side has not
// taken it: the record (exact, as the table's). A reset socket's buffer is
// emptied unsent: it is gone, and no record is made. Bytes in that buffer the
// side had not acknowledged, its FIN not taken, may be data on its way or
// bytes received: which cannot be told (heldUncounted), and the tests the
// connection could have served are left without a verdict, said in one
// log line naming the connection and the bytes (closeUnknown). So it is when
// the processes or the proxy's side cannot be read.
//
// With the holder not yet known (pairedConn.holder) the read is of every
// process the agent may read, as a wait's is (heldSockets): on macOS 27 a
// close pays that scan too.
func (p *Proxy) noteHeldAtClose(pc *pairedConn, pr xnuPCB) (done bool) {
	s, found, err := pc.appSocket(&heldSockets{})
	switch {
	case err != nil:
		p.closeUnknown(pc, fmt.Errorf("%w: macOS's TCP table does not list the application's end of a connection to the proxy, %s, as the proxy closes it, and the processes holding the application's sockets cannot be read: %v", models.ErrProxyConnsUnsupported, pc.app, err))
		return true
	case !found:
		return false
	}
	p.paired.held.Store(true)
	ps := readHeldProxySide(pc.conn)
	switch {
	case s.State == xnuTCPClosed:
		return true
	case ps.err != nil:
		p.closeUnknown(pc, fmt.Errorf("%w: macOS's TCP table does not list the application's end of a connection to the proxy, %s, and the proxy's side of it cannot be read as the proxy closes it: %v", models.ErrProxyConnsUnsupported, pc.app, ps.err))
		return true
	case s.SndCC != 0 && !peerEnded(ps.state):
		p.closeUnknown(pc, fmt.Errorf("%w: macOS's TCP table does not list the application's end of a connection to the proxy, %s, and its process's send buffer held %d bytes the proxy's side had not acknowledged as the proxy closed it: %s", models.ErrProxyConnsUnsupported, pc.app, s.SndCC, heldUncounted),
			zap.Uint32("unacknowledged_bytes", s.SndCC),
			zap.String("why", heldUncounted))
		return true
	}
	fin := heldFINPending(s)
	d := ps.rcvNxt - pr.irs - 1
	if fin && !peerEnded(ps.state) {
		d++
	}
	pc.wroteAtClose.Store(&appWrote{want: pc.count(d), fin: fin, base: targetBase{isn: pr.irs}})
	return true
}

// readSide is pc's proxy side as its close reads it (closing, pairClosing):
// once the close has read what the application had written (noteWroteAtClose),
// what the side received is counted as that is (appWrote.received), so the
// close compares the two; before, or without it, it is not known (counted
// false).
func (pc *pairedConn) readSide() proxySide {
	ps := readProxySide(pc.conn)
	if w := pc.wroteAtClose.Load(); w != nil && ps.err == nil {
		ps.received, ps.counted = w.received(ps.rcvNxt), true
	}
	return ps
}

// received is how much a proxy's side whose next expected sequence number is
// rcvNxt received, its FIN counted, in w's count (from the one after isn).
// Sequence numbers wrap at 2^32, and w.want with them, so it is w.want less
// how far rcvNxt is short of the sequence number after what was written, a
// distance, as a wait compares them (pairReached); never below 0.
func (w appWrote) received(rcvNxt uint32) uint64 {
	short := int64(int32(uint32(w.want) - (rcvNxt - w.base.isn - 1)))
	return uint64(max(int64(w.want)-short, 0))
}

// peerEnded says whether the proxy's side of a connection, in XNU TCP state
// state, has taken the application's FIN or its reset (xnuClientGone).
func peerEnded(state uint8) bool { return xnuClientGone(state) }

// dataReceived is what a proxy's side received of the application's data: its
// count (readSide), but for the application's FIN once it took it. Without a
// count (the close read no record of what the application had written) it is
// not known, and it answers the most it could be: a loss at a close then runs
// to any byte the application wrote (lostTop), so the test open as it is
// counted is always charged (pairLost): an earlier test whose end was cut
// short on the connection may go uncharged its share, and that test is not
// verified already.
func dataReceived(ps proxySide) uint64 {
	if !ps.counted {
		return math.MaxUint64
	}
	if ps.received > 0 && (ps.state == xnuTCPCloseWait || ps.state == xnuTCPLastAck || ps.state == xnuTCPClosing || ps.state == xnuTCPTimeWait) {
		return ps.received - 1
	}
	return ps.received
}
