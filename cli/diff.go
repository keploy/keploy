package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	diffSvc "go.keploy.io/server/v3/pkg/service/diff"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("diff", Diff)
}

func Diff(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "diff <run1> <run2>",
		Short:   "compare two keploy test runs and print regressions/fixes",
		Example: `keploy diff test-run-1 test-run-2 -t "test-set-1,test-set-2"`,
		Args:    cobra.MaximumNArgs(2),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			run1, _ := cmd.Flags().GetString("run1")
			run2, _ := cmd.Flags().GetString("run2")

			if len(args) > 0 {
				run1 = args[0]
			}
			if len(args) > 1 {
				run2 = args[1]
			}
			run1 = strings.TrimSpace(run1)
			run2 = strings.TrimSpace(run2)
			if run1 == "" || run2 == "" {
				return fmt.Errorf("%s expected two run IDs. usage: keploy diff <run1> <run2>", utils.Emoji)
			}

			testSets, err := cmd.Flags().GetStringSlice("test-sets")
			if err != nil {
				return fmt.Errorf("%s failed to read test-sets flag: %w", utils.Emoji, err)
			}

			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return nil
			}
			var diffService diffSvc.Service
			var ok bool
			if diffService, ok = svc.(diffSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy diff service interface")
				return nil
			}

			if err := diffService.Compare(ctx, run1, run2, testSets); err != nil {
				utils.LogError(logger, err, "failed to compare test runs")
				return nil
			}
			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add diff flags")
		return nil
	}

	return cmd
}
