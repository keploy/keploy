package agent

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// DetectCgroupPath returns the mount point of a cgroup2 (unified) hierarchy,
// mounting one itself if the host exposes none.
//
// keploy's network interception attaches cgroup-sock-addr / sock_ops eBPF
// programs, which the Linux kernel permits only on a cgroup **v2** hierarchy
// (cgroup v1 has no equivalent attach point). On a native v2 host — or when the
// agent's `-v /sys/fs/cgroup:/sys/fs/cgroup` bind-mount exposes a v2/hybrid
// host's hierarchy — that mount is found and returned as-is.
//
// On a pure cgroup v1 host no cgroup2 mount is visible. Because the single
// kernel-wide v2 hierarchy always exists (kernel >= 4.5) and the agent runs
// under cgroupns=host on v1 hosts, keploy mounts a view of it itself (see
// mountCgroupV2), yielding the same host-rooted attach point it gets on a v2
// host. Interception stays scoped to the target app by the pid-namespace filter
// in the BPF programs, not by the cgroup subtree, so a host-rooted attach is
// safe on shared nodes.
func DetectCgroupPath(logger *zap.Logger) (string, error) {
	path, err := findCgroupV2Mount("/proc/mounts")
	if err != nil {
		return "", err
	}
	if path != "" {
		return path, nil
	}
	return mountCgroupV2(logger)
}

// CgroupV2Available reports whether a cgroup2 (unified) hierarchy is already
// mounted, without attempting to mount one. The preflight check
// (cli/provider.isCompatible) uses it to warn early when cgroup v2 is missing.
// Compose generation makes the analogous decision via the docker package's own
// hostCgroupV2Available, which fails toward the opposite default (see there).
func CgroupV2Available() bool {
	path, err := findCgroupV2Mount("/proc/mounts")
	return err == nil && path != ""
}

// findCgroupV2Mount scans a mounts table file (e.g. /proc/mounts) and returns
// the mount point of the first cgroup2 entry, or "" if none is present.
func findCgroupV2Mount(mountsFile string) (string, error) {
	f, err := os.Open(mountsFile)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return scanCgroupV2Mount(f), nil
}

// scanCgroupV2Mount is the pure, testable core of findCgroupV2Mount.
func scanCgroupV2Mount(r io.Reader) string {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		// e.g.: cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev,noexec,relatime 0 0
		fields := strings.Split(scanner.Text(), " ")
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return fields[1]
		}
	}
	return ""
}

func GetPortToSendToKernel(_ context.Context, rules []models.BypassRule) []uint {
	// if the rule only contains port, then it should be sent to kernel
	ports := []uint{}
	for _, rule := range rules {
		if rule.Host == "" && rule.Path == "" {
			if rule.Port != 0 {
				ports = append(ports, rule.Port)
			}
		}
	}
	return ports
}
