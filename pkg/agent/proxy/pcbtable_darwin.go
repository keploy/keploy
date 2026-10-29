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
	l, err := readTCPInfo(fd, buf[:])
	if err != nil {
		return 0, 0, fmt.Errorf("getsockopt TCP_INFO: %w", err)
	}
	if l < xnuTCPInfoMinLen {
		return 0, 0, fmt.Errorf("getsockopt TCP_INFO gave %d bytes, fewer than the %d keploy reads", l, xnuTCPInfoMinLen)
	}
	return binary.LittleEndian.Uint32(buf[xnuTCPInfoOffRcvNxt:]), buf[xnuTCPInfoOffState], nil
}

// readTCPInfo copies the socket fd's struct tcp_info into buf, and says how
// many bytes of it the kernel gave. Its rows hand the counts a socket that
// received a segment twice.
var readTCPInfo = func(fd int, buf []byte) (uint32, error) {
	l := uint32(len(buf))
	if err := getsockopt(fd, unix.IPPROTO_TCP, xnuTCPInfoOpt, unsafe.Pointer(&buf[0]), &l); err != nil {
		return 0, err
	}
	return l, nil
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
// macOS gives the counts the waits read in the layout keploy reads them
// (checkXNUCounts says what is checked). A layout that moved would read other
// numbers, and leaves every wait unknown.
func proveXNUCounts() error { return proveXNUCountsFrom(readXNUPCBs) }

// proveXNUCountsFrom is proveXNUCounts reading macOS's TCP table through
// readTable: its rows hand it a table read in a moved layout.
func proveXNUCountsFrom(readTable func() ([]xnuPCB, error)) error {
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
	table, err := readTable()
	if err != nil {
		return err
	}
	return checkXNUCounts(table, a, r, readProxySide(s.(*net.TCPConn)), n)
}
