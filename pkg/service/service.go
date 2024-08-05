// Package service provides the service interface for the service package.
package service

import "context"

type Auth interface {
	GetToken(ctx context.Context) string
	Login(ctx context.Context) bool
}
