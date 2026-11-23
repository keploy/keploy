//go:build linux

package util

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

// TCP states, as the kernel numbers them (include/net/tcp_states.h), for
// TCPSockets' states mask.
const (
	TCPEstablished = 1
	TCPFinWait1    = 4
	TCPFinWait2    = 5
	TCPCloseWait   = 8
	TCPLastAck     = 9
	TCPListen      = 10
	TCPClosing     = 11
)

// TCPSocket is one TCP socket of this process's network namespace, as the
// kernel's socket table (sock_diag) has it.
type TCPSocket struct {
	Family uint8 // the socket's own: AF_INET or AF_INET6
	State  uint8 // TCPEstablished, TCPListen, ...
	// Local and Remote are its addresses; an IPv6 socket's v4-mapped ones
	// as the IPv4 addresses they are.
	Local, Remote netip.AddrPort
	// Inode is the number a process's fd that holds the socket links to
	// ("socket:[inode]"), 0 for a socket no file holds any more.
	Inode uint32
	// Cookie is the socket's cookie (SO_COOKIE).
	Cookie uint64
	// DataSegs counts the segments that carried data, both ways (tcp_info's
	// tcpi_data_segs_in plus tcpi_data_segs_out), when DataKnown (Linux
	// 4.6 or newer). It moves only as the connection carries data: not on
	// its handshake, an ACK, a keep-alive probe or a FIN, each of which the
	// byte counts (tcpi_bytes_acked, tcpi_bytes_received) take in.
	DataSegs  uint64
	DataKnown bool
}

const (
	sockDiagInfo          = 2   // INET_DIAG_INFO: the socket's struct tcp_info
	tcpInfoDataSegsIn     = 152 // tcpi_data_segs_in's offset in struct tcp_info
	tcpInfoDataSegsOut    = 156 // tcpi_data_segs_out's
	tcpInfoDataSegsMinLen = 160
)

// TCPSockets is every TCP socket of this process's network namespace in one
// of the states the mask states has (1<<TCPEstablished, 1<<TCPListen), IPv4
// then IPv6, with each one's count of segments that carried data.
func TCPSockets(states uint32) ([]TCPSocket, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, fmt.Errorf("sock_diag socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	var out []TCPSocket
	for i, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		socks, err := dumpSockets(fd, uint32(i+1), family, states)
		if err != nil {
			return nil, err
		}
		out = append(out, socks...)
	}
	return out, nil
}

// dumpSockets is TCPSockets for one address family, as request seq.
func dumpSockets(fd int, seq uint32, family uint8, states uint32) ([]TCPSocket, error) {
	req := make([]byte, unix.NLMSG_HDRLEN+listenDiagReqLen)
	binary.NativeEndian.PutUint32(req[0:], uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(req[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(req[8:], seq)
	r := req[unix.NLMSG_HDRLEN:]
	r[0] = family
	r[1] = unix.IPPROTO_TCP
	r[2] = 1 << (sockDiagInfo - 1)
	binary.NativeEndian.PutUint32(r[4:], states)
	id := r[8:]
	binary.NativeEndian.PutUint32(id[listenDiagCookie:], ^uint32(0)) // INET_DIAG_NOCOOKIE
	binary.NativeEndian.PutUint32(id[listenDiagCookie+4:], ^uint32(0))
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("sock_diag dump request: %w", err)
	}
	buf := make([]byte, listenDiagBufLen)
	var out []TCPSocket
	for {
		var n int
		var err error
		for {
			if n, _, err = unix.Recvfrom(fd, buf, 0); !errors.Is(err, unix.EINTR) {
				break
			}
		}
		if err != nil {
			return nil, fmt.Errorf("sock_diag dump: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, fmt.Errorf("sock_diag dump: %w", err)
		}
		for _, m := range msgs {
			if m.Header.Seq != seq {
				continue // an earlier request's
			}
			switch m.Header.Type {
			case unix.NLMSG_DONE:
				if len(m.Data) >= 4 {
					if errno := syscall.Errno(-int32(binary.NativeEndian.Uint32(m.Data))); errno != 0 {
						return nil, fmt.Errorf("sock_diag dump of the TCP sockets: %w", errno)
					}
				}
				return out, nil
			case unix.NLMSG_ERROR:
				errno := syscall.EPROTO
				if len(m.Data) >= 4 {
					errno = syscall.Errno(-int32(binary.NativeEndian.Uint32(m.Data)))
				}
				return nil, fmt.Errorf("sock_diag dump of the TCP sockets: %w", errno)
			case unix.SOCK_DIAG_BY_FAMILY:
				if s, ok := parseTCPSocket(m.Data, family); ok {
					out = append(out, s)
				}
			}
		}
	}
}

// parseTCPSocket reads one inet_diag_msg of family, with its attributes.
func parseTCPSocket(b []byte, family uint8) (TCPSocket, bool) {
	if len(b) < listenDiagMsgLen || b[0] != family {
		return TCPSocket{}, false
	}
	sid := b[listenDiagSockid:]
	s := TCPSocket{
		Family: family,
		State:  b[1],
		Inode:  binary.NativeEndian.Uint32(b[listenDiagInode:]),
		Cookie: uint64(binary.NativeEndian.Uint32(sid[listenDiagCookie:])) | uint64(binary.NativeEndian.Uint32(sid[listenDiagCookie+4:]))<<32,
	}
	sport, dport := binary.BigEndian.Uint16(sid[0:]), binary.BigEndian.Uint16(sid[2:])
	if family == unix.AF_INET {
		s.Local = netip.AddrPortFrom(netip.AddrFrom4([4]byte(sid[4:8])), sport)
		s.Remote = netip.AddrPortFrom(netip.AddrFrom4([4]byte(sid[20:24])), dport)
	} else {
		s.Local = netip.AddrPortFrom(netip.AddrFrom16([16]byte(sid[4:20])).Unmap(), sport)
		s.Remote = netip.AddrPortFrom(netip.AddrFrom16([16]byte(sid[20:36])).Unmap(), dport)
	}
	if info, ok := diagAttr(b[listenDiagMsgLen:], sockDiagInfo); ok && len(info) >= tcpInfoDataSegsMinLen {
		s.DataSegs = uint64(binary.NativeEndian.Uint32(info[tcpInfoDataSegsIn:])) + uint64(binary.NativeEndian.Uint32(info[tcpInfoDataSegsOut:]))
		s.DataKnown = true
	}
	return s, true
}

// diagAttr is the value of the attribute of type typ among an inet_diag_msg's
// attributes b (rtattr: length, type, value, each aligned to 4 bytes), and
// whether it is there.
func diagAttr(b []byte, typ uint16) ([]byte, bool) {
	for len(b) >= 4 {
		n := int(binary.NativeEndian.Uint16(b[0:]))
		if n < 4 || n > len(b) {
			return nil, false
		}
		if binary.NativeEndian.Uint16(b[2:])&^(1<<15|1<<14) == typ {
			return b[4:n], true
		}
		b = b[min((n+3)&^3, len(b)):]
	}
	return nil, false
}
