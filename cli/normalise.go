package cli

import (
	"context"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	toolsSvc "go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// func NewCmdNormalise(logger *zap.Logger) *Normalise {
// 	normaliser := normalise.NewNormaliser(logger)
// 	return &Normalise{
// 		normaliser: normaliser,
// 		logger:     logger,
// 	}
// }

//	type Normalise struct {
//		normaliser normalise.Normaliser
//		logger     *zap.Logger
//	}
func init() {
	Register("normalise", Normalise)
}

// Normalise retrieves the command to normalise Keploy
func Normalise(ctx context.Context, logger *zap.Logger, conf *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var normaliseCmd = &cobra.Command{
		Use:     "normalise",
		Short:   "Normalise Keploy",
		Example: "keploy normalise --path /path/to/localdir --test-set testset --test-cases testcases",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := cmd.Flags().GetString("path")
			if err != nil {
				utils.LogError(logger, err, "Error in getting path")
				return err
			}
			//if user provides relative path
			if len(path) > 0 && path[0] != '/' {
				absPath, err := filepath.Abs(path)
				if err != nil {
					utils.LogError(logger, err, "failed to get the absolute path from relative path")
				}
				path = absPath
			} else if len(path) == 0 { // if user doesn't provide any path
				cdirPath, err := os.Getwd()
				if err != nil {
					utils.LogError(logger, err, "failed to get the path of current directory")
				}
				path = cdirPath
			}
			path += "/keploy"
			testSet, err := cmd.Flags().GetString("test-set")
			if err != nil || len(testSet) == 0 {
				utils.LogError(logger, nil, "Please enter the testset to be normalised")
				return err
			}
			testCases, err := cmd.Flags().GetString("test-cases")
			if err != nil || len(testCases) == 0 {
				utils.LogError(logger, nil, "Please enter the testcases to be normalised")
				return err
			}
			svc, err := serviceFactory.GetService(ctx, "normalise")
			if err != nil {
				return err
			}
			var tools toolsSvc.Service
			var ok bool
			if tools, ok = svc.(toolsSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy normalise service interface")
				return nil
			}
			if err := tools.Normalise(ctx, path, testSet, testCases); err != nil {
				utils.LogError(logger, err, "failed to normalise test cases")
				return err
			}
			return nil
		},
	}
	if err := cmdConfigurator.AddFlags(normaliseCmd); err != nil {
		utils.LogError(logger, err, "failed to add nornalise cmd flags")
		return nil
	}
	return normaliseCmd
}
