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
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
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
	// sent is how much of want the application's socket had sent when the
	// wait began (want less tcpi_notsent_bytes, and no less than what the
	// proxy's side had acknowledged): bytes sent that never arrive were lost
	// on their way, whatever ended the connection; bytes not sent yet that a
	// reset leaves missing cannot be told apart (pairReset).
	sent uint64
	// gone: the application's socket was already gone (closed, or reset)
	// when the wait began. Its FIN, after everything it wrote, or a reset,
	// is what the proxy's side waits for.
	gone bool
	// seen is the most the proxy's side was seen to have received: more is
	// progress, for the watchdog.
	seen uint64
}

// WaitPairedBytes returns once the proxy's side of every connection it holds
// that the kernel paired with an application connection numbered 1..n has
// received every byte the application had written to it when the wait began,
// so that a call written to a connection opened long before (a pooled one) is
// in the proxy's hands before a test's end reads what the test did. Writes
// made after it began cannot hold it back. A connection the proxy serves with
// no session (pairedConn.noSession) is not waited on: nothing on it is
// captured. A connection the proxy closed short of what was written is
// judged by what came to its socket after the close, which the proxy keeps
// open until the application's FIN, its reset or a byte of data
// (pairAfterClose): only the FIN is no loss; a reset, the application's or
// the handler's own, is judged by what the application had sent when the wait
// began (pairReset): fewer bytes in than that is a capture loss
// (models.CaptureLossAppBytesLost), only its FIN missing is none, and bytes it
// had not sent yet missing are unknown (dropped unsent, or sent later and
// lost), as is any reset once its socket is gone; data is a capture loss, and
// does not hold it back; so is a close with bytes unread, whenever a wait
// holds it or not (pairClosing). A reset that reached the proxy's side
// before the proxy closed it is judged the same way (pairReached); one whose
// application socket was gone when the wait began is still taken as no loss
// (pairEnded), the older rule.
//
// It has no deadline of its own. It looks again at every read the proxy
// begins or ends on a connection it keeps (inflight.go), at every close of a
// paired one, and at the end of every read of a socket kept open past its
// close: bytes that arrive while the proxy's handler waits on the connection
// end that read, and bytes that arrive while the handler is busy are taken by
// its next read, so no arrival goes unseen. (An epoll watching the socket
// from beside the handler is no such event: in tests it missed arrivals the
// handler's own reads took in.)
//
// It ends with ctx's error, or, for a wait that holds a paired connection,
// with an error wrapping models.ErrProxyConnsUnsupported ("unknown") when it
// cannot tell: this kernel's sock_diag gives no byte counts, the listener has
// closed, or the application's socket cannot be seen while its connection is
// alive at the proxy's side and no application socket has ever been seen (it
// is in another network namespace than the proxy's, which keploy never sets
// up), or what came after the proxy closed one cannot be told
// (pairAfterClose). Once one has been seen, an application socket that is
// gone is one whose FIN or reset is on its way: the wait waits for it (it
// ends the handler's read, or the kept socket's).
//
// mode is the agent's. A replay's wait ends at the first connection it
// cannot tell, or read as it begins. A record's (models.MODE_RECORD) goes on
// waiting for every other connection, and once each is reached answers all
// it could not tell, sorted (readPairTargets, waitPairTargets).
func (p *Proxy) WaitPairedBytes(ctx context.Context, n uint64, mode models.Mode) error {
	record := mode == models.MODE_RECORD
	pending, unknown, err := p.readPairTargets(n, record)
	if err != nil {
		return err
	}
	if err := p.waitPairTargets(ctx, pending, record); err != nil {
		if !record || ctx.Err() != nil {
			return err
		}
		unknown = append(unknown, err)
	}
	return errors.Join(unknown...)
}

// pairTargets reads, for every paired connection the proxy holds whose
// application connection is numbered 1..n, how much the application had
// written to it: what waitPairTargets waits for. It ends at the first
// connection it cannot read (readPairTargets, a replay's).
func (p *Proxy) pairTargets(n uint64) ([]*pairTarget, error) {
	pending, _, err := p.readPairTargets(n, false)
	return pending, err
}

