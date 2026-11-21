//go:build windows

package relay

import (
	"sync/atomic"
	"syscall"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// sioTCPInfo is SIO_TCP_INFO (mstcpip.h: _WSAIORW(IOC_VENDOR, 39)), which
// fills a TCP_INFO_v0 for version 0.
const sioTCPInfo = 0xD8000027

// tcpClientSocket is the proxy's end of a client connection that is a TCP
// socket, asked through its TCP_INFO_v0 (wintcpinfo.go). Windows' socket has
// no count of the bytes its send buffer holds and keeps no error for a reset,
// so written reads the count the proxy put on the socket at its accept
// (client_count_accept.go), or with none the bytes sent, and pendingError is
// told from the connection's state and counts.
type tcpClientSocket struct {
	rc  syscall.RawConn
	ask func(fd uintptr)
	// getInfo is the SIO_TCP_INFO call (getTCPInfo); a row gives a short or
	// failing one.
	getInfo  func(fd uintptr, buf *[wtcpInfoSize]byte) (int, error)
	log      *zap.Logger
	fallback *infoFallback

	// buf and got are look's; no two looks run at once (clientSocket).
	buf [wtcpInfoSize]byte
	got clientLook
	// wbuf, wrote and wroteOK are written's, which can run while resolve
	// looks.
	wbuf    [wtcpInfoSize]byte
	wrote   uint64
	wroteOK bool
	// ebuf and resetGiven are pendingError's, asked once the relay's
	// forwarders are done.
	ebuf       [wtcpInfoSize]byte
	resetGiven bool
	// fbuf is closedBeforeFIN's, asked once, as the destination's forwarder
	// ends, while resolve may look. rbuf is finBeforeReset's, the client's
	// forwarder's.
	fbuf, rbuf [wtcpInfoSize]byte

	// finSent: the relay sent the client its TCP FIN (proxyFINSent).
	finSent atomic.Bool
	// finIn: a look found the client's FIN in (CLOSE_WAIT or LAST_ACK).
	finIn atomic.Bool

	// writes counts the bytes written to the socket through the count the
	// proxy put on it at its accept (countFromAccept), nil when it has none;
	// out0 is the bytes the socket had sent as the count began. bypassed: an
	// answer counted more bytes sent than could have been written through
	// the count, so a write went around it (checkCount).
	writes   *writeCount
	out0     uint64
	bypassed atomic.Bool
}

// newTCPClientSocket is the client socket rc (clientSocketOf); log is where
// a SIO_TCP_INFO that cannot answer is said, once.
func newTCPClientSocket(rc syscall.RawConn, log *zap.Logger) *tcpClientSocket {
	s := &tcpClientSocket{rc: rc, getInfo: getTCPInfo, log: log, fallback: &tcpInfoFallback}
	s.ask = s.lookAt
	return s
}

// getTCPInfo asks the socket fd for its TCP_INFO_v0 into buf: how many bytes
// it answered.
func getTCPInfo(fd uintptr, buf *[wtcpInfoSize]byte) (int, error) {
	return askTCPInfo(windows.WSAIoctl, fd, buf)
}

// wsaIoctl is windows.WSAIoctl's signature.
type wsaIoctl func(s windows.Handle, iocc uint32, inbuf *byte, cbif uint32, outbuf *byte, cbob uint32,
	cbbr *uint32, overlapped *windows.Overlapped, completionRoutine uintptr) error

// askTCPInfo is getTCPInfo through ioctl, which a row gives an answer of any
// length: the bytes the call says it answered, never more than buf holds.
func askTCPInfo(ioctl wsaIoctl, fd uintptr, buf *[wtcpInfoSize]byte) (int, error) {
	var version, got uint32 // version 0: TCP_INFO_v0
	err := ioctl(windows.Handle(fd), sioTCPInfo,
		(*byte)(unsafe.Pointer(&version)), uint32(unsafe.Sizeof(version)),
		&buf[0], uint32(len(buf)), &got, nil, 0)
	if err != nil {
		return 0, err
	}
	return min(int(got), len(buf)), nil
}

// infoFallback says, once each, that SIO_TCP_INFO failed or answered shorter
// than keploy reads: the socket then says nothing, and what the destination
// sends a closed client is teed as it is written, as before.
type infoFallback struct{ shortSaid, failedSaid atomic.Bool }

var tcpInfoFallback infoFallback

// tcpInfo reads the socket fd's TCP_INFO_v0 through buf: false when the call
// failed or its answer ends before the last field read.
func (s *tcpClientSocket) tcpInfo(fd uintptr, buf *[wtcpInfoSize]byte) (winTCPInfo, bool) {
	n, err := s.getInfo(fd, buf)
	if err != nil {
		if s.log != nil && s.fallback.failedSaid.CompareAndSwap(false, true) {
			s.log.Debug("relay: a client socket's SIO_TCP_INFO failed; what the destination sends it once it has "+
				"closed is teed as it is written", zap.Error(err))
		}
		return winTCPInfo{}, false
	}
	info, ok := readWinTCPInfo(buf[:n])
	if !ok && s.log != nil && s.fallback.shortSaid.CompareAndSwap(false, true) {
		s.log.Warn("relay: this Windows answers a SIO_TCP_INFO shorter than keploy reads; what the destination "+
			"sends once the client has closed is teed as it is written, whether the client took it or not",
			zap.Int("answered_bytes", n), zap.Int("needed_bytes", wtcpInfoNeeded))
	}
	return info, ok
}

func (s *tcpClientSocket) look() clientLook {
	s.got = clientLook{said: clientUnknown}
	if err := s.rc.Control(s.ask); err != nil {
		return clientLook{said: clientUnknown}
	}
	return s.got
}

func (s *tcpClientSocket) lookAt(fd uintptr) {
	info, ok := s.tcpInfo(fd, &s.buf)
	if !ok {
		return
	}
	s.checkCount(info)
	s.got = info.look(s.finSent.Load())
	if s.got.said == clientFinned {
		s.finIn.Store(true)
	}
}

// written is the bytes written through the count from the accept, every byte
// of a Write still running or failed among them, as macOS counts them
// (client_took_darwin.go): exact while none is; false once a write went
// around the count. With no count, the bytes sent (winTCPInfo.sent).
func (s *tcpClientSocket) written() (uint64, bool) {
	if s.writes != nil {
		_ = s.rc.Control(func(fd uintptr) {
			if info, ok := s.tcpInfo(fd, &s.wbuf); ok {
				s.checkCount(info)
			}
		})
		w := s.writes.now()
		return s.out0 + w.said + w.maybe, !s.bypassed.Load()
	}
	s.wrote, s.wroteOK = 0, false
	if err := s.rc.Control(func(fd uintptr) {
		if info, ok := s.tcpInfo(fd, &s.wbuf); ok {
			s.wrote, s.wroteOK = info.sent()
		}
	}); err != nil {
		return 0, false
	}
	return s.wrote, s.wroteOK
}

// beginFrom begins the count of writes from a, the count from the socket's
// accept, at the bytes the socket had sent once as it began (sentOnce). Run in
// New, before any look. An accept whose counts cannot say begins from 0: a
// connection with a fast retransmit or a timeout before its count began keeps
// it, so none of its answers is read (counted), and written's 0 can only
// count fewer bytes written before a chunk, which tees it as before.
func (s *tcpClientSocket) beginFrom(a *countedClient) {
	s.writes = a.count
	s.out0, _ = a.begin.sentOnce()
}

// checkCount marks the count from the accept bypassed when info, an answer of
// the socket, counts more bytes sent once (sentOnce) than could have been
// written through the count by the time it answered (read after it): a write
// went around the count, which then says nothing (written). A window probe's
// resend counts in BytesOut and BytesRetrans alike, so it never reads as a
// write around the count. An answer whose counts cannot say (counted) gives
// 0, which marks nothing.
func (s *tcpClientSocket) checkCount(info winTCPInfo) {
	if s.writes == nil {
		return
	}
	sent, _ := info.sentOnce()
	if w := s.writes.now(); sent > s.out0+w.said+w.maybe {
		s.bypassed.Store(true)
	}
}

// pendingError is the reset the connection closed with, as Linux's SO_ERROR
// would hold it (winTCPInfo.resetCode), given once, as the kernel gives an
// error once: nil when there is none, or the socket cannot say (keploy closed
// it under the relay).
func (s *tcpClientSocket) pendingError(finRead bool) error {
	var code syscall.Errno
	_ = s.rc.Control(func(fd uintptr) {
		if s.resetGiven {
			return
		}
		if info, ok := s.tcpInfo(fd, &s.ebuf); ok {
			code = info.resetCode(finRead, s.finSent.Load())
		}
	})
	if code == 0 {
		return nil
	}
	s.resetGiven = true
	return code
}

// finBeforeReset reports whether the reset the client's forwarder read
// (readErr) came after the client's FIN: a look found the FIN in before it
// (no data comes after a FIN, so the relay had all the client sent), or the
// read's code and the socket's TCP_INFO now say it did (winTCPInfo's
// resetAfterFIN), for a FIN that came in after the relay's last look. Windows'
// read returns the reset then, as macOS's does (client_end.go). False when
// the socket cannot answer: the read is then a reset, as before.
func (s *tcpClientSocket) finBeforeReset(readErr error) bool {
	if s.finIn.Load() {
		return true
	}
	after := false
	_ = s.rc.Control(func(fd uintptr) {
		if info, ok := s.tcpInfo(fd, &s.rbuf); ok {
			after = info.resetAfterFIN(readErr, s.finSent.Load())
		}
	})
	return after
}

// proxyFINSent is the relay's word that it sent the client its own TCP FIN
// (finSentSocket).
func (s *tcpClientSocket) proxyFINSent() { s.finSent.Store(true) }

// closedBeforeFIN reports whether the connection had closed (the client reset
// it) when the relay is about to half-close it. Windows' shutdown succeeds on
// such a connection and sends no FIN (measured on Windows 11, loopback: 300
// of 300), so the relay takes the connection as over then, as Linux's failed
// shutdown says, and does not tell the socket of a FIN: a FIN taken as
// sent would take one of the bytes in flight at the reset for it, and read a
// byte of a chunk the client refused as taken (winTCPInfo.acked). False when
// the socket cannot say, and when a reset comes in between this look and the
// shutdown: the FIN is then taken as sent, as before, which can only tee a
// chunk as before, never refuse one the client took.
func (s *tcpClientSocket) closedBeforeFIN() bool {
	closed := false
	_ = s.rc.Control(func(fd uintptr) {
		if info, ok := s.tcpInfo(fd, &s.fbuf); ok {
			closed = info.state == wtcpClosed
		}
	})
	return closed
}

// acceptInfo is what CountClientWrites keeps of the socket's TCP_INFO_v0 as
// its count begins (beginFrom).
type acceptInfo = winTCPInfo

// answerAtAccept reads the socket fd's TCP_INFO_v0 into info as its count from
// the accept begins: false when it answers short. A failed call answers no
// byte, which reads as no answer.
func answerAtAccept(fd uintptr, info *winTCPInfo) bool {
	var buf [wtcpInfoSize]byte
	n, _ := getTCPInfo(fd, &buf)
	i, ok := readWinTCPInfo(buf[:n])
	*info = i
	return ok
}
