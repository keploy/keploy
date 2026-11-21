//go:build windows

package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// The bytes wait on Windows. No table a standard user may read says how much
// an application socket has written (the extended TCP statistics need an
// administrator to switch them on), so what the application wrote comes from
// keploy's shim in its own process, which counts each send call it makes on a
// connection it redirected to the proxy (agent.AppSendCounter, sentWant). The
// proxy's side is read from its own socket (SIO_TCP_INFO's BytesIn and
// State, which a standard user may read of a socket it holds), BEFORE the
// counts, so a byte the side holds was handed to a send call before the
// counts were read: a side holding more than the calls were handed is a send
// the shim missed, and the wait is unknown (sentPastStarted). The wait wakes on
// the proxy's own events, as on Linux (every read the proxy begins or ends on
// a connection it keeps, and every close of a paired one); while a send call
// the application made is pending, nothing of the proxy's marks the moment
// the shim sees its outcome, so the wait looks again on its own
// (targetBase.looks, orphanLooks) and answers unknown once the agent could run
// for the hang budget with nothing it waits on moving.

// WaitPairedBytes returns once the proxy's side of every connection it holds
// that was paired with an application connection numbered 1..n has received
// every byte the application had written to it when the wait began, as on
// Linux (pairbytes_linux.go says what each outcome means); writes made after
// it began cannot hold it back. It has no deadline of its own. It ends with
// ctx's error, or one wrapping models.ErrProxyConnsUnsupported when it cannot
// tell: keploy's hooks count no send calls (no shim), the counts of a
// connection cannot be read (its process cannot be, another may send on it),
// the proxy's socket cannot be read, the side received more than the
// application handed to send calls, or the proxy closed its side short of
// what the application had written with no loss charged.
func (p *Proxy) WaitPairedBytes(ctx context.Context, n uint64) error {
	sc, err := p.appSendCounter()
	if err != nil {
		return err
	}
	pending, err := p.pairTargets(sc, n)
	if err != nil {
		return err
	}
	return p.waitPairTargets(ctx, pending)
}

// appSendCounter is the hooks' count of the application's send calls: an
// error wrapping models.ErrProxyConnsUnsupported when they keep none.
func (p *Proxy) appSendCounter() (agent.AppSendCounter, error) {
	sc, ok := p.listenerCounter.(agent.AppSendCounter)
	if !ok {
		return nil, fmt.Errorf("%w: keploy's hooks count no send calls of the application's on its connections to the proxy, so what it wrote to them cannot be told on Windows", models.ErrProxyConnsUnsupported)
	}
	return sc, nil
}

// pairTargets reads, for every paired connection the proxy holds whose
// application connection is numbered 1..n, how much the application had
// written to it.
func (p *Proxy) pairTargets(sc agent.AppSendCounter, n uint64) ([]*pairTarget, error) {
	pcs, err := p.paired.upTo(n)
	if err != nil || len(pcs) == 0 {
		return nil, err
	}
	var pending []*pairTarget
	for _, pc := range pcs {
		t, err := pairTargetOf(sc, pc)
		if err != nil {
			return nil, err
		}
		if t != nil {
			pending = append(pending, t)
		}
	}
	return pending, nil
}

// pairTargetOf is the wait's target for pc: nil when there is nothing to wait
// for. A side that has taken the application's FIN has everything it wrote
// before it, and one the proxy closed was settled by its close (pairClosing:
// what it tossed or what was on its way is charged there); neither asks the
// counts, which a process that has exited no longer has.
func pairTargetOf(sc agent.AppSendCounter, pc *pairedConn) (*pairTarget, error) {
	ps, closed := pc.side()
	if ps.err != nil {
		return nil, fmt.Errorf("%w: read the proxy's socket from %s: %v", models.ErrProxyConnsUnsupported, pc.app, ps.err)
	}
	if peerEnded(ps.state) || closed {
		return nil, nil
	}
	s, err := appSent(sc, pc)
	if err != nil {
		return nil, err
	}
	want, sending, err := sentWant(ps.received, s)
	if err != nil {
		return nil, fmt.Errorf("from %s: %w", pc.app, err)
	}
	if !sending && ps.received >= want {
		return nil, nil
	}
	return &pairTarget{pc: pc, want: want, seen: ps.received, sent: s.done, base: targetBase{sending: sending}}, nil
}

