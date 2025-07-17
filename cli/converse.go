package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	embedSvc "go.keploy.io/server/v2/pkg/service/embed"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("converse", ConverseCommand)
}

func ConverseCommand(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "converse [question]",
		Short:   "Talk to your codebase in natural language",
		Example: `keploy converse "how does the user authentication work?"`,
		Args:    cobra.MinimumNArgs(1),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(ctx, "embed") // Converse uses the embed service
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

			llmBaseUrl, err := cmd.Flags().GetString("llm-base-url")
			if err == nil && llmBaseUrl != "" {
				cfg.Embed.LLMBaseURL = llmBaseUrl
			}

			question := strings.Join(args, " ")
			if strings.TrimSpace(question) == "" {
				return errors.New("the question cannot be empty")
			}

			err = embedService.Converse(ctx, question)
			if err != nil {
				utils.LogError(logger, err, "failed during conversation")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add converse flags")
		return nil
	}

	return cmd
}
