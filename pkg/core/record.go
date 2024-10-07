package core

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

func (c *Core) GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	return c.Hooks.Record(ctx, id, opts)
}

func (c *Core) GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	m := make(chan *models.Mock, 500)

	err := c.Proxy.Record(ctx, id, m, opts)
	if err != nil {
		return nil, err
	}

	return m, nil
}
