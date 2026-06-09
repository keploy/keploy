package cli

import (
	"context"
	// TEMP-DEBUG(PR-4220): commented out for review; remove before merge.
	// "fmt"
	// "os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	// TEMP-DEBUG(PR-4220): commented out for review; remove before merge.
	// syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("agent", Agent)
}

// writeAgentStateLine emits one comprehensive PROBE/agent-state line
// to stderr. Reads atomic counters from syncMock + the routes package
// to give a snapshot of every agent-side stage where mocks can be
// lost. Caller passes finalSnapshot=true for the post-ctx-cancel
// emission so the trailing line is distinguishable from periodic
// ones in the CI log.
//
// Uses fmt.Fprintf + os.Stderr.Sync so the line survives the
// agent's abrupt death — zap is async-buffered and routinely drops
// the last few hundred ms of output when the process is SIGKILL'd.
func writeAgentStateLine(finalSnapshot bool) {
	// TEMP-DEBUG(PR-4220): commented out for review; remove before merge.
	// snap := syncMock.Get().ShutdownSnapshot()
	// tag := "PROBE/agent-state"
	// if finalSnapshot {
	// 	tag = "PROBE/agent-state-final"
	// }
	// FULL ACCOUNTING IDENTITY (this is what proves there is no
	// hidden counter fault). Every mock counted in total_added must
	// end up in exactly one of these buckets:
	//
	//   total_added = outgoing_forwarded   (made it onto the wire)
	//               + buffer               (still parked in windowing buffer)
	//               + outchan_len          (still queued for the handler)
	//               + outchan_closed_drops (dropped: arrived after close)
	//               + send_drops           (dropped: outChan full past budget)
	//               + unaccounted          (← MUST be ~0; >0 = real leak/bug)
	//
	// If unaccounted is non-zero at a steady moment, a mock left
	// total_added without landing in any known bucket — that is the
	// counter fault / silent drop we are hunting. (Small transient
	// non-zero is normal: a mock mid-flight between outChan and the
	// handler's Encode. It must settle to 0 when production stops.)
	//
	// pressure_dropped is NOT in this identity: pressure drops happen
	// BEFORE total_added is incremented, so they never enter the sum.
	// TEMP-DEBUG(PR-4220): commented out for review; remove before merge.
	// totalAdded := snap.TotalAdded
	// forwarded := routes.OutgoingForwardedTotal()
	// accountedDrops := snap.OutChanClosedDrops + int64(snap.SendDropsTotal)
	// stuck := int64(snap.BufferLen + snap.OutChanLen)
	// unaccounted := totalAdded - forwarded - stuck - accountedDrops
	// fmt.Fprintf(os.Stderr,
	// 	"%s: ts_ms=%d "+
	// 		"syncmock_total_added=%d syncmock_buffer=%d "+
	// 		"syncmock_outchan_len=%d syncmock_outchan_cap=%d "+
	// 		"syncmock_outchan_bound=%v syncmock_outchan_closed=%v "+
	// 		"syncmock_pressure_dropped=%d syncmock_send_drops=%d "+
	// 		"syncmock_outchan_closed_drops=%d "+
	// 		"syncmock_first_req_seen=%v syncmock_recent_windows=%d "+
	// 		"outgoing_forwarded=%d outgoing_handler_inflight=%d "+
	// 		"outgoing_handler_started=%d outgoing_handler_exited=%d "+
	// 		"outgoing_last_forward_ms=%d "+
	// 		"accounting_unaccounted=%d\n",
	// 	tag, time.Now().UnixMilli(),
	// 	totalAdded, snap.BufferLen,
	// 	snap.OutChanLen, snap.OutChanCap,
	// 	snap.OutChanBound, snap.OutChanClosed,
	// 	snap.PressureDropped, snap.SendDropsTotal,
	// 	snap.OutChanClosedDrops,
	// 	snap.FirstReqSeen, snap.RecentWindows,
	// 	forwarded, routes.OutgoingHandlerInFlight(),
	// 	routes.OutgoingHandlerStartedTotal(), routes.OutgoingHandlerExitedTotal(),
	// 	routes.OutgoingLastForwardUnixMs(),
	// 	unaccounted,
	// )
	// _ = os.Stderr.Sync()
	_ = finalSnapshot
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
			// BLACK-BOX RECORDER (comprehensive, per-second snapshots)
			//
			// The agent in docker-compose mode dies abruptly when
			// `docker compose down` fires SIGKILL (its sudo wrapper
			// doesn't forward SIGTERM and the container's 10s grace
			// is exceeded). Pre-cancel hooks that fire INSIDE the Go
			// signal handler never run in that case — the SIGTERM
			// probe we tried in a previous commit never appeared in
			// any mongo lane's log for exactly that reason.
			//
			// This recorder runs as a background goroutine that
			// snapshots every agent-side counter every 1 second and
			// writes one PROBE/agent-state line to stderr (sync,
			// bypasses zap). Even if the agent is SIGKILL'd, the
			// LAST snapshot before death survives in the CI log
			// because stderr writes go straight to the syscall.
			//
			// What this answers, end-to-end:
			//
			//   syncMock.totalAdded                = mocks that ever
			//                                        entered the agent
			//                                        buffer
			//   syncMock.bufferLen                 = mocks parked
			//                                        inside the
			//                                        windowing buffer
			//                                        NOW
			//   syncMock.outChanLen                = mocks queued for
			//                                        the HTTP stream
			//                                        NOW
			//   routes.OutgoingForwardedTotal      = mocks the agent
			//                                        has actually
			//                                        gob-encoded onto
			//                                        the response wire
			//   routes.OutgoingHandlerInFlight     = is the /outgoing
			//                                        handler currently
			//                                        alive?
			//   routes.OutgoingLastForwardUnixMs   = wall-clock of last
			//                                        successful forward
			//                                        (so we can see a
			//                                        stall: handler
			//                                        alive but not
			//                                        moving mocks)
			//
			// At end of recording the LAST printed snapshot answers
			// "where in the agent pipeline are mocks stuck?"
			// definitively, even when the agent dies in 261 ms.
			//
			// The pre-cancel SIGTERM probe is kept as well — if the
			// agent's signal handler DOES happen to run (e.g. native
			// mode, or docker mode with a fixed entrypoint), it adds
			// a final marker line just before cancel().
			go func() {
				ticker := time.NewTicker(1 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						// Print one last snapshot on the way out so
						// the trailing line is anchored to a known
						// "ctx-cancel observed" event when this path
						// actually runs (it may not in SIGKILL).
						writeAgentStateLine(true)
						return
					case <-ticker.C:
						writeAgentStateLine(false)
					}
				}
			}()

			// Keep the SIGTERM-instant probe too — it's a more precise
			// "moment of death" marker IF the signal handler runs.
			utils.RegisterPreCancelHook(func() {
				// TEMP-DEBUG(PR-4220): commented out for review; remove before merge.
				// snap := syncMock.Get().ShutdownSnapshot()
				// fmt.Fprintf(os.Stderr,
				// 	"PROBE/syncmock-at-sigterm: ts_ms=%d "+
				// 		"bufferLen=%d outChanLen=%d outChanCap=%d "+
				// 		"outChanBound=%v outChanClosed=%v "+
				// 		"totalAdded=%d pressureDropped=%d sendDropsTotal=%d "+
				// 		"firstReqSeen=%v recentWindows=%d\n",
				// 	time.Now().UnixMilli(),
				// 	snap.BufferLen, snap.OutChanLen, snap.OutChanCap,
				// 	snap.OutChanBound, snap.OutChanClosed,
				// 	snap.TotalAdded, snap.PressureDropped, snap.SendDropsTotal,
				// 	snap.FirstReqSeen, snap.RecentWindows,
				// )
				// _ = os.Stderr.Sync()
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
