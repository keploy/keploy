//go:build linux

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// tcpFinWait2 is TCP_FIN_WAIT2: a socket closed by its owner whose FIN was
// acknowledged. Once orphaned the kernel keeps it as a timewait socket, which
// sock_diag reports in this state, with no tcp_info.
const tcpFinWait2 = 5

// pairTarget is what the wait needs of one paired connection.
type pairTarget struct {
	pc *pairedConn
	// want is how many bytes (and a FIN) the application had written to
	// the connection when the wait began (appWritten). The proxy's side has
	// received all of them once its own count (tcpi_bytes_received, which a
	// FIN also moves) reaches it.
	want uint64
	// finCounted: want counts the application's FIN, which its socket had
	// sent when the wait began (finSent): the last of want may be that FIN.
	// Otherwise every byte of want is data.
	finCounted bool
	// gone: the application's socket was already gone (closed, or reset)
	// when the wait began. Its FIN, after everything it wrote, or a reset,
	// is what the proxy's side waits for.
	gone bool
	// seen is the most the proxy's side was seen to have received: more is
	// progress, for the watchdog.
	seen uint64
	// orphan is, while the wait waits for the proxy's socket of a
	// connection it closed one FIN short to take that FIN (pairReached),
	// the socket's state at the last look (FIN_WAIT1, FIN_WAIT2, or
	// ESTABLISHED for a close that has not reached the kernel); 0 when it
	// does not.
	orphan uint8
}

// orphanFINBudget is how long, in time the agent could run, a bytes wait
// waits with nothing moving for an orphan to take the application's FIN
// (waitPairTargets) before it answers unknown: the one hang budget
// (stall.HangBudget).
const orphanFINBudget = stall.HangBudget

// WaitPairedBytes returns once the proxy's side of every connection it holds
// that the kernel paired with an application connection numbered 1..n has
// received every byte the application had written to it when the wait began,
// so that a call written to a connection opened long before (a pooled one) is
// in the proxy's hands before a test's end reads what the test did. Writes
// made after it began cannot hold it back. A connection the proxy serves with
// no session (pairedConn.noSession) is not waited on: nothing on it is
// captured. A connection the proxy closed
// short of what was written (anything that arrives at it now is answered
// with a reset) is a capture loss (models.CaptureLossAppBytesLost), and does
// not hold it back; so is one it closed with bytes unread, whenever a wait
// holds it or not (pairClosing). The application's own reset is no loss: the kernel takes
// a reset only at the sequence number it expects next, so every byte the
// application had sent had arrived, and what its abort dropped unsent it
// would have dropped without keploy.
//
// It has no deadline of its own. It looks again at every read the proxy
// begins or ends on a connection it keeps (inflight.go) and at every close of
// a paired one: bytes that arrive while the proxy's handler waits on the
// connection end that read, and bytes that arrive while the handler is busy
// are taken by its next read, so no arrival goes unseen. (An epoll watching
// the socket from beside the handler is no such event: in tests it missed
// arrivals the handler's own reads took in.) What arrives at a connection the
// proxy has closed, at its orphaned socket, ends no read: while it waits for
// such an orphan to take the application's FIN, it looks again on looks of its
// own (waitPairTargets).
//
// It ends with ctx's error, or, for a wait that holds a paired connection,
// with an error wrapping models.ErrProxyConnsUnsupported ("unknown") when it
// cannot tell: this kernel's sock_diag gives no byte counts, the listener has
// closed, or the application's socket cannot be seen while its connection is
// alive at the proxy's side and no application socket has ever been seen (it
// is in another network namespace than the proxy's, which keploy never sets
// up), or the proxy closed its side of one whose orphaned socket shows neither
// the application's FIN taken nor bytes refused (orphanTookFIN): the
// application's end went after the close (its own reset, or one refusing
// bytes it wrote before the close), or the proxy's side is one FIN short and
// its orphan took neither that FIN nor a byte to refuse while the agent could
// run for orphanFINBudget with nothing the wait waits on moving. Once one has
// been seen, an application socket that is gone is one whose FIN or reset is on
// its way: the wait waits for it (it ends the handler's read).
func (p *Proxy) WaitPairedBytes(ctx context.Context, n uint64) error {
	pending, err := p.pairTargets(n)
	if err != nil {
		return err
	}
	return p.waitPairTargets(ctx, pending)
}

