package agent

import (
	"strings"
	"testing"
)

func TestScanCgroupV2Mount(t *testing.T) {
	cases := []struct {
		name    string
		mounts  string
		wantMnt string
	}{
		{
			name: "pure v2 (unified) host",
			mounts: "proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0\n" +
				"cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev,noexec,relatime,nsdelegate 0 0\n",
			wantMnt: "/sys/fs/cgroup",
		},
		{
			name: "hybrid host: v1 controllers + v2 unified subtree",
			mounts: "tmpfs /sys/fs/cgroup tmpfs ro,nosuid,nodev,noexec 0 0\n" +
				"cgroup2 /sys/fs/cgroup/unified cgroup2 rw,nosuid,nodev,noexec,relatime 0 0\n" +
				"cgroup /sys/fs/cgroup/memory cgroup rw,nosuid,nodev,noexec,relatime,memory 0 0\n",
			wantMnt: "/sys/fs/cgroup/unified",
		},
		{
			name: "pure legacy v1 host: no cgroup2 anywhere",
			mounts: "tmpfs /sys/fs/cgroup tmpfs ro,nosuid,nodev,noexec 0 0\n" +
				"cgroup /sys/fs/cgroup/memory cgroup rw,nosuid,nodev,noexec,relatime,memory 0 0\n" +
				"cgroup /sys/fs/cgroup/cpu,cpuacct cgroup rw,nosuid,nodev,noexec,relatime,cpu,cpuacct 0 0\n",
			wantMnt: "",
		},
		{
			name:    "empty mounts table",
			mounts:  "",
			wantMnt: "",
		},
		{
			name: "cgroup2 token must be the fstype field, not the source name",
			// A v1 source literally named "cgroup2" with fstype "cgroup" must
			// not be mistaken for a v2 mount.
			mounts:  "cgroup2 /sys/fs/cgroup/net cgroup rw,net_cls 0 0\n",
			wantMnt: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanCgroupV2Mount(strings.NewReader(tc.mounts))
			if got != tc.wantMnt {
				t.Errorf("scanCgroupV2Mount() = %q, want %q", got, tc.wantMnt)
			}
		})
	}
}
