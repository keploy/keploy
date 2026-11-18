//go:build linux

package proxy

import (
	"bufio"
	"encoding/hex"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/utils"
)

// callerOf is the process that made a redirected connection, by its number in
// the agent's own PID namespace, the one the agent's /proc (listenerOwner,
// descends) names processes by: the eBPF redirect's AgentNsPid. The kernel's
// number (KernelPid) names another process, or none, there when the agent
// runs in a PID namespace of its own (keploy in a container without the
// host's PID namespace), so a test's call to an app it started never
// descended from it and was never recorded as a test case. unnamed is true
// when the kernel named the caller but not in the agent's namespace: a
// process outside it, which the agent cannot place in the run's tree.
func callerOf(dest *agent.NetworkAddress) (caller int, unnamed bool) {
	return int(dest.AgentNsPid), dest.AgentNsPid == 0 && dest.KernelPid != 0
}

// listenerOwner is a process that holds the socket listening at ip:port, by
// its number in the agent's /proc, and whether anything listens there. It is
// asked for every connection a mock run's proxy accepts to a listener of
// this machine (serveTreeListener).
func listenerOwner(ip net.IP, port uint32) (int, bool) {
	return owners.owner(ip, port)
}

// owners remembers the holder listenerOwner found for each listener.
var owners = newOwnerCache()

// ownerCacheMax bounds how many listeners an ownerCache remembers. Past it,
// it forgets them all and finds each again by a walk: a run addresses a few
// listeners, but one restarted per test is a new one each time.
const ownerCacheMax = 256

// ownerCache remembers, for each set of sockets listening at an address,
// the holder a walk of every process's fd table found and the fd it holds
// the socket at. A connection to the same listener then reads that one fd
// link, not the fd table of every process on the machine: the walk read
// every fd of every process for each connection, a cost that grows with
// everything else the machine runs (about a fifth of a mock replay agent's
// CPU on a busy machine).
//
// A remembered holder is the answer only while that fd still names that
// socket, so it is a holder of the listener as the connection arrives; one
// that closed the fd, or exited, sends the next connection to a walk. When
// several processes hold the socket, the answer stays the one the walk
// found while it holds it, where the walk took the first in /proc order
// each time: either is a holder. A listener no process visible here holds
// (its process is in another PID namespace) is walked for each connection,
// as before.
type ownerCache struct {
	mu    sync.Mutex
	held  map[string]heldAt // by listenKey of the listening sockets
	walks int               // walks done, for the rows that pin one per listener
}

// heldAt is where a process holds a socket: pid's fd at path names socket
// inode. The zero heldAt holds nothing.
type heldAt struct {
	pid   int
	path  string
	inode string
}

func newOwnerCache() *ownerCache {
	return &ownerCache{held: map[string]heldAt{}}
}

func (c *ownerCache) owner(ip net.IP, port uint32) (int, bool) {
	inodes := listenInodes(ip, port)
	if len(inodes) == 0 {
		return 0, false
	}
	key := listenKey(inodes)
	c.mu.Lock()
	h := c.held[key]
	c.mu.Unlock()
	if h.holds() {
		return h.pid, true
	}
	// A walk that finds no holder leaves the zero heldAt, which holds
	// nothing: the next connection walks again.
	h = c.walk(inodes)
	c.mu.Lock()
	if len(c.held) >= ownerCacheMax {
		c.held = map[string]heldAt{}
	}
	c.held[key] = h
	c.mu.Unlock()
	return h.pid, true
}

// holds says whether h's fd still names its socket.
func (h heldAt) holds() bool {
	link, _ := os.Readlink(h.path)
	return link == "socket:["+h.inode+"]"
}

// listenKey names a set of listening socket inodes, whatever their order.
func listenKey(inodes map[string]bool) string {
	keys := make([]string, 0, len(inodes))
	for inode := range inodes {
		keys = append(keys, inode)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// walk reads every process's fd table, in /proc's name order as
// filepath.Glob lists it, for the first fd that names one of inodes; the
// zero heldAt when none does.
func (c *ownerCache) walk(inodes map[string]bool) heldAt {
	c.mu.Lock()
	c.walks++
	c.mu.Unlock()
	links := make(map[string]string, len(inodes))
	for inode := range inodes {
		links["socket:["+inode+"]"] = inode
	}
	for _, proc := range procNames("/proc") {
		pid, err := strconv.Atoi(proc)
		if err != nil {
			continue
		}
		dir := "/proc/" + proc + "/fd/"
		for _, fd := range procNames(dir) {
			link, _ := os.Readlink(dir + fd)
			if inode, ok := links[link]; ok {
				return heldAt{pid: pid, path: dir + fd, inode: inode}
			}
		}
	}
	return heldAt{}
}

// procNames is dir's entries sorted by name, none when it cannot be read.
// What it read before an error counts, as filepath.Glob takes it: a process
// that exits mid-read ends its own table, not the walk.
func procNames(dir string) []string {
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	names, _ := f.Readdirnames(-1)
	sort.Strings(names)
	return names
}

func listening(ip net.IP, port uint32) bool {
	return len(listenInodes(ip, port)) > 0
}

func listenInodes(ip net.IP, port uint32) map[string]bool {
	out := map[string]bool{}
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan()
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			addr, p, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			n, err := strconv.ParseUint(p, 16, 32)
			if err != nil || uint32(n) != port {
				continue
			}
			if local := procIP(addr); local != nil && (local.IsUnspecified() || local.Equal(ip) || (ip.IsLoopback() && local.IsLoopback())) {
				out[fields[9]] = true
			}
		}
		f.Close()
	}
	return out
}

func procIP(h string) net.IP {
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil
	}
	for i := 0; i+4 <= len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return net.IP(b)
}

func descends(pid, ancestor int) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		if pid == ancestor {
			return true
		}
		st, ok := utils.ReadProcStat(pid)
		if !ok {
			return false
		}
		pid = st.PPID
	}
	return pid == ancestor
}
