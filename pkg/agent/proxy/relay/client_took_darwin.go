//go:build darwin

package relay

import (
	"crypto/tls"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// On macOS the proxy's end of a client connection is asked through its
// TCP_INFO: xnu's struct tcp_info (bsd/netinet/tcp_private.h, packed to 4
// bytes; read against xnu-12377.121.6, macOS 26), which any process may ask of
// its own socket, filled by tcp_fill_info (bsd/netinet/tcp_usrreq.c). The
// struct is private to xnu and has grown only at its end: a look reads a known
// prefix of it, and only when getsockopt says it answered that much
// (readTCPInfo). A socket whose answer is shorter, or whose call fails, says
// nothing (clientUnknown), so every chunk is teed as it is written, as on
// Windows, and the relay logs it once (tcpInfoFallback). macOS has neither
// tcpi_bytes_acked nor SIOCOUTQ; one TCP_INFO answers, at one instant, what
// they answer on Linux: the state; tcpi_snd_nxt, the next sequence number
// the socket sends; tcpi_txunacked, snd_max less snd_una, the bytes sent that
// the client's stack has not acknowledged yet (the proxy's FIN among them,
// once sent); and tcpi_snd_sbbytes, the bytes its send buffer holds, every
// byte written to it and not acknowledged yet, sent or not (a FIN is no byte
// of it).
//
// snd_una, the first byte the client has not acknowledged, is snd_nxt less
// txunacked whenever snd_nxt is snd_max, the highest sequence number sent: on
// every connection that has not lost a segment. While the socket resends what
// was lost, snd_nxt can be behind snd_max (from a retransmission timeout, in
// fast recovery, on a window the client shrank), and snd_nxt less txunacked
// is then less than snd_una, never more: snd_nxt is never past snd_max. The
// socket says it is in loss recovery (TCPI_FLAG_LOSSRECOVERY), and a look
// that finds snd_nxt behind where an earlier look found it knows it is
// resending. Such a look is not exact: the acknowledgements it counts are
// the most of those the socket said before (they never go back), which is no
// more than the client's stack acknowledged, so a chunk is never taken that
// it did not acknowledge; a closed connection it finds cannot say what the
// client took (clientUnknown: the chunk is teed, as before); and written
// cannot say how many bytes were written. A resend the socket does not flag
// and no look sees start (one look before snd_max moved on, the next after a
// timeout took snd_nxt back and an acknowledgement ended loss recovery)
// counts snd_nxt short, and so both counts: fewer bytes written than were (a
// chunk judged from before its first byte, teed as before the relay asked,
// if the client acknowledges what was written before it), and fewer
// acknowledged than were, at a look that finds the connection closed and
// takes its count as exact (a chunk the client acknowledged in part,
// refused). Either needs a segment lost on the loopback, a retransmission
// timeout (xnu's is 100 ms or more), and no look while it ran; while a chunk
// waits, resolve looks at least every lastLookAgain (5 ms).
//
// Sequence numbers are 32 bits and wrap. The counts start at snd_una as the
// first look found it, and add each move of snd_nxt between looks (each less
// than 2^31: between two looks the socket sends at most one chunk and what
// its send buffer held), so they are 64 bits, like Linux's, and agree with
// each other: clientLook.acked and written count from the same byte.
//
// What the client acknowledged is counted, on a socket the relay is the only
// writer of, from the bytes written to it instead (countWrites). xnu drops
// from the send buffer exactly the bytes an acknowledgement covers (sbdrop in
// tcp_input, bsd/netinet/tcp_input.c:3863, and :5734-5737 for the one that
// acknowledges the proxy's FIN, which is no byte of it), and nothing else
// takes bytes out of it while the socket is open, nor once it is reset:
// tcp_close (bsd/netinet/tcp_subr.c:1782) and soisdisconnected
// (bsd/kern/uipc_socket2.c:278) leave it as it is, and only sbrelease, as the
// socket is torn down (uipc_socket2.c:831), empties it. So every byte
// appended to it, less tcpi_snd_sbbytes, is every byte the client's stack
// acknowledged, at any instant, whatever snd_nxt does: a closed receive
// window's probes and the pull-backs of snd_nxt after them, a resend, a reset.
// xnu counts the bytes appended (so_tc_stats, uipc_socket2.c:839-853), but no
// getsockopt answers that count, so keploy keeps its own: the proxy counts
// every byte written to the client's socket from its accept (CountClientWrites),
// under its tracking and any TLS, so whatever layer writes (a TLS handshake or
// record, a parser, the relay) its bytes are counted as they are appended. A
// client conn with no count from its accept has the relay count its own writes
// (writeCountingConn), from what the send buffer held as that count began (in
// New, before the relay writes). Either count is exact while no Write is
// running and none failed. A
// Write running as the socket answers may have appended any part of its
// bytes; a Write that failed may have appended bytes it did not say it wrote:
// sosend (bsd/kern/uipc_socket.c) appends in pieces, dropping the socket's
// lock to copy each (:2197) and checking the socket again before the next
// (sosendcheck, :2142), so a reset that lands in that gap fails the call with
// pieces already appended, and write(2) answers the error, not their count.
// The count is then a range, from the bytes the Writes said they wrote (lo)
// to those and every byte of the running and failed Writes (hi). A look that
// finds the connection open counts lo, so a chunk is taken only once it was
// acknowledged for certain. A look that finds it closed counts the least of
// what is certain to be no less than what the client acknowledged: hi;
// snd_nxt, which snd_una is never past (tcp_input sets snd_nxt to snd_una
// when it was behind, :4233, :5039 and :5764); and, when the count of
// sequence numbers above is exact, that count, but never less than lo. So a
// chunk is refused only when the client did not acknowledge it, for certain.
//
// The count holds only while every byte appended to the socket went through
// it. The relay's own count is not taken when a TLS layer is between the
// relay and the socket (the bytes the relay writes are not the bytes
// appended: the count of sequence numbers answers alone there), and neither
// count is when the socket cannot answer as it begins. A send buffer that holds more than could have been appended
// through the count says a write went around it: the socket says nothing from
// then on (clientUnknown), and every chunk is teed as it is written.
//
// The states (bsd/netinet/tcp_fsm.h) read as Linux's do (client_took_linux.go):
// ESTABLISHED, open; CLOSE_WAIT and LAST_ACK, the client's FIN is in; CLOSED,
// the connection is closed (reset, timed out, or LAST_ACK's FIN
// acknowledged); TIME_WAIT, the client's FIN came after the proxy's was
// acknowledged, which Linux reads as closed, as here; CLOSING, FIN_WAIT_1 and
// FIN_WAIT_2 read as open.

// xnu's TCP_INFO and the offsets of the fields of its struct tcp_info the
// relay reads. The struct has grown at its end only; tcpInfoNeeded is how
// much of it a look needs: up to the end of tcpi_rxoutoforderbytes, the last
// field read.
const (
	tcpInfoOpt       = 0x200
	tcpiState        = 0
	tcpiFlags        = 4
	tcpiSndNxt       = 52
	tcpiRcvNxt       = 56
	tcpiSndSbbytes   = 64
	tcpiTxunacked    = 92
	tcpiRxbytes      = 108
	tcpiRxdupbytes   = 116
	tcpiRxooobytes   = 124
	tcpInfoNeeded    = tcpiRxooobytes + 8
	tcpInfoBuf       = 512
	tcpiLossRecovery = 0x1
	tcpsClosed       = 0
	tcpsCloseWait    = 5
	tcpsClosing      = 7
	tcpsLastAck      = 8
	tcpsTimeWait     = 10
)

// darwinTCPInfo is what a look reads of the socket's TCP_INFO.
type darwinTCPInfo struct {
	state        uint8
	lossRecovery bool
	sndNxt       uint32
	sbbytes      uint32
	txunacked    uint64
	received
}

// received is what a TCP_INFO says of the bytes the client sent
// (finBeforeReset): tcpi_rcv_nxt, the next sequence number the socket
// expects; tcpi_rxbytes, the bytes of every segment the socket took in
// (inp_mstat's ts_rxbytes); tcpi_rxduplicatebytes, those of them it had
// already; and tcpi_rxoutoforderbytes, those that came ahead of a hole.
type received struct {
	rcvNxt            uint32
	rxBytes, dup, ooo uint64
}

// tcpClientSocket is the proxy's end of a client connection that is a TCP
// socket, asked through its TCP_INFO. Made once per connection, with its
// questions, so neither allocates. look is the forwarders' and resolve's, one
// at a time (client_took.go); written is the destination's forwarder's, and
// may run while resolve looks: each has its own buffer and answer, and the
// counts they share are kept under mu.
type tcpClientSocket struct {
	rc         syscall.RawConn
	ask, count func(fd uintptr)
	// askInfo is the socket's question, TCP_INFO (tcpInfo), into buf. A test
	// answers it from a script.
	askInfo func(fd uintptr, buf *[tcpInfoBuf]byte, info *darwinTCPInfo) bool
	// getInfo is tcpInfo's getsockopt (getTCPInfo); it says to log, once in
	// the process (fallback: tcpInfoFallback), that it could not be read.
	getInfo  func(fd uintptr, buf *[tcpInfoBuf]byte) (int, error)
	log      *zap.Logger
	fallback *infoFallback
	// buf, info and got are look's: its question's buffer, the answer, and
	// what it says.
	buf  [tcpInfoBuf]byte
	info darwinTCPInfo
	got  clientLook
	// wbuf, winfo, wrote and wroteOK are written's.
	wbuf    [tcpInfoBuf]byte
	winfo   darwinTCPInfo
	wrote   uint64
	wroteOK bool
	// rbuf and rinfo are finBeforeReset's, the client's forwarder's.
	rbuf  [tcpInfoBuf]byte
	rinfo darwinTCPInfo
	// ebuf and einfo are unresetError's, clientEnd's; ended: the
	// connection's end was given, by unresetError or by SO_ERROR
	// (endGiven).
	ebuf  [tcpInfoBuf]byte
	einfo darwinTCPInfo
	ended bool

	mu sync.Mutex
	// seen: a look has been taken. lastNxt is snd_nxt as the last look found
	// it; nxt counts it from the start (snd_una at the first look), and
	// maxNxt is the most it has been. acked is the most acknowledged bytes a
	// look has counted.
	seen        bool
	lastNxt     uint32
	nxt, maxNxt int64
	acked       uint64
	// finIn: an answer said the client's FIN was in. Otherwise base is what
	// the last answer that said the connection open said of the bytes
	// received (hasBase: one did), for finBeforeReset.
	finIn, hasBase bool
	base           received

	// writes counts the bytes the relay writes to the socket (countWrites),
	// nil when they are not counted; unsent0 is what its send buffer held as
	// the count began. ackedLo is the most acknowledged bytes a look has
	// counted from them for certain; bypassed: a look found more bytes in the
	// send buffer than could have been written through the count. Under mu,
	// but for writes, which has its own.
	writes   *writeCount
	unsent0  uint64
	ackedLo  uint64
	bypassed bool
}

// newTCPClientSocket is the client socket rc (clientSocketOf), with its
// question; log is where it says, once in the process, that its TCP_INFO
// cannot be read.
func newTCPClientSocket(rc syscall.RawConn, log *zap.Logger) *tcpClientSocket {
	s := &tcpClientSocket{rc: rc, getInfo: getTCPInfo, log: log, fallback: &tcpInfoFallback}
	s.askInfo = s.tcpInfo
	s.ask, s.count = s.lookAt, s.countAt
	return s
}

// getTCPInfo asks the socket fd for its TCP_INFO into buf: how many bytes of
// it the kernel wrote (getsockopt's optlen), or the call's error.
func getTCPInfo(fd uintptr, buf *[tcpInfoBuf]byte) (int, error) {
	size := uint32(len(buf))
	_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.IPPROTO_TCP, tcpInfoOpt,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if errno != 0 {
		return 0, errno
	}
	return min(int(size), len(buf)), nil
}

// tcpInfo is TCP_INFO for the socket fd, read through buf into info; false
// when the call fails, or answers less than a look needs (getsockopt's
// optlen short of the end of the last field read: an older or changed
// struct), which is logged once in the process (s.fallback).
func (s *tcpClientSocket) tcpInfo(fd uintptr, buf *[tcpInfoBuf]byte, info *darwinTCPInfo) bool {
	n, err := s.getInfo(fd, buf)
	switch {
	case err != nil:
		if s.log != nil && s.fallback.failedSaid.CompareAndSwap(false, true) {
			s.log.Debug("relay: a client socket's TCP_INFO failed; what the destination sends it once it has "+
				"closed is teed as it is written", zap.Error(err))
		}
		return false
	case !readTCPInfo(buf[:n], info):
		if s.log != nil && s.fallback.shortSaid.CompareAndSwap(false, true) {
			s.log.Warn("relay: this macOS answers a TCP_INFO shorter than keploy reads; what the destination sends "+
				"once the client has closed is teed as it is written, whether the client took it or not",
				zap.Int("answered_bytes", n), zap.Int("needed_bytes", tcpInfoNeeded))
		}
		return false
	}
	return true
}

// infoFallback is whether a TCP_INFO that could not be read was logged, for
// each of the two ways: an answer shorter than a look needs (the struct is
// not the one these offsets were read from: every look on this macOS says
// nothing), and a call that failed (that socket's look says nothing).
type infoFallback struct{ shortSaid, failedSaid atomic.Bool }

// tcpInfoFallback is the process's.
var tcpInfoFallback infoFallback

// readTCPInfo reads b, the struct tcp_info a TCP_INFO answered, into info;
// false when it is shorter than a look needs.
func readTCPInfo(b []byte, info *darwinTCPInfo) bool {
	if len(b) < tcpInfoNeeded {
		return false
	}
	*info = darwinTCPInfo{
		state:        b[tcpiState],
		lossRecovery: binary.LittleEndian.Uint32(b[tcpiFlags:])&tcpiLossRecovery != 0,
		sndNxt:       binary.LittleEndian.Uint32(b[tcpiSndNxt:]),
		sbbytes:      binary.LittleEndian.Uint32(b[tcpiSndSbbytes:]),
		txunacked:    binary.LittleEndian.Uint64(b[tcpiTxunacked:]),
		received: received{
			rcvNxt:  binary.LittleEndian.Uint32(b[tcpiRcvNxt:]),
			rxBytes: binary.LittleEndian.Uint64(b[tcpiRxbytes:]),
			dup:     binary.LittleEndian.Uint64(b[tcpiRxdupbytes:]),
			ooo:     binary.LittleEndian.Uint64(b[tcpiRxooobytes:]),
		},
	}
	return true
}

// unsentOf is the bytes the send buffer of the socket fd holds, written to it
// and not acknowledged yet, sent or not (SO_NWRITE: tcpi_snd_sbbytes), into
// n; false when the socket cannot say. resetClientOnClose's question.
func unsentOf(fd uintptr, n *int32) bool {
	v, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NWRITE)
	// xnu answers SO_NWRITE with so_snd.sb_cc (sogetoptlock in
	// bsd/kern/uipc_socket.c, xnu-12377.121.6): a uint32_t copied out as an
	// int, which the send buffer's limit (kern.ipc.maxsockbuf, 8 MiB by
	// default) keeps far under 2^31. So no answer is negative today, and no
	// real socket can give one to a row. A negative answer would be a count
	// this reading does not know: the socket cannot say, and the close stays
	// a FIN at once, where taking it as bytes not acknowledged would only
	// look again until the grace ends, to the same FIN.
	if err != nil || v < 0 {
		return false
	}
	*n = int32(v)
	return true
}

