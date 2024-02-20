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
	//make a new channel for the errors
	errCh := make(chan error, 10) // Buffered channel to prevent blocking
	m := make(chan *models.Mock, 500)

	err := r.proxy.Record(ctx, id, m, opts)
	if err != nil {
		errCh <- err
		return nil, errCh
	}

	return m, errCh
}
