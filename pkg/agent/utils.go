package agent

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// detectCgroupPath returns the first-found mount point of type cgroup2
// and stores it in the cgroupPath global variable.
func DetectCgroupPath(logger *zap.Logger) (string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", err
	}
	defer func() {
		err := f.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close /proc/mounts file")
		}
	}()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// example fields: cgroup2 /sys/fs/cgroup/unified cgroup2 rw,nosuid,nodev,noexec,relatime 0 0
		fields := strings.Split(scanner.Text(), " ")
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return fields[1], nil
		}
	}

	return "", errors.New("cgroup2 not mounted")
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
