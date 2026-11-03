package utils

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// tcpListen is /proc/net/tcp's state of a listening socket.
const tcpListen = "0A"

// listenPortsOf is the TCP ports pid and its descendants listen on, read from
// /proc: each process's socket inodes (its fds), matched against the
// listening sockets of the network namespace of the first process holding
// any. Nil when none, or when nothing can be read.
func listenPortsOf(pid int) []int {
	pids := []int{pid}
	if kids, _, err := findChildPIDs(pid); err == nil {
		pids = append(pids, kids...)
	}
	inodes := map[string]bool{}
	netOf := 0
	for _, p := range pids {
		dir := filepath.Join("/proc", strconv.Itoa(p), "fd")
		fds, _ := os.ReadDir(dir) // a process gone or not ours: none
		for _, fd := range fds {
			link, _ := os.Readlink(filepath.Join(dir, fd.Name()))
			inode, ok := strings.CutPrefix(link, "socket:[")
			if !ok {
				continue
			}
			inodes[strings.TrimSuffix(inode, "]")] = true
			if netOf == 0 {
				netOf = p
			}
		}
	}
	if len(inodes) == 0 {
		return nil
	}
	var ports []int
	for _, table := range []string{"tcp", "tcp6"} {
		ports = append(ports, listeningPortsIn(filepath.Join("/proc", strconv.Itoa(netOf), "net", table), inodes)...)
	}
	slices.Sort(ports)
	return slices.Compact(ports)
}

// listeningPortsIn is the local ports of the listening sockets in the
// /proc/net/tcp-shaped file at path whose inode is one of inodes.
func listeningPortsIn(path string, inodes map[string]bool) []int {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ports []int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// sl local_address rem_address st tx:rx tr:when retrnsmt uid timeout inode
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != tcpListen || !inodes[fields[9]] {
			continue
		}
		hexPort := fields[1][strings.LastIndexByte(fields[1], ':')+1:]
		if port, err := strconv.ParseUint(hexPort, 16, 16); err == nil {
			ports = append(ports, int(port))
		}
	}
	return ports
}