// counted adds info's snd_nxt to the counts: the bytes the client's stack
// has acknowledged, counted from the first look (no more than it did), and
// whether that count is exact (the socket is not resending).
func (s *tcpClientSocket) counted(info *darwinTCPInfo) (acked uint64, exact bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteReceived(info)
	if !s.seen {
		s.seen, s.nxt = true, int64(info.txunacked)
		s.maxNxt = s.nxt
	} else {
		s.nxt += int64(int32(info.sndNxt - s.lastNxt))
	}
	s.lastNxt = info.sndNxt
	s.maxNxt = max(s.maxNxt, s.nxt)
	if una := s.nxt - int64(info.txunacked); una > int64(s.acked) {
		s.acked = uint64(una)
	}
	return s.acked, !info.lossRecovery && s.nxt == s.maxNxt
}

func (s *tcpClientSocket) look() clientLook {
	s.got = clientLook{said: clientUnknown}
	if err := s.rc.Control(s.ask); err != nil {
		// The socket is closed (keploy closed it under the relay): it
		// cannot say what the client did.
		return clientLook{said: clientUnknown}
	}
	return s.got
}

func (s *tcpClientSocket) lookAt(fd uintptr) {
	// The count of writes, before and after the question: the socket
	// answered between the two.
	var before tally
	if s.writes != nil {
		before = s.writes.now()
	}
	if !s.askInfo(fd, &s.buf, &s.info) {
		return
	}
	acked, exact := s.counted(&s.info)
	if s.writes != nil {
		if acked, exact = s.fromWrites(&s.info, before, s.writes.now(), acked, exact); !exact {
			return
		}
	}
	switch s.info.state {
	case tcpsClosed, tcpsTimeWait:
		if exact {
			s.got = clientLook{said: clientClosed, acked: acked}
		}
	case tcpsCloseWait, tcpsLastAck:
		s.got = clientLook{said: clientFinned, acked: acked}
	default:
		s.got = clientLook{said: clientOpen, acked: acked}
	}
}

