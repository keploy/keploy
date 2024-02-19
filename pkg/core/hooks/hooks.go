package hooks

import (
	"context"
	"fmt"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func NewHooks() *Hooks {
	return &Hooks{}
}

type Hooks struct {
}

func (h *Hooks) Load(ctx context.Context, tests chan models.Frame, opts core.HookOptions) error {
	proxyIp, err := IPv4ToUint32(a.KeployIPv4Addr())
	if err != nil {
		return fmt.Errorf("failed to convert ip string:[%v] to 32-bit integer", newProxyIpString)
	}

	proxyPort := h.GetProxyPort()
	err = h.SendProxyInfo(proxyIp, proxyPort, [4]uint32{0000, 0000, 0000, 0001})
	if err != nil {
		a.logger.Error("failed to send new proxy ip to kernel", zap.Any("NewProxyIp", proxyIp))
		return err
	}
}

func (h *Hooks) GetProxyPort() uint16 {
	return 0
}
