package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	schemaSvc "go.keploy.io/server/v2/pkg/service/schema"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("schema", Schema)
}

func Schema(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "schema",
		Short: "Manage API schemas for generation and assertion",
		Long:  "Generate OpenAPI schemas from API responses and validate requests/responses against schemas",
	}

	cmd.AddCommand(GenerateSchema(ctx, logger, serviceFactory, cmdConfigurator))
	cmd.AddCommand(AssertSchema(ctx, logger, serviceFactory, cmdConfigurator))
	
	for _, subCmd := range cmd.Commands() {
		err := cmdConfigurator.AddFlags(subCmd)
		if err != nil {
			utils.LogError(logger, err, "failed to add flags to command", zap.String("command", subCmd.Name()))
		}
	}
	return cmd
}

func GenerateSchema(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "generate [file_path]",
		Short:   "Generate OpenAPI schema from API response file",
		Long:    "Parse API response file and generate OpenAPI schemas stored in keploy/api-schema directory",
		Example: `keploy schema generate ./api.txt`,
		Args:    cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			
			svc, err := serviceFactory.GetService(ctx, "schema")
			if err != nil {
				utils.LogError(logger, err, "failed to get schema service", zap.String("command", cmd.Name()))
				return nil
			}
			
			var schemaService schemaSvc.Service
			var ok bool
			if schemaService, ok = svc.(schemaSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy schema service interface")
				return nil
			}
			
			err = schemaService.GenerateSchema(ctx, filePath)
			if err != nil {
				utils.LogError(logger, err, "failed to generate schema")
				return nil
			}
			
			logger.Info("Schema generation completed successfully", zap.String("filePath", filePath))
			return nil
		},
	}

	return cmd
}

func AssertSchema(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "assert [file_path]",
		Short:   "Validate API responses against stored schemas",
		Long:    "Validate requests and responses in the file against previously generated OpenAPI schemas",
		Example: `keploy schema assert ./api.txt`,
		Args:    cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			
			svc, err := serviceFactory.GetService(ctx, "schema")
			if err != nil {
				utils.LogError(logger, err, "failed to get schema service", zap.String("command", cmd.Name()))
				return nil
			}
			
			var schemaService schemaSvc.Service
			var ok bool
			if schemaService, ok = svc.(schemaSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy schema service interface")
				return nil
			}
			
			result, err := schemaService.AssertSchema(ctx, filePath)
			if err != nil {
				utils.LogError(logger, err, "failed to assert schema")
				return nil
			}
			
			// Display results
			logger.Info("Schema assertion completed", 
				zap.Int("total", result.TotalEndpoints),
				zap.Int("passed", result.PassedCount),
				zap.Int("failed", result.FailedCount))
			
			if result.FailedCount > 0 {
				logger.Error("Schema assertion failures detected:")
				for _, schemaError := range result.Errors {
					logger.Error("  - Validation Error", 
						zap.String("endpoint", schemaError.Endpoint),
						zap.String("method", schemaError.Method),
						zap.String("error", schemaError.Error))
				}
			} else {
				logger.Info("✅ All schema assertions passed!")
			}
			
			return nil
		},
	}

	return cmd
}
