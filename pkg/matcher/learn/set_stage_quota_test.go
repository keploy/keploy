package learn

import (
	"math"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// cpuQuota is the CPU bandwidth limit of this process's own cgroup v2
// cgroup, in CPUs (its cpu.max's quota over its period), and false where it
// sets none or none can be read. Its own alone, as Go reads it (go 1.27's
// internal/runtime/cgroup opens the process's own cpu.max): a quota on a
// parent alone is not the agent's GOMAXPROCS, and the staging's readers
// follow GOMAXPROCS.
func cpuQuota(t *testing.T) (float64, bool) {
	t.Helper()
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0, false
	}
	var dir string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(l, "0::") {
			dir = strings.TrimPrefix(l, "0::")
		}
	}
	if dir == "" {
		return 0, false
	}
	b, err = os.ReadFile(path.Join("/sys/fs/cgroup", dir, "cpu.max"))
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] == "max" {
		return 0, false
	}
	q, err1 := strconv.ParseFloat(f[0], 64)
	per, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || per <= 0 || q <= 0 {
		return 0, false
	}
	return q / per, true
}

// A set's staging reads with readersFor the CPUs the agent may use, where
// its cgroup sets a CPU quota (cpu.max): Go's GOMAXPROCS, which stageReaders
// reads, follows that quota (go 1.25 and later, as go.mod says), rounded up
// and at least 2, and the CPUs the agent's affinity allows, so the staging
// needs no reading of cpu.max of its own. A container with no CPU quota
// lets the agent use every CPU of its host, and readersFor those is what it
// reads with.
//
// Its limits, stated once here:
//   - It skips with no CPU quota on the process's own cgroup, or with
//     GOMAXPROCS set by hand. A GitHub runner and an e2e lane's container
//     set no quota, so it does not run in CI: it runs on a test box whose
//     job scope sets one.
//   - It reads the process's own cpu.max alone, as Go does. A quota on a
//     parent alone (a systemd slice, a pod's limit over its containers) is
//     not seen: the staging then reads with readersFor the host's CPUs,
//     inside the parent's quota. The agent's memory limit (memoryLimit) walks the
//     ancestors instead, because the two fail differently: an ancestor's
//     memory.max passed OOM-kills the agent, so the set's budget must sit
//     under the lowest one; an ancestor's CPU quota only throttles, so
//     readers past it share its CPU time, never more of it.
//   - It re-derives Go's own formula from cpu.max, so it guards against a
//     stray runtime.GOMAXPROCS call or GOMAXPROCS setting that would undo
//     the quota, not against a different rule.
//   - A fractional quota reads with what Go rounds up to, not the quota
//     rounded down: 7.5 CPUs give GOMAXPROCS 8 and 4 readers, where half
//     of cpu.max / period rounded down would give 3. The quota's CPU time
//     still bounds the readers together, so the fourth reader shares that
//     time, never more of it.
func TestTheStagingsReadersFollowTheAgentsCPUQuota(t *testing.T) {
	if v := os.Getenv("GOMAXPROCS"); v != "" {
		t.Skipf("GOMAXPROCS=%s is set by hand: Go follows no quota", v)
	}
	q, ok := cpuQuota(t)
	if !ok {
		t.Skipf("no CPU quota in this process's cgroups: the staging reads with %d readers on the %d CPUs", stageReaders(), runtime.NumCPU())
	}
	n := runtime.NumCPU()
	if c := int(math.Max(2, math.Ceil(q))); c < n {
		n = c
	}
	want := readersFor(n)
	if got := stageReaders(); got != want {
		t.Errorf("under a CPU quota of %.2f CPUs (%d CPUs on the host), the staging reads with %d readers; want %d", q, runtime.NumCPU(), got, want)
	}
	t.Logf("CPU quota %.2f CPUs, host %d CPUs, GOMAXPROCS %d: %d staging readers", q, runtime.NumCPU(), runtime.GOMAXPROCS(0), stageReaders())
}
