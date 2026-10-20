//go:build linux

package proxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TCP states as inet_diag_msg's idiag_state and tcp_info's tcpi_state carry
// them (include/net/tcp_states.h).
const (
	tcpEstablished = 1
	tcpFinWait1    = 4
	tcpTimeWait    = 6
	tcpClose       = 7
	tcpCloseWait   = 8
	tcpLastAck     = 9
	tcpListen      = 10
	tcpClosing     = 11
)

// inet_diag wire layout (linux/inet_diag.h).
const (
	diagReqV2Len   = 56 // family, protocol, ext, pad, states, sockid
	diagMsgLen     = 72 // family, state, timer, retrans, sockid, expires, rqueue, wqueue, uid, inode
	diagMsgWqueue  = 60 // idiag_wqueue's offset in inet_diag_msg
	diagSockidLen  = 48
	diagCookieOff  = 40 // idiag_cookie's offset in inet_diag_sockid
	diagInfo       = 2  // INET_DIAG_INFO: the attribute that carries struct tcp_info
	diagRecvBufLen = 8192
)

// diagTCP is what sock_diag says of one TCP socket, found by its two ends.
type diagTCP struct {
	// state is the socket's TCP state (tcp*).
	state uint8
	// wqueue is idiag_wqueue: for a connection, write_seq - snd_una, the
	// bytes (and a FIN) written to the socket that its peer has not
	// acknowledged.
	wqueue uint32
	// info is its tcp_info; nil for a socket that has none (TIME_WAIT).
	info *unix.TCPInfo
}

// errNoSocket is lookupTCP's error when no socket has those two ends.
var errNoSocket = errors.New("no such socket")

// diagSock is a sock_diag netlink socket in this network namespace, for one
// wait's lookups: one socket, not one per lookup.
type diagSock struct {
	fd  int
	seq uint32
	buf []byte
}

func openDiag() (*diagSock, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, fmt.Errorf("sock_diag socket: %w", err)
	}
	return &diagSock{fd: fd, buf: make([]byte, diagRecvBufLen)}, nil
}

func (d *diagSock) close() { _ = unix.Close(d.fd) }

// lookupTCP is one lookup on a socket of its own.
func lookupTCP(local, remote netip.AddrPort) (diagTCP, error) {
	d, err := openDiag()
	if err != nil {
		return diagTCP{}, err
	}
	defer d.close()
	return d.lookup(local, remote)
}

