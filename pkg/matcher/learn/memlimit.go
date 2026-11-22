package learn

import (
	"bufio"
	"bytes"
	"math"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
)

// memoryLimit is the agent's memory limit in bytes, 0 where none is known:
// the least of the Go runtime's (GOMEMLIMIT, or the one the agent's memory
// guard sets), its cgroup's and each of its parents' (memory.max, or
// memory.limit_in_bytes under cgroup v1 on Linux), and the machine's physical
// memory (read natively on every OS: /proc/meminfo on Linux, sysctl hw.memsize
// on macOS, GlobalMemoryStatusEx on Windows). It walks the parents where the
// staging's readers (stageReaders) do not: those follow GOMAXPROCS, which
// reads the process's own cpu.max alone. The two fail differently. A parent's
// memory.max passed OOM-kills the agent, so a set's budget must sit under the
// lowest one; a parent's CPU quota only throttles, and readers past it share
// its CPU time, never more.
func memoryLimit() int64 {
	return memoryLimitFrom(debug.SetMemoryLimit(-1), processCgroupLimit(), hostMemory())
}

// memoryLimitFrom returns the least positive limit among goLimit
// (math.MaxInt64: none), cgroup (0: none), and machine (0: unknown), or 0
// when no limit is known.
func memoryLimitFrom(goLimit, cgroup, machine int64) int64 {
	limit := int64(math.MaxInt64)
	known := false
	if goLimit > 0 && goLimit < math.MaxInt64 {
		limit = goLimit
		known = true
	}
	if cgroup > 0 && (!known || cgroup < limit) {
		limit = cgroup
		known = true
	}
	if machine > 0 && (!known || machine < limit) {
		limit = machine
		known = true
	}
	if !known {
		return 0
	}
	return limit
}

// memoryLimitAt is memoryLimit with the Go runtime's limit goLimit
// (math.MaxInt64: none) and the cgroup and memory files read at these
// paths, for the rows.
func memoryLimitAt(goLimit int64, procCgroup, cgroupRoot, meminfo string) int64 {
	return memoryLimitFrom(goLimit, cgroupLimit(procCgroup, cgroupRoot), machineMemory(meminfo))
}

// clampMemoryBytes converts a positive unsigned byte count from an OS query
// into an int64, returning 0 for 0 and math.MaxInt64 if it exceeds MaxInt64.
func clampMemoryBytes(n uint64) int64 {
	if n == 0 {
		return 0
	}
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}

// cgroupLimit is the least memory limit of the process's cgroup and its
// parents, read under root, 0 where none is set or readable. A cgroup v2
// line ("0::/path") reads memory.max; a v1 memory line ("N:...memory...:/path")
// reads memory.limit_in_bytes under root's memory hierarchy.
func cgroupLimit(procCgroup, root string) int64 {
	data, err := os.ReadFile(procCgroup)
	if err != nil {
		return 0
	}
	var least int64
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), ":", 3)
		if len(f) != 3 {
			continue
		}
		var dir, file string
		switch {
		case f[0] == "0" && f[1] == "":
			dir, file = root, "memory.max"
		case hasController(f[1], "memory"):
			dir, file = path.Join(root, "memory"), "memory.limit_in_bytes"
		default:
			continue
		}
		for p := path.Clean("/" + f[2]); ; p = path.Dir(p) {
			if n := limitIn(path.Join(dir, p, file)); n > 0 && (least == 0 || n < least) {
				least = n
			}
			if p == "/" {
				break
			}
		}
	}
	return least
}

func hasController(list, name string) bool {
	for _, c := range strings.Split(list, ",") {
		if c == name {
			return true
		}
	}
	return false
}

// limitIn is the limit a cgroup memory file states, 0 where it states none
// ("max", or v1's page-rounded largest value) or cannot be read.
func limitIn(file string) int64 {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n <= 0 || n >= math.MaxInt64&^4095 {
		return 0
	}
	return n
}

// machineMemory is MemTotal of meminfo in bytes, 0 where it cannot be read.
func machineMemory(meminfo string) int64 {
	data, err := os.ReadFile(meminfo)
	if err != nil {
		return 0
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil || kb <= 0 || kb > math.MaxInt64>>10 {
				return 0
			}
			return kb << 10
		}
	}
	return 0
}
