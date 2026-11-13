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
	"sync"
)

// memoryLimit is the agent's memory limit in bytes, 0 where none is known:
// the least of the Go runtime's (GOMEMLIMIT, or the one the agent's memory
// guard sets), its cgroup's and each of its parents' (memory.max, or
// memory.limit_in_bytes under cgroup v1), and the machine's memory. It
// walks the parents where the staging's readers (stageReaders) do not:
// those follow GOMAXPROCS, which reads the process's own cpu.max alone. The
// two fail differently. A parent's memory.max passed OOM-kills the agent,
// so a set's budget must sit under the lowest one; a parent's CPU quota
// only throttles, and readers past it share its CPU time, never more.
func memoryLimit() int64 {
	return memoryLimitAt(debug.SetMemoryLimit(-1), "/proc/self/cgroup", "/sys/fs/cgroup", "/proc/meminfo")
}

// holdMu makes holdHeap's read of the runtime's limit and its setting one.
var holdMu sync.Mutex

// holdHeap holds the agent's Go heap under half of its memory limit, where
// nothing set it a limit, as a set starts (setBudget). The agent shares
// that limit with what it serves: a native run's app, browser and
// databases sit in its cgroup. Without a limit the Go heap grows to twice
// what it keeps before it collects (GOGC 100), the whole agent's and not
// only a set's: on enterprise-ui's atg-features replay, a set of 4 GiB (a
// quarter of a 20 GiB container, at the 4 GiB cap) and 1.1 GB more of the
// agent's own grew a heap of 10 GB (13.2 GB resident), and in the lane's
// 12 GiB container the kernel killed the run. The hold is a Go limit like
// any other, so the set's budget is a quarter of it (budgetOf). A limit is
// a soft one: a heap that keeps more grows past it, the collector's share
// of the CPU capped (runtime/debug.SetMemoryLimit), never stopped.
func holdHeap() {
	holdHeapWith(os.Getenv("GOMEMLIMIT"), memoryLimit)
}

// holdHeapWith is holdHeap with GOMEMLIMIT's text env and the agent's
// memory limit read by limit, for the rows. Once it is set the runtime has
// a limit, so a later set holds nothing more; a limit set after it (the
// memory guard's) replaces it.
func holdHeapWith(env string, limit func() int64) {
	holdMu.Lock()
	defer holdMu.Unlock()
	if h := heapHoldFor(env, debug.SetMemoryLimit(-1), limit()); h > 0 {
		debug.SetMemoryLimit(h)
	}
}

// heapHoldFor is the hold holdHeap sets, 0 for none: half of limit (the
// agent's memory limit, 0 where none is known, so none), where GOMEMLIMIT's
// text env is empty and the runtime's limit cur is none (math.MaxInt64). A
// limit the user or the memory guard set is theirs, and stays.
func heapHoldFor(env string, cur, limit int64) int64 {
	if env != "" || cur != math.MaxInt64 {
		return 0
	}
	return limit / 2
}

// memoryLimitAt is memoryLimit with the Go runtime's limit goLimit
// (math.MaxInt64: none) and the cgroup and memory files read at these
// paths, for the rows.
func memoryLimitAt(goLimit int64, procCgroup, cgroupRoot, meminfo string) int64 {
	limit := int64(math.MaxInt64)
	if goLimit > 0 {
		limit = goLimit
	}
	if c := cgroupLimit(procCgroup, cgroupRoot); c > 0 && c < limit {
		limit = c
	}
	if m := machineMemory(meminfo); m > 0 && m < limit {
		limit = m
	}
	if limit == math.MaxInt64 {
		return 0
	}
	return limit
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
			if err != nil || kb <= 0 {
				return 0
			}
			return kb << 10
		}
	}
	return 0
}
