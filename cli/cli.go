// Package cli provides functionality for the command-line interface of the application.
package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// HookFunc builds a registered command. Root calls it when it builds the
// root command, after every init of the program has run, so a command that
// exists only in some processes (only under the proven model, say) decides
// there, not when it registers: a hook returns nil when its command does not
// exist in this process, and Root then adds none.
type HookFunc func(context.Context, *zap.Logger, *config.Config, ServiceFactory, CmdConfigurator) *cobra.Command

// Registered holds the registered command hooks
var Registered map[string]HookFunc

func Register(name string, f HookFunc) {
	if Registered == nil {
		Registered = make(map[string]HookFunc)
	}
	Registered[name] = f
}
