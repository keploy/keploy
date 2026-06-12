package cli

import (
	"context"

	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("agent", Agent)
}

func Agent(ctx context.Context, logger *zap.Logger, conf *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
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

			var a agent.Service
			var ok bool
			if a, ok = svc.(agent.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy agent service interface")
				return nil
			}

			// Self-terminate (gracefully) if the parent keploy client dies
			// abnormally, so the agent never orphans and keeps eBPF hooks /
			// DNS / proxy+ingress ports alive that would hang the next run.
			// Read the flags directly so this never depends on config wiring.
			//
			// Skip it in docker mode: there the agent runs in its OWN
			// `docker run --rm` container (separate PID namespace), so
			// --client-pid is the *host* keploy PID and is not visible here —
			// kill(pid, 0) would return ESRCH and we'd self-terminate
			// immediately, breaking record/replay. The container's --rm
			// lifecycle bounds the agent in that mode instead.
			clientPID, cpErr := cmd.Flags().GetUint32("client-pid")
			isDocker, dockErr := cmd.Flags().GetBool("is-docker")
			switch {
			case cpErr != nil || dockErr != nil:
				// A flag was renamed/removed or the command was built without
				// AddFlags. Don't guess — leave the watchdog off and say so,
				// rather than silently watching a zero PID.
				logger.Debug("could not read client-pid/is-docker flags; parent-death watchdog left disabled",
					zap.NamedError("clientPidErr", cpErr), zap.NamedError("isDockerErr", dockErr))
			case isDocker:
				logger.Debug("parent-death watchdog disabled in docker mode (separate PID namespace; --rm bounds the container)")
			default:
				watchParentProcess(ctx, logger, int(clientPID))
			}

			startAgentCh := make(chan int)
			router := chi.NewRouter()

			routes.ActiveHooks.New(router, a, logger)
			go func() {
				select {
				case <-ctx.Done():
					logger.Info("context cancelled before agent http server could start")
					return
				case p := <-startAgentCh:
					if err := agent.SetupAgentHook.AfterSetup(ctx); err != nil {
						utils.LogError(logger, err, "failed to execute pre-server startup hooks")
						return
					}
					routes.StartAgentServer(ctx, logger, p, router)
				}
			}()

			err = a.Setup(ctx, startAgentCh)
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
