package cmd

import (
	"github.com/spf13/cobra"
	"go.keploy.io/server/server"
)

func RootCommand() *cobra.Command {
	var port string

	cmd := &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy is a functional testing toolkit for developers",
		Example: "keploy --port 9000",
		Run: func(cmd *cobra.Command, args []string) {
			server.Server(port)
		},
	}

	cmd.Flags().StringVarP(&port, "port", "p", "6789", "override Keploy server port")

	return cmd
}
