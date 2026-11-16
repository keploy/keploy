//go:build darwin

package relay

import (
	"encoding/binary"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// On macOS the proxy's end of a client connection is asked through its
// TCP_INFO: xnu's struct tcp_info (bsd/netinet/tcp_private.h, packed to 4
// bytes), which any process may ask of its own socket. macOS has neither
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
// much of it a look needs.
const (
	tcpInfoOpt       = 0x200
	tcpiState        = 0
	tcpiFlags        = 4
	tcpiSndNxt       = 52
	tcpiSndSbbytes   = 64
	tcpiTxunacked    = 92
	tcpInfoNeeded    = tcpiTxunacked + 8
	tcpInfoBuf       = 512
	tcpiLossRecovery = 0x1
	tcpsClosed       = 0
	tcpsCloseWait    = 5
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

	mu sync.Mutex
	// seen: a look has been taken. lastNxt is snd_nxt as the last look found
	// it; nxt counts it from the start (snd_una at the first look), and
	// maxNxt is the most it has been. acked is the most acknowledged bytes a
	// look has counted.
	seen        bool
	lastNxt     uint32
	nxt, maxNxt int64
	acked       uint64
}

// newTCPClientSocket is the client socket rc (clientSocketOf), with its
// question.
func newTCPClientSocket(rc syscall.RawConn) *tcpClientSocket {
	s := &tcpClientSocket{rc: rc, askInfo: tcpInfo}
	s.ask, s.count = s.lookAt, s.countAt
	return s
}

// tcpInfo is TCP_INFO for the socket fd, read through buf into info; false
// when the socket cannot say, or says less than a look needs.
func tcpInfo(fd uintptr, buf *[tcpInfoBuf]byte, info *darwinTCPInfo) bool {
	size := uint32(len(buf))
	_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.IPPROTO_TCP, tcpInfoOpt,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if errno != 0 || size < tcpInfoNeeded {
		return false
	}
	*info = darwinTCPInfo{
		state:        buf[tcpiState],
		lossRecovery: binary.LittleEndian.Uint32(buf[tcpiFlags:])&tcpiLossRecovery != 0,
		sndNxt:       binary.LittleEndian.Uint32(buf[tcpiSndNxt:]),
		sbbytes:      binary.LittleEndian.Uint32(buf[tcpiSndSbbytes:]),
		txunacked:    binary.LittleEndian.Uint64(buf[tcpiTxunacked:]),
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
