// Package service provides the service interface for the service package.
package service

import "context"

type Auth interface {
	GetToken(ctx context.Context) (string, error)
	Login(ctx context.Context) bool
}
