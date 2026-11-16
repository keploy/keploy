//go:build darwin

package relay

import (
	"encoding/binary"
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
// timeout took snd_nxt back and an acknowledgement ended loss recovery) can
// count fewer bytes written than were: a chunk judged from before its first
// byte, teed as before the relay asked, if the client acknowledges what was
// written before it.
//
// Sequence numbers are 32 bits and wrap. The counts start at snd_una as the
// first look found it, and add each move of snd_nxt between looks (each less
// than 2^31: between two looks the socket sends at most one chunk and what
// its send buffer held), so they are 64 bits, like Linux's, and agree with
// each other: clientLook.acked and written count from the same byte.
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
	if !s.askInfo(fd, &s.buf, &s.info) {
		return
	}
	acked, exact := s.counted(&s.info)
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
