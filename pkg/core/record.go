package core

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

func (c *Core) GetIncoming(ctx context.Context, id uint64, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	return c.hook.Record(ctx, id)
}

func (c *Core) GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) (<-chan *models.Mock, <-chan error) {
	//make a new channel for the errors
	errCh := make(chan error, 10) // Buffered channel to prevent blocking
	m := make(chan *models.Mock, 500)

	ports := GetPortToSendToKernel(ctx, opts.Rules)
	if len(ports) > 0 {
		err := c.hook.PassThroughPortsInKernel(ctx, id, ports)
		if err != nil {
			errCh <- err
			return nil, errCh
		}
	}

	err := c.proxy.Record(ctx, id, m, errCh, opts)
	if err != nil {
		errCh <- err
		return nil, errCh
	}

	return m, errCh
}
