//go:build linux

package provider

import (
	"errors"

	"github.com/moby/moby/pkg/parsers/kernel"
	"go.uber.org/zap"
)

// check if keploy is compatable with underlying client os
func isCompatible(logger *zap.Logger) error {
	//check if the version of the kernel is above 5.15 for eBPF support
	isValid := kernel.CheckKernelVersion(5, 15, 0)
	if !isValid {
		c, err := kernel.GetKernelVersion()
		if err != nil {
			logger.Error("Error getting kernel version", zap.Error(err))
			return err
		}
		errMsg := "detected linux kernel version" + c.String() + ". Keploy requires linux kernel version 5.15 or above. Please upgrade your kernel or docker version.\n"
		logger.Error(errMsg)
		return errors.New(errMsg)
	}
	return nil
}