// readPairTargets is pairTargets. A replay's read ends at the first
// connection it cannot read (err). A record's (record) goes on to the others,
// and answers what it could not read of each (unknown, sorted): a
// connection whose application socket a lookup could not read is one the
// record could not tell, and its others are still waited for. What leaves
// no connection readable (the listener's failure, sock_diag that cannot be
// opened) is err in both.
func (p *Proxy) readPairTargets(n uint64, record bool) (pending []*pairTarget, unknown []error, err error) {
	pcs, err := p.paired.upTo(n)
	if err != nil || len(pcs) == 0 {
		return nil, nil, err
	}
	ds, err := openDiag()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", models.ErrProxyConnsUnsupported, err)
	}
	defer ds.close()
	for _, pc := range pcs {
		t, err := p.pairTarget(ds, pc)
		switch {
		case err != nil && record:
			unknown = append(unknown, err)
		case err != nil:
			return nil, nil, err
		case t != nil:
			pending = append(pending, t)
		}
	}
	return pending, sortedErrs(unknown), nil
}

// sortedErrs is errs ordered by their text: the connections they come from
// are read in no set order, and what keploy says of them is the same each
// time.
func sortedErrs(errs []error) []error {
	slices.SortStableFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
}

// waitPairTargets waits until each of pending is reached (pairReached). A
// replay's wait ends at the first connection it cannot tell. A record's
// (record) goes on waiting for the others, and answers what it could not tell
// of each once every other one is reached: each call that reaches the proxy
// before the test's end is one the end then sees, not one that lands in the
// next test, and the record keeps what it could not tell as the test's
// evidence, which the pass that judges each test decides.
func (p *Proxy) waitPairTargets(ctx context.Context, pending []*pairTarget, record bool) error {
	if len(pending) == 0 {
		return nil
	}
	var unknown []error
	for {
		// Taken before looking, so a read or close after the look still
		// wakes.
		reads := p.inflight.Changed()
		closes := p.paired.changes()
		if err := p.paired.failure(); err != nil {
			return err
		}
		next := pending[:0]
		for _, t := range pending {
			reached, err := p.pairReached(t)
			switch {
			case err != nil && record:
				// Resolved, as far as the wait goes: it waits for it no
				// more.
				unknown = append(unknown, err)
				p.paired.progress.Add(1)
			case err != nil:
				return err
			case reached:
				p.paired.progress.Add(1)
			default:
				next = append(next, t)
			}
		}
		pending = next
		if len(pending) == 0 {
			return errors.Join(sortedErrs(unknown)...)
		}
		select {
		case <-reads:
		case <-closes:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	return &pairTarget{pc: pc, want: want, sent: appSent(want, d), finCounted: d.state == tcpFinWait1 || d.state == tcpClosing || d.state == tcpLastAck}, nil
}

// appSent is how much of want, what the application's socket d had written,
// it had sent: want less tcpi_notsent_bytes (write_seq - snd_nxt), read with
// the tcp_info after want's idiag_wqueue, so a write between the two only
// makes it smaller; never less than what the proxy's side acknowledged
// (tcpi_bytes_acked, its SYN counted), which was sent.
func appSent(want uint64, d diagTCP) uint64 {
	acked := d.info.Bytes_acked - 1
	if ns := uint64(d.info.Notsent_bytes); ns <= want && want-ns > acked {
		return want - ns
	}
	return min(acked, want)
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
		// close of its own is read before it begins: atClose): what the
		// application had sent when the wait began decides, as for any
		// reset (pairReset).
		return p.pairReset(t, ps.received)
	case closed:
		// Short of what was written, and the proxy closed it: what came
		// after the close decides (pairAfterClose).
		return p.pairAfterClose(t, ps)
	}
	return false, nil
}

// pairAfterClose judges t, a connection the proxy closed short of what the
// application had written, by what came to the proxy's socket after the close
// began: the proxy keeps that socket open past the close, its write side shut
// down, until the application's FIN, its reset, or a byte of data arrives
// (keepAfterClose), and what ends that read wakes the wait. at is the side as
// the close began: what the proxy had read, the most it can have captured.
//
//   - Only the FIN: everything the application wrote before it had been
//     read (a FIN is taken only in sequence). No loss.
//   - Only a reset: what the application had sent decides (pairReset).
//   - A byte of data: it came after the proxy was done reading, and nothing
//     captured it. A loss. Over TLS it may be the client's close_notify
//     (go-sql-driver/mysql's Close, Go's tls.Conn.Close), which no raw read
//     tells from a call: unknown.
//   - Bytes that came as the close began and that the closing handler read
//     itself (the socket's count past at, before the FIN or reset, with
//     none left for the read): whether it captured them cannot be told.
//
// A close that reset the connection with bytes unread (charged then) took
// nothing after it: a loss. The handler's own reset (SO_LINGER 0) with
// nothing unread is judged as any reset (pairReset). One whose socket could
// not be kept open, or whose read keploy's stop or a failure ended, cannot
// tell.
func (p *Proxy) pairAfterClose(t *pairTarget, at proxySide) (bool, error) {
	pc := t.pc
	if pc.resetAtClose.Load() {
		p.pairLost(pc, "bytes the application wrote to a connection keploy's proxy reset as it closed it never reached the proxy, so what they carried was neither recorded nor mocked")
		return true, nil
	}
	if pc.lingerReset.Load() {
		// The handler's own reset, with nothing unread: a reset like any
		// other, what the application had sent decides.
		if t.gone {
			return false, goneReset(pc)
		}
		return p.pairReset(t, at.received)
	}
	if !pc.kept.Load() {
		why := ""
		if e := pc.keepErr.Load(); e != nil {
			why = ": " + *e
		}
		return false, fmt.Errorf("%w: the proxy closed its side of a connection from %s short of what the application wrote, and could not keep its socket open to see what came after%s", models.ErrProxyConnsUnsupported, pc.app, why)
	}
	end := pc.end.Load()
	switch {
	case end == nil:
		return false, nil
	case end.kind == afterStop:
		return false, fmt.Errorf("%w: keploy stopped before what came to a connection from %s after the proxy closed it could be read", models.ErrProxyConnsUnsupported, pc.app)
	case end.kind == afterFailed:
		return false, fmt.Errorf("%w: read what came to a connection from %s after the proxy closed it: %v", models.ErrProxyConnsUnsupported, pc.app, end.err)
	}
	read := end.side.received // what had arrived as the read ended
	if end.kind == afterFIN && !finIn(at.state) {
		read-- // the FIN itself, which came after the close began
	}
	if read < at.received {
		return false, fmt.Errorf("%w: the proxy's socket of a connection from %s counted %d bytes received as its kept read ended, fewer than the %d it had as its close began", models.ErrProxyConnsUnsupported, pc.app, read, at.received)
	}
	switch end.kind {
	case afterFIN, afterReset:
		if read != at.received {
			return false, fmt.Errorf("%w: %d bytes the application wrote to a connection from %s reached the proxy's socket as the proxy closed it, and its handler read them as it closed: whether it captured them cannot be told", models.ErrProxyConnsUnsupported, read-at.received, pc.app)
		}
		if end.kind == afterFIN {
			return true, nil
		}
		if t.gone {
			return false, goneReset(pc)
		}
		return p.pairReset(t, read)
	case afterData:
		if pc.tls.Load() {
			return false, fmt.Errorf("%w: bytes reached the proxy's TLS connection from %s after the proxy closed it: a client's close_notify, or a call, which keploy cannot tell apart without its TLS layer", models.ErrProxyConnsUnsupported, pc.app)
		}
		p.pairLost(pc, "bytes the application wrote to a connection reached keploy's proxy after the proxy had closed it, so what they carried was neither recorded nor mocked")
		return true, nil
	}
	return false, fmt.Errorf("%w: what came to a connection from %s after the proxy closed it is of no kind keploy knows (%d)", models.ErrProxyConnsUnsupported, pc.app, end.kind)
}

// pairReset judges t, a connection a reset ended with received bytes in at
// the proxy's side, short of t.want. A reset the kernel takes says nothing of
// bytes still on their way: one past them is answered with a challenge ACK
// (RFC 5961), which the application's kernel, its socket gone, answers with a
// reset at exactly the sequence number the proxy's side expects; and one at
// the right edge of a SACKed hole is taken too. What the application had sent
// when the wait began (t.sent) decides instead:
//
//   - a loss already charged on the connection: settled on it;
//   - only its FIN missing: no loss (a FIN carries nothing);
//   - fewer in than it had sent: bytes lost on their way. A loss;
//   - all it had sent in: the rest was unsent when the wait began, and either
//     its abort dropped it unsent, as it would have without keploy, or it was
//     sent after and lost, which cannot be told: unknown.
func (p *Proxy) pairReset(t *pairTarget, received uint64) (bool, error) {
	switch {
	case received >= t.want:
		return true, nil
	case t.pc.lost.Load():
		// A loss already charged on the connection (its close tossed bytes
		// unread), and so counted (pairLost), says it: nothing more to wait
		// for.
		return true, nil
	case t.finCounted && t.want-received == 1:
		return true, nil
	case received < t.sent:
		p.pairLost(t.pc, "bytes the application had sent to keploy's proxy never reached it before the connection was reset, so what they carried was neither recorded nor mocked")
		return true, nil
	}
	return false, fmt.Errorf("%w: a connection from %s was reset with %d bytes the application had written, and not yet sent when the wait began, not in: whether its abort dropped them unsent, or they were sent and lost, cannot be told", models.ErrProxyConnsUnsupported, t.pc.app, t.want-received)
}

// goneReset is the answer for a connection a reset ended whose application
// socket was gone when the wait began: what it had sent cannot be read, and a
// reset says nothing of bytes still on their way (pairReset).
func goneReset(pc *pairedConn) error {
	return fmt.Errorf("%w: a connection from %s was reset, and the application's socket was gone when the wait began: whether bytes it had sent were still on their way cannot be told", models.ErrProxyConnsUnsupported, pc.app)
}

// finIn says whether a socket in state has taken its peer's FIN.
func finIn(state uint8) bool {
	return state == tcpCloseWait || state == tcpLastAck || state == tcpClosing || state == tcpTimeWait
}

// keepAfterClose keeps pc's socket open past the proxy's close (a duplicate of
// its descriptor, which the close does not end) with its write side shut down
// at once, so the application sees the proxy's FIN as the close would send it,
// and reads, beside the handler, what the application sends after
// (readAfterClose). The kernel would orphan the socket otherwise: an orphan
// answers bytes with a reset that destroys it, and one that took the FIN is
// destroyed in turn by a reset answering its own stray ACK once the
// application's socket is gone (tcp_rfc1337 off, TIME_WAIT assassination), so
// what it took cannot be read after. Kept, the socket says it at the read's
// end, and nothing destroys it until the proxy closes it. The read ends at the
// application's FIN, its reset, a byte of data, or keploy's stop (ctx), never
// a clock: a connection the application keeps open after the proxy closed it
// keeps the proxy's socket too, as long as the application's (a socket the
// proxy keeps is no orphan, which the kernel's FIN_WAIT2 timer ends), and it
// is held, and each wait reads it, all that while. An abort whose reset was
// lost is drawn again by the kernel's own retransmission or keepalive probe,
// whose answer ends the read.
//
// A handler that resets the connection as it closes it (SO_LINGER 0: a
// refused mock.real destination, resetOnClose) gets its reset: nothing is
// kept, and nothing after the close is taken (lingerReset).
func (p *Proxy) keepAfterClose(ctx context.Context, pc *pairedConn) {
	failed := func(err error) {
		msg := err.Error()
		pc.keepErr.Store(&msg)
		p.logger.Debug("keploy's proxy cannot keep a closed connection's socket open", zap.String("from", pc.app.String()), zap.Error(err))
	}
	rc, err := pc.conn.SyscallConn()
	if err != nil {
		failed(err)
		return
	}
	dup, derr, reset := -1, error(nil), false
	if err := rc.Control(func(fd uintptr) {
		if lg, lerr := unix.GetsockoptLinger(int(fd), unix.SOL_SOCKET, unix.SO_LINGER); lerr == nil && lg.Onoff != 0 && lg.Linger == 0 {
			reset = true
			return
		}
		if dup, derr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0); derr == nil {
			derr = unix.Shutdown(int(fd), unix.SHUT_WR)
		}
	}); err != nil {
		derr = err
	}
	if reset {
		pc.lingerReset.Store(true)
		return
	}
	if derr != nil {
		if dup >= 0 {
			_ = unix.Close(dup)
		}
		failed(derr)
		return
	}
	f := os.NewFile(uintptr(dup), "keploy-closed-conn")
	c, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		failed(err)
		return
	}
	tc, ok := c.(*net.TCPConn)
	if !ok {
		_ = c.Close()
		failed(errors.New("the kept socket is not TCP"))
		return
	}
	pc.kept.Store(true)
	go p.readAfterClose(ctx, pc, tc)
}

