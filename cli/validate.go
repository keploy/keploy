package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	toolsSvc "go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("ValidateTestCases", ValidateTestCases)
}

// ValidateTestCases retrieves the command to validate Keploy test cases
func ValidateTestCases(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "validate",
		Short: "Validate test case YAML files for structural and semantic correctness",
		Long: `Validate checks all test case files in the keploy directory for:
- YAML syntax errors
- Required fields (version, kind, name)
- HTTP-specific validation (method, URL, status codes, JSON bodies)
- gRPC-specific validation (method names)
- Mock file integrity

Use this command before running tests to catch issues early.`,
		Example: `  keploy validate
  keploy validate -t "test-set-1,test-set-2"
  keploy validate -p "/path/to/keploy/dir"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return nil
			}
			var validateService toolsSvc.Service
			var ok bool
			if validateService, ok = svc.(toolsSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy tools service interface")
				return nil
			}

			err = validateService.Validate(ctx)
			if err != nil {
				utils.LogError(logger, err, "validation failed")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add validate flags")
		return nil
	}

	return cmd
}
