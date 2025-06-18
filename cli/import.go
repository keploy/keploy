package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	toolsSvc "go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("import", Import)
}

func Import(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {

	var importCmd = &cobra.Command{
		Use:     "import",
		Short:   "import postman collection to Keploy tests",
		Example: "keploy import",
		RunE: func(cmd *cobra.Command, _ []string) error {
			disableAnsi, _ := (cmd.Flags().GetBool("disable-ansi"))
			provider.PrintLogo(disableAnsi)
			return cmd.Help()
		},
	}

	var postmanCmd = &cobra.Command{
		Use:     "postman",
		Short:   "import postman collection to Keploy tests",
		Example: "keploy import postman",
		RunE: func(cmd *cobra.Command, _ []string) error {
			disableAnsi, _ := (cmd.Flags().GetBool("disable-ansi"))
			provider.PrintLogo(disableAnsi)
			path, _ := cmd.Flags().GetString("path")
			if path == "" {
				path = "output.json"
			}
			basePath, _ := cmd.Flags().GetString("base-path")
			fileType, _ := cmd.Flags().GetString("type")//new line
			svc, err := serviceFactory.GetService(ctx, "import")
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			var tools toolsSvc.Service
			var ok bool
			if tools, ok = svc.(toolsSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy tools service interface")
				return nil
			}
			fileType, _ := cmd.Flags().GetString("type")
			err = tools.Import(ctx, path, basePath)
			if err != nil {
				utils.LogError(logger, err, "failed to import Postman collection")
			}
			return nil
		},
	}
	importCmd.AddCommand(postmanCmd)

	for _, subCmd := range importCmd.Commands() {
		err := cmdConfigurator.AddFlags(subCmd)
		err = tools.Import(ctx, path, basePath, fileType)//new line
		if err != nil {
			utils.LogError(logger, err, "failed to add flags to command", zap.String("command", subCmd.Name()))
		}
	}

	return importCmd
}
