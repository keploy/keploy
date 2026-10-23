//go:build linux

package relay

import (
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// canAskClientSockets: on Linux a client connection that is a TCP socket is
// asked what its client did (clientSocketOf).
const canAskClientSockets = true

// tcpClientSocket is the proxy's end of a client connection that is a TCP
// socket. Its look asks the socket for its TCP_INFO: the state, and
// tcpi_bytes_acked, how many of the bytes written to it its peer has
// acknowledged in all (Linux 4.1 and later). Its written adds to that
// SIOCOUTQ, the bytes written to it its peer has not acknowledged yet. Made
// once per connection, with its questions, so neither allocates. The client's
// forwarder looks as it reads the client's end and the destination's
// forwarder before it writes, one at a time (Relay.finMu) and only until the
// FIN is found; resolve looks while chunks wait for the client's answer, only
// after. No two looks run at once. written is the destination's forwarder's,
// once the FIN is found, and may run while resolve looks: it has an answer of
// its own.
//
// The states (include/net/tcp_states.h, as golang.org/x/sys/unix names them)
// the socket can be in while the relay asks it: ESTABLISHED, the client has
// sent no FIN; CLOSE_WAIT, its FIN has reached the socket; LAST_ACK, the proxy
// has sent its own FIN after it (the destination's forwarder returned, and the
// relay forwarded the destination's FIN), and the client has not acknowledged
// that yet; CLOSE, the connection is closed: reset by the client, timed out by
// the proxy's stack once the client stopped answering (a retransmission,
// keepalive or user timeout), or, from LAST_ACK, closed by the client's
// acknowledgement of the proxy's FIN. The client's acknowledgements
// (tcpi_bytes_acked) tell those apart. A socket keploy still holds goes from
// LAST_ACK to CLOSE, never TIME_WAIT. The states that follow a FIN the proxy
// sent first (FIN_WAIT1, FIN_WAIT2, CLOSING) come only when the destination
// closed first: the relay forwards its FIN, and by then the destination's
// forwarder has returned and writes nothing more. Only the look as the relay
// reads the client's FIN can find them then: CLOSING (the client's FIN came
// before the client acknowledged the proxy's) reads as open, and a FIN_WAIT2
// socket that takes the client's FIN goes to CLOSE (its TIME_WAIT is a socket
// of its own, which keploy does not hold), which reads as closed, and, the
// relay having read the FIN, as the FIN found. No chunk is written after
// either, so none waits.
type tcpClientSocket struct {
	rc         syscall.RawConn
	ask, count func(fd uintptr)
	// info and got are the answer to the last look, got what it says.
	info unix.TCPInfo
	got  clientLook
	// winfo, unsent, wrote and wroteOK are the answer to the last written.
	winfo   unix.TCPInfo
	unsent  int32
	wrote   uint64
	wroteOK bool
}

// tcpInfo is TCP_INFO for the socket fd, into info; false when the socket
// cannot say.
func tcpInfo(fd uintptr, info *unix.TCPInfo) bool {
	size := uint32(unix.SizeofTCPInfo)
	_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.IPPROTO_TCP, unix.TCP_INFO,
		uintptr(unsafe.Pointer(info)), uintptr(unsafe.Pointer(&size)), 0)
	return errno == 0
}

// clientSocketOf is the TCP socket under the client conn c, through the conns
// that wrap one and say so (NetConn: util.Conn, *tls.Conn, the proxy's
// trackedConn and recordConn); nil for a conn that is not a TCP socket, which
// is not asked.
func clientSocketOf(c net.Conn) clientSocket {
	for i := 0; c != nil && i < 16; i++ { // bounded: a cycle must not hang
		if tc, ok := c.(*net.TCPConn); ok {
			rc, err := tc.SyscallConn()
			if err != nil {
				return nil
			}
			s := &tcpClientSocket{rc: rc}
			s.ask, s.count = s.lookAt, s.countAt
			return s
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return nil
		}
		c = u.NetConn()
	}
	return nil
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
	if !tcpInfo(fd, &s.info) {
		return
	}
	switch s.info.State {
	case unix.BPF_TCP_CLOSE:
		s.got = clientLook{said: clientClosed, acked: s.info.Bytes_acked}
	case unix.BPF_TCP_CLOSE_WAIT, unix.BPF_TCP_LAST_ACK:
		s.got = clientLook{said: clientFinned, acked: s.info.Bytes_acked}
	default:
		s.got = clientLook{said: clientOpen, acked: s.info.Bytes_acked}
	}
}

// How many times written asks again while the client's acknowledgements move
// between its two questions.
const writtenTries = 8

func (s *tcpClientSocket) written() (uint64, bool) {
	s.wrote, s.wroteOK = 0, false
	if err := s.rc.Control(s.count); err != nil {
		return 0, false
	}
	return s.wrote, s.wroteOK
}

// countAt is written's question: tcpi_bytes_acked counts from the first byte
// written (snd_una less the first sequence number), SIOCOUTQ is what was
// written beyond snd_una (write_seq less snd_una), so the two add up to every
// byte written (and the proxy's FIN, once sent). Two questions, between which
// an acknowledgement can move snd_una: tcpi_bytes_acked is asked on either
// side of SIOCOUTQ, and the sum is exact when it did not move. When it moves
// on every try, the sum with the later count is no less than the bytes
// written: a chunk is then judged from no earlier than its first byte, which
// can cost a chunk the client took, never tee one it did not.
func (s *tcpClientSocket) countAt(fd uintptr) {
	for try := 0; try < writtenTries; try++ {
		if !tcpInfo(fd, &s.winfo) {
			return
		}
		before := s.winfo.Bytes_acked
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, unix.SIOCOUTQ, uintptr(unsafe.Pointer(&s.unsent))); errno != 0 || s.unsent < 0 {
			return
		}
		if !tcpInfo(fd, &s.winfo) {
			return
		}
		s.wrote, s.wroteOK = s.winfo.Bytes_acked+uint64(s.unsent), true
		if s.winfo.Bytes_acked == before {
			return
		}
	}
}
