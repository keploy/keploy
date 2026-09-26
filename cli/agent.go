package cli

import (
	"context"
	"errors"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
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

func Agent(ctx context.Context, logger *zap.Logger, conf *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "agent",
		Short: "starts keploy agent for hooking and starting proxy",
		// Hidden: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			// A hang-up kills the agent, as it did before NewCtx made one
			// stop keploy gracefully: see utils.DieOnHangup.
			utils.DieOnHangup()
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

			startAgentCh := make(chan int)
			router := chi.NewRouter()
			// Read ONCE: whether the control plane is guarded and whether it
			// binds at all must come from the same answer. Two reads with
			// Setup's hooks in between could disagree, and a server bound
			// without the middleware installed would serve the whole control
			// plane unauthenticated, silently.
			daemonSet := isDaemonSetAgent()
			if daemonSet {
				// A DaemonSet agent binds no control-plane server (see below),
				// so it has no API to authenticate. Consuming a session token
				// here only produced the "running WITHOUT authentication"
				// warning on every DaemonSet start, about a server that never
				// listens -- a false alarm in exactly the logs operators read
				// for real ones. The token is still fixed, to whatever the
				// environment gave, so nothing in this process later mints one
				// that matches nothing.
				token.Adopt(token.FromEnv())
			} else {
				// Guard every route before any of them are registered. The
				// control plane streams live TLS session keys and captured
				// traffic and accepts session-mutating POSTs, and loopback is
				// not a privilege boundary: other local users, neighbouring
				// containers, and the application under test (which shares
				// this agent's network namespace) can all reach it.
				// Read directly, like client-pid above, so this never depends
				// on config wiring. An unreadable flag is not a reason to
				// refuse to start; SessionToken falls back to the environment
				// and warns if that is empty too.
				tokenFile, tfErr := cmd.Flags().GetString("token-file")
				if tfErr != nil {
					logger.Debug("could not read the token-file flag", zap.Error(tfErr))
				}
				router.Use(routes.Authenticate(logger, routes.ConsumeSessionToken(logger, tokenFile)))
			}

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
						return
					}
					routes.StartAgentServer(ctx, logger, p, isDocker, router)
				}
			}()

			err = a.Setup(ctx, startAgentCh)
			if err != nil {
				utils.LogError(logger, err, "failed to setup agent")
				if agentFailed(ctx, err) {
					// The specific code when the failure carries one: the CLI
					// that launched this agent reads it back as its own
					// (pkg/platform/http AgentClient.Setup), which is how a
					// user is told "grant privileges" apart from "mount
					// tracefs".
					utils.SetExitCodeOnce(utils.ExitCodeFor(err))
				}
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
