package utils

import "testing"

func TestAgentNeedsElevation(t *testing.T) {
	cases := []struct {
		goos string
		want bool
	}{
		{"linux", true},    // eBPF, cgroup attach, bpffs mount
		{"darwin", false},  // userspace interception, unprivileged ports
		{"windows", false}, // has its own no-sudo command builder
		{"freebsd", false},
	}
	for _, tc := range cases {
		if got := AgentNeedsElevation(tc.goos); got != tc.want {
			t.Errorf("AgentNeedsElevation(%q) = %v, want %v", tc.goos, got, tc.want)
		}
	}
}