func (s *tcpClientSocket) written() (uint64, bool) {
	if s.writes != nil {
		w := s.writes.now()
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.unsent0 + w.said + w.maybe, !s.bypassed
	}
	s.wrote, s.wroteOK = 0, false
	if err := s.rc.Control(s.count); err != nil {
		return 0, false
	}
	return s.wrote, s.wroteOK
}

// countAt is written's question: the bytes acknowledged and those the send
// buffer holds add up to every byte written, from the same answer, so
// exactly, unless the socket is resending.
func (s *tcpClientSocket) countAt(fd uintptr) {
	if !s.askInfo(fd, &s.wbuf, &s.winfo) {
		return
	}
	if acked, exact := s.counted(&s.winfo); exact {
		s.wrote, s.wroteOK = acked+uint64(s.winfo.sbbytes), true
	}
}

// fromWrites is what a look counts acknowledged from the bytes written through
// the count, before and after info was answered: for certain, while the
// connection is open, and no less than the client's stack did once it is
// closed; seqAcked is what the count of sequence numbers counted from the same
// answer, exactly when seqExact. False once the send buffer has held more than
// could have been written through the count: the socket cannot say.
func (s *tcpClientSocket) fromWrites(info *darwinTCPInfo, before, after tally, seqAcked uint64, seqExact bool) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := int64(info.sbbytes)
	hi := int64(s.unsent0+after.said+after.maybe) - held
	// The send buffer holds no more than every byte that may have been
	// appended to it, less those acknowledged for certain: more says a
	// write went around the count.
	if hi < int64(s.ackedLo) {
		s.bypassed = true
	}
	if s.bypassed {
		return 0, false
	}
	// What a look counts for certain never goes back: the client's stack
	// acknowledges on, and lo is less than it by the bytes a running Write
	// had appended but not said yet.
	s.ackedLo = max(s.ackedLo, uint64(max(int64(s.unsent0+before.said)-held, 0)))
	if info.state != tcpsClosed && info.state != tcpsTimeWait || uint64(hi) == s.ackedLo {
		return s.ackedLo, true
	}
	// s.nxt is never negative (snd_una is never past snd_nxt, and the count
	// of sequence numbers starts at or before it), and a negative one
	// would bound nothing here.
	acked := min(uint64(hi), uint64(s.nxt))
	if seqExact {
		acked = min(acked, max(seqAcked, s.ackedLo))
	}
	return acked, true
}

