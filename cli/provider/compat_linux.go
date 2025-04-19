//go:build linux

package provider

import (
	"errors"

	"github.com/moby/moby/pkg/parsers/kernel"
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
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		if os.IsNotExist(err) {
			logger.Error("Cgroup v2 is not supported")
			return errors.New("cgroup v2 is not supported")
		}
	} else {
		logger.Info("Cgroup v2 is supported")
	}
	return nil
}
