//go:build linux

package routes

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procRoot is where the kernel's process and socket tables are read; a var so
// a test can point it at a fixture tree.
var procRoot = "/proc"

// tcpOwner finds the process (thread-group ID in the agent's PID namespace)
// owning the TCP socket bound to self and connected to peer.
func tcpOwner(self, peer netip.AddrPort) (int, error) {
	inode, err := tcpInode(self, peer)
	if err != nil {
		return 0, err
	}
	fds, err := filepath.Glob(filepath.Join(procRoot, "[0-9]*", "fd", "*"))
	if err != nil {
		return 0, err
	}
	want := "socket:[" + inode + "]"
	holders := map[int]bool{}
	for _, fd := range fds {
		link, err := os.Readlink(fd)
		if err != nil || link != want {
			continue
		}
		rel, err := filepath.Rel(procRoot, fd)
		if err != nil {
			continue
		}
		name := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		pid, err := pidOf(name)
		if err != nil {
			// A holder whose name is no process ID could be the caller
			// itself: taking another holder for it would hand the run to
			// the wrong process, and reading no holder would hide why the
			// caller is unknown.
			return 0, fmt.Errorf("the socket %s -> %s (inode %s) is held by %s, which is not a process ID: %w", self, peer, inode, filepath.Join(procRoot, name), err)
		}
		holders[pid] = true
	}
	if len(holders) == 0 {
		return 0, fmt.Errorf("no process holds the socket %s -> %s (inode %s)", self, peer, inode)
	}
	return soleHolder(self, peer, holders, procParent)
}

// pidOf reads a /proc directory's name as the process ID it is: a pid_t,
// a positive 32-bit signed integer. Parsed at 32 bits, it fits the agent's
// int and the u32 TGID the instance stamp keys on, so no later conversion
// can truncate it; a name outside that range is no process, never one whose
// low bits name another.
func pidOf(name string) (int, error) {
	v, err := strconv.ParseInt(name, 10, 32)
	if err != nil {
		return 0, err
	}
	if v <= 0 || strconv.FormatInt(v, 10) != name {
		return 0, fmt.Errorf("%q is not a process ID as the kernel writes one", name)
	}
	return int(v), nil
}

// procParent is pid's parent, from its stat in procRoot.
func procParent(pid int) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(raw[i+1:]))
	if len(f) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// tcpInode is the inode of the TCP socket bound to self and connected to peer,
// from /proc/net/tcp and tcp6.
func tcpInode(self, peer netip.AddrPort) (string, error) {
	for _, table := range []string{"net/tcp", "net/tcp6"} {
		f, err := os.Open(filepath.Join(procRoot, table))
		if err != nil {
			continue
		}
		inode, found := scanTCPTable(f, self, peer)
		_ = f.Close()
		if found {
			return inode, nil
		}
	}
	return "", fmt.Errorf("no TCP socket %s -> %s in the kernel's tables", self, peer)
}

// heldState is the TCP states, as /proc/net/tcp{,6} writes them (the kernel's
// tcp_states.h, %02X), in which a process can still hold a connected socket:
// ESTABLISHED, FIN_WAIT1, FIN_WAIT2, CLOSE_WAIT, LAST_ACK and CLOSING.
// SYN_SENT has sent no request yet, TIME_WAIT is no process's, CLOSE is a
// connection reset or never made, and LISTEN and the SYN_RECV states are a
// server's.
var heldState = map[string]bool{
	"01": true, // ESTABLISHED
	"04": true, // FIN_WAIT1
	"05": true, // FIN_WAIT2
	"08": true, // CLOSE_WAIT
	"09": true, // LAST_ACK
	"0B": true, // CLOSING
}

func scanTCPTable(f *os.File, self, peer netip.AddrPort) (string, bool) {
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		// A caller's socket is one a process can still hold: established, or
		// closed by one side or both while the caller keeps it (a caller that
		// called CloseWrite after its request, as macOS's read names it too).
		// Inode 0 is a socket no process holds any more (TIME_WAIT, or one
		// its process closed).
		if !heldState[fields[3]] || fields[9] == "0" {
			continue
		}
		l, okL := procAddrPort(fields[1])
		p, okP := procAddrPort(fields[2])
		if okL && okP && sameEndpoint(l, self) && sameEndpoint(p, peer) {
			return fields[9], true
		}
	}
	return "", false
}

// procAddrPort reads an address of /proc/net/tcp{,6}: hex address, each
// 32-bit word in host (little-endian) order, then ":" and the hex port.
func procAddrPort(s string) (netip.AddrPort, bool) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	b, err := hex.DecodeString(h)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i+4 <= len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	addr, ok := netip.AddrFromSlice(b)
	if !ok {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr, uint16(port)), true
}