// readAfterClose reads the socket keepAfterClose kept open until the first of
// the application's FIN, its reset, a byte of data, or keploy's stop, notes
// what came and the socket's count as it came (pairedConns.ended), and closes
// it: a byte of data with a reset, as the orphan the kernel would have made
// refuses one.
func (p *Proxy) readAfterClose(ctx context.Context, pc *pairedConn, c *net.TCPConn) {
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	var b [1]byte
	n, err := c.Read(b[:])
	stopped := !stop()
	end := &afterClose{side: readProxySide(c)}
	switch {
	case n > 0:
		end.kind = afterData
		_ = c.SetLinger(0)
	case stopped || ctx.Err() != nil:
		end.kind = afterStop
	case errors.Is(err, io.EOF):
		end.kind = afterFIN
	case errors.Is(err, unix.ECONNRESET):
		end.kind = afterReset
	default:
		end.kind, end.err = afterFailed, err
	}
	if end.side.err != nil && end.kind != afterStop {
		end.kind, end.err = afterFailed, end.side.err
	}
	_ = c.Close()
	p.paired.ended(pc, end)
}

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
	case finIn(ps.state):
		return true, nil
	case ps.state == tcpClose:
		// Its reset: taken as no loss, though with its socket gone what it
		// had sent cannot be read, and a byte lost on its way just before
		// the reset (which a challenge ACK draws) is missed: a known false
		// green, kept on purpose for now, unlike pairReached's, which reads
		// what the application had sent (pairReset).
		return true, nil
	case closed:
		// The proxy closed its side while the application's was open (what
		// it read as it closed holds no FIN or reset from it), and the
		// application's socket has gone since: its FIN or its reset, after
		// whatever it wrote, came to the proxy's socket after the close,
		// which tells which (pairAfterClose). What it wrote is not known
		// (its socket is gone): only its FIN, after all it wrote, vouches.
		return p.pairAfterClose(t, ps)
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
// Terminate or GOAWAY left unread still is one.
//
// keploy's own stop (ctx, the proxy's connections' context, done) closes
// connections whatever they hold: nothing the application wrote is charged
// then, as nothing more of the run is judged. Nor is anything on a
// connection the proxy serves with no session (pairedConn.noSession): it
// captures nothing on it by design -- closed unanswered, as an agent not
// started in record or test mode closes it, or passed through -- and no test
// is open on it.
func (p *Proxy) pairClosing(ctx context.Context, conn net.Conn, tc *trackedConn) {
	pc, unread, err := p.paired.closing(conn)
	if pc != nil && tc != nil && tc.tlsLayer.Load() != nil {
		pc.tls.Store(true)
	}
	switch {
	case pc == nil, ctx.Err() != nil, pc.noSession.Load():
	case err != nil:
		p.paired.fail(fmt.Errorf("%w: read what the proxy's socket from %s held unread as the proxy closed it: %v", models.ErrProxyConnsUnsupported, pc.app, err))
		p.pairLost(pc, "keploy cannot tell whether its proxy closed a connection with bytes the application wrote still unread: from now on it cannot vouch that every call of a test is in its hands", zap.Error(err))
	case unread == 0:
		p.keepAfterClose(ctx, pc)
	case tc == nil || tc.tlsLayer.Load() == nil:
		pc.resetAtClose.Store(true)
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
			pc.resetAtClose.Store(true)
			p.pairLost(pc, "keploy's proxy closed a connection with bytes still unread that its TLS layer could not read: whether calls of the application's were among them cannot be told", zap.Error(err))
		case data:
			pc.resetAtClose.Store(true)
			p.pairLost(pc, unreadLost)
		default:
			// The close_notify, or the FIN: kept, the socket says whether
			// only the FIN followed.
			p.keepAfterClose(ctx, pc)
		}
	}
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
// (msg). pc.lost is set only once the loss is counted, and a caller that finds
// the loss being charged by another waits for that count: whoever returns from
// pairLost, or reads lost set, can rely on the count being in, so a wait that
// settles on the loss never lets the test's read of its losses miss it.
func (p *Proxy) pairLost(pc *pairedConn, msg string, fields ...zap.Field) {
	if pc.noSession.Load() {
		return
	}
	if !pc.lossMu.TryLock() {
		if pairLostWaits != nil {
			pairLostWaits()
		}
		pc.lossMu.Lock()
		if pairLostWaited != nil {
			pairLostWaited()
		}
	}
	if pc.lost.Load() {
		pc.lossMu.Unlock()
		return
	}
	if pairLostCounting != nil {
		pairLostCounting()
	}
	p.losses.Add(models.CaptureLossAppBytesLost, "from "+pc.app.String())
	pc.lost.Store(true)
	pc.lossMu.Unlock()
	p.logger.Warn(msg, append([]zap.Field{
		zap.String("from", pc.app.String()),
		zap.String("next_step", "keploy cannot vouch for the calls of the test that was open (under the proven switch, it and every later test are not verified); check the agent's log for why the connection ended"),
	}, fields...)...)
}

// pairLostCounting, pairLostWaits and pairLostWaited are a test's: the first
// runs while a loss is being charged, before it is counted; the second when a
// caller finds one being charged and waits for it; the third once that wait is
// over.
var pairLostCounting, pairLostWaits, pairLostWaited func()

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
