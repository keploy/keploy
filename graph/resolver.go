package graph

//go:generate go run github.com/99designs/gqlgen

import (
	"go.keploy.io/server/pkg/service/regression"
	// "go.keploy.io/server/pkg/service/run"
	"go.keploy.io/server/pkg/service/testCase"
	"go.uber.org/zap"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.
func NewResolver(logger *zap.Logger, reg regression.Service, tcSvc testCase.Service) *Resolver {
	return &Resolver{
		logger: logger,
		// user:   user,
		tcSvc: tcSvc,
		reg:   reg,
	}
}

type Resolver struct {
	logger *zap.Logger
	// user   user.Service
	reg   regression.Service
	tcSvc testCase.Service
}
