package core

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
)

func (c *Core) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error {
	ports := GetPortToSendToKernel(ctx, opts.Rules)
	if len(ports) > 0 {
		err := c.hook.PassThroughPortsInKernel(ctx, id, ports)
		if err != nil {
			return err
		}
	}

	err := c.proxy.Mock(ctx, id, opts)
	if err != nil {
		return err
	}

	return nil
}

func (c *Core) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	err := c.proxy.SetMocks(ctx, id, filtered, unFiltered)
	if err != nil {
		utils.LogError(c.logger, nil, "failed to set mocks")
		return err
	}
	return nil
}
