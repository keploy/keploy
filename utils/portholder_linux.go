//go:build linux

package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// unnamedHolder is why PortTaken names no holder (PortHolder ""): the
// socket's process is in another namespace, or its /proc cannot be read.
const unnamedHolder = "by a process keploy cannot name: another network namespace's, or one it may not read"

// PortHolder names the processes of this network namespace that hold a TCP or
// UDP socket bound to port, as "pid 4242 (keploy)" (at most three, then how
// many more), or "" when it can name none: the socket's process is in another
// pid namespace, or /proc cannot be read.
func PortHolder(port uint32) string {
	inodes := map[string]bool{}
	for _, table := range []string{"tcp", "tcp6", "udp", "udp6"} {
		for ino := range socketInodesOnPort("/proc/net/"+table, port) {
			inodes["socket:["+ino+"]"] = true
		}
	}
	// /proc/<pid>/fd/<n> links to socket:[<inode>]: a link that cannot be
	// read is "", no socket's.
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	pids := map[int]bool{}
	for _, fd := range fds {
		if target, _ := os.Readlink(fd); inodes[target] {
			pid, _ := strconv.Atoi(filepath.Base(filepath.Dir(filepath.Dir(fd))))
			pids[pid] = true
		}
	}
	sorted := make([]int, 0, len(pids))
	for pid := range pids {
		sorted = append(sorted, pid)
	}
	sort.Ints(sorted)
	var named []string
	for i, pid := range sorted {
		if i == 3 {
			named = append(named, fmt.Sprintf("%d more", len(sorted)-3))
			break
		}
		comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		named = append(named, fmt.Sprintf("pid %d (%s)", pid, strings.TrimSpace(string(comm))))
	}
	return strings.Join(named, ", ")
}

// socketInodesOnPort reads a /proc/net table (tcp, tcp6, udp, udp6) and
// returns the inodes of its sockets whose local port is port. A table that
// cannot be read has none.
func socketInodesOnPort(table string, port uint32) map[string]bool {
	data, _ := os.ReadFile(table)
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		// sl local_address rem_address st tx:rx tr:tm retrnsmt uid timeout inode:
		// local_address is the kernel's HEXIP:HEXPORT.
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		local := f[1]
		if p, _ := strconv.ParseUint(local[strings.LastIndexByte(local, ':')+1:], 16, 32); uint32(p) == port {
			out[f[9]] = true
		}
	}
	return out
}
