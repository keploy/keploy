package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	klog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// isDaemonSetAgent reports whether this agent process is running as a per-node
// Kubernetes DaemonSet. In that deployment the control-plane HTTP server has no
// consumers (see the call site in Agent), so it must not be bound.
//
// The signal is the KEPLOY_DAEMONSET_ENABLED env var that the k8s-proxy Helm
// chart's DaemonSet spec sets to "true". It is read straight from the
// environment — the same source pkg/agent/hooks/linux/hooks.go already keys off
// — so the OSS agent needs no dependency on the enterprise DaemonSet config
// (whose `daemonsetenv` helper lives in a different module).
//
// Accept ONLY the exact string "true", matching the canonical gate in
// k8s-proxy's daemonsetenv package and pkg/agent/hooks/linux/hooks.go. Every
// DaemonSet gate must agree on the truthy set: if this one alone also honoured
// "1", a KEPLOY_DAEMONSET_ENABLED=1 pod would skip this server bind while
// k8s-proxy (still seeing sidecar mode) polls the now-unbound agent port. The
// chart always sets "true", so this is strictness for cross-component
// consistency, not a live path.
func isDaemonSetAgent() bool {
	return os.Getenv("KEPLOY_DAEMONSET_ENABLED") == "true"
}

// newAgentRouter builds the control-plane router with every route behind the
// token guard. The guard goes in before any route is registered: the control
// plane streams live TLS session keys and captured traffic and accepts
// session-mutating POSTs, and loopback is not a privilege boundary — other
// local users, neighbouring containers, and the application under test (which
// shares the agent's network namespace) can all reach it.
//
// An error means the agent was handed a token it cannot read, and must not
// start; see routes.ConsumeSessionToken.
//
// A DaemonSet agent never serves this router (see the gate in Agent), so it
// neither consumes a token nor says anything about one: telling every node's
// log that a server it never starts is unauthenticated would be a false alarm.
func newAgentRouter(logger *zap.Logger, a agent.Service, tokenFile string, isDocker, daemonSet bool) (chi.Router, error) {
	sessionToken := ""
	if !daemonSet {
		tok, err := routes.ConsumeSessionToken(logger, tokenFile, isDocker)
		if err != nil {
			return nil, err
		}
		sessionToken = tok
	}
	router := chi.NewRouter()
	router.Use(routes.Authenticate(logger, sessionToken))
	routes.ActiveHooks.New(router, a, logger)
	return router, nil
}

func init() {
	Register("agent", Agent)
}

// agentFailed separates an agent that died from the ways a healthy one ends.
// Setup returns nil when the process is stopped before the agent announced its
// port, and context.Canceled when a serving agent is stopped (SIGTERM from the
// CLI, docker stop, the kubelet); an error that arrives while the root context
// is already cancelled is teardown, not the agent failing. Reading any of
// those as a failure would turn every clean stop into a crash.
func agentFailed(ctx context.Context, err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil
}

// containerTeardownProgressFile is where a containerised agent reports its
// teardown (kdocker.AgentTeardownProgressFile). A variable only so that a test
// running the agent command in-process points it at its own directory, not
// at a path every process on the host shares.
var containerTeardownProgressFile = kdocker.AgentTeardownProgressFile

// containerBootProgressFile is where a containerised agent reports its start
// (kdocker.AgentBootProgressFile), a variable for the same reason.
var containerBootProgressFile = kdocker.AgentBootProgressFile