// lookup finds the TCP socket whose local end is local and remote end is
// remote, by an exact sock_diag lookup in this network namespace, with its
// tcp_info. An IPv4 pair also finds an IPv6 socket connected over
// v4-mapped addresses: the kernel's exact lookup goes through the same hash
// that socket's IPv4 packets do. A listener on the local port, which the
// kernel's lookup falls back to, is not the socket: errNoSocket. Each request
// has exactly one reply (the socket, or an error), matched by its sequence
// number.
func (d *diagSock) lookup(local, remote netip.AddrPort) (diagTCP, error) {
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	family := uint8(unix.AF_INET6)
	if local.Addr().Is4() && remote.Addr().Is4() {
		family = unix.AF_INET
	} else if local.Addr().Is4() != remote.Addr().Is4() {
		return diagTCP{}, fmt.Errorf("mixed address families %s -> %s", local, remote)
	}
	d.seq++
	req := make([]byte, unix.NLMSG_HDRLEN+diagReqV2Len)
	binary.NativeEndian.PutUint32(req[0:], uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(req[6:], unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(req[8:], d.seq)
	r := req[unix.NLMSG_HDRLEN:]
	r[0] = family
	r[1] = unix.IPPROTO_TCP
	r[2] = 1 << (diagInfo - 1)                       // idiag_ext: tcp_info
	binary.NativeEndian.PutUint32(r[4:], ^uint32(0)) // every state
	id := r[8 : 8+diagSockidLen]
	binary.BigEndian.PutUint16(id[0:], local.Port())
	binary.BigEndian.PutUint16(id[2:], remote.Port())
	putDiagAddr(id[4:20], local.Addr())
	putDiagAddr(id[20:36], remote.Addr())
	binary.NativeEndian.PutUint32(id[diagCookieOff:], ^uint32(0)) // INET_DIAG_NOCOOKIE
	binary.NativeEndian.PutUint32(id[diagCookieOff+4:], ^uint32(0))
	if err := unix.Sendto(d.fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return diagTCP{}, fmt.Errorf("sock_diag request: %w", err)
	}
	for {
		var n int
		var err error
		for {
			// The reply to an exact lookup is written before sendto
			// returns; a signal can still interrupt the read.
			if n, _, err = unix.Recvfrom(d.fd, d.buf, 0); !errors.Is(err, unix.EINTR) {
				break
			}
		}
		if err != nil {
			return diagTCP{}, fmt.Errorf("sock_diag reply: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(d.buf[:n])
		if err != nil {
			return diagTCP{}, fmt.Errorf("sock_diag reply: %w", err)
		}
		for _, m := range msgs {
			if m.Header.Seq != d.seq {
				continue // an earlier request's
			}
			switch m.Header.Type {
			case unix.NLMSG_ERROR:
				if len(m.Data) >= 4 {
					errno := syscall.Errno(-int32(binary.NativeEndian.Uint32(m.Data)))
					if errno == syscall.ENOENT {
						return diagTCP{}, errNoSocket
					}
					if errno != 0 {
						return diagTCP{}, fmt.Errorf("sock_diag %s -> %s: %w", local, remote, errno)
					}
				}
				return diagTCP{}, errNoSocket
			case unix.SOCK_DIAG_BY_FAMILY:
				return parseDiagMsg(m.Data)
			}
		}
	}
}

// parseDiagMsg reads an inet_diag_msg and its INET_DIAG_INFO attribute.
func parseDiagMsg(b []byte) (diagTCP, error) {
	if len(b) < diagMsgLen {
		return diagTCP{}, errors.New("short sock_diag reply")
	}
	d := diagTCP{state: b[1], wqueue: binary.NativeEndian.Uint32(b[diagMsgWqueue:])}
	if d.state == tcpListen {
		return diagTCP{}, errNoSocket
	}
	attrs := b[diagMsgLen:]
	for len(attrs) >= unix.SizeofRtAttr {
		alen := int(binary.NativeEndian.Uint16(attrs[0:]))
		atype := binary.NativeEndian.Uint16(attrs[2:])
		if alen < unix.SizeofRtAttr || alen > len(attrs) {
			return diagTCP{}, fmt.Errorf("malformed sock_diag attribute (length %d of %d)", alen, len(attrs))
		}
		if atype == diagInfo {
			var ti unix.TCPInfo
			payload := attrs[unix.SizeofRtAttr:alen]
			// The kernel's tcp_info may be longer (newer) than this one;
			// one too short to carry the byte counts read here (up to
			// tcpi_notsent_bytes, Linux 4.6) would be read as 0, unknown
			// taken for nothing: it is no tcp_info.
			if len(payload) >= int(unsafe.Offsetof(ti.Notsent_bytes)+unsafe.Sizeof(ti.Notsent_bytes)) {
				dst := unsafe.Slice((*byte)(unsafe.Pointer(&ti)), unsafe.Sizeof(ti))
				copy(dst, payload)
				d.info = &ti
			}
		}
		next := (alen + unix.RTA_ALIGNTO - 1) &^ (unix.RTA_ALIGNTO - 1)
		if next >= len(attrs) {
			break
		}
		attrs = attrs[next:]
	}
	return d, nil
}

// putDiagAddr writes a as inet_diag carries it: an IPv4 address in the first
// four bytes, an IPv6 one in all sixteen, both in network order.
func putDiagAddr(dst []byte, a netip.Addr) {
	if a.Is4() {
		b := a.As4()
		copy(dst, b[:])
		return
	}
	b := a.As16()
	copy(dst, b[:])
}
