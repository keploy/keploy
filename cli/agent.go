package cli

import (
	"context"
	"errors"

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
		// Runtime failures below are reported by their own log line and by the
		// process exit status; without this, cobra would dump the whole usage
		// text after every one of them and bury the actual error. Bad flags
		// still print their own message via SetFlagErrorFunc (the error, not
		// the usage listing).
		SilenceUsage: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return err
			}

			var a agent.Service
			var ok bool
			if a, ok = svc.(agent.Service); !ok {
				err = errors.New("service doesn't satisfy agent service interface")
				utils.LogError(logger, err, "failed to start agent")
				return err
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
				// A cancelled context is how the agent is asked to stop
				// (SIGINT/SIGTERM, parent exit) — that is a normal shutdown,
				// not a failure, and must keep exiting 0. Anything else means
				// the agent never came up: it must exit non-zero so kubelet,
				// docker and CI see a failure instead of "Completed".
				//
				// Check the CONTEXT as well as the error: a shutdown that
				// races agent startup surfaces as whatever error the
				// interrupted step happened to produce, which need not carry
				// context.Canceled. We own this context, so its state is the
				// authoritative answer to "was this a shutdown?".
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					return nil
				}
				return err
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
