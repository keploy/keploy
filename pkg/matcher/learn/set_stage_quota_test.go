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

// cpuQuota is the CPU bandwidth limit of the cgroup v2 hierarchy this
// process runs in, in CPUs (cpu.max's quota over its period, the lowest of
// its cgroup and their ancestors it can see), and false where none is set or
// none can be read.
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
	limit, found := math.Inf(1), false
	for p := dir; ; p = path.Dir(p) {
		if b, err := os.ReadFile(path.Join("/sys/fs/cgroup", p, "cpu.max")); err == nil {
			f := strings.Fields(string(b))
			if len(f) == 2 && f[0] != "max" {
				q, err1 := strconv.ParseFloat(f[0], 64)
				per, err2 := strconv.ParseFloat(f[1], 64)
				if err1 == nil && err2 == nil && per > 0 && q/per < limit {
					limit, found = q/per, true
				}
			}
		}
		if p == "/" || p == "." || p == "" {
			break
		}
	}
	return limit, found
}

// (mongo-stuck-handler, r3g.) A set's staging reads with half the CPUs the
// agent may use, where its cgroup sets a CPU quota (cpu.max): Go's
// GOMAXPROCS, which stageReaders halves, follows that quota (go 1.25 and
// later, as go.mod says), rounded up and at least 2, so the staging needs
// no reading of cpu.max of its own. A remote-run job's scope sets one (its
// CPUs); a lane's container sets none (lane-container.sh runs docker with
// --memory alone), so there the agent may use every CPU of the box, and
// half of them is what it reads with. The row reads the quota itself and
// compares; with no quota, or with GOMAXPROCS set by hand, it says so and
// stops.
func TestTheStagingsReadersAreHalfTheAgentsCPUQuota(t *testing.T) {
	if v := os.Getenv("GOMAXPROCS"); v != "" {
		t.Skipf("GOMAXPROCS=%s is set by hand: Go follows no quota", v)
	}
	q, ok := cpuQuota(t)
	if !ok {
		t.Skipf("no CPU quota in this process's cgroups: the staging reads with half of the %d CPUs (%d)", runtime.NumCPU(), halfTheCPUs())
	}
	n := runtime.NumCPU()
	if c := int(math.Max(2, math.Ceil(q))); c < n {
		n = c
	}
	want := n / 2
	if want < 1 {
		want = 1
	}
	if got := halfTheCPUs(); got != want {
		t.Errorf("under a CPU quota of %.2f CPUs (%d CPUs on the host), the staging reads with %d readers; want %d", q, runtime.NumCPU(), got, want)
	}
	t.Logf("CPU quota %.2f CPUs, host %d CPUs, GOMAXPROCS %d: %d staging readers", q, runtime.NumCPU(), runtime.GOMAXPROCS(0), halfTheCPUs())
}
