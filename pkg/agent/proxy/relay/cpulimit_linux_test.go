//go:build linux

package relay

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCPULimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  map[string]string
		cgroup string
		mounts func(root string) string
		want   float64
		ok     bool
	}{{
		name:   "container with --cpus 0.4 (cgroup v2, its own namespace)",
		files:  map[string]string{"cg/cpu.max": "40000 100000\n"},
		cgroup: "0::/\n",
		mounts: func(r string) string { return "2453 2452 0:28 / " + r + "/cg ro,nosuid - cgroup2 cgroup rw\n" },
		want:   0.4, ok: true,
	}, {
		name: "a pod's limit above its container's none (cgroup v2, host view)",
		files: map[string]string{
			"cg/kubepods/pod1/cpu.max":      "25000 100000\n",
			"cg/kubepods/pod1/ctr/cpu.max":  "max 100000\n",
			"cg/kubepods/cpu.max":           "max 100000\n",
			"cg/kubepods/pod1/ctr/cpu.stat": "",
		},
		cgroup: "0::/kubepods/pod1/ctr\n",
		mounts: func(r string) string { return "33 24 0:28 / " + r + "/cg rw shared:8 - cgroup2 cgroup2 rw\n" },
		want:   0.25, ok: true,
	}, {
		name: "hybrid: the cpu controller on cgroup v1",
		files: map[string]string{
			"cpu/kubepods/pod1/cpu.cfs_quota_us":  "50000\n",
			"cpu/kubepods/pod1/cpu.cfs_period_us": "100000\n",
			"cpu/kubepods/cpu.cfs_quota_us":       "-1\n",
			"cpu/kubepods/cpu.cfs_period_us":      "100000\n",
		},
		cgroup: "5:cpuacct,cpu:/kubepods/pod1\n4:memory:/kubepods/pod1\n0::/kubepods/pod1\n",
		mounts: func(r string) string {
			return "30 25 0:26 / " + r + "/unified rw - cgroup2 cgroup2 rw\n" +
				"35 25 0:31 / " + r + "/cpu rw - cgroup cgroup rw,cpu,cpuacct\n" +
				"36 25 0:32 / " + r + "/cpuset rw - cgroup cgroup rw,cpuset\n"
		},
		want: 0.5, ok: true,
	}, {
		name:   "no limit",
		files:  map[string]string{"cg/user.slice/cpu.max": "max 100000\n"},
		cgroup: "0::/user.slice\n",
		mounts: func(r string) string { return "33 24 0:28 / " + r + "/cg rw - cgroup2 cgroup2 rw\n" },
		ok:     false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeFiles(t, tc.files)
			cg := filepath.Join(root, "proc-cgroup")
			mi := filepath.Join(root, "proc-mountinfo")
			if err := os.WriteFile(cg, []byte(tc.cgroup), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mi, []byte(tc.mounts(root)), 0o644); err != nil {
				t.Fatal(err)
			}
			got, ok := cpuLimitFrom(cg, mi)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Fatalf("cpuLimitFrom = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
