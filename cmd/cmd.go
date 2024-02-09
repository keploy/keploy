package cmd

import (
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// create interface for cobra commands and subcommands
type Cmd interface {
	GetCmd(*zap.Logger) *cobra.Command
}

// create global array of registered commands
var RegisteredCmds []Cmd
