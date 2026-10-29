//go:build darwin

package proxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// readXNUPCBs reads macOS's TCP connection table (pcbtable_xnu.go). The
// kernel sizes its estimate with room for sockets opened between the two
// sysctl calls; a table that outgrew even that (ENOMEM) is read again, as
// often as it grew.
func readXNUPCBs() ([]xnuPCB, error) {
	for {
		buf, err := unix.SysctlRaw("net.inet.tcp.pcblist64")
		if errors.Is(err, unix.ENOMEM) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read macOS's TCP connection table (sysctl net.inet.tcp.pcblist64): %w", err)
		}
		return parseXNUPCBs(buf)
	}
}

// XNU's struct tcp_info (netinet/tcp.h, getsockopt IPPROTO_TCP TCP_INFO,
// 0x200): the option and the fields' offsets, unchanged since macOS 10.x.
// The SDK keeps the struct private; x/sys/unix has only TCP_CONNECTION_INFO,
// which gives no sequence numbers.
const (
	xnuTCPInfoOpt       = 0x200
	xnuTCPInfoOffState  = 0  // tcpi_state
	xnuTCPInfoOffRcvNxt = 56 // tcpi_rcv_nxt
	xnuTCPInfoMinLen    = 60
)

// xnuTCPInfo reads, from the socket fd, the next sequence number it expects
// and its TCP state, both from one copy of its tcp_info.
func xnuTCPInfo(fd int) (rcvNxt uint32, state uint8, err error) {
	var buf [512]byte
	l := uint32(len(buf))
	if err := getsockopt(fd, unix.IPPROTO_TCP, xnuTCPInfoOpt, unsafe.Pointer(&buf[0]), &l); err != nil {
		return 0, 0, fmt.Errorf("getsockopt TCP_INFO: %w", err)
	}
	if l < xnuTCPInfoMinLen {
		return 0, 0, fmt.Errorf("getsockopt TCP_INFO gave %d bytes, fewer than the %d keploy reads", l, xnuTCPInfoMinLen)
	}
	return binary.LittleEndian.Uint32(buf[xnuTCPInfoOffRcvNxt:]), buf[xnuTCPInfoOffState], nil
}

// getsockopt reads a socket option into a caller's buffer. It stays a raw
// call only for xnuTCPInfo: x/sys/unix wraps getsockopt for ints, strings
// and its own structs (TCP_CONNECTION_INFO among them), but not for XNU's
// private struct tcp_info, so there is no wrapper to read it into a buffer.
// Everything else (SO_NREAD in readUnread) goes through x/sys's wrappers.
func getsockopt(fd, level, name int, val unsafe.Pointer, vallen *uint32) error {
	_, _, errno := unix.Syscall6(
		unix.SYS_GETSOCKOPT,
		uintptr(fd),
		uintptr(level),
		uintptr(name),
		uintptr(val),
		uintptr(unsafe.Pointer(vallen)),
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// proveXNUCounts proves, once, on a loopback connection of its own, that this
// macOS gives the counts the wait reads in the layout keploy reads them: the
// table finds both ends, they began from one sequence number, the writer's
// writeSeq counts the three bytes it wrote, and the reader's rcv_nxt, in the
// table and in its tcp_info alike, counts the three it read. A layout that
// moved would read other numbers, and leaves every wait unknown.
func proveXNUCounts() error {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for the counts' proof: %w", err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		return fmt.Errorf("dial the counts' proof: %w", err)
	}
	defer c.Close()
	s, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept the counts' proof: %w", err)
	}
	defer s.Close()
	const n = 3
	if _, err := c.Write(make([]byte, n)); err != nil {
		return fmt.Errorf("write the counts' proof: %w", err)
	}
	// Read on the reader's own socket: once it returns, the bytes are in,
	// and no count the proof reads moves again.
	if _, err := io.ReadFull(s, make([]byte, n)); err != nil {
		return fmt.Errorf("read the counts' proof: %w", err)
	}
	a, ok1 := addrPortOf(c.LocalAddr())
	r, ok2 := addrPortOf(s.LocalAddr())
	if !ok1 || !ok2 {
		return errors.New("the counts' proof's addresses are not TCP")
	}
	table, err := readXNUPCBs()
	if err != nil {
		return err
	}
	w, ok1 := findXNUPCB(table, a, r)
	rd, ok2 := findXNUPCB(table, r, a)
	if !ok1 || !ok2 {
		return fmt.Errorf("macOS's TCP table does not list keploy's own loopback connection %s -> %s (writer found %v, reader %v)", a, r, ok1, ok2)
	}
	ps := readProxySide(s.(*net.TCPConn))
	switch {
	case ps.err != nil:
		return ps.err
	case w.iss != rd.irs:
		return fmt.Errorf("macOS's TCP table gives the two ends of one connection different first sequence numbers (%d, %d)", w.iss, rd.irs)
	case w.writeSeq()-w.iss-1 != n || rd.rcvNxt-rd.irs-1 != n:
		return fmt.Errorf("macOS's TCP table counts %d bytes written and %d received of the %d keploy sent", w.writeSeq()-w.iss-1, rd.rcvNxt-rd.irs-1, n)
	case ps.rcvNxt != rd.rcvNxt || ps.state != xnuTCPEstablished:
		return fmt.Errorf("the socket's tcp_info (rcv_nxt %d, state %d) disagrees with macOS's TCP table (rcv_nxt %d, state %d)", ps.rcvNxt, ps.state, rd.rcvNxt, xnuTCPEstablished)
	}
	return nil
}
