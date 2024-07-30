package agent

import (
	"context"

	"go.uber.org/zap"
)

type Agent struct {
	logger *zap.Logger
}

func New() *Agent {
	return &Agent{}
}

func Instrument(ctx context.Context, opts Options) error {

	return nil
}
