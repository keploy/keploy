package core

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
)

func (c *Core) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) <-chan error {
	//make a new channel for the errors
	errCh := make(chan error, 10) // Buffered channel to prevent blocking

	err := c.proxy.Mock(ctx, id, opts)
	if err != nil {
		errCh <- err
		return errCh
	}

	return errCh
}

func (c *Core) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	err := c.proxy.SetMocks(ctx, id, filtered, unFiltered)
	if err != nil {
		c.logger.Error("Failed to set mocks")
		return err
	}
	return nil
}
