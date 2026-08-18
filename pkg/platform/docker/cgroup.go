package docker

import (
	"bufio"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// cgroupV2AvailableOnHost reports whether the host generating the compose
// already has a cgroup2 hierarchy mounted. It is a package var so tests can
// exercise both the v1 and v2 branches of GenerateKeployAgentService without
// depending on the test host's /proc/mounts.
var cgroupV2AvailableOnHost = hostCgroupV2Available

// hostCgroupV2Available reports whether a cgroup2 (unified) hierarchy is mounted
// on the host generating the compose. On a pure cgroup v1 host none is present,
// so the keploy-agent must mount one itself at runtime (see
// agent.DetectCgroupPath) — which needs CAP_SYS_ADMIN and an unconfined
// seccomp/AppArmor profile. Those are granted to the agent service only in that
// case, so the agent stays least-privileged on the common cgroup v2 host.
func hostCgroupV2Available() bool {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		// /proc/mounts is unreadable or absent (e.g. the CLI host is macOS /
		// Windows, where Docker runs in a Linux VM). Assume v2 and do not
		// broaden the agent's privileges; the agent's own runtime check emits a
		// precise error if a mount turns out to be required.
		return true
	}
	defer func() { _ = f.Close() }()
	return scanForCgroupV2(f)
}

// scanForCgroupV2 is the pure, testable core of hostCgroupV2Available.
func scanForCgroupV2(r io.Reader) bool {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), " ")
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return true
		}
	}
	return false
}

// appendCapIfMissing appends a cap_add entry unless the capability is already
// present, so a host-specific grant (e.g. SYS_ADMIN for the cgroup2 mount) does
// not duplicate one added for another reason (e.g. the channel-binding shim).
func appendCapIfMissing(caps []*yaml.Node, name, comment string) []*yaml.Node {
	for _, c := range caps {
		if c.Kind == yaml.ScalarNode && c.Value == name {
			return caps
		}
	}
	return append(caps, &yaml.Node{Kind: yaml.ScalarNode, Value: name, LineComment: comment})
}
