package graph

import (
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.uber.org/zap"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

//go:generate go run github.com/99designs/gqlgen generate

type Resolver struct {
	logger *zap.Logger
	replay replay.Service
}