// writeCount counts the bytes written to a client's socket through
// writeCountingConn: said, those the Writes said they wrote; running, every
// byte of the Writes still running; unsure, those failed Writes did not say
// they wrote, which they may have.
type writeCount struct {
	mu                    sync.Mutex
	said, running, unsure uint64
}

// tally is a writeCount at one instant: the bytes the Writes said they wrote,
// and maybe, those that may have been written as well.
type tally struct{ said, maybe uint64 }

func (c *writeCount) now() tally {
	c.mu.Lock()
	defer c.mu.Unlock()
	return tally{said: c.said, maybe: c.running + c.unsure}
}

// writeCountingConn is the client conn the relay writes through on macOS, counting
// each Write (writeCount). It promotes only net.Conn's methods: a ReadFrom of
// the conn under it would write around the count.
type writeCountingConn struct {
	net.Conn
	count *writeCount
}

func (c *writeCountingConn) Write(b []byte) (int, error) { return c.count.write(c.Conn, b) }

// write writes b to conn, counting it.
func (c *writeCount) write(conn net.Conn, b []byte) (int, error) {
	c.mu.Lock()
	c.running += uint64(len(b))
	c.mu.Unlock()
	n, err := conn.Write(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	// A Write returns no more than it was given (io.Writer).
	c.running -= uint64(len(b))
	c.said += uint64(n)
	c.unsure += uint64(len(b) - n)
	return n, err
}

// NetConn is the conn under the count, for the walks that look under the
// relay's conns for the socket or a TLS layer.
func (c *writeCountingConn) NetConn() net.Conn { return c.Conn }

// CloseWrite half-closes the conn under the count (halfCloseWrite): an
// embedded net.Conn does not promote it. halfCloseWrite's caller reads only
// whether it did.
func (c *writeCountingConn) CloseWrite() error {
	if !halfCloseWrite(&c.Conn) {
		return errNoCloseWrite
	}
	return nil
}

// countWrites is src as the relay writes to it: through a count of every byte
// written (writeCountingConn) when c is the TCP socket under src (clientSocketOf)
// with no TLS layer between them, and the socket answers as the count begins;
// src as it is otherwise, and its socket answers by the count of sequence
// numbers alone.
//
// A client conn the proxy counts from its accept (CountClientWrites, under
// src) is counted from there instead, and src is returned as it is: every
// layer's writes, the relay's among them, reach the socket through that
// count.
func countWrites(src net.Conn, c clientSocket) net.Conn {
	s, ok := c.(*tcpClientSocket)
	if !ok {
		return src
	}
	if a := countedClientUnder(src); a != nil {
		s.beginFrom(a)
		return src
	}
	if tlsLayerUnder(src) || !s.beginCount() {
		return src
	}
	return &writeCountingConn{Conn: src, count: s.writes}
}

// countedClient is a client's socket as the proxy accepted it (tcp), counting
// every byte written to it (writeCount) from begin, its TCP_INFO as the count
// began (CountClientWrites). It promotes only net.Conn's methods, so a
// ReadFrom of the socket (sendfile) cannot write around the count, and gives
// the socket to what reads its state (SyscallConn: the proxy's tracking, the
// walks to the socket) or half-closes it (CloseWrite), and the socket itself
// (NetConn, AcceptedConn).
type countedClient struct {
	net.Conn
	tcp   *net.TCPConn
	count *writeCount
	begin darwinTCPInfo
}

func (c *countedClient) Write(b []byte) (int, error) { return c.count.write(c.tcp, b) }

// NetConn is the socket under the count.
func (c *countedClient) NetConn() net.Conn { return c.tcp }

// SyscallConn is the socket's own: what reads the kernel's state of the
// socket reads it through the count. Nothing writes through it.
func (c *countedClient) SyscallConn() (syscall.RawConn, error) { return c.tcp.SyscallConn() }

// CloseWrite half-closes the socket.
func (c *countedClient) CloseWrite() error { return c.tcp.CloseWrite() }

// CountClientWrites is the client connection c, as the proxy accepted it, for
// every layer above it (the proxy's tracking, TLS, a parser, the relay) to
// write through: a count of every byte written to its socket, which the
// relay's look at the client reads what the client acknowledged from
// (countWrites). c itself when it is not a *net.TCPConn, or its socket
// cannot answer as the count begins: the relay then counts its own writes
// where it can. Call it once, as c is accepted, before anything is written
// to it. What keys or reads the accepted socket itself finds it under the
// count (AcceptedConn, NetConn).
func CountClientWrites(c net.Conn) net.Conn {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return c
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return c
	}
	a := &countedClient{Conn: tc, tcp: tc, count: &writeCount{}}
	var buf [tcpInfoBuf]byte
	answered := false
	// A failed call answers no byte, which reads as no answer; so does a
	// socket Control cannot reach (closed), which asks nothing.
	_ = rc.Control(func(fd uintptr) {
		n, _ := getTCPInfo(fd, &buf)
		answered = readTCPInfo(buf[:n], &a.begin)
	})
	if !answered {
		return c
	}
	return a
}

