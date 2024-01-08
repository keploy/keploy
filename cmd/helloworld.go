package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func NewCmdHelloWorld(logger *zap.Logger) *HelloWorld {
	return &HelloWorld{
		logger: logger,
	}
}

type HelloWorld struct {
	logger *zap.Logger
}

func (h *HelloWorld) GetCmd() *cobra.Command {
	// Hello World command
	var helloCmd = &cobra.Command{
		Use:     "hello-world",
		Short:   "Prints 'Hello, World!'",
		Example: "keploy hello-world",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Hello, World!")
			return nil
		},
	}

	return helloCmd
}