// reportBoot has the agent report its start's progress, for the CLI that waits
// for it to be ready: it gives up on an agent's start only once that has
// stopped making progress, and fails it at once on a failure the agent
// reports (utils.FailBoot) (utils.ReportBootProgress). The ERRORs the agent
// logs while it starts go in the report as detail, until the start is over
// (utils.BootOver). A native agent writes next to its teardown progress file
// (--teardown-progress-file, flagPath, checked as that is); a containerised
// one into its own container (containerBootProgressFile).
func reportBoot(ctx context.Context, logger *zap.Logger, flagPath string, isDocker bool) {
	klog.ObserveErrors(utils.NoteBootError)
	go func() {
		select {
		case <-utils.BootOver():
		case <-ctx.Done():
		}
		klog.ObserveErrors(nil)
	}()
	utils.BootStep("the agent's process runs")
	switch {
	case flagPath != "":
		if err := utils.ReportBootProgressTo(ctx, logger, flagPath); err != nil {
			logger.Debug("not reporting the agent's start's progress: the CLI that waits for it gives its process a minute of runnable time to make its first report", zap.Error(err))
		}
	case isDocker:
		utils.ReportBootProgress(ctx, logger, containerBootProgressFile)
	}
}

// reportTeardown has the agent report its teardown's progress, for the CLI
// that stops it: it kills a stopping agent only once that has stopped making
// progress (utils.ReportTeardownProgress). A native agent is told where
// (--teardown-progress-file, flagPath), and that path is checked before the
// agent, root under sudo, writes there; a containerised one writes into its
// own container (containerTeardownProgressFile), where the CLI reads it
// back.
func reportTeardown(ctx context.Context, logger *zap.Logger, flagPath string, isDocker bool) {
	switch {
	case flagPath != "":
		if err := utils.ReportTeardownProgressTo(ctx, logger, flagPath); err != nil {
			logger.Warn("not reporting the agent's teardown progress: the CLI that stops it will wait for it to exit however long it takes", zap.Error(err))
		}
	case isDocker:
		utils.ReportTeardownProgress(ctx, logger, containerTeardownProgressFile)
	}
}

