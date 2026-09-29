//go:build linux

package relay

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// processCPULimit is the CPU bandwidth this process may use, in CPUs: the
// tightest CFS quota on its cgroup or any ancestor (cgroup v2 cpu.max, v1
// cpu.cfs_quota_us over cpu.cfs_period_us). A container run with --cpus 0.4,
// or a pod with a CPU limit, has one. ok is false when there is none, or it
// cannot be read.
func processCPULimit() (float64, bool) {
	return cpuLimitFrom("/proc/self/cgroup", "/proc/self/mountinfo")
}

// cgroupMount is one mounted cgroup hierarchy: root is the mount's root within
// the hierarchy, point where it is mounted.
type cgroupMount struct{ root, point string }

func cpuLimitFrom(cgroupFile, mountinfoFile string) (float64, bool) {
	cg, err := os.ReadFile(cgroupFile)
	if err != nil {
		return 0, false
	}
	mi, err := os.ReadFile(mountinfoFile)
	if err != nil {
		return 0, false
	}
	var v2, v1 *cgroupMount
	for _, line := range strings.Split(string(mi), "\n") {
		f := strings.Fields(line)
		sep := -1
		for i, x := range f {
			if x == "-" {
				sep = i
				break
			}
		}
		if sep < 5 || len(f) < sep+4 {
			continue
		}
		m := &cgroupMount{root: f[3], point: f[4]}
		switch fstype := f[sep+1]; {
		case fstype == "cgroup2" && v2 == nil:
			v2 = m
		case fstype == "cgroup" && v1 == nil && hasCPUController(f[sep+3]):
			v1 = m
		}
	}
	best := math.Inf(1)
	for _, line := range strings.Split(string(cg), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case parts[0] == "0" && parts[1] == "" && v2 != nil:
			walkCgroup(*v2, parts[2], func(dir string) {
				if l, ok := readCPUMax(filepath.Join(dir, "cpu.max")); ok {
					best = math.Min(best, l)
				}
			})
		case hasCPUController(parts[1]) && v1 != nil:
			walkCgroup(*v1, parts[2], func(dir string) {
				if l, ok := readCFSQuota(dir); ok {
					best = math.Min(best, l)
				}
			})
		}
	}
	if math.IsInf(best, 1) {
		return 0, false
	}
	return best, true
}

// hasCPUController reports whether a comma-separated controller list names
// the cpu controller (cpu,cpuacct), not only cpuacct or cpuset.
func hasCPUController(list string) bool {
	for _, c := range strings.Split(list, ",") {
		if c == "cpu" {
			return true
		}
	}
	return false
}

// walkCgroup calls fn for the directory of cgroup path under m and each of
// its ancestors up to the mount point: a limit can be set at any level (a
// pod's, above its container's).
func walkCgroup(m cgroupMount, path string, fn func(dir string)) {
	rel := path
	if m.root != "/" {
		if path != m.root && !strings.HasPrefix(path, m.root+"/") {
			return // not under this mount
		}
		rel = strings.TrimPrefix(path, m.root)
	}
	dir := filepath.Join(m.point, rel)
	for {
		fn(dir)
		if dir == m.point || !strings.HasPrefix(dir, m.point) {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// readCPUMax reads a cgroup v2 cpu.max ("max 100000", or "40000 100000").
func readCPUMax(file string) (float64, bool) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] == "max" {
		return 0, false
	}
	return quotaOverPeriod(f[0], f[1])
}

// readCFSQuota reads a cgroup v1 cpu controller's quota (-1: none) and period.
func readCFSQuota(dir string) (float64, bool) {
	q, err := os.ReadFile(filepath.Join(dir, "cpu.cfs_quota_us"))
	if err != nil {
		return 0, false
	}
	p, err := os.ReadFile(filepath.Join(dir, "cpu.cfs_period_us"))
	if err != nil {
		return 0, false
	}
	return quotaOverPeriod(strings.TrimSpace(string(q)), strings.TrimSpace(string(p)))
}

func quotaOverPeriod(quota, period string) (float64, bool) {
	q, err1 := strconv.ParseFloat(quota, 64)
	p, err2 := strconv.ParseFloat(period, 64)
	if err1 != nil || err2 != nil || q <= 0 || p <= 0 {
		return 0, false
	}
	return q / p, true
}
