package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
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
			// PROBE: register a SIGTERM-time snapshot that writes the
			// SyncMockManager's buffer state directly to stderr (NOT
			// via zap — zap is async-buffered and silently drops lines
			// when the process dies in the few hundred milliseconds
			// between SIGTERM and container destruction in docker mode).
			//
			// This hook fires in utils.NewCtx's signal handler at the
			// exact instant before cancel() is called, so it reads
			// live state untainted by shutdown propagation. If the
			// agent has buffered mocks the host never received, the
			// PROBE/syncmock-at-sigterm line in the CI log will show
			// it definitively (BufferLen, OutChanLen, totalAdded vs
			// what host actually persisted to mocks.yaml). If those
			// counters are zero, the loss is happening upstream of
			// SyncMockManager — in the mongo decoder itself or the
			// proxy connection layer — and the next investigation
			// round needs to instrument there.
			utils.RegisterPreCancelHook(func() {
				snap := syncMock.Get().ShutdownSnapshot()
				fmt.Fprintf(os.Stderr,
					"PROBE/syncmock-at-sigterm: ts_ms=%d "+
						"bufferLen=%d outChanLen=%d outChanCap=%d "+
						"outChanBound=%v outChanClosed=%v "+
						"totalAdded=%d pressureDropped=%d sendDropsTotal=%d "+
						"firstReqSeen=%v recentWindows=%d\n",
					time.Now().UnixMilli(),
					snap.BufferLen, snap.OutChanLen, snap.OutChanCap,
					snap.OutChanBound, snap.OutChanClosed,
					snap.TotalAdded, snap.PressureDropped, snap.SendDropsTotal,
					snap.FirstReqSeen, snap.RecentWindows,
				)
				// Force stderr to flush — Go's os.Stderr writes are
				// synchronous syscalls, but Sync explicitly ensures
				// kernel buffers are committed before SIGKILL can hit.
				_ = os.Stderr.Sync()
			})

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
