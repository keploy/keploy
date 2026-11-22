//go:build linux

package util

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// rtmsg (linux/rtnetlink.h) and its attributes, for one route lookup.
const (
	routeMsgLen  = 12 // rtmsg: family, dst_len, src_len, tos, table, protocol, scope, type, flags
	routeMsgType = 7  // rtm_type's offset in rtmsg
	routeAttrLen = 4  // rtattr: len, type
	routeBufLen  = 8 << 10
)

// IsLocalAddr reports whether a is one of this process's network namespace's
// own addresses: the kernel's answer to a route lookup of a is a local route
// (RTN_LOCAL), so a connection to it is delivered in this namespace and never
// leaves it. That holds for an address of any of its interfaces (the
// loopback's, a pod's or a container's IP) and for one of a range routed
// local (AnyIP); it does not for an address a socket took with IP_FREEBIND
// or IP_TRANSPARENT that the namespace does not have, where a connection
// would be routed off the host. A link-local IPv6 address is looked up on
// the device its zone names.
func IsLocalAddr(a netip.Addr) (bool, error) {
	if !a.IsValid() {
		return false, fmt.Errorf("route lookup of an invalid address")
	}
	a = a.Unmap()
	family, raw := uint8(unix.AF_INET), a.AsSlice()
	if a.Is6() {
		family = unix.AF_INET6
	}
	var oif uint32
	if z := a.Zone(); z != "" {
		if n, err := strconv.ParseUint(z, 10, 32); err == nil {
			oif = uint32(n)
		} else if ifc, err := net.InterfaceByName(z); err == nil {
			oif = uint32(ifc.Index)
		} else {
			return false, fmt.Errorf("route lookup of %s: no device %q: %w", a, z, err)
		}
	}
	n := unix.NLMSG_HDRLEN + routeMsgLen + routeAttrLen + len(raw)
	if oif != 0 {
		n += routeAttrLen + 4
	}
	req := make([]byte, n)
	binary.NativeEndian.PutUint32(req[0:], uint32(n))
	binary.NativeEndian.PutUint16(req[4:], unix.RTM_GETROUTE)
	binary.NativeEndian.PutUint16(req[6:], unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(req[8:], 1)
	m := req[unix.NLMSG_HDRLEN:]
	m[0] = family
	m[1] = uint8(8 * len(raw)) // rtm_dst_len: the one address
	at := m[routeMsgLen:]
	binary.NativeEndian.PutUint16(at[0:], uint16(routeAttrLen+len(raw)))
	binary.NativeEndian.PutUint16(at[2:], unix.RTA_DST)
	copy(at[routeAttrLen:], raw)
	if oif != 0 {
		at = at[routeAttrLen+len(raw):]
		binary.NativeEndian.PutUint16(at[0:], routeAttrLen+4)
		binary.NativeEndian.PutUint16(at[2:], unix.RTA_OIF)
		binary.NativeEndian.PutUint32(at[routeAttrLen:], oif)
	}

	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return false, fmt.Errorf("rtnetlink socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return false, fmt.Errorf("route lookup of %s: %w", a, err)
	}
	buf := make([]byte, routeBufLen)
	for {
		var got int
		for {
			if got, _, err = unix.Recvfrom(fd, buf, 0); !errors.Is(err, unix.EINTR) {
				break
			}
		}
		if err != nil {
			return false, fmt.Errorf("route lookup of %s: %w", a, err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:got])
		if err != nil {
			return false, fmt.Errorf("route lookup of %s: %w", a, err)
		}
		for _, msg := range msgs {
			if msg.Header.Seq != 1 {
				continue
			}
			switch msg.Header.Type {
			case unix.NLMSG_ERROR:
				errno := syscall.EPROTO
				if len(msg.Data) >= 4 {
					errno = syscall.Errno(-int32(binary.NativeEndian.Uint32(msg.Data)))
				}
				switch errno {
				case 0:
					continue // an ack, not the answer
				case unix.ENETUNREACH, unix.EHOSTUNREACH:
					return false, nil // no route at all: not the namespace's own
				}
				return false, fmt.Errorf("route lookup of %s: %w", a, errno)
			case unix.RTM_NEWROUTE:
				if len(msg.Data) < routeMsgLen {
					return false, fmt.Errorf("route lookup of %s: a short answer", a)
				}
				return msg.Data[routeMsgType] == unix.RTN_LOCAL, nil
			}
		}
	}
}