// AcceptedConn is the conn the listener gave, under CountClientWrites' count;
// c itself when c is not that count.
func AcceptedConn(c net.Conn) net.Conn {
	if a, ok := c.(*countedClient); ok {
		return a.tcp
	}
	return c
}

// countedClientUnder is the count the proxy put on the client's socket at its
// accept (countedClient) among c and the conns under it (NetConn), within the
// bound clientSocketOf walks; nil when there is none. The walk stops at the
// first *net.TCPConn, which has no conn under it, so the count it finds is
// over the socket clientSocketOf found.
func countedClientUnder(c net.Conn) *countedClient {
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		if a, ok := c.(*countedClient); ok {
			return a
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return nil
		}
		c = u.NetConn()
	}
	return nil
}

// beginFrom begins the count of writes from a, the count from the socket's
// accept: what its send buffer held then, and the count of sequence numbers'
// first look, from a's one answer, so both count from the client's first
// unacknowledged byte at the accept. Between that answer and the relay's
// next look the socket sends what the proxy wrote before the relay began (a
// TLS handshake, a CONNECT's answer): far less than the 2^31 bytes a move of
// snd_nxt between looks is counted within. Run in New, before any look.
func (s *tcpClientSocket) beginFrom(a *countedClient) {
	s.counted(&a.begin)
	s.unsent0, s.writes = uint64(a.begin.sbbytes), a.count
}