// pairTargets reads, for every paired connection the proxy holds whose
// application connection is numbered 1..n, how much the application had
// written to it: what waitPairTargets waits for.
func (p *Proxy) pairTargets(n uint64) ([]*pairTarget, error) {
	pcs, err := p.paired.upTo(n)
	if err != nil || len(pcs) == 0 {
		return nil, err
	}
	ds, err := openDiag()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrProxyConnsUnsupported, err)
	}
	defer ds.close()
	var pending []*pairTarget
	for _, pc := range pcs {
		t, err := p.pairTarget(ds, pc)
		if err != nil {
			return nil, err
		}
		if t != nil {
			pending = append(pending, t)
		}
	}
	return pending, nil
}

// waitPairTargets waits until each of pending is reached (pairReached).
//
// An orphan's taking a FIN is no event of the proxy's: the proxy has closed
// the socket, so no read or close of its own follows, and the kernel tells no
// one when a timewait socket takes a FIN. While one waits for the
// application's FIN, the wait looks at it again on looks of its own (the first
// a stall.Tick on, then twice as far apart each time nothing moved, up to a
// second), and answers unknown only once the agent could run for
// orphanFINBudget with nothing it waits on moving: no connection received
// more, was resolved, or saw its orphan change state. A starved agent accrues
// that budget only as fast as it gets to run (stall.Meter). The looks only
// say when to look: what ends the wait is what the orphan shows.
func (p *Proxy) waitPairTargets(ctx context.Context, pending []*pairTarget) error {
	if len(pending) == 0 {
		return nil
	}
	looks := orphanLooks{clk: p.paired.clk}
	defer looks.close()
	for {
		// Taken before looking, so a read or close after the look still
		// wakes.
		reads := p.inflight.Changed()
		closes := p.paired.changes()
		if err := p.paired.failure(); err != nil {
			return err
		}
		moved := false
		next := pending[:0]
		for _, t := range pending {
			seen, orphan := t.seen, t.orphan
			reached, err := p.pairReached(t)
			if err != nil {
				return err
			}
			if reached {
				p.paired.progress.Add(1)
				moved = true
				continue
			}
			moved = moved || t.seen != seen || t.orphan != orphan
			next = append(next, t)
		}
		pending = next
		if len(pending) == 0 {
			return nil
		}
		if err := looks.arm(pending, moved); err != nil {
			return err
		}
		select {
		case <-reads:
		case <-closes:
		case <-looks.c:
			looks.c, looks.stop = nil, nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// orphanLooks are a bytes wait's own looks at the orphans it waits on to take
// the application's FIN (waitPairTargets), and its measure of how long the
// agent could run with nothing the wait waits on moving.
type orphanLooks struct {
	clk   stall.Clock // nil: the process's (stall.Process)
	meter *stall.Meter
	c     <-chan time.Time // the next look's, nil when none is armed
	stop  func() bool
	every time.Duration // the last look's distance; 0 after a move
}

// arm, after a look at pending that saw them move or not (moved), arms the
// next look while any of them waits for its orphan, unless the agent has
// since run for orphanFINBudget with nothing moving: then it answers unknown,
// saying what the wait waited for. The look after a move is a stall.Tick on
// (one already armed stays), and each look that saw none puts the next twice
// as far, up to stall.CheckEvery(orphanFINBudget). With none waiting for an
// orphan, no look is armed, and the measure ends.
func (o *orphanLooks) arm(pending []*pairTarget, moved bool) error {
	if !awaitsOrphan(pending) {
		o.disarm()
		o.meter.Stop()
		o.meter = nil
		return nil
	}
	switch {
	case o.meter == nil:
		o.meter, o.every = stall.NewMeter(o.clk, orphanFINBudget), 0
	case moved:
		o.meter.Reset()
		o.every = 0
	case o.meter.Expired():
		return orphansUnmoved(pending)
	}
	if o.c != nil {
		return nil
	}
	if o.every == 0 {
		o.every = stall.Tick
	} else {
		o.every = min(2*o.every, stall.CheckEvery(orphanFINBudget))
	}
	if o.clk == nil {
		o.c, o.stop = stall.ProcessClock().After(o.every)
	} else {
		o.c, o.stop = o.clk.After(o.every)
	}
	return nil
}

// awaitsOrphan says whether any of pending waits for its orphan to take the
// application's FIN.
func awaitsOrphan(pending []*pairTarget) bool {
	for _, t := range pending {
		if t.orphan != 0 {
			return true
		}
	}
	return false
}

// orphansUnmoved is the wait's answer once the agent could run for
// orphanFINBudget with nothing moving: unknown, naming each connection whose
// orphan it waited on, and the state the orphan was in.
func orphansUnmoved(pending []*pairTarget) error {
	var waited []string
	for _, t := range pending {
		switch t.orphan {
		case 0:
		case tcpEstablished:
			waited = append(waited, fmt.Sprintf("%s (its socket still ESTABLISHED: the proxy's close had not reached the kernel, so it was no orphan yet)", t.pc.app))
		default:
			waited = append(waited, fmt.Sprintf("%s (its orphan in %s)", t.pc.app, tcpStateName(t.orphan)))
		}
	}
	what := "the connection from " + waited[0]
	if len(waited) > 1 {
		what = "the connections from " + strings.Join(waited, ", ")
	}
	return fmt.Errorf("%w: keploy waited for the application's FIN to reach the proxy's socket of %s, which the proxy closed one FIN short of what the application wrote, and while keploy could run for %v nothing it waited on moved: the socket took neither that FIN nor a byte it would refuse", models.ErrProxyConnsUnsupported, what, orphanFINBudget)
}

// disarm releases the armed look, if any.
func (o *orphanLooks) disarm() {
	if o.stop != nil {
		o.stop()
	}
	o.c, o.stop = nil, nil
}

// close releases the armed look and the meter.
func (o *orphanLooks) close() {
	o.disarm()
	o.meter.Stop()
}

// tcpStateName names the TCP states a bytes wait reports.
func tcpStateName(s uint8) string {
	switch s {
	case tcpEstablished:
		return "ESTABLISHED"
	case tcpFinWait1:
		return "FIN_WAIT1"
	case tcpFinWait2:
		return "FIN_WAIT2"
	}
	return fmt.Sprintf("TCP state %d", s)
}

// pairTarget reads, from the application's socket, how much it had written
// to pc: nil when there is nothing to wait for.
func (p *Proxy) pairTarget(ds *diagSock, pc *pairedConn) (*pairTarget, error) {
	if pc.noSession.Load() {
		// Marked since upTo took it: nothing on it is captured. One marked
		// after this look is released by pairReached, which reads the mark
		// after the connection's side.
		return nil, nil
	}
	want, fin, d, err := appWritten(ds, pc.app, pc.proxy)
	if err == nil {
		p.paired.visible.Store(true)
	}
	switch {
	case errors.Is(err, errNoSocket):
		return &pairTarget{pc: pc, gone: true}, nil
	case err != nil:
		return nil, fmt.Errorf("%w: read the application's socket %s: %v", models.ErrProxyConnsUnsupported, pc.app, err)
	case d.info == nil:
		// A timewait socket: closed, and its FIN acknowledged, so the
		// proxy's side received everything it wrote.
		if d.state == tcpTimeWait || d.state == tcpFinWait2 {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: the application's socket %s (TCP state %d) has no tcp_info", models.ErrProxyConnsUnsupported, pc.app, d.state)
	}
	return &pairTarget{pc: pc, want: want, finCounted: fin}, nil
}

// finSent says whether a socket in state, one with tcp_info, has queued its
// FIN: its owner closed it, or shut its write side down (FIN_WAIT1, then
// FIN_WAIT2 once the FIN is acknowledged; LAST_ACK after its peer's FIN;
// CLOSING when both closed at once).
func finSent(state uint8) bool {
	return state == tcpFinWait1 || state == tcpFinWait2 || state == tcpClosing || state == tcpLastAck
}

// appWritten is how many bytes (and a FIN) the application had written to
// the socket whose ends are app and proxy: write_seq - ISN - 1, from
// tcpi_bytes_acked (snd_una - ISN: its SYN counted) and idiag_wqueue
// (write_seq - snd_una, a FIN counted), and whether that count holds its FIN
// (fin). tcpi_bytes_sent is no such count: it counts a segment before IP
// takes it, so one the qdisc or netfilter drops is counted again when TCP
// sends it again as new data, never as a retransmission.
//
// sock_diag reads idiag_wqueue apart from, and before, the tcp_info it pairs
// with (tcp_diag_get_info), so an ACK between the two would count its bytes
// twice: two lookups that agree on tcpi_bytes_acked, which tcp_get_info reads
// under the socket's lock, had no ACK between the first's tcp_info and the
// second's, which brackets the second's wqueue.
//
// Whether that wqueue counts the FIN is told the same way, by lock order, not
// by idiag_state or tcpi_state, which are read without the lock: idiag_state
// before idiag_wqueue, so a close that runs between the two counts its FIN in
// a socket read as open (the wait then takes the FIN for a byte of data: a
// loss), and tcpi_state after it, which a weakly ordered CPU may see before the
// FIN or after. A close (tcp_close, or a shutdown of the write side) sets the
// socket's state and queues its FIN (tcp_close_state, then tcp_send_fin) in
// one hold of the socket's lock. A lookup whose tcpi_state shows a FIN sent
// (finSent) saw that state set, so its byte counts waited for the lock until
// the close let go of it: the second's wqueue, read after them, counts the
// FIN. A lookup after the second whose tcpi_state shows no FIN sent saw no
// close that held the lock before the second's byte counts took it; one that
// took it after could not have queued the FIN before the second's wqueue, read
// before them. So the second lookup's wqueue counts its FIN when the first saw
// one sent, and does not when the third saw none; with neither (a close
// between them), the reads move on by one. A socket read in CLOSE is leaving
// the kernel's tables (reset, or closed at both ends): gone, as one not found,
// which also tells no FIN from a reset.
func appWritten(ds *diagSock, app, proxy netip.AddrPort) (want uint64, fin bool, d diagTCP, err error) {
	read := func() (diagTCP, error) {
		d, err := lookupWritten(ds, app, proxy)
		if err == nil && d.info != nil && d.info.State == tcpClose {
			return d, errNoSocket
		}
		return d, err
	}
	prev, err := read()
	if err != nil || prev.info == nil {
		return 0, false, prev, err
	}
	cur, err := read()
	for {
		if err != nil || cur.info == nil {
			return 0, false, cur, err
		}
		if cur.info.Bytes_acked == 0 {
			return 0, false, cur, errors.New("tcpi_bytes_acked is 0 on a connected socket: this kernel's tcp_info has no byte counts")
		}
		written := cur.info.Bytes_acked - 1 + uint64(cur.wqueue)
		bracketed := cur.info.Bytes_acked == prev.info.Bytes_acked
		if bracketed && finSent(prev.info.State) {
			return written, true, cur, nil
		}
		next, nerr := read()
		if nerr != nil || next.info == nil {
			return 0, false, next, nerr
		}
		if bracketed && !finSent(next.info.State) {
			return written, false, cur, nil
		}
		prev, cur, err = cur, next, nil
	}
}

// lookupWritten is how appWritten reads the application's socket; a test
// replaces it to give the reads a close can straddle.
var lookupWritten = (*diagSock).lookup

// pairReached says whether t needs no more waiting: the proxy's side received
// what the application had written, or it never will (a capture loss).
func (p *Proxy) pairReached(t *pairTarget) (bool, error) {
	pc := t.pc
	ps, closed := pc.side()
	if pc.noSession.Load() {
		// The proxy serves it with no session (serveWithoutSession), marked
		// after the wait took it: a wait takes its connections before their
		// handlers have all classified them. Nothing on it is captured, so
		// no wait holds it, as none would have had the mark come first; its
		// close, which resets the application's socket when the bytes on it
		// were never read, is no loss and no unknown. Read after its side:
		// the handler marks it before it closes it, so a side read closed
		// (pc.done, or atClose stored by closing) is one whose mark, if
		// any, is seen here.
		return true, nil
	}
	if ps.err != nil {
		return false, fmt.Errorf("%w: read the proxy's socket from %s: %v", models.ErrProxyConnsUnsupported, pc.app, ps.err)
	}
	if t.gone {
		return p.pairEnded(t, ps, closed)
	}
	if ps.received > t.seen {
		t.seen = ps.received
		p.paired.progress.Add(1)
	}
	switch {
	case ps.received >= t.want:
		return true, nil
	case ps.state == tcpClose:
		// A reset reached the proxy's side before the proxy closed it (a
		// close of its own is read before it begins: atClose). The
		// kernel takes a reset only at the sequence number it expects next
		// (RFC 5961), the application's snd_nxt: every byte the application
		// had sent had arrived, and what it had not sent its own abort
		// dropped, as it would have without keploy.
		return true, nil
	case closed:
		// Short of what was written, and the proxy closed it. An orphan
		// that took the application's FIN refused nothing: the shortfall
		// is that FIN, which what was written counts once the application
		// has closed (its socket in LAST_ACK, its FIN not yet acknowledged,
		// when the wait began). One short with the orphan still waiting
		// for a FIN (FIN_WAIT1/2): that FIN, or a byte the orphan will
		// refuse, is on its way (a loaded host's softirq, a held packet),
		// and the orphan will say which: it takes the FIN, or a refusal
		// destroys it. Nothing of the proxy's wakes the wait for either,
		// so it looks at the orphan again (waitPairTargets), and a change
		// of its state is the wait moving.
		// One short with the orphan gone says no more of itself: the kernel
		// destroys an orphan it has no timewait bucket for, or no room for
		// under its orphan and memory limits, without a refusal
		// (TCPTimeWaitOverflow, TCPAbortOnMemory), as it destroys one that
		// refused a byte, and one that took the FIN before its close (from
		// CLOSE_WAIT to LAST_ACK) goes as its own FIN is acknowledged. The
		// application's socket tells that last one (appFINAcknowledged).
		// One short with the orphan unreadable (sock_diag failing) says
		// nothing either. Otherwise what arrives now is answered with a
		// reset: a loss.
		// Without the FIN counted, one short is a byte of data, which an
		// orphan refuses: a loss, whatever its state.
		//
		// The close may not have reached the kernel yet: the descriptor is
		// marked closing, which is what side reads as closed, before its
		// last reference goes and the close(2) runs, and a loaded host can
		// hold a thread in that gap. The socket is then no orphan. One FIN
		// short, a socket in CLOSE_WAIT took that FIN in sequence, after
		// every byte counted as the close began, so nothing was refused;
		// one still ESTABLISHED waits for it as an orphan does. More short
		// than that, the bytes went with the close: nothing reads the
		// descriptor again, and the close tosses them.
		//
		// What was written is counted up to what the application had
		// written as the close began (noteWroteAtClose), when that is less:
		// bytes it wrote after keploy's close are no capture loss, and are
		// not waited for.
		want, finCounted := t.want, t.finCounted
		if w := pc.wroteAtClose.Load(); w != nil && w.want < want {
			want, finCounted = w.want, w.fin
		}
		if ps.received >= want {
			return true, nil
		}
		d, took, err := orphanTookFIN(pc)
		oneShort := finCounted && want-ps.received == 1
		switch {
		case took:
			return true, nil
		case err == nil && oneShort && d.state == tcpCloseWait:
			return true, nil
		case errors.Is(err, errNoSocket) && oneShort && appFINAcknowledged(pc):
			return true, nil
		case errors.Is(err, errNoSocket) && oneShort:
			return false, fmt.Errorf("%w: the proxy closed its side of a connection from %s one FIN short of what the application wrote, and its orphaned socket is gone: the kernel destroys one it has no room for, or one that refused a byte, alike", models.ErrProxyConnsUnsupported, pc.app)
		case err != nil && oneShort:
			return false, fmt.Errorf("%w: the proxy closed its side of a connection from %s one FIN short of what the application wrote, and its orphaned socket, which tells that FIN from a byte it refused, cannot be read: %v", models.ErrProxyConnsUnsupported, pc.app, err)
		case err == nil && oneShort && (d.state == tcpEstablished || d.state == tcpFinWait1 || d.state == tcpFinWait2):
			if t.orphan != d.state {
				t.orphan = d.state
				p.paired.progress.Add(1)
			}
			return false, nil
		}
		p.pairLost(pc, "bytes the application wrote to a connection keploy's proxy holds never reached the proxy: the connection was reset, or closed, first, so what they carried was neither recorded nor mocked")
		return true, nil
	}
	return false, nil
}

// orphanTookFIN reads the proxy's socket of pc, orphaned when the proxy closed
// its side: it took the application's FIN in sequence when it is in TIME_WAIT,
// CLOSING (both ends closed at once) or LAST_ACK (the FIN arrived as the proxy
// closed). An orphan answers any byte that arrives after its close with a
// reset that destroys it, and a close with bytes unread resets at once, so one
// that took the FIN refused nothing: every byte the application wrote before
// its FIN had arrived before the proxy closed.
//
// The orphan moves to the kernel's timewait table as it is acknowledged in
// FIN_WAIT1 or takes the FIN, and an exact lookup made as it moves can miss
// it, or find it closed, while it is there: it is gone only once a read
// ordered against that move agrees (lookupOrdered).
func orphanTookFIN(pc *pairedConn) (diagTCP, bool, error) {
	d, err := lookupOrphan(pc.proxy, pc.app)
	if err != nil {
		return d, false, err
	}
	return d, d.state == tcpTimeWait || d.state == tcpClosing || d.state == tcpLastAck, nil
}

// appFINAcknowledged says whether the application's socket of pc shows its FIN
// acknowledged: TIME_WAIT, which it reaches only on an ACK of that FIN. The
// proxy's socket acknowledges a FIN only in sequence, after every byte before
// it, and a socket that refused a byte answers with a reset, which destroys the
// application's, so nothing was refused. It reads the socket as orphanTookFIN
// does: a socket moving to the timewait table is no miss.
func appFINAcknowledged(pc *pairedConn) bool {
	d, err := lookupOrphan(pc.app, pc.proxy)
	return err == nil && d.state == tcpTimeWait
}

// lookupOrphan is how orphanTookFIN and appFINAcknowledged read the socket
// (lookupTCPOrdered); a test replaces it to make the read fail.
var lookupOrphan = lookupTCPOrdered

// side is the proxy's side of pc: read from its socket while the proxy holds
// it, or as it was when the proxy closed it (closed).
func (pc *pairedConn) side() (proxySide, bool) {
	select {
	case <-pc.done:
		if ps := pc.atClose.Load(); ps != nil {
			return *ps, true
		}
		return proxySide{err: errors.New("the proxy closed its socket before it could be read")}, true
	default:
	}
	ps := readProxySide(pc.conn)
	if errors.Is(ps.err, net.ErrClosed) {
		// Closed since: the close read it first.
		if at := pc.atClose.Load(); at != nil {
			return *at, true
		}
	}
	return ps, false
}

// readProxySide reads the proxy's side of a connection from its own socket,
// in whatever state it is (a reset one too, which sock_diag no longer finds).
func readProxySide(conn *net.TCPConn) proxySide {
	rc, err := conn.SyscallConn()
	if err != nil {
		return proxySide{err: err}
	}
	var ti *unix.TCPInfo
	var gerr error
	if err := rc.Control(func(fd uintptr) { ti, gerr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO) }); err != nil {
		return proxySide{err: err}
	}
	if gerr != nil {
		return proxySide{err: gerr}
	}
	return proxySide{received: ti.Bytes_received, state: ti.State}
}

// pairEnded is pairReached for a connection whose application socket was gone
// when the wait began: the proxy's side waits for its FIN (everything it
// wrote came before) or its reset.
func (p *Proxy) pairEnded(t *pairTarget, ps proxySide, closed bool) (bool, error) {
	pc := t.pc
	switch {
	case ps.state == tcpCloseWait || ps.state == tcpLastAck || ps.state == tcpClosing || ps.state == tcpTimeWait:
		return true, nil
	case ps.state == tcpClose:
		// Its reset: every byte it had sent had arrived (pairReached).
		return true, nil
	case closed:
		// The proxy closed its side while the application's was open (what
		// it read as it closed holds no FIN or reset from it), and the
		// application's socket has gone since: either its FIN, after every
		// byte it wrote, reached the proxy's orphaned socket, or a reset
		// ended it: its own, or the orphan's refusing bytes that arrived
		// after the close, bytes written before it and never captured. The
		// orphan tells the FIN (orphanTookFIN); nothing tells the resets
		// apart. Unless the proxy's side had received, as it closed,
		// everything the application had written as the close began
		// (noteWroteAtClose): then whatever reset ended it refused only
		// bytes written after keploy's close, no capture loss (a TLS
		// client's close_notify, or its own reset as it closes with
		// keploy's close_notify unread).
		if w := pc.wroteAtClose.Load(); w != nil && ps.received >= w.want {
			return true, nil
		}
		if _, took, _ := orphanTookFIN(pc); took {
			return true, nil
		}
		return false, fmt.Errorf("%w: the application's end of a connection to the proxy, %s, went after the proxy closed its side, and whether bytes it wrote before that close were refused cannot be told", models.ErrProxyConnsUnsupported, pc.app)
	case p.paired.visible.Load():
		// The application's sockets are seen from here, so this one is
		// gone with its FIN or reset on its way (a held packet, a loaded
		// host): its arrival ends the handler's read, and the wait looks
		// again.
		return false, nil
	default:
		return false, fmt.Errorf("%w: the application's end of a connection to the proxy, %s, cannot be seen from the proxy's network namespace while the connection is alive, and no application socket has been", models.ErrProxyConnsUnsupported, pc.app)
	}
}

// proveAppVisible notes, until it has once, that the application's sockets
// are seen from the proxy's network namespace: the accepted connection's
// application end, just paired, is found by sock_diag.
func (p *Proxy) proveAppVisible(conn *net.TCPConn) {
	if p.paired.visible.Load() {
		return
	}
	app, ok1 := addrPortOf(conn.RemoteAddr())
	pr, ok2 := addrPortOf(conn.LocalAddr())
	if !ok1 || !ok2 {
		return
	}
	if _, err := lookupTCP(app, pr); err == nil {
		p.paired.visible.Store(true)
	}
}

// pairClosing is a paired connection's beforeClose: its side as the proxy
// closes it (closing), and a capture loss when the close tosses bytes the
// application wrote that the proxy never read. Its side counts those
// received, so no wait would find them missing. Bytes held that cannot be
// counted leave every wait unknown, and are a loss of this connection's
// whether or not a wait holds it.
//
// Over TLS (tc's layer) what is unread is read through the layer first: a
// TLS client's goodbye ends with a close_notify -- go-sql-driver/mysql's
// Close (COM_QUIT, then close_notify, then FIN); pgx's Terminate and gRPC's
// GOAWAY once the handler has read them -- which a handler done with the
// connection never reads, and which carries no call. The socket's count
// cannot tell it from data; only plaintext is a loss (tlsUnreadData), so a
// Terminate or GOAWAY left unread still is one. Bytes the application had
// written before the close that are still on their way to the socket
// (noteWroteAtClose) are read through the layer the same way: a close_notify
// sent before the close is no loss either, and the read waits for it.
//
// keploy's own stop (ctx, the proxy's connections' context, done) closes
// connections whatever they hold: nothing the application wrote is charged
// then, as nothing more of the run is judged. Nor is anything on a
// connection the proxy serves with no session (pairedConn.noSession): it
// captures nothing on it by design -- closed unanswered, as an agent not
// started in record or test mode closes it, or passed through -- and no test
// is open on it.
func (p *Proxy) pairClosing(ctx context.Context, conn net.Conn, tc *trackedConn) {
	pc, unread, err := p.paired.closing(conn, func(pc *pairedConn) {
		if ctx.Err() == nil && !pc.noSession.Load() {
			noteWroteAtClose(pc)
		}
	})
	tlsLayer := tc != nil && tc.tlsLayer.Load() != nil
	switch {
	case pc == nil, ctx.Err() != nil, pc.noSession.Load():
	case err != nil:
		p.paired.fail(fmt.Errorf("%w: read what the proxy's socket from %s held unread as the proxy closed it: %v", models.ErrProxyConnsUnsupported, pc.app, err))
		p.pairLost(pc, "keploy cannot tell whether its proxy closed a connection with bytes the application wrote still unread: from now on it cannot vouch that every call of a test is in its hands", zap.Error(err))
	case unread == 0 && (!tlsLayer || !onItsWayAtClose(pc)):
	case !tlsLayer:
		p.pairLost(pc, unreadLost)
	default:
		data, err := tlsUnreadData(ctx, pc.conn, tc.tlsLayer.Load())
		// Its side as the read left it: the application's FIN, if the read
		// waited for it, is in, which the wait counts (pairReached).
		ps := readProxySide(pc.conn)
		pc.atClose.Store(&ps)
		switch {
		case ctx.Err() != nil:
		case err != nil:
			p.pairLost(pc, "keploy's proxy closed a connection with bytes still unread that its TLS layer could not read: whether calls of the application's were among them cannot be told", zap.Error(err))
		case data:
			p.pairLost(pc, unreadLost)
		}
	}
}

// noteWroteAtClose reads, as the proxy begins closing pc, what the application
// had written to it (pairedConn.wroteAtClose). It is read before the proxy's
// side, so a byte it counts that the side has not received was written before
// the close and is still on its way. What the application writes after it is
// no capture loss: keploy's close, not a capture keploy missed, ended the
// connection under it, as a server's close would (the application writing on
// meets the same reset). A TLS client's close_notify, sent once it has read
// the answer keploy closed after (a 502 with Connection: close), is such a
// write. A socket that cannot be read leaves it unset, and the wait judges the
// connection by what was written when it began.
func noteWroteAtClose(pc *pairedConn) {
	ds, err := openDiag()
	if err != nil {
		return
	}
	defer ds.close()
	if want, fin, _, err := appWritten(ds, pc.app, pc.proxy); err == nil {
		pc.wroteAtClose.Store(&appWrote{want: want, fin: fin})
	}
}

// onItsWayAtClose says whether, as the proxy began closing pc, bytes the
// application had written to it were still on their way: written, and not yet
// received by the proxy's side.
func onItsWayAtClose(pc *pairedConn) bool {
	w, ps := pc.wroteAtClose.Load(), pc.atClose.Load()
	return w != nil && ps != nil && ps.err == nil && w.want > ps.received
}

// tlsUnreadData reads, through layer, the TLS layer the proxy serves conn
// over, what conn's socket still holds as the proxy closes it, and reports
// whether any of it is data (plaintext). It reads until the first byte of data
// (data), the close_notify or the application's FIN (none), or what the layer
// cannot read (an error: the bytes cannot be told to be no data). It never
// reads past the first byte of data: nothing more is needed to charge the
// loss.
//
// Its write side is shut down first: the application, told the proxy is done
// (its close_notify, if the layer's close sent one, then its FIN), ends its
// own side, so a read that waits -- a record not all in yet -- ends at the
// application's FIN, not on a clock. A read deadline its handler left (a
// poll's, or a nudge's in the past) is cleared, and so is one another reader
// of the connection sets while the read waits: either would end the read at
// once, or on a clock, as a loss. If that FIN never comes, the read holds the
// close until keploy's stop (ctx) ends it for good by shutting the read side
// down, which no deadline another reader of the connection sets or clears
// undoes; the stop charges nothing.
func tlsUnreadData(ctx context.Context, conn *net.TCPConn, layer *tls.Conn) (data bool, err error) {
	_ = conn.CloseWrite()
	_ = conn.SetReadDeadline(time.Time{})
	defer context.AfterFunc(ctx, func() { _ = conn.CloseRead() })()
	var b [1]byte
	var n int
	for {
		n, err = layer.Read(b[:])
		// A deadline another reader of the connection set since (a
		// parser's poll, re-armed before its next read, which waits behind
		// this one) is no end of this read either: only the application's
		// end or keploy's stop is. The layer keeps a record a timeout cut
		// short.
		if n == 0 && errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil {
			_ = conn.SetReadDeadline(time.Time{})
			continue
		}
		break
	}
	switch {
	case n > 0:
		return true, nil
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// The close_notify, or the application's FIN, at a record's end or
		// in one's middle: the application ended there.
		return false, nil
	case err == nil:
		return false, errors.New("the TLS layer read nothing and said nothing")
	}
	return false, err
}

// readUnread is how many bytes conn's socket holds unread (SIOCINQ): received
// in order, never read.
func readUnread(conn *net.TCPConn) (int, error) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var ierr error
	if err := rc.Control(func(fd uintptr) { n, ierr = unix.IoctlGetInt(int(fd), unix.SIOCINQ) }); err != nil {
		return 0, err
	}
	return n, ierr
}

// unreadLost is pairLost's message for a close that tossed bytes unread.
const unreadLost = "keploy's proxy closed a connection with bytes the application wrote still unread: what they carried was neither recorded nor mocked"

// pairLost charges, once per connection, bytes the application wrote to pc
// that the proxy never captured, or that it cannot tell it did, saying which
// (msg).
func (p *Proxy) pairLost(pc *pairedConn, msg string, fields ...zap.Field) {
	if pc.noSession.Load() || !pc.lost.CompareAndSwap(false, true) {
		return
	}
	p.losses.Add(models.CaptureLossAppBytesLost, "from "+pc.app.String())
	p.logger.Warn(msg, append([]zap.Field{
		zap.String("from", pc.app.String()),
		zap.String("next_step", "keploy cannot vouch for the calls of the test that was open (under the proven switch, that test is not verified); check the agent's log for why the connection ended"),
	}, fields...)...)
}

// checkPairedBytes proves, once, on the canary's connection, that this
// kernel's sock_diag gives the counts the wait reads (inet_diag and tcp_diag,
// which the kernel loads on first use, and tcp_info's byte counts): without
// them no wait could be answered, and every one is unknown from the start.
func (p *Proxy) checkPairedBytes(app, proxy net.Conn) {
	a, ok1 := addrPortOf(app.LocalAddr())
	pr, ok2 := addrPortOf(proxy.LocalAddr())
	if !ok1 || !ok2 {
		p.pairedBytesUnsupported(errors.New("the canary's addresses are not TCP"))
		return
	}
	d, err := lookupTCP(a, pr)
	if err != nil {
		p.pairedBytesUnsupported(fmt.Errorf("sock_diag cannot find the canary's own socket %s -> %s: %w", a, pr, err))
		return
	}
	// Its SYN alone is acknowledged once it connects (tcpi_bytes_acked 1):
	// a kernel whose tcp_info stops short of the byte counts reads 0.
	if d.state != tcpEstablished || d.info == nil || d.info.Bytes_acked == 0 {
		p.pairedBytesUnsupported(fmt.Errorf("sock_diag gives the canary's own socket no byte counts (TCP state %d, tcp_info %v)", d.state, d.info != nil))
	}
}

// PairedBytesErr is why the bytes on the proxy's paired connections cannot be
// counted, nil while they can: found at the proxy's start (its canary,
// checkPairedBytes), or since, by a failure that makes every later wait on a
// paired connection unknown (agent.PairedBytesChecker).
func (p *Proxy) PairedBytesErr() error {
	p.paired.mu.Lock()
	defer p.paired.mu.Unlock()
	return p.paired.err
}

func (p *Proxy) pairedBytesUnsupported(err error) {
	p.paired.fail(fmt.Errorf("%w: %v", models.ErrProxyConnsUnsupported, err))
	p.logger.Warn("keploy cannot tell whether the bytes the application wrote to the connections its proxy holds have reached the proxy: while one is open, it cannot vouch that every call of a test is in its hands",
		zap.Error(err),
		zap.String("next_step", "the kernel needs sock_diag for TCP (inet_diag and tcp_diag) in the agent's network namespace"))
}
