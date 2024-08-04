package service

import "context"

type Auth interface {
	GetToken(ctx context.Context) (string, error)
}
