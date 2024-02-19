package core

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
)

type Record struct {
	Core
}

func (r *Record) GetIncoming(ctx context.Context, id int, opts models.IncomingOptions) (chan models.Frame, error) {
	//TODO implement me
	panic("implement me")
}

func (r *Record) GetOutgoing(ctx context.Context, id int, opts models.OutgoingOptions) (chan models.Frame, error) {
	//TODO implement me
	panic("implement me")
}
