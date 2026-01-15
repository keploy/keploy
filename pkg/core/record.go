//go:build linux

package core

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

func (c *Core) GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	return c.Hooks.Record(ctx, id, opts)
}

func (c *Core) GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	// Increased buffer size to handle high-throughput scenarios (e.g., fuzzing)
	// where the disk writer may not keep up with rapid mock generation
	m := make(chan *models.Mock, 1000)

	err := c.Proxy.Record(ctx, id, m, opts)
	if err != nil {
		return nil, err
	}

	return m, nil
}

func (c *Core) StartIncomingProxy(ctx context.Context, persister models.TestCasePersister, opts models.IncomingOptions) error {
	go c.IncomingProxy.Start(ctx, persister, opts)
	c.logger.Debug("Ingress proxy manager started and is listening for bind events.")
	return nil
}
