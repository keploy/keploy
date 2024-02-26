package cli

import (
	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
)

type ServiceFactory interface {
	GetService(cmd string, config config.Config) (interface{}, error)
}

type CmdConfigurator interface {
	AddFlags(cmd *cobra.Command, config *config.Config) error
	GetHelpTemplate() string
	GetExampleTemplate() string
	GetVersionTemplate() string
	ValidateFlags(cmd *cobra.Command, config *config.Config) error
}
