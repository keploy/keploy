package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	embedSvc "go.keploy.io/server/v2/pkg/service/embed"
	"go.keploy.io/server/v2/utils"

	"go.uber.org/zap"
)

func init() {
	Register("embed", EmbedCommand)
}

func EmbedCommand(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "embed",
		Short:   "generate embeddings for source code using AI",
		Example: `keploy embed --source-path="/project"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return nil
			}
			var embedService embedSvc.Service
			var ok bool
			if embedService, ok = svc.(embedSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy embed service interface")
				return nil
			}

			forceReindex, err := cmd.Flags().GetBool("force-reindex")
			if err == nil && forceReindex {
				// Set incremental to false when force reindex is requested
				cfg.Embed.Incremental = false
				logger.Info("Force reindex requested - will process all files")
			}

			err = embedService.Start(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to generate embeddings")
				return nil
			}

			fmt.Println("✅ Codebase indexed successfully")
			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add embed flags")
		return nil
	}

	cmd.Flags().Bool("force-reindex", false, "Force re-indexing of all files, ignoring previously generated embeddings.")

	return cmd
}