// appSent reads the shim's counts of pc's application socket.
func appSent(sc agent.AppSendCounter, pc *pairedConn) (appSends, error) {
	var s appSends
	var err error
	s.done, s.started, s.pending, err = sc.AppSent(pc.app)
	if err != nil {
		return appSends{}, fmt.Errorf("%w: what the application wrote to its connection to the proxy from %s cannot be told: %v", models.ErrProxyConnsUnsupported, pc.app, err)
	}
	return s, nil
}

// targetBase is, on Windows, whether the wait's want still waits for send
// calls the application had pending (sentWant): the wait then asks the
// counts again at each look, and looks on its own.
type targetBase struct {
	sending bool
}

// looks says whether a wait on a target that begins from b looks at it on its
// own (orphanLooks): nothing of the proxy's marks the moment the shim sees a
// pending send call's outcome.
func (b targetBase) looks() bool { return b.sending }

// looksUnmoved says what a wait that looked on its own waited for (apps, the
// connections' application ends), for its unknown answer.
func looksUnmoved(apps string) string {
	return fmt.Sprintf("send calls the application made on its connections to the proxy from %s were still pending, as keploy's shim counted them, and the proxy's side received no more", apps)
}

// pairReached says whether t's connection has received everything the
// application had written to it when the wait began.
func (p *Proxy) pairReached(t *pairTarget) (bool, error) {
	pc := t.pc
	ps, closed := pc.side()
	if pc.noSession.Load() {
		// Served with no session: nothing on it is captured (Linux's
		// pairReached says why it is read after its side).
		return true, nil
	}
	if ps.err != nil {
		return false, fmt.Errorf("%w: read the proxy's socket from %s: %v", models.ErrProxyConnsUnsupported, pc.app, ps.err)
	}
	if ps.received > t.seen {
		t.seen = ps.received
		p.paired.progress.Add(1)
	}
	switch {
	case ps.received >= t.want, peerEnded(ps.state):
		// Everything wanted, or the application's FIN, after everything
		// it wrote.
		return true, nil
	case closed:
		return p.closedReached(pc, ps)
	case !t.base.sending:
		return false, nil
	}
	sc, err := p.appSendCounter()
	if err != nil {
		return false, err
	}
	s, err := appSent(sc, pc)
	if err != nil {
		return false, err
	}
	if s.done != t.sent {
		t.sent = s.done
		p.paired.progress.Add(1)
	}
	want, sending, err := sentAgain(t.want, ps.received, s)
	if err != nil {
		return false, fmt.Errorf("from %s: %w", pc.app, err)
	}
	t.want, t.base.sending = want, sending
	return ps.received >= t.want, nil
}

// closedReached is pairReached for a connection the proxy closed short of
// what the wait wants: settled when its side had received, as the close
// read it, everything the application had written as the close began
// (noteWroteAtClose, whose count is never below the wait's), or when the
// close charged what it tossed (pairedConn.lost: that loss is the bytes',
// pairClosing). Anything else cannot be told: a close that could not tell
// what was on its way (closeUnknown) settles nothing, as on Linux and macOS.
func (p *Proxy) closedReached(pc *pairedConn, ps proxySide) (bool, error) {
	if w := pc.wroteAtClose.Load(); w != nil && ps.received >= w.want {
		return true, nil
	}
	if pc.lost.Load() {
		return true, nil
	}
	return false, fmt.Errorf("%w: the proxy closed its side of the connection from %s before it had received everything the application wrote, and whether the rest arrived cannot be told", models.ErrProxyConnsUnsupported, pc.app)
}

// noteWroteAtClose records what the application had written to pc as the
// proxy begins closing it (pairClosing), from the shim's counts. A side that
// has taken the application's FIN needs no record: nothing is written after
// it. Counts that cannot be read, from hooks that count no send calls or for
// this connection now (its process is gone, or another process may send on
// its socket, or one of the run has not said what it holds yet), leave the
// close unable to tell what the application wrote, as a TCP table or
// sock_diag read that fails does on macOS and Linux (closeUnknown): the tests
// this connection could have served are left without a verdict, and no
// other. A later wait reads its own connections' counts; hooks that count no
// send calls leave every wait unknown (PairedBytesErr).
func (p *Proxy) noteWroteAtClose(_ context.Context, pc *pairedConn) {
	ps := readProxySide(pc.conn)
	if ps.err == nil && peerEnded(ps.state) {
		return
	}
	sc, err := p.appSendCounter()
	if err != nil {
		p.closeUnknown(pc, err)
		return
	}
	s, err := appSent(sc, pc)
	if err != nil {
		p.closeUnknown(pc, fmt.Errorf("%w: keploy's shim could not give the send counts of the application's end of a connection to the proxy, %s, as the proxy closes it: %v", models.ErrProxyConnsUnsupported, pc.app, err))
		return
	}
	w := sentWrote(s)
	pc.wroteAtClose.Store(&w)
}