// tlsLayerUnder reports whether a *tls.Conn is among c and the conns under it
// (NetConn), down to the socket clientSocketOf found within the same bound.
func tlsLayerUnder(c net.Conn) bool {
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		if _, ok := c.(*tls.Conn); ok {
			return true
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return false
		}
		c = u.NetConn()
	}
	return false
}

// beginCount begins the count of writes: what the send buffer holds now, and
// the count of sequence numbers' first look, from one answer, so both count
// from the client's first unacknowledged byte; false when the socket cannot
// answer. Run in New, before the relay writes, and before any look.
func (s *tcpClientSocket) beginCount() bool {
	// A socket Control cannot reach (closed) asks nothing, and ok stays
	// false.
	ok := false
	_ = s.rc.Control(func(fd uintptr) { ok = s.askInfo(fd, &s.buf, &s.info) })
	if !ok {
		return false
	}
	s.counted(&s.info)
	s.unsent0, s.writes = uint64(s.info.sbbytes), &writeCount{}
	return true
}

// A reset after the client's FIN, read as the client's end.
//
// A closed client resets what reaches it after its FIN (client_took.go), and
// the relay reads that FIN as the client's end, as Linux gives it: there a
// read of a socket that holds the FIN and then a reset returns the FIN
// (tcp_recvmsg answers SOCK_DONE before sk_err), and the reset waits on the
// socket for clientEnd. xnu answers the other way round: soreceive
// (bsd/kern/uipc_socket.c) returns so_error before the end it holds once no
// byte is left to read, and tcp_input's reset of a socket in CLOSE_WAIT sets
// so_error to ECONNRESET. So when the FIN and the reset both come in before
// the client's forwarder reads again (the answer the FIN raced written, and
// reset by the closed client), the read returns ECONNRESET, and the FIN is
// never read. The client's forwarder asks the socket then whether the FIN
// came first (finBeforeReset) and, if it did, reads the FIN and keeps the
// reset for clientEnd, as on Linux.
//
// The socket says so when an answer before the reset said the FIN was in
// (CLOSE_WAIT, CLOSING, LAST_ACK, TIME_WAIT), or by its counts. The FIN takes
// one sequence number, a reset none, and no segment moves rcv_nxt on after
// the reset closed the connection. Between an answer that found the
// connection open (base) and one after the reset, rcv_nxt moved on by the
// bytes the socket took in in order and by one for the FIN, if it came. On a
// connection where no segment ever came ahead of a hole (tcpi_rxoutoforderbytes
// 0: tcp_reass counts every segment it queues behind one), every byte counted
// in tcpi_rxbytes either moved rcv_nxt on as it was counted (tcp_input's two
// in-order paths add tlen to both) or was one the socket had already, counted
// in tcpi_rxduplicatebytes too (PAWS, and the trimmed head of a resent
// segment). So rcv_nxt moved on by the bytes counted less the duplicates, and
// by one more only for the FIN (finCameFirst): never with no FIN. A
// connection that had a segment come out of order can hold bytes queued at
// base, counted before it, that move rcv_nxt on after it: its read is a
// reset, as before, and so is a socket with no answer that found the
// connection open, or none now. xnu counts these only while nstat_collect is
// set (bsd/net/ntstat.c), which it is from boot, and only DEBUG and
// DEVELOPMENT kernels can clear (net.statistics).