func Agent(ctx context.Context, logger *zap.Logger, conf *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "agent",
		Short: "starts keploy agent for hooking and starting proxy",
		// Hidden: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			// A hang-up kills the agent, as it did before NewCtx made one
			// stop keploy gracefully: see utils.DieOnHangup. Not once it is
			// shutting down: see utils.IgnoreHangupOnceStopping.
			utils.DieOnHangup()
			utils.IgnoreHangupOnceStopping(ctx)
			// The CLI's forced stop of an agent it may not signal itself
			// (sudo's): see utils.DieOnEscalation.
			utils.DieOnEscalation(ctx)
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Every failure below arms a non-zero exit and returns nil: the
			// failure is already logged, and a returned error would have cobra
			// print usage on top of it. What must not happen is what used to:
			// `return nil` alone, so an agent that could not start exited 0 --
			// and the CLI that launched it, docker, and the kubelet all read
			// that as a clean finish.
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				utils.SetExitCodeOnce(utils.ExitKeployError)
				return nil
			}

			var a agent.Service
			var ok bool
			if a, ok = svc.(agent.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy agent service interface")
				utils.SetExitCodeOnce(utils.ExitKeployError)
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

			progressFile, pfErr := cmd.Flags().GetString("teardown-progress-file")
			if pfErr != nil {
				logger.Debug("could not read the teardown-progress-file flag", zap.Error(pfErr))
			}
			reportTeardown(ctx, logger, progressFile, isDocker)
			reportBoot(ctx, logger, progressFile, isDocker)

			startAgentCh := make(chan int)
			daemonSet := isDaemonSetAgent()
			// Read directly, like client-pid above, so this never depends on
			// config wiring. An unreadable flag is not a reason to refuse to
			// start: it means no launcher passed a file, and the environment
			// is consulted instead.
			tokenFile, tfErr := cmd.Flags().GetString("token-file")
			if tfErr != nil {
				logger.Debug("could not read the token-file flag", zap.Error(tfErr))
			}
			router, err := newAgentRouter(logger, a, tokenFile, isDocker, daemonSet)
			if err != nil {
				utils.LogError(logger, err, "refusing to start the agent")
				utils.FailBoot(fmt.Errorf("refusing to start the agent: %w", err))
				utils.SetExitCodeOnce(utils.ExitKeployError)
				return nil
			}
			// A recording's client owns this agent's lifetime. A stop signal,
			// often the same one the client got (`pkill -INT keploy`, a
			// container stop), waits while the client still holds open a
			// frame stream it drains after the stop
			// (models.DrainsAfterStopHeader): the client is draining what this
			// agent captured before the stop, and stops the agent once it has
			// (POST /agent/stop). A client that dies drops its streams, and
			// the parent-death watchdog above ends a native agent's stop that
			// still holds for it (endForGoneClient). A
			// repeated signal does not cut this short: it may be the first
			// again, as the sudo that runs the agent hands it on
			// (utils.SetSignalHold). The user's second Ctrl+C reaches the
			// client, whose drain then ends and closes the streams; a client
			// that must stop the agent regardless asks it over its API, or
			// escalates (utils.DieOnEscalation), and either ends this wait.
			//
			// An agent something else stops (docker compose, with the app's
			// project, also when the app exits on its own) is not stopped by
			// its client, and the client learns of the stop only from the
			// agent: its streams ask for that (models.DrainsOnAgentStopHeader).
			// While such a client holds a stream and this agent has something
			// for it to save, the stop says so to it (routes.AnnounceStop:
			// the client then drains the streams, and closes them) and waits
			// for it, as for a client that stops the agent itself.
			utils.SetSignalHold(routes.HoldStop(logger))
			go func() {
				select {
				case <-ctx.Done():
					logger.Info("context cancelled before agent http server could start")
					return
				case p := <-startAgentCh:
					// The hooks can take long without a step (enterprise
					// stages its shims here): a stage of the start
					// (utils.BeginBootWork). Their failure is the start's:
					// the agent lives on, without its server.
					endHooks := utils.BeginBootWork("the pre-server startup hooks")
					err := agent.SetupAgentHook.AfterSetup(ctx)
					endHooks()
					if err != nil {
						utils.LogContentError(logger, err, "failed to execute pre-server startup hooks")
						utils.FailBoot(fmt.Errorf("failed to execute the agent's pre-server startup hooks: %w", err))
						return
					}
					// Kubernetes DaemonSet mode is push-based: the agent captures
					// traffic and pushes it to the k8s-proxy over an outbound
					// /ds/stream POST, and receives session control over an
					// outbound CRD/ConfigMap watch (or the agent-initiated dssync
					// gRPC stream). Nothing ever makes an INBOUND call to the
					// agent's control-plane HTTP server — that path is the
					// sidecar/pull deployment, driven by the local keploy CLI's
					// AgentClient. So binding the server here is dead weight in
					// DaemonSet mode, and because the DaemonSet runs with
					// hostNetwork the listener would otherwise sit unauthenticated
					// on the node's network namespace, exposing mutating endpoints
					// (/agent/stop, /agent/storemocks, …). Skip it. We still
					// receive on startAgentCh above so Agent.Setup's unbuffered
					// port handoff never blocks.
					if daemonSet {
						logger.Info("running as a Kubernetes DaemonSet agent; not starting the control-plane HTTP server (push architecture has no inbound HTTP consumers)")
						// Its start is over: no server to listen.
						utils.MarkBootReady()
						return
					}
					routes.StartAgentServer(ctx, logger, p, isDocker, router)
				}
			}()

			err = a.Setup(ctx, startAgentCh)
			if err != nil {
				if !agentFailed(ctx, err) {
					// Stopped, not failed (agentFailed): Setup said so.
					return nil
				}
				utils.LogError(logger, err, "failed to setup agent")
				// Reported as its start's failure, unless it started: the
				// CLI that waits for it fails the start with it at once,
				// with the code it exits with.
				utils.FailBoot(err)
				// The specific code when the failure carries one: the CLI
				// that launched this agent reads it back as its own
				// (pkg/platform/http AgentClient.Setup), which is how a user
				// is told "grant privileges" apart from "mount tracefs".
				utils.SetExitCodeOnce(utils.ExitCodeFor(err))
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
