// Package graph provides the resolver implementation for the GraphQL schema.
package graph

import (
	"context"

	"go.keploy.io/server/v2/pkg/service/replay"
	"go.uber.org/zap"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

//go:generate go run github.com/99designs/gqlgen generate

type Resolver struct {
	logger     *zap.Logger
	replay     replay.Service
	hookCtx    context.Context
	hookCancel context.CancelFunc
	appCtx     context.Context
	appCancel  context.CancelFunc
}

func (r *Resolver) getHookCtxWithCancel() (context.Context, context.CancelFunc) {
	return r.hookCtx, r.hookCancel
}

func (r *Resolver) getAppCtxWithCancel() (context.Context, context.CancelFunc) {
	return r.appCtx, r.appCancel
}
