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
// so written counts only the bytes sent, and pendingError is told from the
// connection's state and counts.
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

	// finSent: the relay sent the client its TCP FIN (proxyFINSent).
	finSent atomic.Bool
	// finIn: a look found the client's FIN in (CLOSE_WAIT or LAST_ACK).
	finIn atomic.Bool
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
	var version, got uint32 // version 0: TCP_INFO_v0
	err := windows.WSAIoctl(windows.Handle(fd), sioTCPInfo,
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
	s.got = info.look(s.finSent.Load())
	if s.got.said == clientFinned {
		s.finIn.Store(true)
	}
}

func (s *tcpClientSocket) written() (uint64, bool) {
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

// finBeforeReset reports whether the reset the client's forwarder read came
// after the client's FIN: a look found the FIN in before it (no data comes
// after a FIN, so the relay had all the client sent). Windows' read returns
// the reset then, as macOS's does (client_end.go).
func (s *tcpClientSocket) finBeforeReset() bool { return s.finIn.Load() }

// proxyFINSent is the relay's word that it sent the client its own TCP FIN
// (finSentSocket).
func (s *tcpClientSocket) proxyFINSent() { s.finSent.Store(true) }
