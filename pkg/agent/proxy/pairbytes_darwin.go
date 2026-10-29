//go:build darwin

package proxy

import (
	"context"
	"errors"
	"fmt"
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
	table, err := readXNUPCBs()
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
	return &pairTarget{pc: pc, isn: app.iss, want: uint64(app.writeSeq() - app.iss - 1), finCounted: app.finPending()}, nil
}

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
	got := ps.rcvNxt - t.isn - 1 // what the proxy's side received, its FIN counted
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
		// Short of what was written, and the proxy closed it: as on Linux,
		// an orphan that took the application's FIN refused nothing; one
		// FIN short with the orphan still waiting for that FIN is looked
		// at again; anything else is a loss.
		d, took, err := orphanTookFIN(pc, t.isn)
		oneShort := t.finCounted && short == 1
		switch {
		case took:
			return true, nil
		case errors.Is(err, errNoXNUPCB) && oneShort:
			return false, fmt.Errorf("%w: the proxy closed its side of a connection from %s one FIN short of what the application wrote, and its orphaned socket is gone: whether it took that FIN or refused a byte cannot be told", models.ErrProxyConnsUnsupported, pc.app)
		case err != nil && oneShort:
			return false, fmt.Errorf("%w: the proxy closed its side of a connection from %s one FIN short of what the application wrote, and its orphaned socket, which tells that FIN from a byte it refused, cannot be read: %v", models.ErrProxyConnsUnsupported, pc.app, err)
		case err == nil && oneShort && (d.state == xnuTCPFinWait1 || d.state == xnuTCPFinWait2):
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

// errNoXNUPCB: the table lists no such socket.
var errNoXNUPCB = errors.New("macOS's TCP table lists no such socket")

// orphanTookFIN reads the proxy's socket of pc, orphaned when the proxy closed
// its side (the one that began from isn): it took the application's FIN in
// sequence when it is in TIME_WAIT, CLOSING or LAST_ACK (pairbytes_linux.go).
func orphanTookFIN(pc *pairedConn, isn uint32) (xnuPCB, bool, error) {
	table, err := readXNUPCBs()
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
		if d, err := readXNUPCBs(); err == nil {
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