// noteReceived keeps what info says of the client's FIN and the bytes
// received, for finBeforeReset. Under s.mu.
func (s *tcpClientSocket) noteReceived(info *darwinTCPInfo) {
	switch info.state {
	case tcpsCloseWait, tcpsClosing, tcpsLastAck, tcpsTimeWait:
		s.finIn = true
	case tcpsClosed:
	default:
		s.hasBase, s.base = true, info.received
	}
}

// finBeforeReset reports whether the client's FIN reached the socket before
// the reset the client's forwarder's read just returned (ECONNRESET).
func (s *tcpClientSocket) finBeforeReset() bool {
	ok := false
	if err := s.rc.Control(func(fd uintptr) { ok = s.askInfo(fd, &s.rbuf, &s.rinfo) }); err != nil || !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finIn || s.hasBase && finCameFirst(s.base, &s.rinfo)
}

// finCameFirst reports whether now, an answer of the socket once reset,
// counts the client's FIN since base, an answer that found the connection
// open: on a connection that never had a segment come out of order, rcv_nxt
// moved on by one more than the bytes counted in since, less the duplicates.
// Counts that went back say no.
func finCameFirst(base received, now *darwinTCPInfo) bool {
	if now.state != tcpsClosed || now.ooo != 0 || now.rxBytes < base.rxBytes || now.dup < base.dup {
		return false
	}
	in, dup := now.rxBytes-base.rxBytes, now.dup-base.dup
	if dup > in {
		return false
	}
	return uint64(now.rcvNxt-base.rcvNxt) == in-dup+1
}
