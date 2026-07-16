//go:build linux

package provider

import (
	"errors"

	"github.com/moby/moby/pkg/parsers/kernel"
	"go.keploy.io/server/v3/pkg/agent"
	"go.uber.org/zap"
)

func isCompatible(logger *zap.Logger) error {
	//check if the version of the kernel is above 5.10 for eBPF support
	isValid := kernel.CheckKernelVersion(5, 10, 0)
	if !isValid {
		c, err := kernel.GetKernelVersion()
		if err != nil {
			logger.Error("Error getting kernel version", zap.Error(err))
			return err
		}
		errMsg := "detected linux kernel version" + c.String() + ". Keploy requires linux kernel version 5.10 or above. Please upgrade your kernel or docker version.\n"
		logger.Error(errMsg)
		return errors.New(errMsg)
	}

	// keploy's eBPF interception attaches cgroup-sock-addr / sock_ops programs,
	// which the kernel permits only on a cgroup v2 (unified) hierarchy. Warn
	// early — before any image pull — when none is mounted. This is non-fatal:
	// on a legacy cgroup v1 host the agent mounts a cgroup2 hierarchy itself at
	// runtime (keploy grants it CAP_SYS_ADMIN for docker/compose runs), so we
	// let the run proceed and surface a precise error only if that mount fails.
	if !agent.CgroupV2Available() {
		logger.Warn("cgroup v2 (unified hierarchy) not detected; keploy's eBPF agent requires it and will attempt to mount a cgroup2 hierarchy at runtime. If startup fails with 'cgroup2 not mounted', switch the host to unified cgroup v2 (boot with systemd.unified_cgroup_hierarchy=1) or mount a cgroup2 hierarchy before running keploy.")
	}
	return nil
}
