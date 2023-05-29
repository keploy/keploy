package cmd

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	// "go.keploy.io/server/server"
)

type Root struct {
	logger *zap.Logger
}

func NewRoot() *Root {
	logCfg := zap.NewDevelopmentConfig()
	logCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	logger, err := logCfg.Build()
	if err != nil {
		log.Panic("failed to start the logger for the CLI")
		return nil
	}
	return &Root{
		logger: logger,
	}
}

// Execute adds all child commands to the root command.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	NewRoot().execute()
}

func (r *Root) execute() {
	// Root command
	var rootCmd = &cobra.Command{
		Use:   "keploy",
		Short: "Keploy CLI",
		// Run: func(cmd *cobra.Command, args []string) {

		// },
	}

	subCommands = append(subCommands, NewCmdRecord())

	// add the registered keploy plugins as subcommands of the rootCmd
	for _, sc := range subCommands {
		rootCmd.AddCommand(sc.GetCmd())
	}

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// Plugins is an interface used to define plugins.
type Plugins interface {
	GetCmd() *cobra.Command
}

// subCommands holds a list of registered plugins.
var subCommands = []Plugins{}

// RegisterPlugin registers a plugin by appending it to the list of subCommands.
func RegisterPlugin(p Plugins) {
	subCommands = append(subCommands, p)
}
