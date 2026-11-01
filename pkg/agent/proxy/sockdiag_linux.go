//go:build linux

package proxy

import (
	"bytes"
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

// lookupTCPOrdered is lookupOrdered on a socket of its own.
func lookupTCPOrdered(local, remote netip.AddrPort) (diagTCP, error) {
	d, err := openDiag()
	if err != nil {
		return diagTCP{}, err
	}
	defer d.close()
	return d.lookupOrdered(local, remote)
}

// lookupOrdered is lookup for a socket that may be moving between the
// kernel's tables, as a closing connection's does: a miss, or the socket
// caught closed, is what the kernel holds only once a read ordered against
// that move agrees (dumpOne). lookup, too, dumps after a miss (for a socket
// bound to a device); lookupOrdered also dumps after a socket caught closed.
//
// The exact lookup is lockless (__inet_lookup_established walks the socket's
// hash chain under RCU while other CPUs change it, and gives up at a socket
// whose last reference is going rather than walk again). A socket that
// becomes a timewait socket (tcp_time_wait: an orphan acknowledged in
// FIN_WAIT1, a socket taking its peer's FIN) is replaced in its chain and
// then closed, and a lookup made as it moves can miss it, or find it closed,
// while its timewait socket is in the chain; before 6.19 (6.12.63 in 6.12's
// stable line) the chain held the timewait socket before it could be taken,
// too. Neither is the socket gone. Either comes only once the move has
// begun: the kernel unhashes a socket before it marks it closed. The kernel
// changes a chain only under that chain's lock, which a dump takes to walk
// it (inet_diag's dump of the established hash), so a dump made after the
// miss walks the chain once the move is done: it finds the timewait socket,
// and a socket it does not find is not in the hash. (The dump reads a
// socket's state after it lets go of the lock, so one it walked to before a
// move could read closed; one made after the move began walks to none.
// Before 6.11 the kernel let go of the lock a step before it let the
// timewait socket be taken, a gap no read from user space can close: a dump
// in it answers gone, which a bytes wait reads as unknown, never as
// settled.)
func (d *diagSock) lookupOrdered(local, remote netip.AddrPort) (diagTCP, error) {
	s, err := lookupExact(d, local, remote)
	if (err == nil && s.state != tcpClose) || (err != nil && !errors.Is(err, errNoSocket)) {
		return s, err
	}
	return d.dumpOne(local, remote)
}

// lookupExact is how lookupOrdered makes its exact lookup: exact, not lookup,
// whose dump after a miss lookupOrdered's own dump would repeat. A test
// replaces it to miss as the kernel's lockless lookup can.
var lookupExact = (*diagSock).exact

// lookup finds the TCP socket whose local end is local and remote end is
// remote in this network namespace, with its tcp_info: by an exact sock_diag
// lookup (exact), and, when that misses, by a dump (dumpOne).
//
// The exact lookup finds a socket bound to a device (SO_BINDTODEVICE; a
// VRF's sockets are bound to its device) only when it is asked with that
// device's index: the kernel matches the asked index against the socket's
// bound one (inet_bound_dev_eq), and a socket bound to none matches any. The
// two ends do not tell the device, and the application's socket is not
// keploy's to ask, so it asks with none, and misses every socket bound to
// one. A dump does not filter on the device. A miss so costs a dump, a walk
// of the established hash: for a socket that is gone, or bound to a device.
func (d *diagSock) lookup(local, remote netip.AddrPort) (diagTCP, error) {
	s, err := d.exact(local, remote)
	if !errors.Is(err, errNoSocket) {
		return s, err
	}
	return d.dumpOne(local, remote)
}

// dumpStates are the states dumpOne asks for: those of a socket in the
// established hash, which is where the exact lookup finds one. With SYN_RECV
// the kernel also gives request sockets (NEW_SYN_RECV), which are in that
// hash too. Not a listener (a listener on the local port is never the
// socket, and leaving it out skips the listening hash), nor a bound socket
// that never connected (the bind hash, which 6.6 and later walk for it).
const dumpStates = (1<<12 - 1) &^ (1 << tcpListen)

// dumpBufLen is the largest message a dump sends: the kernel sizes a dump's
// messages to the reader's largest read, up to 32 KiB (netlink_dump).
const dumpBufLen = 32 << 10

// dumpOne finds the TCP socket whose ends are local and remote by a dump of
// the established hash, which walks each chain under its lock, filtered to
// the two ports, and does not filter on the device a socket is bound to:
// errNoSocket when no socket in the hash has the two ends. A dump keeps to
// one address family, so an IPv4 pair is looked for among IPv4 sockets and
// among IPv6 ones connected over v4-mapped addresses, as the exact lookup
// finds both.
//
// Two ends name one socket only up to the device: sockets bound to different
// devices may share them (the kernel's uniqueness check, too, matches a
// socket's bound device against the connecting one's). The exact lookup,
// asked with no device, finds one bound to none; a dump after its miss can
// find more than one, each bound to a device, and which one keploy means
// cannot be told from its two ends: that is an error, never one of them.
func (d *diagSock) dumpOne(local, remote netip.AddrPort) (diagTCP, error) {
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	families := []uint8{unix.AF_INET6}
	if local.Addr().Is4() && remote.Addr().Is4() {
		families = []uint8{unix.AF_INET, unix.AF_INET6}
	} else if local.Addr().Is4() != remote.Addr().Is4() {
		return diagTCP{}, fmt.Errorf("mixed address families %s -> %s", local, remote)
	}
	var found diagTCP
	n := 0
	for _, family := range families {
		s, m, err := d.dumpFamily(family, local, remote)
		if err != nil {
			return diagTCP{}, err
		}
		if m > 0 {
			found = s
		}
		n += m
	}
	switch {
	case n == 0:
		return diagTCP{}, errNoSocket
	case n > 1:
		return diagTCP{}, fmt.Errorf("sock_diag: %d sockets have the ends %s -> %s, each bound to a device of its own, and which one is meant cannot be told from its ends", n, local, remote)
	}
	return found, nil
}

// dumpFamily is dumpOne among the sockets of one address family: the last
// socket found with the two ends, and how many were.
func (d *diagSock) dumpFamily(family uint8, local, remote netip.AddrPort) (diagTCP, int, error) {
	d.seq++
	req := diagRequest(d.seq, unix.NLM_F_REQUEST|unix.NLM_F_DUMP, family, dumpStates, local, remote)
	if err := unix.Sendto(d.fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return diagTCP{}, 0, fmt.Errorf("sock_diag dump request: %w", err)
	}
	want := make([]byte, 2*16)
	putDiagAddr(want[:16], diagFamilyAddr(family, local.Addr()))
	putDiagAddr(want[16:], diagFamilyAddr(family, remote.Addr()))
	buf := make([]byte, dumpBufLen)
	var found diagTCP
	var ferr error
	n := 0
	for {
		var got int
		var err error
		for {
			if got, _, err = unix.Recvfrom(d.fd, buf, 0); !errors.Is(err, unix.EINTR) {
				break
			}
		}
		if err != nil {
			return diagTCP{}, 0, fmt.Errorf("sock_diag dump: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:got])
		if err != nil {
			return diagTCP{}, 0, fmt.Errorf("sock_diag dump: %w", err)
		}
		for _, m := range msgs {
			if m.Header.Seq != d.seq {
				continue // an earlier request's
			}
			switch m.Header.Type {
			case unix.NLMSG_DONE:
				if len(m.Data) >= 4 {
					if errno := syscall.Errno(-int32(binary.NativeEndian.Uint32(m.Data))); errno != 0 {
						return diagTCP{}, 0, fmt.Errorf("sock_diag dump %s -> %s: %w", local, remote, errno)
					}
				}
				if ferr != nil {
					return diagTCP{}, 0, ferr
				}
				return found, n, nil
			case unix.NLMSG_ERROR:
				errno := syscall.EPROTO
				if len(m.Data) >= 4 {
					errno = syscall.Errno(-int32(binary.NativeEndian.Uint32(m.Data)))
				}
				return diagTCP{}, 0, fmt.Errorf("sock_diag dump %s -> %s: %w", local, remote, errno)
			case unix.SOCK_DIAG_BY_FAMILY:
				b := m.Data
				if len(b) < diagMsgLen || b[0] != family {
					continue
				}
				id := b[4 : 4+diagSockidLen]
				if binary.BigEndian.Uint16(id[0:]) != local.Port() || binary.BigEndian.Uint16(id[2:]) != remote.Port() {
					continue
				}
				if family == unix.AF_INET && (!bytes.Equal(id[4:8], want[:4]) || !bytes.Equal(id[20:24], want[16:20])) ||
					family == unix.AF_INET6 && (!bytes.Equal(id[4:20], want[:16]) || !bytes.Equal(id[20:36], want[16:])) {
					continue
				}
				s, err := parseDiagMsg(b)
				if err != nil {
					if ferr == nil {
						ferr = err
					}
					continue
				}
				found = s
				n++
			}
		}
	}
}

// diagFamilyAddr is a as a socket of family carries it: an IPv4 address
// v4-mapped in an IPv6 socket.
func diagFamilyAddr(family uint8, a netip.Addr) netip.Addr {
	if family == unix.AF_INET6 && a.Is4() {
		return netip.AddrFrom16(a.As16())
	}
	return a
}

// diagRequest is a sock_diag request for TCP sockets of family in states
// (a bit per TCP state) whose ends are local and remote, with their
// tcp_info: an exact lookup with flags NLM_F_REQUEST, a dump (which filters
// on the two ports alone) with NLM_F_DUMP too. Both ask with no device
// (idiag_if 0).
func diagRequest(seq uint32, flags uint16, family uint8, states uint32, local, remote netip.AddrPort) []byte {
	req := make([]byte, unix.NLMSG_HDRLEN+diagReqV2Len)
	binary.NativeEndian.PutUint32(req[0:], uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(req[6:], flags)
	binary.NativeEndian.PutUint32(req[8:], seq)
	r := req[unix.NLMSG_HDRLEN:]
	r[0] = family
	r[1] = unix.IPPROTO_TCP
	r[2] = 1 << (diagInfo - 1) // idiag_ext: tcp_info
	binary.NativeEndian.PutUint32(r[4:], states)
	id := r[8 : 8+diagSockidLen]
	binary.BigEndian.PutUint16(id[0:], local.Port())
	binary.BigEndian.PutUint16(id[2:], remote.Port())
	putDiagAddr(id[4:20], diagFamilyAddr(family, local.Addr()))
	putDiagAddr(id[20:36], diagFamilyAddr(family, remote.Addr()))
	binary.NativeEndian.PutUint32(id[diagCookieOff:], ^uint32(0)) // INET_DIAG_NOCOOKIE
	binary.NativeEndian.PutUint32(id[diagCookieOff+4:], ^uint32(0))
	return req
}

// exact finds the TCP socket whose local end is local and remote end is
// remote, by an exact sock_diag lookup in this network namespace, with its
// tcp_info, among the sockets bound to no device (lookup). An IPv4 pair
// also finds an IPv6 socket connected over v4-mapped addresses: the kernel's
// exact lookup goes through the same hash that socket's IPv4 packets do. A
// listener on the local port, which the kernel's lookup falls back to, is
// not the socket: errNoSocket. Each request has exactly one reply (the
// socket, or an error), matched by its sequence number.
func (d *diagSock) exact(local, remote netip.AddrPort) (diagTCP, error) {
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	family := uint8(unix.AF_INET6)
	if local.Addr().Is4() && remote.Addr().Is4() {
		family = unix.AF_INET
	} else if local.Addr().Is4() != remote.Addr().Is4() {
		return diagTCP{}, fmt.Errorf("mixed address families %s -> %s", local, remote)
	}
	d.seq++
	req := diagRequest(d.seq, unix.NLM_F_REQUEST, family, ^uint32(0), local, remote) // every state
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
			// one too short to carry the byte counts read here would be
			// read as 0, unknown taken for nothing: it is no tcp_info.
			if len(payload) >= int(unsafe.Offsetof(ti.Bytes_received)+unsafe.Sizeof(ti.Bytes_received)) {
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
