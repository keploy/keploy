package graph

//go:generate go run github.com/99designs/gqlgen

import (
	"go.keploy.io/server/pkg/service/regression"
	"go.keploy.io/server/pkg/service/run"
	"go.uber.org/zap"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

func NewResolver(logger *zap.Logger, run run.Service, reg regression.Service) *Resolver {
	return &Resolver{
		logger: logger,
		// user:   user,
		run: run,
		reg: reg,
	}
}

type Resolver struct {
	logger *zap.Logger
	// user   user.Service
	run run.Service
	reg regression.Service
}
