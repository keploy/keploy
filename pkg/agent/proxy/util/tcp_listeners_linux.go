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

// inet_diag wire layout (linux/inet_diag.h) for a dump of listeners.
const (
	listenDiagReqLen = 56 // inet_diag_req_v2: family, protocol, ext, pad, states, sockid
	listenDiagMsgLen = 72 // inet_diag_msg: family, state, timer, retrans, sockid, expires, rqueue, wqueue, uid, inode
	listenDiagSockid = 4  // idiag_id's offset in inet_diag_msg (and in inet_diag_req_v2, after states)
	listenDiagCookie = 40 // idiag_cookie's offset in inet_diag_sockid
	listenDiagBufLen = 32 << 10
	listenDiagListen = 10 // TCP_LISTEN
)

// TCPListeners is the local addresses of the TCP sockets that listen on port
// in this process's network namespace (the one a dial from it is made in), IPv4
// and IPv6, as the kernel's socket table (sock_diag) has them at this moment:
// a dump of the listening sockets alone, which the kernel filters to port. An
// IPv6 socket bound to a v4-mapped address is given as the IPv4 address it
// takes; one on :: as ::, whether or not it also takes IPv4 (IPV6_V6ONLY).
func TCPListeners(port uint16) ([]netip.Addr, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, fmt.Errorf("sock_diag socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	var out []netip.Addr
	for i, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		addrs, err := dumpListeners(fd, uint32(i+1), family, port)
		if err != nil {
			return nil, err
		}
		out = append(out, addrs...)
	}
	return out, nil
}

// dumpListeners is TCPListeners for one address family, as request seq.
func dumpListeners(fd int, seq uint32, family uint8, port uint16) ([]netip.Addr, error) {
	req := make([]byte, unix.NLMSG_HDRLEN+listenDiagReqLen)
	binary.NativeEndian.PutUint32(req[0:], uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(req[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(req[8:], seq)
	r := req[unix.NLMSG_HDRLEN:]
	r[0] = family
	r[1] = unix.IPPROTO_TCP
	binary.NativeEndian.PutUint32(r[4:], 1<<listenDiagListen)
	id := r[8:]
	binary.BigEndian.PutUint16(id[0:], port)                         // idiag_sport: the kernel's dump skips a listener on another port
	binary.NativeEndian.PutUint32(id[listenDiagCookie:], ^uint32(0)) // INET_DIAG_NOCOOKIE
	binary.NativeEndian.PutUint32(id[listenDiagCookie+4:], ^uint32(0))
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("sock_diag dump request: %w", err)
	}
	buf := make([]byte, listenDiagBufLen)
	var out []netip.Addr
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
						return nil, fmt.Errorf("sock_diag dump of the listeners on port %d: %w", port, errno)
					}
				}
				return out, nil
			case unix.NLMSG_ERROR:
				errno := syscall.EPROTO
				if len(m.Data) >= 4 {
					errno = syscall.Errno(-int32(binary.NativeEndian.Uint32(m.Data)))
				}
				return nil, fmt.Errorf("sock_diag dump of the listeners on port %d: %w", port, errno)
			case unix.SOCK_DIAG_BY_FAMILY:
				b := m.Data
				if len(b) < listenDiagMsgLen || b[0] != family || b[1] != listenDiagListen {
					continue
				}
				sid := b[listenDiagSockid:]
				if binary.BigEndian.Uint16(sid[0:]) != port {
					continue
				}
				var a netip.Addr
				if family == unix.AF_INET {
					a = netip.AddrFrom4([4]byte(sid[4:8]))
				} else {
					a = netip.AddrFrom16([16]byte(sid[4:20])).Unmap()
				}
				out = append(out, a)
			}
		}
	}
}
