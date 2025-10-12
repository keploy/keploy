package hooks

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

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
