package core

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
)

type Record struct {
	Core
}

func (r *Record) GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, <-chan error) {
	return r.hook.Record(ctx, id)
}

func (r *Record) GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) (<-chan *models.Mock, <-chan error) {
	//TODO implement me
	panic("implement me")
}
