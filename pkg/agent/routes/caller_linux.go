//go:build linux

package routes

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// procRoot is where the kernel's process and socket tables are read; a var so
// a test can point it at a fixture tree.
var procRoot = "/proc"

// callerPID is the process (thread-group ID in the agent's PID namespace) that
// owns the client end of the request's TCP connection: the socket whose local
// address is the request's remote address and whose remote address is the
// server's own. It is read from the kernel's socket tables, never from what
// the caller says.
func callerPID(r *http.Request) (int, error) {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return 0, fmt.Errorf("the connection's remote address %q: %w", r.RemoteAddr, err)
	}
	la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || la == nil {
		return 0, errors.New("the connection's local address is unknown")
	}
	local, err := netip.ParseAddrPort(la.String())
	if err != nil {
		return 0, fmt.Errorf("the connection's local address %q: %w", la.String(), err)
	}
	return tcpOwner(remote, local)
}

// tcpOwner finds the process owning the TCP socket bound to self and
// connected to peer.
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
		pid, err := strconv.Atoi(strings.SplitN(filepath.ToSlash(rel), "/", 2)[0])
		if err != nil {
			continue
		}
		holders[pid] = true
	}
	if len(holders) == 0 {
		return 0, fmt.Errorf("no process holds the socket %s -> %s (inode %s)", self, peer, inode)
	}
	// A child the caller is starting (between its fork and its exec) holds
	// a copy of the caller's socket for a moment: the holder every other one
	// descends from is the caller. A socket processes share otherwise
	// (passed, or inherited by a child that kept it) names no one caller.
	for pid := range holders {
		all := true
		for other := range holders {
			if other != pid && !descendsFrom(other, pid) {
				all = false
				break
			}
		}
		if all {
			return pid, nil
		}
	}
	pids := make([]int, 0, len(holders))
	for pid := range holders {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return 0, fmt.Errorf("the socket %s -> %s is held by processes %v", self, peer, pids)
}

// descendsFrom reports whether pid is a descendant of ancestor, from the
// parent chain in procRoot.
func descendsFrom(pid, ancestor int) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
		if err != nil {
			return false
		}
		i := strings.LastIndexByte(string(raw), ')')
		if i < 0 {
			return false
		}
		f := strings.Fields(string(raw[i+1:]))
		if len(f) < 2 {
			return false
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			return false
		}
		if ppid == ancestor {
			return true
		}
		pid = ppid
	}
	return false
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

func scanTCPTable(f *os.File, self, peer netip.AddrPort) (string, bool) {
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		// Only an established connection with a socket (inode 0 is one in
		// TIME_WAIT, owned by no process) is a caller.
		if fields[3] != "01" || fields[9] == "0" {
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

// sameEndpoint compares two endpoints with IPv4-mapped IPv6 read as IPv4.
func sameEndpoint(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap() == b.Addr().Unmap()
}
