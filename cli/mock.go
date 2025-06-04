package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	replaySvc "go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("mock", Mock)
}

func Mock(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "mock",
		Short: "Managing mocks",
	}

	cmd.AddCommand(DownloadMocks(ctx, logger, serviceFactory, cmdConfigurator))
	cmd.AddCommand(UploadMocks(ctx, logger, serviceFactory, cmdConfigurator))
	for _, subCmd := range cmd.Commands() {
		err := cmdConfigurator.AddFlags(subCmd)
		if err != nil {
			utils.LogError(logger, err, "failed to add flags to command", zap.String("command", subCmd.Name()))
		}
	}
	return cmd
}

func DownloadMocks(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "download",
		Short:   "Download mocks from the keploy registry",
		Example: `keploy mock download`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Parent().Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Parent().Name()))
				return nil
			}
			var replay replaySvc.Service
			var ok bool
			if replay, ok = svc.(replaySvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy replay service interface")
				return nil
			}
			if err := replay.DownloadMocks(ctx); err != nil {
				utils.LogError(logger, err, "failed to download mocks from keploy registry")
				return nil
			}
			return nil
		},
	}

	return cmd
}

func UploadMocks(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "upload",
		Short:   "Upload mocks to the keploy registry",
		Example: `keploy mock upload`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Parent().Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Parent().Name()))
				return nil
			}
			var replay replaySvc.Service
			var ok bool
			if replay, ok = svc.(replaySvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy replay service interface")
				return nil
			}
			if err := replay.UploadMocks(ctx); err != nil {
				utils.LogError(logger, err, "failed to upload mocks to the keploy registry")
				return nil
			}
			return nil
		},
	}

	return cmd
}
