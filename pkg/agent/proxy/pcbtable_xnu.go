package proxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// macOS's TCP connection table, as sysctl net.inet.tcp.pcblist64 lists it:
// what the bytes wait reads of the application's socket on darwin, where
// Linux reads sock_diag. Any user may read it (netstat does), and it lists
// every TCP socket of the host, each copied under its socket's lock, so one
// record is one instant of one socket.
//
// The parsing is plain Go with no build tag, so its rows run on every
// platform's CI; only reading the table is darwin's (pcbtable_darwin.go).

// The layout of the records (struct xinpgen, struct xtcpcb64 of the SDK's
// netinet/in_pcb.h and netinet/tcp_var.h, packed to 4 bytes), as measured on
// macOS 26 arm64 with offsetof. A record of any other length is a layout
// this code does not know: the table is refused (errXNUTableLayout), never
// read at the wrong offsets.
const (
	xnuXinpgenLen   = 24
	xnuXtcpcb64Len  = 472
	xnuOffFport     = 20  // xt_inpcb.inp_fport, network order
	xnuOffLport     = 22  // xt_inpcb.inp_lport, network order
	xnuOffVflag     = 96  // xt_inpcb.inp_vflag
	xnuOffFaddr     = 100 // xt_inpcb.inp_dependfaddr (16 bytes)
	xnuOffLaddr     = 116 // xt_inpcb.inp_dependladdr (16 bytes)
	xnuOffSndCC     = 228 // xt_inpcb.xi_socket.so_snd.sb_cc
	xnuOffState     = 292 // t_state
	xnuOffSndUna    = 304 // snd_una
	xnuOffIss       = 328 // iss
	xnuOffIrs       = 332 // irs
	xnuOffRcvNxt    = 336 // rcv_nxt
	xnuOffSndWnd    = 352 // snd_wnd
	xnuInpIPv4      = 0x1 // INP_IPV4
	xnuInpIPv6      = 0x2 // INP_IPV6
	xnuIn4in6Offset = 12  // struct in_addr_4in6: three pad words, then the address
)

// errXNUTableLayout: the table's records are not the layout this code reads.
var errXNUTableLayout = errors.New("macOS's TCP connection table (net.inet.tcp.pcblist64) is not in the layout keploy reads")

// xnuPCB is one TCP socket of the table.
type xnuPCB struct {
	local, remote netip.AddrPort
	state         uint8
	// sndUna is the first sequence number the peer has not acknowledged,
	// sndCC what the send buffer holds: written, not yet acknowledged (sent
	// or not). Neither counts the FIN.
	sndUna, sndCC uint32
	iss, irs      uint32
	rcvNxt        uint32
	// sndWnd is the window its peer last advertised: 0 while the peer's
	// receive buffer is full, and what its own send buffer holds stays.
	sndWnd uint32
}

// finPending: the socket's owner closed it and its FIN, after everything it
// wrote, is not yet acknowledged (FIN_WAIT_1, CLOSING, LAST_ACK).
func (b xnuPCB) finPending() bool {
	return b.state == xnuTCPFinWait1 || b.state == xnuTCPClosing || b.state == xnuTCPLastAck
}

// writeSeq is the sequence number after the last thing the socket's owner
// wrote to it: every byte, and its FIN once it closed. Its peer has received
// all of it once its rcv_nxt reaches this.
func (b xnuPCB) writeSeq() uint32 {
	s := b.sndUna + b.sndCC
	if b.finPending() {
		s++
	}
	return s
}

// parseXNUPCBs reads the records of a pcblist64 table.
func parseXNUPCBs(buf []byte) ([]xnuPCB, error) {
	if len(buf) < xnuXinpgenLen || binary.LittleEndian.Uint32(buf) != xnuXinpgenLen {
		return nil, fmt.Errorf("%w: its header is not a struct xinpgen", errXNUTableLayout)
	}
	var out []xnuPCB
	for off := xnuXinpgenLen; ; {
		if len(buf)-off < 4 {
			return nil, fmt.Errorf("%w: it ends without its closing struct xinpgen", errXNUTableLayout)
		}
		l := int(binary.LittleEndian.Uint32(buf[off:]))
		if l == xnuXinpgenLen {
			return out, nil
		}
		if l != xnuXtcpcb64Len || len(buf)-off < l {
			return nil, fmt.Errorf("%w: a record is %d bytes, not %d", errXNUTableLayout, l, xnuXtcpcb64Len)
		}
		r := buf[off : off+l]
		local, ok1 := xnuAddr(r, xnuOffLaddr, xnuOffLport)
		remote, ok2 := xnuAddr(r, xnuOffFaddr, xnuOffFport)
		if ok1 && ok2 {
			out = append(out, xnuPCB{
				local:  local,
				remote: remote,
				state:  uint8(binary.LittleEndian.Uint32(r[xnuOffState:])),
				sndUna: binary.LittleEndian.Uint32(r[xnuOffSndUna:]),
				sndCC:  binary.LittleEndian.Uint32(r[xnuOffSndCC:]),
				iss:    binary.LittleEndian.Uint32(r[xnuOffIss:]),
				irs:    binary.LittleEndian.Uint32(r[xnuOffIrs:]),
				rcvNxt: binary.LittleEndian.Uint32(r[xnuOffRcvNxt:]),
				sndWnd: binary.LittleEndian.Uint32(r[xnuOffSndWnd:]),
			})
		}
		off += l
	}
}

// xnuAddr reads one end of a record: false for a record that is neither IPv4
// nor IPv6.
func xnuAddr(r []byte, addrOff, portOff int) (netip.AddrPort, bool) {
	port := binary.BigEndian.Uint16(r[portOff:])
	switch v := r[xnuOffVflag]; {
	case v&xnuInpIPv4 != 0:
		a := netip.AddrFrom4([4]byte(r[addrOff+xnuIn4in6Offset : addrOff+xnuIn4in6Offset+4]))
		return netip.AddrPortFrom(a, port), true
	case v&xnuInpIPv6 != 0:
		a := netip.AddrFrom16([16]byte(r[addrOff : addrOff+16])).Unmap()
		return netip.AddrPortFrom(a, port), true
	}
	return netip.AddrPort{}, false
}

// sameEnd says whether a and b are one end of a connection, whether either
// is written as an IPv4-mapped IPv6 address or with a zone.
func sameEnd(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap().WithZone("") == b.Addr().Unmap().WithZone("")
}

// findXNUPCB is the socket of table whose ends are local and remote.
func findXNUPCB(table []xnuPCB, local, remote netip.AddrPort) (xnuPCB, bool) {
	for _, b := range table {
		if sameEnd(b.local, local) && sameEnd(b.remote, remote) {
			return b, true
		}
	}
	return xnuPCB{}, false
}
