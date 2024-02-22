package graph

//go:generate go run github.com/99designs/gqlgen generate

import (
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.uber.org/zap"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.
var Emoji = "\U0001F430" + " Keploy:"

type Resolver struct {
	Tester         replay.Service
	Logger         *zap.Logger
	ServeTest      bool
}