// Windows's TCPSTATE values (mstcpip.h; SIO_TCP_INFO's State), in the
// enum's order: CLOSE_WAIT comes before CLOSING, LAST_ACK and TIME_WAIT.
const (
	winTCPClosed      = 0
	winTCPListen      = 1
	winTCPSynSent     = 2
	winTCPSynRcvd     = 3
	winTCPEstablished = 4
	winTCPFinWait1    = 5
	winTCPFinWait2    = 6
	winTCPCloseWait   = 7
	winTCPClosing     = 8
	winTCPLastAck     = 9
	winTCPTimeWait    = 10
)

// winTCPClosedByFIN is no TCPSTATE: it is a proxy's side whose connection
// ended with the application's FIN, which its socket reads as CLOSED. Windows
// takes the TIME_WAIT that follows the side's own FIN, and the end of a
// LAST_ACK, off the socket while the proxy still holds it, so an open socket
// reads CLOSED after a FIN as after a reset (readProxySide tells them apart).
const winTCPClosedByFIN = winTCPTimeWait + 1

// orphanEstablished is never a Windows wait's: it waits for no orphan.
const orphanEstablished = winTCPEstablished

func tcpStateName(s uint8) string {
	names := [...]string{"CLOSED", "LISTEN", "SYN_SENT", "SYN_RCVD", "ESTABLISHED", "FIN_WAIT1", "FIN_WAIT2", "CLOSE_WAIT", "CLOSING", "LAST_ACK", "TIME_WAIT", "CLOSED by the application's FIN"}
	if int(s) < len(names) {
		return names[s]
	}
	return fmt.Sprintf("state %d", s)
}

// peerEnded says whether the proxy's side of a connection, in state, has taken
// the application's FIN: CLOSE_WAIT, or LAST_ACK, CLOSING or TIME_WAIT after
// a FIN of its own, or CLOSED after either (winTCPClosedByFIN). A reset is not
// one: Windows's side is not known to take a reset only after every byte sent
// before it, so a reset side short of what was written is waited for, and
// unknown when nothing moves.
func peerEnded(state uint8) bool {
	return state == winTCPCloseWait || state == winTCPLastAck || state == winTCPClosing || state == winTCPTimeWait || state == winTCPClosedByFIN
}

// dataReceived is what ps, a proxy's side, received of the application's
// data: BytesIn counts data alone.
func dataReceived(ps proxySide) uint64 { return ps.received }

// readSide is pc's proxy side as its close reads it.
func (pc *pairedConn) readSide() proxySide { return readProxySide(pc.conn) }

// sioTCPInfo is SIO_TCP_INFO: _WSAIORW(IOC_VENDOR, 39).
const sioTCPInfo = 0xD8000027

// readProxySide reads the proxy's side of a connection from its own socket:
// TCP_INFO_v0's State (offset 0) and BytesIn (offset 56). A side that reads
// CLOSED ended either with the application's FIN or with a reset, told apart
// by endedByFIN; the FIN's is winTCPClosedByFIN.
func readProxySide(conn *net.TCPConn) proxySide {
	rc, err := conn.SyscallConn()
	if err != nil {
		return proxySide{err: err}
	}
	received, state, err := tcpInfo(rc)
	if err != nil {
		return proxySide{err: err}
	}
	if state == winTCPClosed && endedByFIN(rc) {
		state = winTCPClosedByFIN
	}
	return proxySide{received: received, counted: true, state: state}
}

