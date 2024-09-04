package cli

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/cli/provider"
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

// keploy record -> keploy agent
func Agent(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, _ CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "agent",
		Short: "starts keploy agent for hooking and starting proxy",
		// Hidden: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			// validate the flags

			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Println("Starting keploy agent")
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}

			disableAnsi, _ := (cmd.Flags().GetBool("disable-ansi"))
			provider.PrintLogo(disableAnsi)
			var a agent.Service
			var ok bool
			if a, ok = svc.(agent.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy agent service interface")
				return nil
			}

			router := chi.NewRouter()

			routes.New(router, a, logger)

			go func() {
				if err := http.ListenAndServe(":8086", router); err != nil {
					logger.Error("failed to start HTTP server", zap.Error(err))
				}
			}()
			// Doubt: How can I provide the setup options for the first time?
			_, err = a.Setup(ctx, "", models.SetupOptions{})
			if err != nil {
				utils.LogError(logger, err, "failed to setup agent")
				return nil
			}

			return nil
		},
	}

	return cmd
}
