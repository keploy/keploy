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
// arrivals the handler's own reads took in.)
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
// bytes it wrote before the close), or the proxy's side is one FIN short with
// that FIN, or a byte the orphan will refuse, still to come. Once one has been
// seen, an application socket that is gone is one whose FIN or reset is on
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
func (p *Proxy) waitPairTargets(ctx context.Context, pending []*pairTarget) error {
	if len(pending) == 0 {
		return nil
	}
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
			if err != nil {
				return err
			}
			if reached {
				p.paired.progress.Add(1)
			} else {
				next = append(next, t)
			}
		}
		pending = next
		if len(pending) == 0 {
			return nil
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
	return &pairTarget{pc: pc, want: want}, nil
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
		// refuse, is on its way, and no event of the wait's says which.
		// Otherwise what arrives now is answered with a reset: a loss.
		d, took, err := orphanTookFIN(pc)
		switch {
		case took:
			return true, nil
		case err == nil && t.want-ps.received == 1 && (d.state == tcpFinWait1 || d.state == tcpFinWait2):
			return false, fmt.Errorf("%w: the proxy closed its side of a connection from %s one FIN short of what the application wrote, and that FIN, or a byte its orphaned socket will refuse, is still on its way", models.ErrProxyConnsUnsupported, pc.app)
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
	d, err := lookupTCP(pc.proxy, pc.app)
	if err != nil {
		return d, false, err
	}
	return d, d.state == tcpTimeWait || d.state == tcpClosing || d.state == tcpLastAck, nil
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
	switch {
	case pc == nil, ctx.Err() != nil, pc.noSession.Load():
	case err != nil:
		p.paired.fail(fmt.Errorf("%w: read what the proxy's socket from %s held unread as the proxy closed it: %v", models.ErrProxyConnsUnsupported, pc.app, err))
		p.pairLost(pc, "keploy cannot tell whether its proxy closed a connection with bytes the application wrote still unread: no test's end can be verified from now on", zap.Error(err))
	case unread == 0:
	case tc == nil || tc.tlsLayer.Load() == nil:
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
		zap.String("next_step", "the test that was open, and every later one, is not verified; check the agent's log for why the connection ended"),
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

func (p *Proxy) pairedBytesUnsupported(err error) {
	p.paired.fail(fmt.Errorf("%w: %v", models.ErrProxyConnsUnsupported, err))
	p.logger.Warn("keploy cannot tell whether the bytes the application wrote to the connections its proxy holds have reached the proxy: no test's end can be verified while one is open",
		zap.Error(err),
		zap.String("next_step", "the kernel needs sock_diag for TCP (inet_diag and tcp_diag) in the agent's network namespace"))
}
