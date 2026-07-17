package docker

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func TestScanForCgroupV2(t *testing.T) {
	cases := []struct {
		name   string
		mounts string
		want   bool
	}{
		{
			name:   "v2 present",
			mounts: "cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev,noexec,relatime,nsdelegate 0 0\n",
			want:   true,
		},
		{
			name:   "v2 present in a hybrid layout",
			mounts: "tmpfs /sys/fs/cgroup tmpfs ro 0 0\ncgroup2 /sys/fs/cgroup/unified cgroup2 rw 0 0\n",
			want:   true,
		},
		{
			name:   "legacy v1 only",
			mounts: "tmpfs /sys/fs/cgroup tmpfs ro 0 0\ncgroup /sys/fs/cgroup/memory cgroup rw,memory 0 0\n",
			want:   false,
		},
		{
			name:   "empty",
			mounts: "",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanForCgroupV2(strings.NewReader(tc.mounts)); got != tc.want {
				t.Errorf("scanForCgroupV2() = %v, want %v", got, tc.want)
			}
		})
	}
}

func capValues(caps []*yaml.Node) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, c.Value)
	}
	return out
}

// TestGenerateKeployAgentService_CgroupV2Mount asserts the agent service is
// granted exactly the extra privileges it needs to self-mount a cgroup2
// hierarchy on a legacy cgroup v1 host, and none of them on a cgroup v2 host.
//
// Not parallel: it toggles the package-level cgroupV2AvailableOnHost that
// GenerateKeployAgentService reads. Go runs non-parallel tests to completion
// before any t.Parallel() test resumes, so the save/restore is race-free w.r.t.
// the other generator tests in this package.
func TestGenerateKeployAgentService_CgroupV2Mount(t *testing.T) {
	orig := cgroupV2AvailableOnHost
	defer func() { cgroupV2AvailableOnHost = orig }()

	newSvc := func(opts models.SetupOptions) *yaml.Node {
		t.Helper()
		opts.KeployContainer = "keploy-agent"
		opts.AgentPort, opts.ProxyPort, opts.DnsPort = 16789, 16790, 16791
		opts.Mode = models.MODE_TEST
		node, err := (&Impl{
			logger: zap.NewNop(),
			conf:   &config.Config{},
		}).GenerateKeployAgentService(opts)
		if err != nil {
			t.Fatalf("GenerateKeployAgentService: %v", err)
		}
		return node
	}

	// Legacy cgroup v1 host: SYS_ADMIN + unconfined AppArmor so the agent can
	// mount(2) a cgroup2 hierarchy itself.
	cgroupV2AvailableOnHost = func() bool { return false }
	v1 := newSvc(models.SetupOptions{})
	if caps := mappingValue(v1, "cap_add"); !sequenceContains(caps, "SYS_ADMIN") {
		t.Errorf("v1 host: expected SYS_ADMIN in cap_add, got %s", formatSequence(caps))
	}
	secOpt := mappingValue(v1, "security_opt")
	// AppArmor must be unconfined (its default profile denies mount); seccomp is
	// NOT relaxed — the default profile already permits mount(2) with SYS_ADMIN.
	if !sequenceContains(secOpt, "apparmor:unconfined") {
		t.Errorf("v1 host: expected apparmor:unconfined, got %s", formatSequence(secOpt))
	}
	if sequenceContains(secOpt, "seccomp:unconfined") {
		t.Errorf("v1 host: seccomp:unconfined is an over-grant (SYS_ADMIN already unblocks mount in default seccomp); got %s", formatSequence(secOpt))
	}

	// Normal file-based docker on a cgroup v2 host: least-privilege — no
	// cgroup-mount SYS_ADMIN, no security_opt (this process and the agent share
	// the same cgroup view, so detection is reliable).
	cgroupV2AvailableOnHost = func() bool { return true }
	v2 := newSvc(models.SetupOptions{})
	if sequenceContains(mappingValue(v2, "cap_add"), "SYS_ADMIN") {
		t.Errorf("v2 host: SYS_ADMIN must not be granted for the cgroup mount")
	}
	if so := mappingValue(v2, "security_opt"); so != nil {
		t.Errorf("v2 host: no security_opt should be added, got %s", formatSequence(so))
	}

	// Cloud replay (in-memory compose) on a host that reports cgroup v2: the
	// grant must STILL fire, because cloud replay runs in a nested/DinD
	// environment where this process's cgroup view (the pod) can differ from the
	// agent container's, so cgroupV2AvailableOnHost() cannot rule out a
	// self-mount. Skipping the grant here is what left the agent with EPERM.
	cgroupV2AvailableOnHost = func() bool { return true }
	cloud := newSvc(models.SetupOptions{InMemoryCompose: []byte("services: {}\n")})
	if caps := mappingValue(cloud, "cap_add"); !sequenceContains(caps, "SYS_ADMIN") {
		t.Errorf("cloud replay: expected SYS_ADMIN even when host reports cgroup v2, got %s", formatSequence(caps))
	}
	cloudSecOpt := mappingValue(cloud, "security_opt")
	if !sequenceContains(cloudSecOpt, "apparmor:unconfined") {
		t.Errorf("cloud replay: expected apparmor:unconfined even when host reports cgroup v2, got %s", formatSequence(cloudSecOpt))
	}
	if sequenceContains(cloudSecOpt, "seccomp:unconfined") {
		t.Errorf("cloud replay: seccomp:unconfined is an over-grant (SYS_ADMIN already unblocks mount in default seccomp); got %s", formatSequence(cloudSecOpt))
	}
}

func TestAppendCapIfMissing(t *testing.T) {
	base := []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "BPF"},
		{Kind: yaml.ScalarNode, Value: "SYS_ADMIN"},
	}

	// Already present -> unchanged (no duplicate).
	got := appendCapIfMissing(base, "SYS_ADMIN", "dup")
	if vals := capValues(got); len(vals) != 2 {
		t.Errorf("appendCapIfMissing added a duplicate SYS_ADMIN: %v", vals)
	}

	// Missing -> appended once with the comment.
	got = appendCapIfMissing(base, "PERFMON", "needed")
	vals := capValues(got)
	if len(vals) != 3 || vals[2] != "PERFMON" {
		t.Fatalf("appendCapIfMissing did not append PERFMON: %v", vals)
	}
	if got[2].LineComment != "needed" {
		t.Errorf("appended cap missing comment: %q", got[2].LineComment)
	}
}
