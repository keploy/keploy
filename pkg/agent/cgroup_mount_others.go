//go:build !linux

package agent

import (
	"errors"

	"go.uber.org/zap"
)

// mountCgroupV2 is a stub on non-Linux platforms, where keploy's eBPF
// interception (and therefore cgroup v2) does not apply.
func mountCgroupV2(_ *zap.Logger) (string, error) {
	return "", errors.New("cgroup2 not mounted: mounting a cgroup2 hierarchy is only supported on Linux")
}
