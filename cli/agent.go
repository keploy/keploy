package cli

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
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

func Agent(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
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

			isdocker, err := cmd.Flags().GetBool("is-docker")
			if err != nil {
				utils.LogError(logger, err, "failed to get is-docker flag")
				return nil
			}
			enableTesting, err := cmd.Flags().GetBool("enable-testing")
			if err != nil {
				utils.LogError(logger, err, "failed to get enable-testing flag")
				return nil
			}

			port, err := cmd.Flags().GetUint32("port")
			if err != nil {
				utils.LogError(logger, err, "failed to get port flag")
				return nil
			}

			if port == 0 {
				port = 8086
			}

			var a agent.Service
			var ok bool
			if a, ok = svc.(agent.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy agent service interface")
				return nil
			}

			startCh := make(chan struct{})

			router := chi.NewRouter()

			routes.New(router, a, logger)

			opts := models.SetupOptions{
				IsDocker:      isdocker,
				EnableTesting: enableTesting,
			}

			if enableTesting {
				opts.Mode = models.MODE_TEST
			}

			go func() {
				select {
				case <-ctx.Done():
					logger.Info("context cancelled before agent http server could start")
					return
				case <-startCh:
					logger.Info("keploy agent successfully loaded hooks and proxies, will start the http server now")
					if err := http.ListenAndServe(fmt.Sprintf(":%d", port), router); err != nil {
						logger.Error("failed to start HTTP server", zap.Error(err))
					} else {
						logger.Info("HTTP server started successfully on port ", zap.Uint32("port", port))
					}
				}
			}()

			err = a.Setup(ctx, opts, startCh)
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
