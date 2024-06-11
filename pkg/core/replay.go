//go:build linux 

package core

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

func (c *Core) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error {
	ports := GetPortToSendToKernel(ctx, opts.Rules)
	if len(ports) > 0 {
		err := c.Hooks.PassThroughPortsInKernel(ctx, id, ports)
		if err != nil {
			return err
		}
	}

	err := c.Proxy.Mock(ctx, id, opts)
	if err != nil {
		return err
	}

	return nil
}
