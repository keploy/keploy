//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
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
	// sent when the wait began (FIN_WAIT1, CLOSING, LAST_ACK): the last of
	// want may be that FIN. Otherwise every byte of want is data.
	finCounted bool
	// gone: the application's socket was already gone (closed, or reset)
	// when the wait began. Its FIN, after everything it wrote, or a reset,
	// is what the proxy's side waits for.
	gone bool
	// seen is the most the proxy's side was seen to have received: more is
	// progress, for the watchdog.
	seen uint64
	// orphan is, while the wait waits for the proxy's orphaned socket of
	// a connection it closed one FIN short to take that FIN (pairReached),
	// the orphan's state at the last look (FIN_WAIT1, FIN_WAIT2); 0 when
	// it does not.
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
		if t.orphan != 0 {
			waited = append(waited, fmt.Sprintf("%s (its orphan in %s)", t.pc.app, tcpStateName(t.orphan)))
		}
	}
	what := "the connection from " + waited[0]
	if len(waited) > 1 {
		what = "the connections from " + strings.Join(waited, ", ")
	}
	return fmt.Errorf("%w: keploy waited for the application's FIN to reach the proxy's orphaned socket of %s, which the proxy closed one FIN short of what the application wrote, and while keploy could run for %v nothing it waited on moved: the orphan took neither that FIN nor a byte it would refuse", models.ErrProxyConnsUnsupported, what, orphanFINBudget)
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
	want, d, err := appWritten(ds, pc.app, pc.proxy)
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
	return &pairTarget{pc: pc, want: want, finCounted: d.state == tcpFinWait1 || d.state == tcpClosing || d.state == tcpLastAck}, nil
}

// appWritten is how many bytes (and a FIN) the application had written to
// the socket whose ends are app and proxy: write_seq - ISN - 1, from
// tcpi_bytes_acked (snd_una - ISN: its SYN counted) and idiag_wqueue
// (write_seq - snd_una, a FIN counted). tcpi_bytes_sent is no such count: it
// counts a segment before IP takes it, so one the qdisc or netfilter drops
// is counted again when TCP sends it again as new data, never as a
// retransmission. sock_diag reads idiag_wqueue apart from, and before, the
// tcp_info it pairs with, so an ACK between the two would count its bytes
// twice: two lookups that agree on tcpi_bytes_acked had no ACK between the
// first's tcp_info and the second's, which brackets the second's wqueue.
func appWritten(ds *diagSock, app, proxy netip.AddrPort) (uint64, diagTCP, error) {
	prev, err := ds.lookup(app, proxy)
	for err == nil && prev.info != nil {
		cur, err := ds.lookup(app, proxy)
		if err != nil || cur.info == nil {
			return 0, cur, err
		}
		if cur.info.Bytes_acked == prev.info.Bytes_acked {
			if cur.info.Bytes_acked == 0 {
				return 0, cur, errors.New("tcpi_bytes_acked is 0 on a connected socket: this kernel's tcp_info has no byte counts")
			}
			return cur.info.Bytes_acked - 1 + uint64(cur.wqueue), cur, nil
		}
		prev = cur
	}
	return 0, prev, err
}

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
		// One short with the orphan gone says no more: the kernel destroys
		// an orphan it has no timewait bucket for, or no room for under its
		// orphan and memory limits, without a refusal (TCPTimeWaitOverflow,
		// TCPAbortOnMemory), as it destroys one that refused a byte.
		// One short with the orphan unreadable (sock_diag failing) says
		// nothing either. Otherwise what arrives now is answered with a
		// reset: a loss.
		// Without the FIN counted, one short is a byte of data, which an
		// orphan refuses: a loss, whatever its state.
		d, took, err := orphanTookFIN(pc)
		oneShort := t.finCounted && t.want-ps.received == 1
		switch {
		case took:
			return true, nil
		case errors.Is(err, errNoSocket) && oneShort:
			return false, fmt.Errorf("%w: the proxy closed its side of a connection from %s one FIN short of what the application wrote, and its orphaned socket is gone: the kernel destroys one it has no room for, or one that refused a byte, alike", models.ErrProxyConnsUnsupported, pc.app)
		case err != nil && oneShort:
			return false, fmt.Errorf("%w: the proxy closed its side of a connection from %s one FIN short of what the application wrote, and its orphaned socket, which tells that FIN from a byte it refused, cannot be read: %v", models.ErrProxyConnsUnsupported, pc.app, err)
		case err == nil && oneShort && (d.state == tcpFinWait1 || d.state == tcpFinWait2):
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
func orphanTookFIN(pc *pairedConn) (diagTCP, bool, error) {
	d, err := lookupOrphan(pc.proxy, pc.app)
	if err != nil {
		return d, false, err
	}
	return d, d.state == tcpTimeWait || d.state == tcpClosing || d.state == tcpLastAck, nil
}

// lookupOrphan is how orphanTookFIN reads the orphan (lookupTCP); a test
// replaces it to make the read fail.
var lookupOrphan = lookupTCP

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
		// apart.
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
