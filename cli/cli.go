package cli

import (
	"context"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/pkg/service/tools"
	"go.uber.org/zap"
)

type HookFunc func(context.Context, *zap.Logger, *config.Config, Services) *cobra.Command

// Registered holds the registered command hooks
var Registered map[string]HookFunc

func Register(name string, f HookFunc) {
	if Registered == nil {
		Registered = make(map[string]HookFunc)
	}
	Registered[name] = f
}

// Services holds the services required by the commands
type Services struct {
	Tools  tools.Service
	Record record.Service
	Replay replay.Service
}

func NewServices(t tools.Service) Services {
	return Services{Tools: t}
}
