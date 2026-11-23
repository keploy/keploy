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
// macOS 26 arm64 with offsetof (xnuOffSoState is derived from the same
// 4-byte-packed struct xsocket64 layout, cross-checked against so_snd.sb_cc
// at 228 and t_state at 292, and verified at runtime by proveXNUAppClosed
// before any early release). A record of any other length is a layout
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
	xnuOffSoState   = 166 // xt_inpcb.xi_socket.so_state (int16)
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
	// XNU socket state flags in xsocket64.so_state (bsd/sys/socketvar.h).
	xnuSSNoFDRef     uint16 = 0x0001 // SS_NOFDREF: no file table reference left
	xnuSSIsConnected uint16 = 0x0002 // SS_ISCONNECTED: socket connected to a peer
	xnuSSCantSend    uint16 = 0x0010 // SS_CANTSENDMORE: shutdown(SHUT_WR)
	xnuSSCantRecv    uint16 = 0x0020 // SS_CANTRCVMORE: shutdown(SHUT_RD) / peer FIN
)

// errXNUTableLayout: the table's records are not the layout this code reads.
var errXNUTableLayout = errors.New("macOS's TCP connection table (net.inet.tcp.pcblist64) is not in the layout keploy reads")

// xnuPCB is one TCP socket of the table.
type xnuPCB struct {
	local, remote netip.AddrPort
	state         uint8
	// soState is xt_inpcb.xi_socket.so_state (bsd/sys/socketvar.h):
	// xnuSSNoFDRef is set once no process holds a file descriptor for the
	// socket (closed whole, or its process exited); a half-closed socket
	// (shutdown(SHUT_WR)) still has a descriptor and leaves it unset.
	soState uint16
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
				local:   local,
				remote:  remote,
				state:   uint8(binary.LittleEndian.Uint32(r[xnuOffState:])),
				soState: binary.LittleEndian.Uint16(r[xnuOffSoState:]),
				sndUna:  binary.LittleEndian.Uint32(r[xnuOffSndUna:]),
				sndCC:   binary.LittleEndian.Uint32(r[xnuOffSndCC:]),
				iss:     binary.LittleEndian.Uint32(r[xnuOffIss:]),
				irs:     binary.LittleEndian.Uint32(r[xnuOffIrs:]),
				rcvNxt:  binary.LittleEndian.Uint32(r[xnuOffRcvNxt:]),
				sndWnd:  binary.LittleEndian.Uint32(r[xnuOffSndWnd:]),
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

// checkXNUCounts is the verdict of the counts' proof (proveXNUCounts) on what
// it read once its writer had written n bytes and its reader had read them
// all: table, macOS's TCP table, which must list the writer's socket (ends
// writer -> reader) and the reader's (reader -> writer), and ps, the reader's
// own tcp_info. The two ends began from one sequence number; both are
// ESTABLISHED in the table; the writer's writeSeq counts the n bytes it
// wrote; the reader's rcv_nxt, in the table and in its tcp_info alike,
// counts the n it read; and its tcp_info says ESTABLISHED too. A layout that
// moved reads other numbers, and the answer is an error: every wait is then
// unknown. Plain Go with no build tag, so its rows run on every platform.
func checkXNUCounts(table []xnuPCB, writer, reader netip.AddrPort, ps proxySide, n uint32) error {
	w, ok1 := findXNUPCB(table, writer, reader)
	rd, ok2 := findXNUPCB(table, reader, writer)
	switch {
	case !ok1 || !ok2:
		return fmt.Errorf("macOS's TCP table does not list keploy's own loopback connection %s -> %s (writer found %v, reader %v)", writer, reader, ok1, ok2)
	case ps.err != nil:
		return ps.err
	case w.iss != rd.irs:
		return fmt.Errorf("macOS's TCP table gives the two ends of one connection different first sequence numbers (%d, %d)", w.iss, rd.irs)
	case w.state != xnuTCPEstablished || rd.state != xnuTCPEstablished:
		return fmt.Errorf("macOS's TCP table gives the two ends of an open connection the TCP states %d and %d, not ESTABLISHED (%d)", w.state, rd.state, xnuTCPEstablished)
	case w.writeSeq()-w.iss-1 != n || rd.rcvNxt-rd.irs-1 != n:
		return fmt.Errorf("macOS's TCP table counts %d bytes written and %d received of the %d keploy sent", w.writeSeq()-w.iss-1, rd.rcvNxt-rd.irs-1, n)
	case ps.rcvNxt != rd.rcvNxt || ps.state != xnuTCPEstablished:
		return fmt.Errorf("the socket's tcp_info (rcv_nxt %d, state %d) disagrees with macOS's TCP table (rcv_nxt %d, state %d)", ps.rcvNxt, ps.state, rd.rcvNxt, xnuTCPEstablished)
	}
	return nil
}

// checkXNUAppClosed is the verdict of the SS_NOFDREF layout proof
// (proveXNUAppClosed): on a loopback pair keploy opened, liveTable (read while
// both descriptors are open) must list a -> r with SS_ISCONNECTED set and
// SS_NOFDREF / SS_CANTSENDMORE / SS_CANTRCVMORE clear, and closedTable (read
// after a closed whole while r stays open) must list a -> r with SS_NOFDREF
// set. A layout where so_state moved fails this check, leaving early release
// off (fail closed) without affecting WaitPairedBytes. Plain Go with no build
// tag so its rows run on every platform.
func checkXNUAppClosed(liveTable, closedTable []xnuPCB, a, r netip.AddrPort) error {
	live, ok := findXNUPCB(liveTable, a, r)
	if !ok {
		return fmt.Errorf("macOS's TCP table does not list keploy's open loopback socket %s -> %s", a, r)
	}
	if live.soState&xnuSSIsConnected == 0 || live.soState&(xnuSSNoFDRef|xnuSSCantSend|xnuSSCantRecv) != 0 {
		return fmt.Errorf("macOS's TCP table gives open loopback socket %s -> %s so_state 0x%x, not connected with an open descriptor", a, r, live.soState)
	}
	closed, ok := findXNUPCB(closedTable, a, r)
	if !ok {
		return fmt.Errorf("macOS's TCP table does not list keploy's closed loopback socket %s -> %s while its peer stays open", a, r)
	}
	if closed.soState&xnuSSNoFDRef == 0 {
		return fmt.Errorf("macOS's TCP table gives closed loopback socket %s -> %s so_state 0x%x without SS_NOFDREF", a, r, closed.soState)
	}
	return nil
}

// xnuAppSocketClosed reports whether macOS's TCP table — or, when the table
// hides application sockets (!visible, e.g. macOS 27 without root), the
// process-descriptor lookup lookupHeld — shows that no process holds the
// application socket app -> proxy any more (the drain's word to a handler held
// past the application's FIN, pairedConn.appClosed). noFDRefProven gates only
// the xsocket64.so_state & SS_NOFDREF read; CLOSED/TIME_WAIT (already proven
// by xnuTableReadable), vanished-while-visible, and the process-descriptor
// fallback do not depend on so_state and stay active even if the SS_NOFDREF
// proof fails. It also reports whether the socket was seen in the TCP table
// (sawVisible) or in a readable process's descriptor table (sawHeld) so the
// caller can update paired.visible / paired.held. Plain Go with no build tag
// so every branch runs on every platform.
func xnuAppSocketClosed(table []xnuPCB, app, proxy netip.AddrPort, visible, held, noFDRefProven bool, lookupHeld func() (state uint8, found bool, err error)) (closed, sawVisible, sawHeld bool) {
	if b, ok := findXNUPCB(table, app, proxy); ok {
		return b.state == xnuTCPClosed || b.state == xnuTCPTimeWait || (noFDRefProven && b.soState&xnuSSNoFDRef != 0), true, false
	}
	if visible {
		return true, false, false
	}
	if lookupHeld == nil {
		return false, false, false
	}
	state, found, err := lookupHeld()
	if err != nil {
		return false, false, false
	}
	if found {
		return state == xnuTCPClosed || state == xnuTCPTimeWait, false, true
	}
	return held, false, false
}
