//go:build darwin

package proxy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
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
	for _, pc := range pcs {
		t, err := p.pairTarget(table, pc)
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
// written to pc: nil when there is nothing to wait for.
func (p *Proxy) pairTarget(table []xnuPCB, pc *pairedConn) (*pairTarget, error) {
	if pc.noSession.Load() {
		return nil, nil
	}
	app, ok := findXNUPCB(table, pc.app, pc.proxy)
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
	return &pairTarget{pc: pc, base: targetBase{isn: app.iss}, want: uint64(app.writeSeq() - app.iss - 1), finCounted: app.finPending()}, nil
}

// targetBase is where, on darwin, the proxy's side's count of what it
// received of a connection begins: the application socket's initial sequence
// number (the proxy's side's irs). want and seen count from the one after it.
type targetBase struct{ isn uint32 }

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
		p.pairLost(pc, uint64(got)+1, top, "bytes the application wrote to a connection keploy's proxy holds never reached the proxy: the connection was reset, or closed, first, so what they carried was neither recorded nor mocked")
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
	}
	// macOS has one network stack for every process, and its table lists
	// every TCP socket of the host: an application socket gone from it is
	// one whose FIN or reset is on its way (loopback's input thread), and
	// its arrival ends the handler's read.
	return false, nil
}

// tcpStateName names the TCP states a bytes wait reports.
func tcpStateName(s uint8) string {
	switch s {
	case xnuTCPFinWait1:
		return "FIN_WAIT_1"
	case xnuTCPFinWait2:
		return "FIN_WAIT_2"
	}
	return fmt.Sprintf("TCP state %d", s)
}

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

// proveAppVisible on Linux uses sock_diag to confirm the app's sockets are
// visible from the proxy's network namespace. On darwin the app and proxy
// share one namespace (no containers), so visibility is always true; there is
// nothing to prove.
func (p *Proxy) proveAppVisible(*net.TCPConn) {}

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
// wait is unknown then), from a table that cannot be read, or for an
// application socket the table no longer lists with the first sequence number
// the proxy's side began from (gone: reset, its reset the last it sent), as
// Linux makes none for a socket it cannot read: the close then finds nothing
// on its way.
func noteWroteAtClose(pc *pairedConn) {
	if xnuTableReadable() != nil {
		return
	}
	table, err := readXNUTable()
	if err != nil {
		return
	}
	app, ok := findXNUPCB(table, pc.app, pc.proxy)
	if !ok {
		return
	}
	if pr, ok := findXNUPCB(table, pc.proxy, pc.app); !ok || pr.irs != app.iss {
		return
	}
	pc.wroteAtClose.Store(&appWrote{want: uint64(app.writeSeq() - app.iss - 1), fin: app.finPending(), isn: app.iss})
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
	short := int64(int32(uint32(w.want) - (rcvNxt - w.isn - 1)))
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
