package core

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
)

type Replay struct {
	Core
}

func (r *Replay) MockOutgoing(ctx context.Context, id uint64, mocks []models.Frame, opts models.IncomingOptions) <-chan error {
	//TODO implement me
	panic("implement me")
}

func (r *Replay) SetMocks(ctx context.Context, id uint64, mocks []models.Frame) error {
	//TODO implement me
	panic("implement me")
}
