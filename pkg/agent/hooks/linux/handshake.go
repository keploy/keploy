//go:build linux

package linux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"golang.org/x/sys/unix"
)

// GetForHandshake names the original destination of the connection whose SYN
// went from client to proxy, while that SYN is still held. redirect_proxy_map
// (Get) is only written once the connection is established, so this goes
// through the socket instead: sock_diag finds the application's connecting
// socket by its two ends, and orig_dst_by_cookie, written by the connect hook
// for that socket, holds where the application asked to go.
//
// The proxy shares the application's network namespace (the host for a native
// run; the agent container, which the application joins, for a docker one),
// so the application's socket is visible to sock_diag from here.
func (h *Hooks) GetForHandshake(_ context.Context, client, proxy netip.AddrPort) (*agent.NetworkAddress, error) {
	cookie, err := socketCookie(client, proxy)
	if err != nil {
		return nil, err
	}
	// unLoad closes the maps under objectsMutex.
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	m := h.objects.OrigDstByCookie
	if m == nil {
		return nil, errors.New("connect hook map not loaded")
	}
	var d structs.DestInfo
	if err := m.Lookup(cookie, &d); err != nil {
		return nil, fmt.Errorf("no original destination for socket %#x: %w", cookie, err)
	}
	return &agent.NetworkAddress{
		Version:   d.IPVersion,
		IPv4Addr:  d.DestIP4,
		IPv6Addr:  d.DestIP6,
		Port:      d.DestPort,
		KernelPid: d.KernelPid,
	}, nil
}

// CheckHandshakeLookup proves, once, that GetForHandshake can name a socket
// here: sock_diag must answer an exact lookup (it needs inet_diag and
// tcp_diag, which the kernel loads on first use) with the socket's own
// cookie. Without that every held handshake would be let through unexamined,
// so the proxy does not hold them at all.
func (h *Hooks) CheckHandshakeLookup(context.Context) error {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer l.Close()
	c, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		return err
	}
	defer c.Close()
	rc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		return err
	}
	var want uint64
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		want, gerr = unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_COOKIE)
	}); err != nil {
		return err
	}
	if gerr != nil {
		return fmt.Errorf("SO_COOKIE: %w", gerr)
	}
	local, _ := netip.ParseAddrPort(c.LocalAddr().String())
	remote, _ := netip.ParseAddrPort(c.RemoteAddr().String())
	got, err := socketCookie(local, remote)
	if err != nil {
		return fmt.Errorf("sock_diag: %w", err)
	}
	if got != want {
		return fmt.Errorf("sock_diag named socket %#x, not %#x", got, want)
	}
	return nil
}

// tcpListen is TCP_LISTEN (include/net/tcp_states.h), as inet_diag_msg's
// idiag_state carries it.
const tcpListen = 10

// inet_diag wire sizes (linux/inet_diag.h).
const (
	inetDiagReqV2Len = 56 // family, protocol, ext, pad, states, sockid
	inetDiagMsgLen   = 72 // family, state, timer, retrans, sockid, 5 x u32
	sockIDCookieOff  = 40 // offset of idiag_cookie within inet_diag_sockid
)

// socketCookie returns the cookie of the socket whose local end is local and
// remote end remote, by an exact sock_diag lookup (not a dump). An IPv4 pair
// also finds an IPv6 socket connected over v4-mapped addresses: the kernel's
// exact lookup goes through the same hash the socket's IPv4 packets do.
func socketCookie(local, remote netip.AddrPort) (uint64, error) {
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	family := uint8(unix.AF_INET6)
	if local.Addr().Is4() && remote.Addr().Is4() {
		family = unix.AF_INET
	} else if local.Addr().Is4() != remote.Addr().Is4() {
		return 0, fmt.Errorf("mixed address families %s -> %s", local, remote)
	}

	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return 0, fmt.Errorf("sock_diag socket: %w", err)
	}
	defer unix.Close(fd)
	tv := unix.NsecToTimeval(time.Second.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return 0, err
	}

	req := make([]byte, unix.NLMSG_HDRLEN+inetDiagReqV2Len)
	binary.NativeEndian.PutUint32(req[0:], uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(req[6:], unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(req[8:], 1) // sequence
	r := req[unix.NLMSG_HDRLEN:]
	r[0] = family
	r[1] = unix.IPPROTO_TCP
	binary.NativeEndian.PutUint32(r[4:], ^uint32(0)) // every state
	id := r[8:]
	binary.BigEndian.PutUint16(id[0:], local.Port())
	binary.BigEndian.PutUint16(id[2:], remote.Port())
	putAddr(id[4:20], local.Addr())
	putAddr(id[20:36], remote.Addr())
	binary.NativeEndian.PutUint32(id[sockIDCookieOff:], ^uint32(0)) // INET_DIAG_NOCOOKIE
	binary.NativeEndian.PutUint32(id[sockIDCookieOff+4:], ^uint32(0))

	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, fmt.Errorf("sock_diag request: %w", err)
	}
	buf := make([]byte, 4096)
	var n int
	for {
		// SO_RCVTIMEO makes a signal interrupt the read even under
		// SA_RESTART; the reply is still there to read.
		if n, _, err = unix.Recvfrom(fd, buf, 0); !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return 0, fmt.Errorf("sock_diag reply: %w", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return 0, fmt.Errorf("sock_diag reply: %w", err)
	}
	for _, m := range msgs {
		switch m.Header.Type {
		case unix.NLMSG_ERROR:
			if len(m.Data) >= 4 {
				if errno := -int32(binary.NativeEndian.Uint32(m.Data)); errno != 0 {
					if syscall.Errno(errno) == syscall.ENOENT {
						return 0, fmt.Errorf("no socket %s -> %s: %w", local, remote, agent.ErrConnectingSocketGone)
					}
					return 0, fmt.Errorf("sock_diag %s -> %s: %w", local, remote, syscall.Errno(errno))
				}
			}
		case unix.SOCK_DIAG_BY_FAMILY:
			if len(m.Data) < inetDiagMsgLen {
				return 0, errors.New("short sock_diag reply")
			}
			// The kernel's exact lookup falls back to a listener on the
			// local port when no connection has those ends: that is not the
			// application's connecting socket.
			if m.Data[1] == tcpListen {
				return 0, fmt.Errorf("no socket %s -> %s (only a listener on its port): %w", local, remote, agent.ErrConnectingSocketGone)
			}
			c := m.Data[4+sockIDCookieOff:]
			return uint64(binary.NativeEndian.Uint32(c)) | uint64(binary.NativeEndian.Uint32(c[4:]))<<32, nil
		}
	}
	return 0, fmt.Errorf("no socket %s -> %s: %w", local, remote, agent.ErrConnectingSocketGone)
}

// putAddr writes a as inet_diag carries it: an IPv4 address in the first four
// bytes, an IPv6 one in all sixteen, both in network order.
func putAddr(dst []byte, a netip.Addr) {
	if a.Is4() {
		b := a.As4()
		copy(dst, b[:])
		return
	}
	b := a.As16()
	copy(dst, b[:])
}
