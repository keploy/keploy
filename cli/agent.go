package cli

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/agent/routes"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/agent"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("agent", Agent)
}

func Agent(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "agent",
		Short: "starts keploy agent for hooking and starting proxy",
		// Hidden: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}

			isdocker, _ := cmd.Flags().GetBool("is-docker")
			var port uint32 = 8086
			if isdocker {
				port, _ = cmd.Flags().GetUint32("port")
			}

			var a agent.Service
			var ok bool
			if a, ok = svc.(agent.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy agent service interface")
				return nil
			}

			router := chi.NewRouter()

			routes.New(router, a, logger)

			go func() {
				if err := http.ListenAndServe(fmt.Sprintf(":%d", port), router); err != nil {
					logger.Error("failed to start HTTP server", zap.Error(err))
				}
			}()
			
			err = a.Setup(ctx, "", models.SetupOptions{
				IsDocker: isdocker,
			})
			if err != nil {
				utils.LogError(logger, err, "failed to setup agent")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add record flags")
		return nil
	}

	return cmd
}
