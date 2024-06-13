package cli

import (
	"context"

	"github.com/spf13/cobra"
)

type ServiceFactory interface {
	GetService(ctx context.Context, cmd string, teleGlobalMap map[string]interface{}) (interface{}, error)
}

type CmdConfigurator interface {
	AddFlags(cmd *cobra.Command) error
	ValidateFlags(ctx context.Context, cmd *cobra.Command) error
	Validate(ctx context.Context, cmd *cobra.Command) error
}
