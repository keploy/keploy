//go:build linux

package proxy

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func listenerOwner(ip net.IP, port uint32) (int, bool) {
	inodes := listenInodes(ip, port)
	if len(inodes) == 0 {
		return 0, false
	}
	procs, err := filepath.Glob("/proc/[0-9]*/fd/*")
	if err != nil {
		return 0, false
	}
	for _, fd := range procs {
		link, err := os.Readlink(fd)
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] {
			pid, err := strconv.Atoi(strings.Split(fd, "/")[2])
			if err == nil {
				return pid, true
			}
		}
	}
	return 0, false
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
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return false
		}
		s := string(raw)
		end := strings.LastIndexByte(s, ')')
		if end < 0 || end+2 >= len(s) {
			return false
		}
		fields := strings.Fields(s[end+2:])
		if len(fields) < 2 {
			return false
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return false
		}
		pid = ppid
	}
	return pid == ancestor
}