// tcpInfo reads rc's TCP_INFO_v0 (SIO_TCP_INFO): BytesIn (offset 56), the
// application's data alone, and State (offset 0), a TCPSTATE, as the socket
// reads it (a CLOSED one is not told apart: readProxySide does that).
func tcpInfo(rc syscall.RawConn) (received uint64, state uint8, err error) {
	var info [88]byte
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		version := uint32(0)
		var n uint32
		ierr = windows.WSAIoctl(windows.Handle(fd), sioTCPInfo, (*byte)(unsafe.Pointer(&version)), uint32(unsafe.Sizeof(version)),
			&info[0], uint32(len(info)), &n, nil, 0)
		if ierr == nil && n < uint32(len(info)) {
			ierr = fmt.Errorf("SIO_TCP_INFO gave %d bytes of TCP_INFO_v0's %d", n, len(info))
		}
	}); err != nil {
		return 0, 0, err
	}
	if ierr != nil {
		return 0, 0, fmt.Errorf("SIO_TCP_INFO: %w", ierr)
	}
	s := binary.LittleEndian.Uint32(info[0:4])
	if s > winTCPTimeWait {
		return 0, 0, fmt.Errorf("SIO_TCP_INFO gave an unknown TCP state %d", s)
	}
	return binary.LittleEndian.Uint64(info[56:64]), uint8(s), nil
}

// resetDropsUnread says whether at, the proxy's side as its close read it,
// was reset by the application: Windows drops what such a socket held unread
// (recv answers WSAECONNRESET, FIONREAD 0) while BytesIn still counts it, so
// the bytes no read of the proxy's took are gone (resetDropped). Measured on
// Windows 11 (10.0.26200): a reset in ESTABLISHED with 4 bytes unread reads
// CLOSED, FIONREAD 0, and BytesIn 4.
func resetDropsUnread(at proxySide) bool {
	return at.err == nil && at.state == winTCPClosed
}

// msgPeek is MSG_PEEK.
const msgPeek = 0x2

// endedByFIN says whether rc, a socket that reads CLOSED, ended with the
// application's FIN. A one-byte MSG_PEEK takes nothing, and on a CLOSED
// socket it answers at once (nothing more can arrive): with the next unread
// byte, or with 0 bytes, the FIN, after a FIN; with WSAECONNRESET after a
// reset, which drops whatever was unread. Any other answer is not taken as
// the FIN. Measured on Windows 11 (10.0.26200) for the side's FIN first
// (TIME_WAIT), the application's first (LAST_ACK), and a reset in
// ESTABLISHED and in FIN_WAIT_2, with bytes unread and without.
func endedByFIN(rc syscall.RawConn) bool {
	peeked := false
	// A socket closed since is read as no FIN: Control runs nothing.
	_ = rc.Control(func(fd uintptr) {
		var b byte
		buf := windows.WSABuf{Len: 1, Buf: &b}
		var n uint32
		flags := uint32(msgPeek)
		peeked = windows.WSARecv(windows.Handle(fd), &buf, 1, &n, &flags, nil, nil) == nil
	})
	return peeked
}

// fionread is FIONREAD: _IOR('f', 127, u_long).
const fionread = 0x4004667F

// readUnread is how many bytes the proxy's socket holds unread.
func readUnread(conn *net.TCPConn) (int, error) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n, got uint32
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		ierr = windows.WSAIoctl(windows.Handle(fd), fionread, nil, 0, (*byte)(unsafe.Pointer(&n)), uint32(unsafe.Sizeof(n)), &got, nil, 0)
	}); err != nil {
		return 0, err
	}
	return int(n), ierr
}

// PairedBytesErr says whether the waits can be answered: not once the
// proxy's listener has closed, nor with hooks that count no send calls. A
// close that could not tell what was written (closeUnknown) charges its own
// connection's tests and leaves the other waits answerable.
func (p *Proxy) PairedBytesErr() error {
	p.paired.mu.Lock()
	err := p.paired.err
	p.paired.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = p.appSendCounter()
	return err
}

// checkPairedBytes proves nothing on Windows: the counts are the shim's, read
// per connection, and a connection they miss is unknown when it is read.
func (p *Proxy) checkPairedBytes(net.Conn, net.Conn) {}

// proveAppVisible notes nothing on Windows: no table of the application's
// sockets is read.
func (p *Proxy) proveAppVisible(*net.TCPConn) {}

// pairedConnOS is nothing on Windows: the counts are the shim's. pairedConn
// embeds it on every OS, so it stays, holding nothing here.
type pairedConnOS struct{} //nolint:unused // embedded by pairedConn on every OS; Windows reads nothing of its own

// pairedConnsOS is nothing on Windows: no application socket is read.
type pairedConnsOS struct{} //nolint:unused // embedded by pairedConns on every OS; Windows reads nothing of its own

// resetAfterSent is false on Windows: its side is not known to take a reset
// only after every byte sent before it (peerEnded), so a close's read of
// bytes on their way that ends in a reset cannot tell whether data was lost.
func resetAfterSent(error) bool { return false }
