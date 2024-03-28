// Package cli provides functionality for the command-line interface of the application.
package cli

import (
	"context"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

type HookFunc func(context.Context, *zap.Logger, *config.Config, ServiceFactory, CmdConfigurator) *cobra.Command

// Registered holds the registered command hooks
var Registered map[string]HookFunc

func Register(name string, f HookFunc) {
	if Registered == nil {
		Registered = make(map[string]HookFunc)
	}
	Registered[name] = f
}
