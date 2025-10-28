//go:build linux

package core

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

func (c *Core) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error {

	err := c.Mock(ctx, id, opts)
	if err != nil {
		return err
	}

	return nil
}
