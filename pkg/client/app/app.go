// Package app provides functionality for managing applications.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	cli "github.com/docker/cli/cli"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	mobyclient "github.com/moby/moby/client"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"

	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/platform/engine"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// resolveKind picks the launch mode for this App. opts.CommandType is the
// value ValidateFlags already resolved — auto-detected from the command in
// the ordinary case, but overridden when the user passed --cmd-type. Falling
// back to FindDockerCmd(cmd) keeps every caller that leaves CommandType
// unset behaving exactly as before.
//
// Deriving it from the command string again here was the reason --cmd-type
// could not work end to end (#4399): agent.go honoured opts.CommandType
// while this App did not, so `--cmd-type docker-compose -c "make up"` left
// the agent expecting a compose project that SetupCompose was never asked to
// create, and the run died waiting for an agent that could not appear.
func resolveKind(commandType, cmd string) utils.CmdType {
	switch kind := utils.CmdType(commandType); kind {
	case utils.Native, utils.DockerRun, utils.DockerStart, utils.DockerCompose, utils.FromContainer:
		return kind
	}
	return utils.FindDockerCmd(cmd)
}

func NewApp(logger *zap.Logger, cmd string, client docker.Client, opts models.SetupOptions) *App {
	app := &App{
		logger:          logger,
		cmd:             cmd,
		docker:          client,
		kind:            resolveKind(opts.CommandType, cmd),
		opts:            opts,
		keployContainer: opts.KeployContainer,
		container:       opts.Container,
	}
	return app
}

type App struct {
	// onAgentRestart, when set, sets keploy's agent up again after the
	// compose retry's `up` started it again; see SetOnAgentRestart.
	onAgentRestart func(ctx context.Context) error
	// setEndKill is why the latest stop at a test set's end had to kill the
	// app (setend_stop.go), until run reports it; nil when it did not.
	setEndKill      atomic.Pointer[string]
	logger          *zap.Logger
	docker          docker.Client
	cmd             string
	kind            utils.CmdType
	opts            models.SetupOptions
	container       string
	composeService  string
	keployContainer string
	composeFile     string // path to the temp compose file (set during SetupCompose)
	composeContent  []byte // in-memory compose YAML; set when InMemoryCompose is used
	// composeServices are the service keys of the compose source keploy actually
	// runs — the user's services plus the injected agent. It is the set
	// `docker compose down` targets, and the ONLY set the teardown sweep is
	// allowed to remove. See sweepStragglingProjectContainers for why the
	// project label is not a substitute.
	composeServices []string
	// composeRunner drives the in-memory compose stack through the compose
	// library instead of a `docker` binary. Built lazily on first use (the
	// engine must be reachable) and reset by setComposeSource, because the
	// per-test-set replay loop re-generates the document between rounds and a
	// cached project would then address the previous stack.
	composeRunnerMu  sync.Mutex
	composeRunnerVal composeStack
	// composeProjectSeen is the compose project a down of this app resolved
	// (ComposeDown, composeDownProject: its containers' labels, else the
	// command's or the environment's): the project its up ran, for the calls
	// made once those containers are gone (the stale-agent probe before the
	// next up).
	composeProjectSeen atomic.Pointer[string]
	// composeChildOut keeps the tails of keploy's own compose stack when it
	// runs as a `docker compose` child (no compose library in this build):
	// the last Up's, set before its child starts (composeChildTee), and
	// dropped with the document it ran (setComposeSource).
	composeChildOut atomic.Pointer[docker.ComposeChildOutput]
	// composeChildWarned: composeChildTee has said that it keeps none of the
	// document's output; once per document, reset with it (setComposeSource).
	composeChildWarned atomic.Bool
	// teardown is the teardown scope of the run that set the app up or ran
	// it last (utils.TeardownScopeOf of Setup's or Run's context): each step
	// keploy sees its stop take -- the app's processes or container on
	// their way out, the compose project's down -- is a movement of it,
	// which that run's drains wait on (utils.DrainErrGroupProgress).
	teardown atomic.Pointer[utils.TeardownScope]
	// composeAPI is the Engine API client the compose library drives the
	// stack through (docker.NewComposeAPIClient). Built with the first runner
	// and kept for the App's life, as the docker client is: the runner is
	// rebuilt for every test-set, and a client per runner would leave a
	// connection pool behind each time. Guarded by composeRunnerMu.
	composeAPI mobyclient.APIClient
	// newComposeStack overrides how the runner is built. Production leaves it
	// nil and gets the compose library; tests substitute a fake so the
	// in-memory path keeps unit coverage without a docker daemon.
	newComposeStack func(context.Context) (composeStack, error)
	// --from-container only. sourceContainer is the user's own container, which
	// the agent client stops for the session and starts again on teardown;
	// replacementID is the copy keploy created and runs in its place.
	sourceContainer string
	replacementID   string
	// sourceTTY mirrors the source's Config.Tty. It decides how the
	// replacement's log stream is read: a TTY stream is raw, every other is
	// multiplexed, and the two are not interchangeable.
	sourceTTY     bool
	EnableTesting bool
	Mode          models.Mode
	// stopped is what readStoppedAgent read from the keploy-agent compose
	// service's container after compose stopped it.
	stopped stoppedAgent
	// stopReached is what the latest stop of a native app reached with its
	// interrupt, under the proven switch, until TakeStopReached takes it.
	stopReached atomic.Pointer[[]utils.Reached]
	// composeApp is what keploy knows of a compose app's container, which it
	// stops and watches itself under the proven switch (compose_appstop.go).
	composeApp composeAppState
}

func (a *App) Setup(ctx context.Context) error {
	a.noteTeardownScope(ctx)

	// isDetachMode scans the command for -d/--detach; --from-container has no
	// command, so there is nothing to scan and nothing to refuse.
	if utils.IsDockerCmd(a.kind) && utils.HasUserCommand(a.kind) && isDetachMode(a.logger, a.cmd, a.kind) {
		return fmt.Errorf("application could not be started in detached mode")
	}

	switch a.kind {
	case utils.FromContainer:
		return a.SetupFromContainer(ctx)
	case utils.DockerRun, utils.DockerStart:
		err := a.SetupDocker(ctx)
		if err != nil {
			return err
		}
	case utils.DockerCompose:
		// Before anything else: compose starts the agent, and a sudo that
		// cannot hand it the token would start it unauthenticated.
		if err := a.checkAgentTokenHandoff(ctx); err != nil {
			return err
		}
		extraArgs := agent.StartupAgentHook.GetArgs(ctx)
		err := a.SetupCompose(ctx, extraArgs)
		if err != nil {
			return err
		}
	default:
		// setup native binary
	}
	return nil
}

// SetupDocker readies a `docker run`/`docker start` command. ctx is the run's:
// the before-setup hook does its work under it, so cancelling the run (Ctrl-C)
// ends that work too.
func (a *App) SetupDocker(ctx context.Context) error {

	if a.kind == utils.DockerStart {
		running, err := a.docker.IsContainerRunning(a.container)
		if err != nil {
			return err
		}
		if running {
			return fmt.Errorf("docker container is already in running state")
		}
	}

	a.logger.Debug("inside setup docker", zap.String("cmd", a.cmd))

	if HookImpl != nil {
		newCmd, err := HookImpl.BeforeDockerSetup(ctx, a.cmd)
		if err != nil {
			utils.LogError(a.logger, err, "hook failed during docker setup")
			return err
		}
		a.cmd = newCmd
	}

	a.logger.Debug("after before docker setup hook", zap.String("cmd", a.cmd))

	// attaching the init container's PID namespace to the app container
	err := a.modifyDockerRun(ctx)
	if err != nil {
		utils.LogError(a.logger, err, "failed to attach init pid")
		return err
	}
	return nil
}

// SetOnAgentRestart sets what sets keploy's agent up again when the compose
// retry's `up` started it again: compose starts an agent it stopped, and
// recreates one under a recreate flag, with none of what the run set up on the
// one before. The agent client puts that setup back.
func (a *App) SetOnAgentRestart(f func(ctx context.Context) error) {
	a.onAgentRestart = f
}

// ModifyDockerRun modifies the existing Docker command to attach the init container's PID namespace
func (a *App) modifyDockerRun(_ context.Context) error {
	if a.cmd == "" {
		return fmt.Errorf("no command provided to modify")
	}
	// Attach the keploy agent container's PID namespace and network namespace to the app container
	// Sharing network namespace so that the docker container IPs of both agent and app remain same
	// sharing pid namespace so that they share the same process tree (common ancestor) in docker
	pidMode := fmt.Sprintf("--pid=container:%s", a.keployContainer)
	networkMode := fmt.Sprintf("--network=container:%s", a.keployContainer)

	keployTLSVolumeName := "keploy-tls-certs"
	keployTLSMountPath := "/tmp/keploy-tls"
	certPath := fmt.Sprintf("%s/ca.crt", keployTLSMountPath)
	trustStorePath := fmt.Sprintf("%s/truststore.jks", keployTLSMountPath)

	tlsFlags := fmt.Sprintf("-v %s:%s:ro ", keployTLSVolumeName, keployTLSMountPath)
	tlsFlags += fmt.Sprintf("-e NODE_EXTRA_CA_CERTS=%s ", certPath)
	tlsFlags += fmt.Sprintf("-e REQUESTS_CA_BUNDLE=%s ", certPath)
	tlsFlags += fmt.Sprintf("-e SSL_CERT_FILE=%s ", certPath)
	tlsFlags += fmt.Sprintf("-e CARGO_HTTP_CAINFO=%s ", certPath)
	// For Java, we append to existing options if possible, or just set it.
	// In CLI args, setting it blindly is usually safe as it overrides or adds.
	// Ideally we would check if -e JAVA_TOOL_OPTIONS exists, but for now:
	javaOpts := fmt.Sprintf("-Djavax.net.ssl.trustStore=%s -Djavax.net.ssl.trustStorePassword=changeit", trustStorePath)
	tlsFlags += fmt.Sprintf("-e JAVA_TOOL_OPTIONS='%s' ", javaOpts)

	if a.opts.MockMode && a.opts.AgentPort != 0 {
		// The app shares the agent's network namespace, so a test can mark its own start and end here.
		tlsFlags += fmt.Sprintf("-e KEPLOY_MOCK_AGENT=http://localhost:%d ", a.opts.AgentPort)
		if token.Session() != "" {
			tlsFlags += "-e " + token.MockAgentTokenEnv + " "
		}
	}
	at := utils.RunSubcommandEnd(a.cmd)
	if at < 0 {
		if a.kind == utils.DockerStart {
			// A container that already exists cannot be given these: its
			// PID and network namespaces were settled when it was created.
			return engine.StartUnsupported(engine.Detect(a.cmd))
		}
		return fmt.Errorf("invalid command structure: no `run` subcommand to add keploy's flags to: %s", a.cmd)
	}

	// The flags go right after the engine's run subcommand, wherever it is:
	// in `sudo docker run` and `docker container run` that is past the second
	// word, and docker takes nothing in front of `run` but its own flags.
	injection := fmt.Sprintf("%s %s %s", pidMode, networkMode, tlsFlags)
	a.cmd = fmt.Sprintf("%s %s %s", a.cmd[:at], injection, strings.TrimLeft(a.cmd[at:], " "))
	a.logger.Debug("added network namespace and pid to docker command", zap.String("cmd", a.cmd))
	return nil
}

// SetupCompose readies a compose command. ctx is the run's, as for SetupDocker:
// the before-setup hook does its work under it.
func (a *App) SetupCompose(ctx context.Context, extraArgs []string) error {
	if a.container == "" {
		utils.LogError(a.logger, nil, "container name not found", zap.String("AppCmd", a.cmd))
		return errors.New("container name not found")
	}

	// In-memory path: compose content was provided directly (e.g. from the enterprise
	// cloud command) to avoid writing sensitive env vars to disk.
	if len(a.opts.InMemoryCompose) > 0 {
		return a.setupComposeInMemory(ctx, extraArgs)
	}

	// In SetupCompose, first we try to find all the docker compose file paths in the current context.
	// Then, we find the compose file which contains the user app container.
	// After that, we add the keploy agent service in a copy of the found user app compose file (by creating docker-compose-tmp.yaml).
	// Finally, we use this modified docker compose file in place of the user's original compose file to run the application with keploy integration.

	paths := findComposeFile(a.cmd)
	if len(paths) == 0 {
		return errors.New("can't find the docker compose file of user. Are you in the right directory? ")
	}

	a.logger.Debug(fmt.Sprintf("Found docker compose file paths: %v", paths))

	newPath := "docker-compose-tmp.yaml"

	serviceInfo, err := a.docker.FindContainerInComposeFiles(paths, a.container)
	if err != nil {
		utils.LogError(a.logger, err, "failed to find container in compose files")
		return err
	}

	if serviceInfo == nil {
		utils.LogError(a.logger, nil, "container not found in any of the compose files", zap.Strings("composePaths", paths), zap.String("container", a.container))
		return fmt.Errorf("container:%v not found in any of the compose files", a.container)
	}

	a.opts.AppPorts = serviceInfo.Ports
	a.opts.AppNetworks = serviceInfo.Networks
	a.opts.ExtraArgs = extraArgs
	a.composeService = serviceInfo.AppServiceName
	compose := serviceInfo.Compose

	err = a.docker.ModifyComposeForAgent(compose, a.opts, a.container)
	if err != nil {
		utils.LogError(a.logger, err, "failed to modify compose for keploy integration")
		return err
	}
	if HookImpl != nil {
		changed, err := HookImpl.BeforeDockerComposeSetup(ctx, compose, a.container)
		if err != nil {
			utils.LogError(a.logger, err, "hook failed during docker compose setup")
			return err
		}
		if changed {
			a.logger.Debug("Successfully ran BeforeDockerComposeSetup hook and modified volumes")
		}
	}
	err = a.docker.WriteComposeFile(compose, newPath)
	if err != nil {
		utils.LogError(a.logger, nil, "failed to write the compose file", zap.String("path", newPath))
	}
	a.setComposeSource(newPath, nil, compose)
	a.logger.Debug("Created new temporary docker-compose for keploy internal use", zap.String("path", newPath))

	// Completion dependencies (keploy-app-gate) require
	// --abort-on-container-failure so a successful one-shot exit does not
	// tear the stack down the way --abort-on-container-exit would.
	preferFailureAbort := composeHasCompletionDependency(compose)
	newCmd, composeFileEnv := composeLaunchPlan(a.cmd, newPath, serviceInfo.ComposePath, serviceInfo.AppServiceName, preferFailureAbort)

	if newCmd != "" {
		// Literal compose command: our file is spliced in, as before.
		a.cmd = newCmd
		a.logger.Info(
			"Running application using a temporary Keploy-generated Docker Compose file (will be cleaned up automatically)",
			zap.String("cmd", a.cmd),
			zap.String("composePath", newPath),
		)
		return nil
	}

	// A wrapper (make up, ./start.sh, an npm script) that runs compose
	// internally. Nothing to splice into, so leave the command alone and let
	// the compose it eventually runs find our file through the environment.
	//
	// This is process-global and inherited by every child keploy spawns from
	// here on, not just the application command. That is tolerable because
	// every compose invocation keploy makes itself passes -f explicitly, so
	// none of them consult COMPOSE_FILE.
	if previous, had := os.LookupEnv("COMPOSE_FILE"); had && previous != composeFileEnv {
		a.logger.Warn("overriding an existing COMPOSE_FILE for this run; any files it named "+
			"beyond the one keploy copied are not part of the application's configuration",
			zap.String("previous", previous), zap.String("now", composeFileEnv))
	}
	if err := os.Setenv("COMPOSE_FILE", composeFileEnv); err != nil {
		utils.LogError(a.logger, err, "failed to point the wrapper command at keploy's compose file")
		return err
	}

	a.logger.Info(
		"Running application through your command with COMPOSE_FILE pointed at a temporary "+
			"Keploy-generated Docker Compose file (will be cleaned up automatically)",
		zap.String("cmd", a.cmd),
		zap.String("composePath", composeFileEnv),
	)
	// Stated up front because each one otherwise fails a long way from its cause.
	a.logger.Warn(
		"the command is not a literal docker compose invocation, so keploy cannot modify it directly",
		zap.String("cmd", a.cmd),
		zap.String("if it passes its own -f", "COMPOSE_FILE is ignored and keploy's agent will not be injected"),
		zap.String("if it runs compose detached (up -d)", "the command returns immediately and keploy stops, treating it as the app exiting"),
		zap.String("if it sets its own project name", "keploy's teardown may not reach every container it started"),
		zap.String("also", "--abort-on-container-exit cannot be added, so keploy will not exit on its own if the app container dies"),
	)
	// keploy-app-gate exits 0 when MakeAgentReady releases the app. A wrapper
	// that itself passes --abort-on-container-exit will tear the stack down at
	// that moment (#4894). Keploy cannot rewrite the wrapper, so say so here.
	if preferFailureAbort && strings.Contains(a.cmd, "--abort-on-container-exit") {
		a.logger.Warn(
			"your command passes --abort-on-container-exit; with keploy-app-gate that aborts the stack when the gate releases the app — use --abort-on-container-failure instead",
			zap.String("cmd", a.cmd),
		)
	}

	return nil
}

// setupComposeInMemory handles the in-memory variant of SetupCompose. It parses the
// compose YAML that was passed via opts.InMemoryCompose (never written to disk),
// injects the keploy-agent service, serialises the result back to bytes, and
// configures the command to pipe the content via stdin ("-f -").
func (a *App) setupComposeInMemory(ctx context.Context, extraArgs []string) error {
	var compose docker.Compose
	if err := yaml.Unmarshal(a.opts.InMemoryCompose, &compose); err != nil {
		return fmt.Errorf("failed to parse in-memory compose YAML: %w", err)
	}

	serviceInfo, err := a.docker.FindContainerInCompose(&compose, a.container)
	if err != nil {
		utils.LogError(a.logger, err, "failed to find container in in-memory compose")
		return err
	}

	a.opts.AppPorts = serviceInfo.Ports
	a.opts.AppNetworks = serviceInfo.Networks
	a.opts.ExtraArgs = extraArgs
	a.composeService = serviceInfo.AppServiceName

	if err := a.docker.ModifyComposeForAgent(&compose, a.opts, a.container); err != nil {
		utils.LogError(a.logger, err, "failed to modify in-memory compose for keploy integration")
		return err
	}

	if HookImpl != nil {
		changed, err := HookImpl.BeforeDockerComposeSetup(ctx, &compose, a.container)
		if err != nil {
			utils.LogError(a.logger, err, "hook failed during in-memory docker compose setup")
			return err
		}
		if changed {
			a.logger.Debug("Successfully ran BeforeDockerComposeSetup hook and modified volumes")
		}
	}

	content, err := a.docker.MarshalCompose(&compose)
	if err != nil {
		return fmt.Errorf("failed to serialise modified compose to YAML: %w", err)
	}
	a.setComposeSource("", content, &compose)

	// Ensure the command uses stdin ("-f -") and has the exit-code-from flags.
	preferFailureAbort := composeHasCompletionDependency(&compose)
	a.cmd = ensureInMemoryComposeFlags(a.cmd, a.composeService, preferFailureAbort)

	a.logger.Info("Running application using in-memory Keploy-generated Docker Compose (no file written to disk)",
		zap.String("cmd", a.cmd))

	return nil
}

func composeHasCompletionDependency(compose *docker.Compose) bool {
	if compose == nil || compose.Services.Content == nil {
		return false
	}
	for i := 0; i+1 < len(compose.Services.Content); i += 2 {
		service := compose.Services.Content[i+1]
		if service == nil {
			continue
		}
		for j := 0; j+1 < len(service.Content); j += 2 {
			if service.Content[j].Value == "depends_on" && dependsOnRequiresCompletion(service.Content[j+1]) {
				return true
			}
		}
	}
	return false
}

func dependsOnRequiresCompletion(dependsOn *yaml.Node) bool {
	if dependsOn == nil || dependsOn.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(dependsOn.Content); i += 2 {
		dep := dependsOn.Content[i+1]
		if dep == nil || dep.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(dep.Content); j += 2 {
			if dep.Content[j].Value == "condition" && dep.Content[j+1].Value == "service_completed_successfully" {
				return true
			}
		}
	}
	return false
}

func (a *App) SetAppCommand(appCommand string) {
	a.logger.Debug("Setting App Command", zap.String("cmd", appCommand))
	a.cmd = appCommand
}

func (a *App) GetAppCommand() string {
	return a.cmd
}

func (a *App) Kind(_ context.Context) utils.CmdType {
	return a.kind
}

func (a *App) Run(ctx context.Context) models.AppError {
	a.noteTeardownScope(ctx)
	return a.run(ctx)
}

// noteTeardownScope keeps the teardown scope ctx carries, if any, as the
// app's (App.teardown).
func (a *App) noteTeardownScope(ctx context.Context) {
	if s := utils.TeardownScopeOf(ctx); s != nil {
		a.teardown.Store(s)
	}
}

func (a *App) RecentLogs(ctx context.Context) string {
	return a.recentAppLogs(ctx)
}

func (a *App) waitTillExit() {
	// Bounded overall (waitTillExitBudget), its reads of the container
	// included, so a saturated daemon can't wedge this on the teardown
	// goroutine. Each read asks the daemon again when it fails, until it
	// answers or the wait's bound ends (inspectContainer): with a 2 s budget
	// per read, a loaded daemon's slow answer ended the wait at once, the
	// container still running. Only reached on the non-compose docker path.
	ctx, cancel := context.WithTimeout(context.Background(), waitTillExitBudget)
	defer cancel()
	logTicker := time.NewTicker(1 * time.Second)
	defer logTicker.Stop()

	containerID := a.container
	for {
		select {
		case <-logTicker.C:
			info, read, gone := a.inspectContainer(ctx, containerID, fmt.Sprintf("container %q (with no answer, keploy stops waiting for it to exit)", containerID))
			if gone {
				a.logger.Debug("the container is gone", zap.String("containerID", containerID))
				return
			}
			if !read {
				a.logger.Debug("timeout waiting for the container to stop", zap.String("containerID", containerID))
				return
			}

			a.logger.Debug("container status", zap.String("status", info.State.Status), zap.String("containerName", a.container))
			// Check if container is stopped or dead
			if info.State.Status == "exited" || info.State.Status == "dead" {
				return
			}
		case <-ctx.Done():
			a.logger.Debug("timeout waiting for the container to stop", zap.String("containerID", containerID))
			return
		}
	}
}

// composeGraceTick is the pace at which composeAppGrace looks at the app's
// container and at compose's up: a walking pace, not a bound.
const composeGraceTick = 500 * time.Millisecond

// composeAppGrace gives the app's container keploy interrupted graceBudget to
// exit: its coverage flush is done once it has (Go has written GOCOVERDIR,
// Java its jacoco .exec), with no reason to keep waiting on the slower
// sibling services. It ends early once upReturned (set) says compose's up has
// returned, all services stopped, or the daemon says the container exited.
// The grace is wall-clock-bounded; its reads end only on the daemon's answer
// or the grace's end (inspectContainer): with a 2 s budget per read, a loaded
// daemon's slow answers never came in time, and an app that had exited at
// once was given the whole grace, and then some. The up's return ends the
// grace while a read waits on the daemon too: it is watched beside the reads,
// not between them, so a daemon that fails every read does not hold a grace
// whose up has returned.
func (a *App) composeAppGrace(upReturned func() bool) {
	ctx, cancel := context.WithTimeout(context.Background(), graceBudget)
	defer cancel()
	if upReturned != nil {
		watched := make(chan struct{})
		defer func() { cancel(); <-watched }()
		go func() {
			defer close(watched)
			tick := time.NewTicker(composeGraceTick)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				if upReturned() {
					a.logger.Debug("docker compose process exited")
					cancel()
					return
				}
			}
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(composeGraceTick):
		}
		info, read, _ := a.inspectContainer(ctx, a.container, fmt.Sprintf("the application's container %q (with no answer, keploy gives it the rest of its grace)", a.container))
		if read && (info.State.Status == "exited" || info.State.Status == "dead") {
			a.logger.Debug("app container stopped gracefully; tearing down the rest of the compose stack")
			return
		}
	}
}

func (a *App) recentAppLogs(ctx context.Context) string {
	if !utils.IsDockerCmd(a.kind) {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if len(a.composeContent) > 0 {
		return a.recentAppOutput()
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	logTarget := a.logTargetContainer(ctx)
	if logTarget == "" {
		return ""
	}

	logReader, err := a.docker.ContainerLogs(ctx, logTarget, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "50",
	})
	if err != nil {
		a.logger.Debug("failed to fetch recent app logs", zap.String("container", logTarget), zap.Error(err))
		return ""
	}
	defer func() {
		if closeErr := logReader.Close(); closeErr != nil {
			a.logger.Debug("failed to close recent app logs reader", zap.String("container", logTarget), zap.Error(closeErr))
		}
	}()

	rawLogs, err := io.ReadAll(logReader)
	if err != nil {
		a.logger.Debug("failed to read recent app logs", zap.String("container", logTarget), zap.Error(err))
		return ""
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdoutBuf, &stderrBuf, bytes.NewReader(rawLogs)); err != nil {
		return trimRecentAppLogs(string(rawLogs), 20)
	}

	combined := strings.TrimSpace(stdoutBuf.String())
	stderrLogs := strings.TrimSpace(stderrBuf.String())
	if stderrLogs != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += stderrLogs
	}

	return trimRecentAppLogs(combined, 20)
}

// recentAppOutput is recentAppLogs for keploy's own compose stack (in-memory
// compose content), in-process or run as a `docker compose` child: the app
// container's tail as keploy took it from what compose attached to
// (docker.ServiceOutputTail), cut, and never `docker logs`. So the stack's
// containers need no log driver that stores what they print on the docker
// host's disk, and the answer comes after compose has removed the container.
// The tail is of the stack's project and of its last Up alone (one test
// set), as `docker logs` of that Up's container was; with no stack built or
// started, nothing ran and there is none.
func (a *App) recentAppOutput() string {
	project := a.composeOutputProject()
	if project == "" {
		return ""
	}
	var tail []byte
	if a.composeService != "" {
		tail = docker.ServiceOutputTail(project, a.composeService)
	} else if a.container != "" {
		tail = docker.AppOutputTail(project, a.container)
	}
	return trimRecentAppLogs(string(tail), 20)
}

// composeOutputProject is the compose project the tails of keploy's own
// stack are kept under: the library's project when this build drives the
// stack in-process, else the project of the last `docker compose` child
// keploy started for it (composeChildTee); "" when there is none.
func (a *App) composeOutputProject() string {
	if a.useComposeLibrary() {
		a.composeRunnerMu.Lock()
		r := a.composeRunnerVal
		a.composeRunnerMu.Unlock()
		if r == nil {
			return ""
		}
		return r.ProjectName()
	}
	return a.composeChildOut.Load().Project()
}

// executeCommand runs runCmd once as a child process (utils.ExecuteCommand),
// with keploy's own compose document on its stdin when there is one, and that
// document's containers' output kept in their tails (composeChildTee).
func (a *App) executeCommand(ctx context.Context, runCmd string, runEnv []string, cancel func(*exec.Cmd) func() error) utils.CmdError {
	return utils.ExecuteCommand(ctx, a.logger, runCmd, a.kind, cancel, 25*time.Second, a.composeContent, runEnv, a.composeChildTee(ctx)...)
}

// composeChildTee is the output tee for the `docker compose` child that runs
// keploy's own compose stack in a build without the compose library: it
// starts a new run of the project's tails (one per Up, as the library's Up
// does) and keeps each container's lines there, cut, from what compose prints
// (docker.ComposeChildOutput). The project is the one the child's compose
// takes: the command's -p, else its COMPOSE_PROJECT_NAME assignment, else
// keploy's environment or an env file's (the command's --env-file, else the
// project's .env), the document's name: or the project directory's name. None
// for any other command, which keeps `docker logs`; and none when keploy's
// loader cannot load the document (the library path refuses it alike), when
// the app's recent output is then empty, said once per document. The loader
// sees keploy's environment and the env files compose reads, not what only
// the shell gives the child: an assignment in the command (`DB_PASS=x docker
// compose up`) or a flag value the shell expands (`--env-file $HOME/x.env`).
func (a *App) composeChildTee(ctx context.Context) []utils.ExecOption {
	if len(a.composeContent) == 0 || a.useComposeLibrary() {
		return nil
	}
	projectName, projectDir := composeProjectScope(a.cmd)
	if projectName == "" {
		projectName = normalizeComposeProjectName(composeProjectEnvAssignment(a.cmd))
	}
	out, err := docker.StartComposeChildOutput(ctx, a.composeContent, projectName, projectDir, composeEnvFiles(a.cmd))
	a.composeChildOut.Store(out)
	if err != nil {
		if a.composeChildWarned.CompareAndSwap(false, true) {
			a.logger.Warn("keploy could not load the compose document it runs, so it keeps none of its containers' output, and an app crash will show no recent output. Keploy reads its own environment and the env files compose reads (--env-file, COMPOSE_ENV_FILES, the project's .env), not an assignment in the command (DB_PASS=x docker compose up) or a flag value the shell expands: export such a variable or put it in an env file, and write an --env-file path out in full",
				zap.Error(err))
		}
		return nil
	}
	return []utils.ExecOption{utils.WithOutputTee(out.Stream)}
}

func (a *App) logTargetContainer(ctx context.Context) string {
	if !utils.IsDockerCmd(a.kind) {
		return ""
	}
	if a.kind != utils.DockerCompose || a.composeService == "" {
		return a.container
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if projectLabel := a.composeProjectLabel(ctx); projectLabel != "" {
		projectArgs := filters.NewArgs(
			filters.Arg("label", "com.docker.compose.service="+a.composeService),
			filters.Arg("label", "com.docker.compose.project="+projectLabel),
		)
		containers, err := a.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: projectArgs})
		if err != nil {
			a.logger.Debug(
				"failed to list compose containers for recent app logs using compose project label; falling back to service-only matching",
				zap.String("service", a.composeService),
				zap.String("project", projectLabel),
				zap.Error(err),
			)
		} else if len(containers) > 0 {
			sort.Slice(containers, func(i, j int) bool {
				return containers[i].Created > containers[j].Created
			})
			return containers[0].ID
		}
	}

	args := filters.NewArgs(filters.Arg("label", "com.docker.compose.service="+a.composeService))
	containers, err := a.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		a.logger.Debug("failed to list compose containers for recent app logs", zap.String("service", a.composeService), zap.Error(err))
		return a.container
	}
	if len(containers) == 0 {
		return a.container
	}
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].Created > containers[j].Created
	})

	return containers[0].ID
}

func (a *App) composeProjectLabel(ctx context.Context) string {
	if a.container == "" {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}

	containerInfo, err := a.docker.ContainerInspect(ctx, a.container)
	if err != nil {
		a.logger.Debug(
			"failed to inspect compose container for project label when fetching recent app logs; falling back to service-only matching",
			zap.String("service", a.composeService),
			zap.String("container", a.container),
			zap.Error(err),
		)
		return ""
	}
	if containerInfo.Config == nil {
		return ""
	}

	return strings.TrimSpace(containerInfo.Config.Labels["com.docker.compose.project"])
}

func trimRecentAppLogs(logs string, maxLines int) string {
	if logs == "" || maxLines <= 0 {
		return ""
	}

	lines := strings.Split(strings.ReplaceAll(logs, "\r\n", "\n"), "\n")
	meaningful := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(sanitizeAppLogLine(line))
		if trimmed == "" {
			continue
		}
		meaningful = append(meaningful, trimmed)
	}

	if len(meaningful) > maxLines {
		meaningful = meaningful[len(meaningful)-maxLines:]
	}

	return strings.Join(meaningful, "\n")
}

func sanitizeAppLogLine(line string) string {
	line = ansiEscapePattern.ReplaceAllString(line, "")
	line = strings.ReplaceAll(line, "\t", "  ")
	return strings.TrimSpace(line)
}

// ComposeDown runs docker compose down to remove all containers and networks
// created by the compose stack. Without this, stopped containers retain
// references to image layers; a subsequent docker image prune can delete
// those layers and corrupt Docker Desktop's overlayfs snapshots. Exported so the
// replay loop can tear the stack down on a per-test-set setup failure
// (agent-readiness timeout) before a retry, not only via run()'s defer.
func (a *App) ComposeDown() {
	// The down runs while the project makes progress on its way down: its
	// containers stopping and going, its network going, as the daemon's own
	// events for the project tell (composeDownContext). It is cut only when
	// nothing of the project moved for the stall bound of keploy's runnable
	// time, never on a number of seconds: a daemon loaded by a stressed CI run
	// takes as long as it takes to stop and remove a stack, and a down cut
	// short leaves the project behind (its dependencies, keploy's agent, its
	// network), for the next `up` to trip over. It was cut at 8 s, with the
	// agent and the app's dependencies still stopping.
	//
	// The teardown's drains (utils.DrainErrGroupProgress) wait for it: the down stops
	// keploy's agent with the stack, an agent's stop in progress
	// (utils.BeginAgentStop), which ends on its own.
	if !a.useComposeLibrary() && len(a.composeContent) == 0 && a.composeFile == "" {
		return
	}
	defer utils.BeginAgentStop()()
	project := a.composeDownProject(context.Background())
	if project != "" {
		a.composeProjectSeen.Store(&project)
	}
	downCtx, downCancel := a.composeDownContext(project)
	defer downCancel()

	downSucceeded := true

	var downCmd *exec.Cmd

	switch {
	case a.useComposeLibrary():
		// In-memory mode: the stack keploy generated itself, so tear it down
		// through the compose library — no `docker` binary, no stdin pipe. The
		// project is resolved from the same -p/--project-directory the `up`
		// used, so this targets exactly the stack that was started.
		//
		// composeDownStopGrace is the library form of `--timeout 1`: stop
		// containers fast instead of the default 10s graceful wait PER
		// container. Between test-sets `keploy test` restarts the whole compose
		// stack; under loaded CI a slow `down` was exceeding the per-test-set
		// teardown drain budget and being abandoned, leaving a half-removed
		// agent container that the next test-set's `up` then collided with
		// ("container name already in use" -> dependency keploy-agent failed to
		// start -> test-set abandoned, no report).
		a.logger.Debug("Running compose down (in-process) using in-memory compose content")
		runner, err := a.composeRunner(downCtx)
		if err != nil {
			a.logger.Debug("could not build the compose runner for teardown; falling through to the force-remove below",
				zap.Error(err))
			downSucceeded = false
			break
		}
		if err := runner.Down(downCtx, composeDownStopGrace); err != nil {
			downSucceeded = false
			a.logger.Debug("compose down finished with error (may be expected if containers already removed, or keploy cut a down that made no progress)",
				zap.Error(err))
		}
	// Reached only where the library is not linked (darwin): same teardown,
	// through `docker compose -f -` with the generated document on stdin.
	case len(a.composeContent) > 0:
		a.logger.Debug("Running docker compose down using in-memory compose content")
		args := []string{"-f", "-"}
		args = append(args, a.composeProjectArgs(project)...)
		args = append(args, "down", "--timeout", "1")
		downCmd = composeCommand(downCtx, args...)
		downCmd.Stdin = bytes.NewReader(a.composeContent)
	case a.composeFile != "":
		a.logger.Debug("Running docker compose down to clean up containers and networks",
			zap.String("composeFile", a.composeFile))
		// The SAME project the `up` created (composeProjectArgs): a down that
		// resolved another, cwd-derived, left this stack running, and took
		// down a stack of that name the user runs.
		args := []string{"-f", a.composeFile}
		args = append(args, a.composeProjectArgs(project)...)
		args = append(args, "down", "--timeout", "1")
		downCmd = composeCommand(downCtx, args...)
	default:
		return
	}

	if downCmd != nil {
		if output, err := downCmd.CombinedOutput(); err != nil {
			downSucceeded = false
			a.logger.Debug("docker compose down finished with error (may be expected if containers already removed, or keploy cut a down that made no progress)",
				zap.Error(err), zap.String("output", string(output)))
		}
	}

	// Coverage safety: this runs only AFTER the app has already been stopped by
	// run()'s cmdCancel (SIGINT + grace period, force-kill only if it doesn't
	// exit), so the app's coverage flush has already happened — Go writes
	// GOCOVERDIR, Java its jacoco .exec (both to host-mounted paths that survive
	// container removal), and the Java SDK streams coverage over a socket during
	// the run. The --timeout 1 above and the force-remove below therefore only
	// reclaim already-stopped containers; they never shorten a runtime's
	// coverage-flush window (that window is cmdCancel's grace, not down).
	//
	// Belt-and-suspenders: guarantee the agent + app container names are free
	// before the next test-set's compose up reuses them. `compose down` already
	// removes them, but if it errored or was cut short under load the names
	// could linger and the next up would hit "container name already in use".
	// A force-remove by name is near-instant and idempotent.
	a.forceRemoveContainerByName(a.keployContainer)
	a.forceRemoveContainerByName(a.container)

	// Reap-barrier: `compose down` / `rm -f` can return before the daemon has
	// finished reaping under load, so the next compose up's same-name create
	// races the async reap and stalls at "Creating". Poll until the names are
	// gone, BOUNDED so it never reintroduces the unbounded teardown that
	// --timeout 1 removed. Largely a no-op once the stack is reused across
	// test-sets (one down per lane), but defends the first/last and
	// non-keep-alive boundaries.
	// Everything ABOVE frees two names: the agent and the app. A compose stack
	// is not two containers, and `down` removes every service in the compose
	// file — so when the bounded down is cut short, the dependency containers it
	// had not reached yet simply survive. The next test-set's `up` then REUSES
	// them: same container id, same filesystem, same database rows as the
	// previous test-set, while the agent and app around them are brand new. A
	// fresh app talking to a stale database is how one test-set's writes become
	// the next one's unexplained mock mismatches (#4614).
	//
	// Reproduced with a 26-container project whose `down` was killed mid-way:
	// 23 of the 26 containers carried the SAME container id across the teardown
	// into the next up. Only the agent and the app were fresh.
	//
	// Only when the down did NOT succeed. A down that exited 0 removed every
	// service it targets, so there is nothing left to finish, and skipping keeps
	// both the extra docker calls and the risk below off the healthy path.
	var stragglers []string
	if !downSucceeded {
		// The project read before the down: keploy's agent and app, whose
		// labels said it, are gone by now (removed above), and resolved
		// again the sweep took the directory's default project, a stack the
		// user runs, and left keploy's own leftovers.
		stragglers = a.sweepStragglingProjectContainersOf(project)
	}

	a.waitContainersRemoved(append([]string{a.keployContainer, a.container}, stragglers...), reapBarrierBudget)

	// A down cut short leaves the project's networks too: compose removes
	// them last, once the containers on them are gone. The sweep above
	// removed those containers; their networks go now.
	if !downSucceeded {
		a.sweepProjectNetworks(project)
	}
}

// composeDownStall is how long, in keploy's runnable time, a compose down may
// go with nothing of its project moving before keploy cuts it (the watchdog's
// stall bound, stall.HangBudget). A test sets it shorter.
var composeDownStall = stall.HangBudget

// composeDownProject is the compose project the down takes down. In-process
// (the compose library), the project the library's up ran, which it knows
// without asking the daemon. Else the label compose put on the app's
// container, or on keploy's agent's; else the project compose itself takes,
// as the compose command names it (-p), or as COMPOSE_PROJECT_NAME does, set
// in the command (which only the shell a `docker compose` command runs in
// reads) or in keploy's environment; "" when none tells it. Each label is
// read on the daemon's answer, however slow (composeProjectLabelAnswered):
// with a 2 s budget, a loaded daemon's slow reads named no project, and the
// down took the one compose takes from the cwd, a stack the user runs.
func (a *App) composeDownProject(ctx context.Context) string {
	if a.useComposeLibrary() {
		a.composeRunnerMu.Lock()
		r := a.composeRunnerVal
		a.composeRunnerMu.Unlock()
		if r != nil {
			if p := r.ProjectName(); p != "" {
				return p
			}
		}
	}
	const unanswered = "keploy names the project by what the command says"
	if p := a.composeProjectLabelAnswered(ctx, a.container, unanswered); p != "" {
		return p
	}
	if p := a.composeProjectLabelAnswered(ctx, a.keployContainer, unanswered); p != "" {
		return p
	}
	// Read off the labels by an earlier down, when the containers that bore
	// them are gone (the project the up ran, so a -p of a.cmd's too).
	if p := a.composeProjectSeen.Load(); p != nil && *p != "" {
		return *p
	}
	if p := composeProjectFlag(a.cmd); p != "" {
		return p
	}
	// The library never runs the command in a shell: an assignment in it
	// does not reach its project, keploy's own environment does.
	if !a.useComposeLibrary() {
		if p := composeProjectEnvAssignment(a.cmd); p != "" {
			return normalizeComposeProjectName(p)
		}
	}
	return normalizeComposeProjectName(strings.TrimSpace(os.Getenv("COMPOSE_PROJECT_NAME")))
}

// normalizeComposeProjectName is a project name as compose-go's
// loader.NormalizeProjectName makes it: lowercased, only [a-z0-9_-] kept, no
// leading '_' or '-'. compose v2 refuses an unnormalized COMPOSE_PROJECT_NAME
// at the up, as it refuses such a -p, so for a project it ran this is no
// change; docker-compose v1 normalized the name it read, and passed raw as
// -p to keploy's own (v2) calls, a name like "MyProj" was refused.
func normalizeComposeProjectName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "_-")
}

// composeProjectEnvAssignment is COMPOSE_PROJECT_NAME as cmd's leading
// assignments set it (`COMPOSE_PROJECT_NAME=x docker compose up`), "" when
// they do not.
func composeProjectEnvAssignment(cmd string) string {
	for _, f := range strings.Fields(cmd) {
		name, value, ok := strings.Cut(f, "=")
		if !ok || name == "" || strings.HasPrefix(name, "-") {
			return ""
		}
		if name == "COMPOSE_PROJECT_NAME" {
			return strings.Trim(value, `"'`)
		}
	}
	return ""
}

// composeProjectLabelAnswered is the compose project label on container
// name, as the daemon says it: asked again when a read fails (askDaemon),
// until the daemon answers, says it has no such container, ctx ends, or
// keploy's runnable-time bound on no answer passes. "" when it has none, or
// the daemon never said; unanswered, in the log, says what keploy then does.
func (a *App) composeProjectLabelAnswered(ctx context.Context, name, unanswered string) string {
	if name == "" {
		return ""
	}
	info, err := askDaemon(ctx, a.logger, fmt.Sprintf("the compose project label of container %q (with no answer, %s)", name, unanswered),
		func(ctx context.Context) (container.InspectResponse, error) {
			return a.docker.ContainerInspect(ctx, name)
		})
	if err != nil || info.Config == nil {
		return ""
	}
	return strings.TrimSpace(info.Config.Labels["com.docker.compose.project"])
}

// composeProjectArgs are the project flags of a compose call keploy makes on
// the app's project (its down, its ps): the compose command's own -p,
// --project-name and --project-directory, and, when it names no project, -p
// project, the project the up ran (composeDownProject: the compose label on
// the app's or keploy's agent's container, which compose put there). A
// project the command set some other way does not reach keploy's own calls:
// an assignment in the command (`COMPOSE_PROJECT_NAME=x docker compose up`),
// which only the shell the up ran in read, or an --env-file. Resolved without
// it, the down took the cwd's default project down, a stack the user runs,
// and left the app's running. With no project known, the command's flags
// alone, as before.
func (a *App) composeProjectArgs(project string) []string {
	flags := extractProjectFlags(a.cmd)
	if project == "" || composeProjectFlag(a.cmd) != "" {
		return flags
	}
	return append(flags, "-p", project)
}

// composeProjectFlag is the project the compose command cmd names (-p,
// --project-name), "" when it names none.
func composeProjectFlag(cmd string) string {
	flags := extractProjectFlags(cmd)
	for i := 0; i < len(flags); i++ {
		switch {
		case (flags[i] == "-p" || flags[i] == "--project-name") && i+1 < len(flags):
			return flags[i+1]
		case strings.HasPrefix(flags[i], "-p="):
			return strings.TrimPrefix(flags[i], "-p=")
		case strings.HasPrefix(flags[i], "--project-name="):
			return strings.TrimPrefix(flags[i], "--project-name=")
		}
	}
	return ""
}

// composeDownSteps are the steps a container of the project takes on its way
// out in a down; each one a container reaches, once, is the down's progress.
var composeDownSteps = []events.Action{events.ActionKill, events.ActionDie, events.ActionStop, events.ActionDestroy}

// composeDownContext is the context a compose down of project runs under. It
// is cancelled when no container of the project (under its compose label)
// has reached a step on its way out (composeDownSteps) for composeDownStall
// of keploy's runnable time, or at the user's Ctrl+C, which the wait offers
// after the run's interrupt (utils.StopNowNotice), as a stop that waits on
// progress does, so the wait can always be ended. Nothing else of the
// project is progress: a running container's healthcheck (keploy's agent
// runs one every 5 s) and its health status, or a restart loop's dying over
// and over, go on while a down is wedged. Network events carry no labels,
// so the network's going is not seen; its containers' going is. With no
// project to watch, nothing tells progress, and the stall bound alone
// bounds the down.
//
// Its cancel returns once the meter's watch has ended. The subscription's
// goroutine is not waited for: it ends as the docker client gives up the
// subscription at ctx's end, and a client that did not would hang the
// teardown instead of leaking it.
func (a *App) composeDownContext(project string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	var moved atomic.Uint64
	if project != "" {
		// Subscribed on its own goroutine, not before the down: the docker
		// client's Events returns only once the daemon has answered, which a
		// saturated one may not do soon, and the stall bound has to stand
		// from the down's start whatever the daemon does.
		go func() {
			args := filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project),
				filters.Arg("type", string(events.ContainerEventType)))
			for _, step := range composeDownSteps {
				args.Add("event", string(step))
			}
			msgs, errs := a.docker.Events(ctx, events.ListOptions{Filters: args})
			seen := map[string]bool{}
			for {
				select {
				case m, ok := <-msgs:
					if !ok {
						return
					}
					// Checked here too, not left to the daemon's filter: a
					// step counts once per container, whatever the daemon
					// sends.
					if !isComposeDownStep(m) {
						continue
					}
					if k := m.Actor.ID + "/" + string(m.Action); !seen[k] {
						seen[k] = true
						moved.Add(1)
					}
				case err := <-errs:
					// No more news of the project: the stall bound stands.
					if ctx.Err() == nil {
						a.logger.Debug("the docker events of the compose project stopped during its down", zap.String("project", project), zap.Error(err))
					}
					return
				}
			}
		}()
	} else {
		a.logger.Debug("no compose project name for the down to watch; it is bounded by the stall bound alone")
	}
	meter := stall.NewMeter(nil, composeDownStall)
	notice := utils.NewStopNowNotice(utils.StopOffer)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		defer meter.Stop()
		defer notice.Withdraw()
		offering := false
		last := moved.Load()
		for {
			select {
			case <-ctx.Done():
				return
			case <-notice.Interrupt():
				a.logger.Warn("keploy cuts docker compose down at your Ctrl+C, and removes what it left", zap.String("project", project))
				cancel()
				return
			case <-meter.Wait():
			}
			if !offering && notice.Next() {
				offering = true
				a.logger.Info("keploy waits for docker compose down to take the project down, as long as it makes progress; press Ctrl+C again to cut it now",
					zap.String("project", project))
			}
			if n := moved.Load(); n != last {
				last = n
				meter.Reset()
				a.teardown.Load().Note()
				continue
			}
			if meter.Expired() {
				a.logger.Warn("docker compose down made no progress taking the project down; keploy cuts it, and removes what it left",
					zap.String("project", project), zap.Duration("stall", meter.Grace()))
				cancel()
				return
			}
		}
	}()
	return ctx, func() { cancel(); <-watched }
}

// isComposeDownStep reports whether m is a container's step on its way out
// in a down (composeDownSteps).
func isComposeDownStep(m events.Message) bool {
	if m.Type != events.ContainerEventType || m.Actor.ID == "" {
		return false
	}
	for _, step := range composeDownSteps {
		if m.Action == step {
			return true
		}
	}
	return false
}

// sweepProjectNetworks removes the networks of project that a cut-short down
// left, each removal bounded as the container sweep's is (projectSweepBudget).
// A network another container of the user's still uses is not removed (the
// daemon refuses it), and is the user's.
func (a *App) sweepProjectNetworks(project string) {
	if project == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), projectSweepBudget)
	defer cancel()
	nets, err := a.docker.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project))})
	if err != nil {
		a.logger.Debug("could not list the compose project's networks after a cut-short down", zap.String("project", project), zap.Error(err))
		return
	}
	for _, n := range nets {
		if err := a.docker.NetworkRemove(ctx, n.ID); err != nil {
			a.logger.Debug("could not remove a network the compose down left", zap.String("network", n.Name), zap.Error(err))
		}
	}
}

// setComposeSource records the compose source this run will drive, and the
// service list that goes with it, in ONE place.
//
// They are a unit, and separating them has a silent failure mode: the sweep
// needs the service list to tell keploy's containers from the user's, and
// without it there is nothing to do but skip — which is indistinguishable from
// "the teardown was clean". Assigning them together means a future refactor
// that drops the service list is a compile error rather than a quiet return of
// the bug the sweep exists to fix.
//
// It must be called AFTER ModifyComposeForAgent so the injected agent service
// is in the set; the sweep is allowed to remove that one.
func (a *App) setComposeSource(file string, content []byte, compose *docker.Compose) {
	a.composeFile = file
	a.composeContent = content
	a.composeServices = composeServiceNames(compose)

	// Drop any runner built from the PREVIOUS document. Keeping it would point
	// `down`/`ps` at the stack of the last test-set.
	a.composeRunnerMu.Lock()
	a.composeRunnerVal = nil
	a.composeRunnerMu.Unlock()
	a.composeChildOut.Store(nil)
	a.composeChildWarned.Store(false)
}

// composeServiceNames returns the service keys of a parsed compose document.
// Services is a YAML mapping node, so its Content alternates key, value.
func composeServiceNames(compose *docker.Compose) []string {
	if compose == nil || compose.Services.Content == nil {
		return nil
	}
	names := make([]string, 0, len(compose.Services.Content)/2)
	for i := 0; i+1 < len(compose.Services.Content); i += 2 {
		if name := compose.Services.Content[i].Value; name != "" {
			names = append(names, name)
		}
	}
	return names
}

// sweepStragglingProjectContainers force-removes the containers of KEPLOY'S OWN
// compose services that a cut-short `down` left standing, and returns their ids
// so the reap barrier can cover them too.
//
// Scoped to a.composeServices, and that scoping is the point rather than a
// refinement. The obvious implementation — take everything compose reports for
// the project — is wrong, because `docker compose down` does NOT remove
// everything carrying the project label. It removes the services in the compose
// file it was given, and deliberately leaves ORPHANS: containers in the same
// project whose service is not in that file. Those belong to the user.
//
// The default project name is the working directory's basename, so this needs
// no exotic setup to bite. Someone doing the ordinary thing —
//
//	docker compose -f docker-compose.deps.yml up -d     # their postgres, redis
//	keploy test -c "docker compose up"                  # keploy's stack
//
// lands both in one project. An unscoped sweep force-removes their postgres;
// it is not in keploy's compose file, so the next `up` never recreates it and
// every later test-set fails to reach a database. That is a worse outcome than
// the bug this exists to fix, and it would fire on the dependency-retry path
// mid-run, not just at the end.
//
// Asking compose — rather than filtering `docker ps` on the project label —
// keeps the project resolving exactly as it did for the `up` and the `down`:
// same -f source, same -p/--project-directory off the user's command, same cwd
// and environment. Best-effort and bounded throughout; on any failure the
// teardown behaves exactly as it did before this sweep existed.
//
// Not covered, and a real gap: a multi-file command
// (`docker compose -f app.yml -f deps.yml up`). SetupCompose tmp-copies only
// the file that contains the app, so composeServices holds only that file's
// services — and `down` on the generated file treats deps.yml's containers as
// orphans and leaves them too. The sweep is still exactly `down`'s set, so it
// is not WRONG, but that user's dependency containers survive every teardown
// and the next `up` reuses them with the same rows, which is the defect this
// function exists to fix. Closing it means making the teardown aware of every
// -f the user passed, not widening this filter.
func (a *App) sweepStragglingProjectContainers() []string {
	return a.sweepStragglingProjectContainersOf("")
}

// sweepStragglingProjectContainersOf is sweepStragglingProjectContainers on
// project, the project the up ran ("": resolved now, composeDownProject).
func (a *App) sweepStragglingProjectContainersOf(project string) []string {
	if len(a.composeServices) == 0 {
		// Not expected: both compose setup paths populate this. Said out loud
		// because the failure is otherwise indistinguishable from "nothing to
		// clean up", which is how this whole function came to be a no-op once
		// before.
		a.logger.Debug("no compose service list for this run; skipping the teardown sweep")
		return nil
	}
	owned := make(map[string]struct{}, len(a.composeServices))
	for _, svc := range a.composeServices {
		owned[svc] = struct{}{}
	}

	var ids []string
	for _, st := range a.composeServiceStatesOfWithin(projectSweepBudget, project) {
		if st.ID == "" {
			continue
		}
		if isComposeOneOff(st.Labels) {
			continue
		}
		if _, mine := owned[st.Service]; mine {
			ids = append(ids, st.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	a.logger.Debug("compose down left containers behind; removing the rest of keploy's own services",
		zap.Int("count", len(ids)), zap.Strings("services", a.composeServices))
	a.forceRemoveContainers(ids)
	return ids
}

// forceRemoveContainers removes the given containers in ONE `docker rm -f`
// call, under a single shared deadline. One call rather than one per container
// because this runs inside the teardown drain budget: N sequential removals
// against a saturated daemon is exactly the unbounded teardown the per-call
// budgets exist to prevent.
func (a *App) forceRemoveContainers(ids []string) {
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), projectSweepBudget)
	defer cancel()

	// Concurrent, not sequential: the Engine API removes one container per
	// call, and N sequential removals against a saturated daemon is exactly the
	// unbounded teardown the per-call budgets exist to prevent. Issuing them
	// together under ONE deadline keeps the original single-call bound.
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := a.docker.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
				a.logger.Debug("force-remove of a remaining project container finished "+
					"(it may already be gone, or the bounded deadline elapsed)",
					zap.String("container", id), zap.Error(err))
			}
		}(id)
	}
	wg.Wait()
}

// Teardown budgets. The record/replay teardown drains the app-runner goroutine
// while the run's teardown makes progress, and gives it up after 30 s of
// keploy's runnable time without any (utils.DrainErrGroupProgress, in
// pkg/service/record/record.go and pkg/service/replay/replay.go). The
// compose down is not under that bound: it holds the drains while it runs
// (utils.BeginAgentStop, ComposeDown), and ends on
// its own, at the down's end or at keploy's cut of a down that made no
// progress (composeDownStall). The docker calls around it are each bounded,
// so the rest of a compose teardown (run()'s cmdCancel) returns well inside
// 30s even when the daemon is saturated: the grace loop (graceBudget, its
// reads of the app's container included: composeAppGrace), and, once the
// down ends, 2×forceRemoveBudget + 2×projectSweepBudget + reapBarrierBudget +
// projectSweepBudget (the network sweep) ≈ 5 + 2×2 + 2×2 + 3 + 2 ≈ 18s with
// every single call timed out, the drain's bound starting again at the down's
// end. The margin
// matters more than it looks: reaching the SIGKILL is not the end of the
// drain — os/exec only starts its WaitDelay once Cancel returns, and
// record.go gates the post-record mock rewrite on the drain NOT having timed
// out, so an overrun silently skips it.
//
// The sweeps' downSucceeded gate buys nothing here, and it is worth being
// explicit about that rather than counting it twice: a daemon slow enough to
// leave the grace's reads unanswered and time out both force-removes is a daemon whose down
// has likely failed, so in the worst case the gate is open. It exists to keep
// the sweeps off the HEALTHY path, not to bound the bad one; waitTillExit is
// then skipped on the compose path and the ctx-cancelled run returns before
// recentAppLogs — comfortably under 30s.
const (
	// composeDownStopGrace is the library form of the shell-out's `--timeout 1`:
	// the per-container stop grace `down` allows before killing. It bounds the
	// containers, NOT the down call itself, which runs as long as it makes
	// progress (composeDownContext).
	composeDownStopGrace = 1 * time.Second
	// projectSweepBudget bounds EACH of the calls that clean up whatever a
	// cut-short `down` did not reach: the `compose ps` that lists its
	// containers, the single `docker rm -f` that removes them, and the network
	// sweep's list and removals.
	projectSweepBudget = 2 * time.Second
	forceRemoveBudget  = 2 * time.Second // each `docker rm -f` in the teardown drain path
	reapBarrierBudget  = 3 * time.Second // waitContainersRemoved poll
	graceBudget        = 5 * time.Second // cmdCancel coverage-flush grace
	waitTillExitBudget = 8 * time.Second // non-compose post-cancel container wait
	// preRunRemoveBudget bounds the startup-time force-remove of a leftover
	// --name container before a docker-run. Unlike the teardown force-remove it
	// is NOT in the SIGINT drain path, so under a SATURATED docker daemon it
	// should WAIT for the prior container's `docker rm -f` to actually complete
	// rather than give up and let the next `docker run --name` hit "name already
	// in use" (the go-docker-timefreeze flake). Work-slow-not-fail: a busy daemon
	// means a slower removal, never a failed run — sized generously (90s here,
	// ×dockerRunNameConflictRetries below) to outlast realistic CI oversubscription,
	// with perAttempt = budget/3 so each individual rm has room to finish.
	preRunRemoveBudget = 90 * time.Second
	// dockerRunNameConflictRetries bounds how many times a user-app `docker run`
	// is re-issued after a transient container-name conflict (docker exit 125)
	// before the error is surfaced; the name is force-removed (waiting up to
	// preRunRemoveBudget) between attempts. Generous so a saturated daemon is
	// tolerated as slowness, not surfaced as a hard failure.
	dockerRunNameConflictRetries = 5
	// composeDepFailureRetries bounds how many times `docker compose up` is
	// re-issued after a TRANSIENT dependency-startup failure — the case where a
	// container the app depends_on (a DB/emulator/broker) crashes during compose's
	// health-wait under CI contention, so compose aborts with "dependency failed
	// to start: container <dep> exited (N)" before the app service is ever started.
	// Work-slow-not-fail: a flaky dependency container should slow the recording
	// (a bounded, backed-off retry with that dependency afresh), not abort it. Strictly
	// gated by isTransientComposeDependencyFailure so a genuine app failure is
	// NEVER retried — see that predicate. Small N: a dependency that fails to come
	// up on every attempt is a real environment problem the run must still surface.
	composeDepFailureRetries = 3
	// composeDepFailureBaseBackoff is the base inter-attempt backoff for the
	// transient-dependency retry; it grows linearly per attempt (base, 2×base, …)
	// to give a contention-starved daemon progressively more breathing room
	// between bring-ups without unbounding the retry.
	composeDepFailureBaseBackoff = 2 * time.Second
	// composePsStateBudget bounds the `docker compose ps -a` state probe used to
	// classify a failed `up` (transient dependency crash vs genuine app failure).
	composePsStateBudget = 5 * time.Second
)

// waitContainersRemoved polls until the named containers no longer exist (or the
// budget elapses), converting the daemon's async reap into a bounded synchronous
// barrier so the next compose up cannot race a still-removing same-named
// container.
func (a *App) waitContainersRemoved(names []string, budget time.Duration) {
	// Bounded on its own deadline so the reap barrier can never push ComposeDown
	// past the teardown budget.
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		allGone := true
		for _, n := range names {
			if n == "" {
				continue
			}
			// ContainerInspect returns a non-nil error once the container is
			// gone; treat any error as "no longer blocking" (best-effort barrier).
			if _, err := a.docker.ContainerInspect(ctx, n); err == nil {
				allGone = false // still present
				break
			}
		}
		if allGone {
			return
		}
		select {
		case <-ctx.Done():
			a.logger.Debug("waitContainersRemoved: timed out waiting for reap", zap.Strings("containers", names))
			return
		case <-ticker.C:
		}
	}
}

// forceRemoveContainerByName removes a container by name, ignoring the
// "no such container" case. Used after compose down to guarantee a name is free
// for reuse on the next per-test-set compose up.
func (a *App) forceRemoveContainerByName(name string) {
	a.forceRemoveContainerByNameWithin(name, forceRemoveBudget)
}

// forceRemoveContainerByNameWithin is forceRemoveContainerByName with a caller-
// chosen deadline. Teardown callers pass the tight forceRemoveBudget (drain
// path); the startup pre-run cleanup passes preRunRemoveBudget so it can wait
// out a still-running prior container under contention.
func (a *App) forceRemoveContainerByNameWithin(name string, budget time.Duration) {
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := a.docker.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
		a.logger.Debug("force-remove container finished (may already be gone, or the bounded deadline elapsed)",
			zap.String("container", name), zap.Error(err))
	}
}

// keployAgentComposeService is the FIXED compose service key under which keploy
// injects its agent (pkg/platform/docker/docker.go AddKeployAgentToCompose). The
// service's container_name is the per-process-random keploy-v3-<hash>, but the
// service KEY is constant, so docker-compose identifies the agent across phases
// by (project, service=keploy-agent) — not by the changing container name.
const keployAgentComposeService = "keploy-agent"

// removeStaleComposeAgentWithin force-removes any container that docker-compose
// still tracks as the keploy-agent service of THIS run's project, then waits for
// the daemon to finish reaping it — all bounded by the budget. It is the compose
// analogue of ensureContainerNameFreeWithin, but keyed on the compose SERVICE
// rather than the container name.
//
// Root cause it closes: keploy injects the agent under a fixed service key
// (keploy-agent) with a per-process-random container_name (keploy-v3-<hash>).
// Across the record -> auto-replay boundary (atg sandbox) and the keep-app-alive
// recovery, a second `docker compose up` runs in the SAME project while the
// PRIOR phase's agent container is still present (its async teardown removes it
// out-of-band by name). Because the new compose-tmp.yaml gives the keploy-agent
// service a DIFFERENT container_name, compose sees its config drift and plans a
// Recreate of the keploy-agent service: remove-the-old-container-then-create.
// That remove races the concurrent out-of-band removal of the same id and loses
// — "Error response from daemon: No such container: <stale-id>" (or "removal of
// container <id> is already in progress") — aborting `compose up` and failing
// the run (the flaky atg-with-mocks "No such container" regression).
//
// ensureContainerNameFreeWithin(a.keployContainer) does NOT cover this: on a
// fresh phase a.keployContainer is the NEW name, which does not exist yet, so it
// is a no-op; the container that triggers the Recreate is the PRIOR phase's
// agent under a name this App instance never knew. Resolving it via the compose
// project/service (the same way the upcoming `up` will) and force-removing +
// reaping it BEFORE the up guarantees compose only ever CREATES the agent (no
// Recreate, no remove step, no stale-id race). A no-op on the first up of a
// project, when no prior keploy-agent container exists.
func (a *App) removeStaleComposeAgentWithin(budget time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	ids := a.composeAgentContainerIDs(ctx)
	if len(ids) == 0 {
		return // first up of this project, or the prior agent is already gone
	}
	a.logger.Debug("removing stale keploy-agent container(s) from a prior compose phase before the next up",
		zap.Strings("ids", ids))
	for _, id := range ids {
		a.forceRemoveContainerByNameWithin(id, forceRemoveBudget)
	}
	// Reuse the existing reap barrier so the next up cannot race a still-removing
	// agent. waitContainersRemoved tolerates ids as well as names (it inspects by
	// the given string and treats a non-nil error as "gone").
	a.waitContainersRemoved(ids, budget)
}

// composeAgentContainerIDs returns the container ids docker-compose currently
// tracks for the keploy-agent service of this run's project. It shells out to
// `docker compose ... ps -aq keploy-agent` so compose resolves the project name
// EXACTLY as the upcoming `up` will (same -f/-p/project-directory flags and same
// cwd), which is what makes the result line up with the container compose would
// otherwise try to Recreate. Mirrors ComposeDown's branch selection for the
// file-based vs in-memory compose source.
func (a *App) composeAgentContainerIDs(ctx context.Context) []string {
	// In-memory mode: ask the compose library directly. It resolves the project
	// from the same -p/--project-directory the upcoming `up` will use, which is
	// what makes the result line up with the container compose would otherwise
	// try to Recreate.
	if a.useComposeLibrary() {
		runner, err := a.composeRunner(ctx)
		if err != nil {
			a.logger.Debug("could not build the compose runner to list the prior keploy-agent container",
				zap.Error(err))
			return nil
		}
		ids, err := runner.ContainerIDsForService(ctx, keployAgentComposeService)
		if err != nil {
			// A non-existent project / no such service is the common "first up"
			// case; treat it as "nothing to clean up" rather than failing the run.
			a.logger.Debug("could not list the prior keploy-agent compose container (likely the first up of this project)",
				zap.Error(err))
			return nil
		}
		return ids
	}

	var args []string
	switch {
	// Reached only where the library is not linked (darwin): the generated
	// document goes to `docker compose -f -` on stdin, exactly as before.
	case len(a.composeContent) > 0:
		args = []string{"-f", "-"}
		args = append(args, a.composeProjectArgs(a.composeDownProject(ctx))...)
		args = append(args, "ps", "-aq", keployAgentComposeService)
	case a.composeFile != "":
		args = []string{"-f", a.composeFile}
		args = append(args, a.composeProjectArgs(a.composeDownProject(ctx))...)
		args = append(args, "ps", "-aq", keployAgentComposeService)
	default:
		return nil
	}

	cmd := composeCommand(ctx, args...)
	if len(a.composeContent) > 0 {
		cmd.Stdin = bytes.NewReader(a.composeContent)
	}
	// Output(), NOT CombinedOutput(): compose writes warnings to stderr, and
	// this output is parsed as a list of container ids. An obsolete `version:`
	// key or an unset ${VAR} — both of which survive into keploy's generated
	// compose file — would otherwise be handed to `docker rm -f` as if it were
	// a container id.
	out, err := cmd.Output()
	if err != nil {
		// A non-existent project / no such service is the common "first up" case
		// and surfaces here as an error or empty output; treat it as "nothing to
		// clean up" rather than failing the run.
		a.logger.Debug("could not list the prior keploy-agent compose container (likely the first up of this project)",
			zap.Error(err), zap.String("stderr", commandStderr(err)))
		return nil
	}
	return parseComposePSIDs(string(out))
}

// useComposeLibrary reports whether THIS build drives keploy's own generated
// stack in-process.
//
// Two conditions, and both matter. composeContent is the seam: only a document
// keploy generated itself is ever driven by the library. ComposeLibrarySupported
// is false on darwin, which keeps the docker CLI that installed its engine
// (see pkg/platform/docker/compose_backend_unsupported.go, which also covers
// every build made WITHOUT -tags composelib). Every site that would use the
// library asks this first and otherwise keeps the `docker compose -f -`
// shell-out, which is what keploy did before the library existed and is always
// available on a laptop.
func (a *App) useComposeLibrary() bool {
	return len(a.composeContent) > 0 && composeLibrarySupported
}

// composeLibrarySupported is docker.ComposeLibrarySupported. It is a variable
// only so a row can take the library path's decisions (recentAppLogs reading
// the attach tail) in a build without -tags composelib, which is how OSS CI
// builds; production never sets it.
var composeLibrarySupported = docker.ComposeLibrarySupported

// parseComposePSIDs extracts the container ids from `docker compose ps -aq`
// output: one id per line, ignoring blank lines and surrounding whitespace.
// Split out as a pure function so the parsing the stale-agent cleanup hinges on
// is unit-testable without a docker daemon.
func parseComposePSIDs(out string) []string {
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// composeServiceState is one row of `docker compose ps -a` output: the compose
// SERVICE key (stable across phases, unlike the per-process-random
// container_name), the container State ("created", "exited", "running", …), and
// its ExitCode. Only the fields the dependency-failure classifier needs.
type composeServiceState struct {
	Service  string `json:"Service"`
	State    string `json:"State"`
	ExitCode int    `json:"ExitCode"`
	// ID is read only by the teardown sweep, which needs something to hand
	// `docker rm -f`; the dependency-failure classifier ignores it.
	ID string `json:"ID"`
	// Labels is the raw comma-separated label string compose prints. The sweep
	// reads one label out of it, com.docker.compose.oneoff, which is the only
	// way to tell a `compose run` container from a `compose up` one — they carry
	// the same service label.
	Labels string `json:"Labels"`
}

// isComposeOneOff reports whether a `docker compose ps` row describes a
// `compose run` container rather than a `compose up` one.
//
// It matters because `down` leaves one-offs alone, so the teardown sweep must
// too. Service name is not enough to tell them apart: a.composeServices comes
// from a copy of the USER's compose file, so keploy's service names ARE the
// user's, and `docker compose run --rm app rake db:migrate` produces a
// container labelled service=app that is in keploy's set and is still the
// user's to keep.
func isComposeOneOff(labels string) bool {
	for _, l := range strings.Split(labels, ",") {
		if strings.TrimSpace(l) == "com.docker.compose.oneoff=True" {
			return true
		}
	}
	return false
}

// commandStderr pulls the child's stderr out of an *exec.ExitError, for the
// debug logs that used to get it free from CombinedOutput.
func commandStderr(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(exitErr.Stderr)
	}
	return ""
}

// composeServiceStates returns the per-service container states docker-compose
// currently tracks for THIS run's project, by shelling out to
// `docker compose ... ps -a --format json` (the same project-scoping as the
// `up`/`down` so it sees exactly this stack). Mirrors composeAgentContainerIDs'
// branch selection for the file-based vs in-memory compose source. A nil/empty
// result (no source, a daemon error) makes the classifier conservatively report
// "not a transient dependency failure", so the run fails fast rather than
// retrying blindly — the safe default.
func (a *App) composeServiceStates(ctx context.Context) []composeServiceState {
	return a.composeServiceStatesOf(ctx, "")
}

// composeServiceStatesOf is composeServiceStates of project, the project the
// up ran ("": resolved now, composeDownProject).
func (a *App) composeServiceStatesOf(ctx context.Context, project string) []composeServiceState {
	if project == "" && !a.useComposeLibrary() {
		project = a.composeDownProject(ctx)
	}
	// In-memory mode: ask the compose library directly, with the same project
	// scoping the `up`/`down` use so it sees exactly this stack.
	if a.useComposeLibrary() {
		runner, err := a.composeRunner(ctx)
		if err != nil {
			a.logger.Debug("could not build the compose runner to read service states", zap.Error(err))
			return nil
		}
		rows, err := runner.Ps(ctx)
		if err != nil {
			a.logger.Debug("could not read compose service states", zap.Error(err))
			return nil
		}
		states := make([]composeServiceState, 0, len(rows))
		for _, r := range rows {
			states = append(states, composeServiceState{
				Service:  r.Service,
				State:    r.State,
				ExitCode: r.ExitCode,
				ID:       r.ID,
				// Re-join to the comma-separated "k=v,k=v" form `docker compose
				// ps --format json` prints, so isComposeOneOff stays the single
				// classifier for both the library and the shell-out path.
				Labels: joinComposeLabels(r.Labels),
			})
		}
		return states
	}

	var args []string
	switch {
	// Reached only where the library is not linked (darwin).
	case len(a.composeContent) > 0:
		args = []string{"-f", "-"}
		args = append(args, a.composeProjectArgs(project)...)
		args = append(args, "ps", "-a", "--format", "json")
	case a.composeFile != "":
		args = []string{"-f", a.composeFile}
		// The SAME project the `up` created (composeProjectArgs). Without it,
		// the probe queried another (cwd-derived) project, saw nothing, and the
		// failure was (mis)classified as non-transient — silently disabling
		// the retry.
		args = append(args, a.composeProjectArgs(project)...)
		args = append(args, "ps", "-a", "--format", "json")
	default:
		return nil
	}

	cmd := composeCommand(ctx, args...)
	if len(a.composeContent) > 0 {
		cmd.Stdin = bytes.NewReader(a.composeContent)
	}
	// Output(), NOT CombinedOutput(). Compose writes its warnings to stderr —
	//
	//	level=warning msg="The \"TAG\" variable is not set. Defaulting to a blank string."
	//	level=warning msg="... the attribute `version` is obsolete ..."
	//
	// and parseComposeServiceStates returns nil on its first unparseable line.
	// Merging the streams therefore made this function answer "no containers"
	// for any user whose compose file emits a warning, which an obsolete
	// `version:` key or a single unset ${VAR} is enough to do. Both survive into
	// keploy's generated file: Version round-trips via `yaml:"version,omitempty"`
	// and Services is a raw yaml.Node, so `image: app:${TAG}` is preserved
	// verbatim.
	//
	// That silently disabled BOTH callers — the teardown sweep, and the
	// transient-dependency-failure retry, which reads a nil list as
	// "not transient" and fails the run instead of retrying it.
	//
	// With the streams separated the deliberate bail below means what it says
	// again: an unparseable line on STDOUT really is untrustworthy.
	out, err := cmd.Output()
	if err != nil {
		a.logger.Debug("could not list compose service states (treating the failed up as non-transient)",
			zap.Error(err), zap.String("stderr", commandStderr(err)))
		return nil
	}
	return parseComposeServiceStates(string(out))
}

// composeServiceStatesWithin is composeServiceStates bounded by its own
// deadline, so the post-failure state probe can never wedge the run on a
// saturated daemon. Used by the transient-dependency-failure classifier in
// run()'s retry loop.
func (a *App) composeServiceStatesWithin(budget time.Duration) []composeServiceState {
	return a.composeServiceStatesOfWithin(budget, "")
}

// composeServiceStatesOfWithin is composeServiceStatesOf bounded by its own
// deadline, as composeServiceStatesWithin is.
func (a *App) composeServiceStatesOfWithin(budget time.Duration, project string) []composeServiceState {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return a.composeServiceStatesOf(ctx, project)
}

// parseComposeServiceStates parses `docker compose ps -a --format json` output.
// Compose emits NDJSON (one JSON object per line) for `ps`, but older/newer CLIs
// have emitted a single JSON array; tolerate both. Split out as a pure function
// so the classifier it feeds is unit-testable without a docker daemon.
func parseComposeServiceStates(out string) []composeServiceState {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	// Array form: `[{...},{...}]`.
	if strings.HasPrefix(out, "[") {
		var states []composeServiceState
		if err := json.Unmarshal([]byte(out), &states); err != nil {
			return nil
		}
		return states
	}
	// NDJSON form: one object per line.
	var states []composeServiceState
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s composeServiceState
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			// A malformed line means we can't trust the classification; bail so the
			// caller treats the failure as non-transient and fails fast.
			return nil
		}
		states = append(states, s)
	}
	return states
}

// isTransientComposeDependencyFailure classifies a FAILED `docker compose up`
// (runErr != nil, runtime type) as the transient dependency-startup case that a
// bounded retry can recover — and ONLY that case. It is the gate that keeps the
// retry from masking a genuine application failure.
//
// The signature (verified against docker compose v2, and against the compose
// v5.5.1 library in-process: pkg/platform/docker
// TestLiveUpReturnsWhenADependencyFails): when a service the app
// `depends_on: condition: service_healthy/completed_successfully` crashes during
// compose's health-wait, compose prints "dependency failed to start: container
// <dep> exited (N)" and ABORTS BEFORE EVER STARTING THE APP SERVICE. So the app
// service is left in state "created" (exit 0), while the dependency is "exited"
// with a non-zero code. That message is written to the up's STDERR, which keploy
// streams straight to the terminal rather than capturing (same as the docker-run
// name-conflict case), so cmdErr carries only "exit status N" — the classifier
// therefore reads docker's OWN post-mortem state, not the error string.
//
// Returns true iff ALL of:
//  1. the up failed at runtime (runErr != nil, errType == Runtime), AND
//  2. the APP service (appService) is in state "created" — i.e. compose never
//     started it because a depended-on container failed its health gate, AND
//  3. some OTHER service (not the app, not the injected keploy-agent) is in state
//     "exited" with a NON-ZERO exit code — the dependency that actually crashed,
//     AND
//  4. the injected keploy-agent did not crash: stopped by compose (exit 0, or
//     143/137 from its TERM and KILL) is fine, and the retry sets it up again
//     if compose starts it again; exited otherwise, the agent is the failure.
//
// Why it cannot misclassify a genuine app failure: an app that fails to start on
// its OWN (its container runs and exits non-zero, a bad image, a crash on boot)
// leaves the APP service in state "exited", NEVER "created". Condition (2) is the
// firewall — a started-then-failed app can never satisfy it, so it is never
// retried and fails fast with its real exit code. Condition (3) further requires
// an actual crashed dependency to recover, so an abort for any other reason
// (e.g. the app's own dependency is genuinely misconfigured and exits non-zero
// every time) is bounded by composeDepFailureRetries and then surfaced — the
// retry can only DELAY a persistent failure, never hide it.
func isTransientComposeDependencyFailure(runErr error, errType utils.ErrType, appService string, states []composeServiceState) bool {
	if runErr == nil || errType != utils.Runtime || appService == "" {
		return false
	}
	appCreated := false
	depCrashed := false
	agentCrashed := false
	for _, s := range states {
		switch {
		case s.Service == appService:
			// The app must NOT have started. "created" is compose's state for a
			// service it provisioned but never started because a dependency gate
			// failed. Any other state (running/exited/restarting) means the app
			// itself ran — not a dependency-abort — so this is not retryable.
			if s.State == "created" {
				appCreated = true
			}
		case s.Service == keployAgentComposeService:
			// The injected agent is keploy's own service; never count it as the
			// user's crashed dependency. Stopped by compose (0, or 143/137 from
			// its TERM and KILL), it is set up again after the retry; crashed,
			// it is the failure, and retrying would only delay its error.
			if s.State == "exited" && s.ExitCode != 0 && s.ExitCode != 143 && s.ExitCode != 137 {
				agentCrashed = true
			}
		default:
			if isCrashedDependency(appService, s) {
				depCrashed = true
			}
		}
	}
	return appCreated && depCrashed && !agentCrashed
}

// isCrashedDependency reports whether a `docker compose ps` row is a service
// the app depends on that crashed: neither the app, keploy's agent, nor the
// app-gate, and exited non-zero. The gate is keploy infrastructure (like the
// agent); retrying a user dependency crash must not recreate it mid-release.
func isCrashedDependency(appService string, s composeServiceState) bool {
	return s.Service != appService &&
		s.Service != keployAgentComposeService &&
		s.Service != docker.KeployAppGateServiceName &&
		s.State == "exited" && s.ExitCode != 0
}

// removeCrashedDependencies removes the containers of the dependencies that
// crashed, so that the retry's `up` creates them afresh, as the teardown it
// replaced did: a dependency that crashed over state it left in its own
// filesystem would crash the same way again in the same container. The app's
// container, keploy's agent, and `compose run` containers (the user's) stay.
//
// It waits for the removals, so that `up` does not meet a container still
// being removed, up to preRunRemoveBudget in all. It runs on the app-runner
// goroutine, which record and replay drain under a bound (DrainErrGroupProgress), so it
// stops as soon as the run is cancelled: there is no retry to clean up for.
func (a *App) removeCrashedDependencies(ctx context.Context, states []composeServiceState) {
	ctx, cancel := context.WithTimeout(ctx, preRunRemoveBudget)
	defer cancel()
	var ids []string
	for _, s := range states {
		if ctx.Err() != nil {
			return
		}
		if s.ID == "" || isComposeOneOff(s.Labels) || !isCrashedDependency(a.composeService, s) {
			continue
		}
		if err := a.docker.ContainerRemove(ctx, s.ID, container.RemoveOptions{Force: true}); err != nil {
			a.logger.Debug("failed to remove a crashed dependency's container before the retry; compose starts it again as it is",
				zap.String("service", s.Service), zap.String("container", s.ID), zap.Error(err))
			continue
		}
		ids = append(ids, s.ID)
	}
	for _, id := range ids {
		for {
			if _, err := a.docker.ContainerInspect(ctx, id); err != nil {
				break // gone, or ctx is done
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

// shouldRetryComposeUp is the SINGLE decision the compose-up retry loop turns
// on, extracted so the production loop in run() and its unit test call the EXACT
// same predicate (no hand-copied condition that could silently drift). It returns
// true iff a failed `docker compose up` should be retried as a transient
// dependency-startup failure:
//
//   - the run is a docker-compose run (kind), AND
//   - we are still within the bounded attempt budget (attempt <= maxRetries), AND
//   - the run is not being cancelled (ctxErr == nil), AND
//   - the up failed at runtime (runErr != nil, errType == Runtime), AND
//   - isTransientComposeDependencyFailure classifies it as the recoverable
//     dependency-crash case (and ONLY that case).
//
// The per-service states are supplied LAZILY via statesFn, which is invoked ONLY
// after the four cheap predicates above all pass. This matters because the loop
// re-evaluates this predicate on EVERY iteration (including the success and
// ctx-cancelled graceful-stop paths); statesFn shells out to `docker compose ps`
// (up to composePsStateBudget), so gating it behind the cheap checks keeps that
// subprocess off the non-failure paths — it never runs on a successful up or a
// cancelled run, only when a transient-dependency retry is actually plausible.
//
// kind/maxRetries/statesFn are passed in (rather than read off App) so the whole
// decision is a pure function exercisable without a docker daemon.
func shouldRetryComposeUp(kind utils.CmdType, attempt, maxRetries int, ctxErr, runErr error, errType utils.ErrType, appService string, statesFn func() []composeServiceState) bool {
	if kind != utils.DockerCompose ||
		attempt > maxRetries ||
		ctxErr != nil ||
		runErr == nil ||
		errType != utils.Runtime {
		return false
	}
	return isTransientComposeDependencyFailure(runErr, errType, appService, statesFn())
}

// ensureContainerNameFreeWithin force-removes the named container and then
// VERIFIES the name is actually free before returning, retrying within the
// budget. A bare `docker rm -f` returns as soon as it has *initiated* removal,
// but under docker-daemon contention the `--rm` async reaper (kicked off when
// the prior per-test-set container exits) can keep holding the name for a brief
// window after that — long enough for the immediately following
// `docker run --name` to hit "Conflict. The container name ... is already in
// use" (docker exit 125), which fails the test set. Polling `docker ps` until
// the name is gone closes that window deterministically instead of racing into
// the conflict. Best-effort: if the name still isn't free at the deadline we
// warn and proceed (a still-stuck name is a deeper docker problem the run will
// surface anyway).
func (a *App) ensureContainerNameFreeWithin(name string, budget time.Duration) {
	if name == "" {
		return
	}
	deadline := time.Now().Add(budget)
	// Each force-remove gets a generous slice of the overall budget rather than
	// the tight teardown-drain forceRemoveBudget. Under heavy docker-daemon
	// contention a single `docker rm -f` of a still-running prior container can
	// run well past 2s; SIGKILLing it there (CommandContext) tears the docker
	// CLI down mid-request, so the removal may not complete, the name never
	// frees, and the whole budget is burned on repeated stillborn 2s attempts
	// (the observed "container name still in use after the pre-run remove
	// budget" → next `docker run --name` Conflict → got=0). Size the per-attempt
	// deadline to ~1/3 of the budget (floored at forceRemoveBudget) so the rm
	// can actually finish while still leaving room for a couple of retries
	// against the async --rm reaper.
	perAttempt := budget / 3
	if perAttempt < forceRemoveBudget {
		perAttempt = forceRemoveBudget
	}
	for {
		a.forceRemoveContainerByNameWithin(name, perAttempt)
		if a.containerNameFree(name) {
			return
		}
		if time.Now().After(deadline) {
			a.logger.Warn("container name still in use after the pre-run remove budget; the next docker run may conflict",
				zap.String("container", name), zap.Duration("budget", budget))
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// containerNameFree reports whether no container (in any state, including one
// the `--rm` reaper is mid-way through removing) currently holds the given
// name. Docker stores container names with a leading slash, so the filter is
// anchored to an exact `^/<name>$` match to avoid matching name substrings. A
// query error is treated as "not free" so the caller keeps waiting rather than
// racing into a `docker run --name` conflict.
func (a *App) containerNameFree(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The anchored regex is the daemon's own name filter, identical to the
	// `--filter name=^/<name>$` the CLI passed: an unanchored match would report
	// "taken" for any container whose name merely CONTAINS this one.
	//
	// A query error is deliberately read as "still in use" rather than "free":
	// treating an unreachable daemon as a free name would let the caller race
	// straight into a create that then fails on the conflict.
	list, err := a.docker.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", "^/"+name+"$")),
	})
	if err != nil {
		a.logger.Debug("could not query container-name availability; treating as still-in-use",
			zap.String("container", name), zap.Error(err))
		return false
	}
	return len(list) == 0
}

// isExit125 reports whether a finished user-app command failed at runtime with
// docker's exit code 125 — the code docker returns when the daemon refuses to
// create/start the container (a name conflict, but also a bad image reference,
// an unsatisfiable mount, a malformed flag, …). It is a NECESSARY-but-not-
// sufficient signal for the name-conflict retry; isDockerRunNameConflict layers
// the name-occupied check on top to separate the conflict from those other 125s.
func isExit125(runErr error, errType utils.ErrType) bool {
	if runErr == nil || errType != utils.Runtime {
		return false
	}
	return strings.Contains(runErr.Error(), "exit status 125")
}

// isDockerRunNameConflict reports whether a failed user-app `docker run` failed
// SPECIFICALLY because its --name was still taken by a leftover container — the
// only exit-125 cause that force-removing the name + retrying can clear. It
// guards against blanket-retrying every exit 125: a genuinely broken run (bad
// image, missing mount, bad flag) also exits 125 but must fail fast, not be
// re-issued repeatedly.
//
// docker's name-conflict message ("Conflict. The container name ... is already
// in use by container ...") is written to the run's stderr, which keploy streams
// straight to the terminal rather than capturing, so the returned CmdError only
// carries the exit code ("exit status 125"). The conflict is instead confirmed
// positively from docker's own state: nameOccupied is true exactly when a
// container still holds the name at the moment the run failed — which is the
// precondition docker reports as the name conflict. Requiring BOTH the 125 exit
// and an occupied name keeps the retry strictly scoped to the recreate race.
func isDockerRunNameConflict(runErr error, errType utils.ErrType, nameOccupied bool) bool {
	return isExit125(runErr, errType) && nameOccupied
}

// extractProjectFlags returns any project-scoping flags (-p/--project-name,
// --project-directory) found in the given docker compose command.
func extractProjectFlags(cmd string) []string {
	parts := strings.Fields(cmd)
	var flags []string
	for i := 0; i < len(parts); i++ {
		switch {
		case (parts[i] == "-p" || parts[i] == "--project-name" || parts[i] == "--project-directory") && i+1 < len(parts):
			flags = append(flags, parts[i], parts[i+1])
			i++
		case strings.HasPrefix(parts[i], "-p=") || strings.HasPrefix(parts[i], "--project-name=") || strings.HasPrefix(parts[i], "--project-directory="):
			flags = append(flags, parts[i])
		}
	}
	return flags
}

// withAgentToken returns the app command as it must run, and what it needs in
// its environment on top of what it inherits from keploy.
//
// Under docker compose that command is what starts the agent: the generated
// keploy-agent service names the control-plane token without a value, and
// compose fills it in from the environment given here. A leading sudo or doas
// would reset that environment first, so a root keploy drops it and any other
// keploy tells sudo to keep the one variable (agentTokenCommand). Every other
// kind starts its agent some other way, and the application under test is
// handed nothing.
//
// One place for both, whichever way the compose command was put together —
// rewritten around a generated file, piped in memory, or a wrapper keploy
// could not rewrite at all.
//
// And the one place a compose agent is launched, so it is recorded here
// (token.RecordLaunch), once for every run: a docker compose `keploy test`
// runs the app again for each test-set, and with it a new agent at the same
// address, which the client's check that its agent enforces the token has to
// see as the new agent it is.
func (a *App) withAgentToken(cmd string) (string, []string) {
	var mock []string
	if a.opts.MockMode && token.Session() != "" {
		mock = []string{token.MockAgentTokenEnv}
	}
	if a.kind != utils.DockerCompose {
		if a.kind == utils.DockerRun {
			cmd, _ = agentTokenCommand(cmd, effectiveUID() == 0, mock...)
			return cmd, nil
		}
		return cmd, nil
	}
	token.RecordLaunch(a.opts.AgentURI)
	cmd, _ = agentTokenCommand(cmd, effectiveUID() == 0, append([]string{token.Env}, mock...)...)
	return cmd, docker.AgentTokenEnv()
}

func (a *App) run(ctx context.Context) models.AppError {
	// --from-container never reaches a shell: the app is started, streamed and
	// waited on through the Engine API, so none of the command-string handling
	// below (name-conflict retries, detach scanning, ExecuteCommand) applies.
	if a.kind == utils.FromContainer {
		// The replacement is removed here on the ordinary path so the user's
		// container gets its ports back promptly; AgentClient.RestoreFromContainer
		// covers every path that never reaches this function, and both are
		// idempotent.
		defer a.RemoveReplacement()
		return a.runFromContainer(ctx)
	}

	userCmd := a.cmd

	// ComposeDown can fire on two paths within a single run() — cmdCancel below
	// (SIGINT/teardown) and this deferred call (normal exit / the
	// --abort-on-container-exit path). Collapse them with a run-local Once so
	// a ComposeDown never runs twice and doubles the teardown; whichever path
	// fires first does the teardown, the other is a no-op. External callers
	// (the replay per-test-set loop) invoke a.ComposeDown directly and are
	// unaffected by this run-local guard.
	var downOnce sync.Once
	composeDown := func() { downOnce.Do(a.ComposeDown) }
	// composeHaltSeen is closed when the run is ended on a halt
	// (composeStopsTheApp), by composeEnd, the watch's end, which the run
	// itself takes too on an up that returned on the app's own exit; nil
	// otherwise.
	var composeHaltSeen <-chan struct{}
	var composeEnd func()
	if a.kind == utils.DockerCompose {
		defer composeDown()
		// Deferred after the teardown so that it runs BEFORE it: the agent's
		// container has to be read between compose stopping it and keploy
		// removing it. See readStoppedAgent.
		defer a.readStoppedAgent(ctx)
		// Under the switch keploy, not compose's abort, ends the run on an
		// exit of the app it did not ask for (compose_appstop.go). Deferred
		// last, so the watch ends before the stack is taken down.
		if composeStopsTheApp() {
			a.composeApp.halted.Store(nil)
			a.composeApp.stopping.Store(false)
			a.composeApp.project.Store(nil)
			halt := make(chan struct{})
			composeHaltSeen = halt
			composeEnd = a.composeHaltEnd(ctx, halt, composeDown)
			stopWatch := a.watchComposeApp(ctx, composeEnd)
			defer stopWatch()
		}
	}

	// dockerRunName is the resolved user-app container name; reused by the
	// post-run container-name-conflict retry below.
	var dockerRunName string
	if a.kind == utils.DockerRun {
		userCmd = utils.EnsureRmBeforeName(userCmd)
		// Clear any container left over from a prior run with the same --name
		// before starting. --rm removes the container on exit, but under heavy
		// docker-daemon contention that reap can lag past the next
		// `docker run --name`, so a re-run (e.g. a dedup lane re-running the app
		// per test-set) hits "Conflict. The container name ... is already in use"
		// (docker exit 125). Resolve the name from --container-name (a.container)
		// or, when that isn't set, the --name in the command itself, then
		// force-remove it with preRunRemoveBudget — generous because this is a
		// startup cleanup, and a still-running prior container can take more than
		// the 2s teardown cap to remove under contention. Best-effort: a no-op
		// when nothing is lingering.
		dockerRunName = a.container
		if dockerRunName == "" {
			dockerRunName = utils.ContainerNameFromDockerRun(userCmd)
		}
		a.ensureContainerNameFreeWithin(dockerRunName, preRunRemoveBudget)
	}

	// Compose mode: before `docker compose up`, make sure the keploy agent
	// container from a prior compose up/phase is fully gone. keploy injects the
	// agent as a compose service AND force-removes it by name on teardown, so a
	// fresh up that starts while the previous compose is still "Stopping
	// Gracefully" (the keep-app-alive recovery, and the atg sandbox's per-phase
	// re-record) reads the stale project state and tries to Recreate the
	// being-removed agent container — failing with "Error response from daemon:
	// No such container: <stale-id>" and aborting the whole run (the
	// atg-with-mocks flake).
	//
	// Two complementary guards, because the agent is tracked by compose under a
	// FIXED service key (keploy-agent) but a per-process-random container_name
	// (keploy-v3-<hash>):
	//
	//   1. removeStaleComposeAgentWithin resolves the prior agent via the compose
	//      project/service (the same way the upcoming up will) and force-removes +
	//      reaps it. This is the one that closes the record -> auto-replay race:
	//      the prior phase's agent has a DIFFERENT random name this App never
	//      knew, so name-based cleanup can't see it, yet compose still tries to
	//      Recreate it because the SERVICE key is constant and its container_name
	//      drifted between phases.
	//   2. ensureContainerNameFreeWithin frees THIS run's agent name, defending
	//      the same-name reuse across the per-test-set up/down boundary.
	//
	// Both are bounded and a no-op on the first up of a project. Mirror the
	// docker-run pre-run guard above.
	if a.kind == utils.DockerCompose {
		a.removeStaleComposeAgentWithin(preRunRemoveBudget)
		if a.keployContainer != "" {
			a.ensureContainerNameFreeWithin(a.keployContainer, preRunRemoveBudget)
		}
	}

	// Define the function to cancel the command
	cmdCancel := func(cmd *exec.Cmd) func() error {
		return func() error {
			// keploy's stop at a test set's end (setend_stop.go) is the
			// teardown's stop, which says whether it had to kill the app.
			setEnd := a.kind != utils.DockerCompose && stopsForSetEnd(ctx)
			// keploy record's stop of the app, under the switch, says
			// when it had to kill too: what the app would have done as it
			// stopped is then not in the recording (P101-b-k).
			// A compose app's stop is keploy's own under the switch
			// (compose_appstop.go), so it says so too.
			recordEnd := !setEnd && a.StopsARecording()
			if a.kind == utils.DockerCompose && composeStopsTheApp() {
				// It says a recording's kill itself.
				// An app that exited on its own needs no stop.
				if _, err := a.StopComposeApp(context.WithoutCancel(ctx)); err != nil && !errors.Is(err, models.ErrAppExitedOnItsOwn) {
					a.logger.Warn("keploy could not stop the application's container before taking its compose stack down", zap.Error(err))
				}
				composeDown()
				return utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGKILL)
			}
			if utils.IsDockerCmd(a.kind) {
				a.logger.Debug("sending SIGINT to the container", zap.Any("cmd.Process.Pid", cmd.Process.Pid))
				err := utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGINT)
				if err != nil {
					warning := fmt.Sprintf("error sending SIGINT: %s", err)
					a.logger.Debug(warning)
				}

				// Docker Compose teardown differs from a single `docker run`: the
				// stack has several containers (the app, its dependencies, and the
				// injected keploy-agent). The SIGINT above makes `docker compose up`
				// stop them gracefully in the foreground, but the app container
				// usually exits first — e.g. a Go server with no SIGTERM handler
				// exits immediately on Go's default signal disposition — while the
				// SLOWER services keep stopping, most notably the eBPF agent.
				// `docker compose up` does not return until every service has
				// stopped, and os/exec's WaitDelay then blocks the teardown for ~25s
				// waiting on it. When the app is kept alive on the outer replay
				// errgroup (compose + mocking), that teardown runs under
				// a drain's 30s bound (utils.DrainErrGroupProgress), so the overrun trips a spurious
				// "teardown drain timed out … a goroutine is ignoring context
				// cancellation" error and fails an otherwise-green run.
				//
				// So for compose: give the app a short grace to flush coverage during
				// the graceful stop, then bring the WHOLE project down fast via
				// ComposeDown (`docker compose down --timeout 1`) so `docker compose
				// up` returns promptly instead of blocking on the slow agent. The
				// run()-deferred ComposeDown then becomes an idempotent no-op.
				if a.kind == utils.DockerCompose {
					// A grace bounded by the wall clock, its reads by the
					// daemon's answers (composeAppGrace).
					a.composeAppGrace(func() bool { return cmd.Process.Signal(syscall.Signal(0)) != nil })
					composeDown()
					return utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGKILL)
				}

				if stopsByEvents(setEnd || recordEnd) {
					killed, err := a.stopDockerRunByEvents(cmd)
					a.noteStopKill(setEnd, recordEnd, killed)
					return err
				}
				killed, err := a.stopDockerRun(cmd, setEnd || recordEnd)
				if killed {
					a.noteStopKill(setEnd, recordEnd, fmt.Sprintf("its container had not stopped %s later (the grace keploy's stop gives a docker run app), so keploy killed it", graceBudget))
				}
				return err
			}
			var killed string
			var err error
			if provenswitch.On() {
				// What the interrupt reached and acts on is stopping:
				// the stop of what the app leaves behind waits for it
				// rather than ask it again (TakeStopReached).
				var reached []utils.Reached
				if setEnd || recordEnd {
					// The stop waits on the app's exit, and kills only
					// on the stall rule, or at the user's Ctrl+C
					// (setend_stop.go); the drains wait for it.
					killed, reached, err = func() (string, []utils.Reached, error) {
						defer utils.BeginAppStop()()
						return utils.InterruptProcessTreeStop(a.logger, cmd.Process.Pid, syscall.SIGINT, a.teardown.Load())
					}()
				} else {
					killed, reached, err = utils.InterruptProcessTreeReached(a.logger, cmd.Process.Pid, syscall.SIGINT)
				}
				a.stopReached.Store(&reached)
			} else {
				killed, err = utils.InterruptProcessTreeKilled(a.logger, cmd.Process.Pid, syscall.SIGINT)
			}
			a.noteStopKill(setEnd, recordEnd, killed)
			return err
		}
	}

	// executeApp starts the application once.
	//
	// For a compose document keploy generated ITSELF (composeContent set) the
	// stack is brought up in-process through the compose library, so no `docker`
	// binary is needed on PATH. Every other kind — an on-disk compose file, a
	// `docker run`, a native command — is the USER's command and keeps the
	// existing shell-out: it may carry flags and shell syntax keploy does not
	// model, and it is not keploy's to reinterpret.
	executeApp := func(runCmd string, runEnv []string) utils.CmdError {
		if a.useComposeLibrary() {
			return a.runComposeInProcess(ctx, composeDown)
		}
		return a.executeCommand(ctx, runCmd, runEnv, cmdCancel)
	}

	var err error
	// withAgentToken also records the launch (token.RecordLaunch), which the
	// client's "does my agent enforce the token" check depends on seeing for
	// EVERY run — so it must happen on the in-process path too, not only on the
	// shell-out. The rewritten command and the env it returns are consumed by
	// the shell-out branch inside executeApp; the in-process branch needs
	// neither (there is no child process and no sudo to survive), and reads the
	// token straight from the session when it loads the project.
	runCmd, runEnv := a.withAgentToken(userCmd)
	cmdErr := executeApp(runCmd, runEnv)
	// A user-app `docker run --name X` can still lose the container-name race to
	// the prior test-set's --rm reaper on a saturated CI daemon even after the
	// pre-run ensureContainerNameFreeWithin verified the name was free — the
	// reaper can re-touch the name in the narrow window before the run, and
	// docker then exits 125 without starting the app ("Conflict. The container
	// name ... is already in use"). Force-remove the name and retry rather than
	// failing the whole test-set on a transient race.
	//
	// The retry is gated STRICTLY on the name-conflict signature: an exit-125
	// run whose --name is still held by a leftover container (isDockerRunNameConflict).
	// Every other exit-125 cause (bad image, missing mount, malformed flag) leaves
	// the name free, so it is NOT retried and fails fast with its real error —
	// re-issuing it would only burn the remove budget and bury the root cause.
	for attempt := 1; dockerRunName != "" && attempt <= dockerRunNameConflictRetries &&
		ctx.Err() == nil && isExit125(cmdErr.Err, cmdErr.Type) &&
		isDockerRunNameConflict(cmdErr.Err, cmdErr.Type, !a.containerNameFree(dockerRunName)); attempt++ {
		a.logger.Warn("docker run exited 125 with the --name still in use (container-name conflict); force-removing the name and retrying",
			zap.String("container", dockerRunName), zap.Int("attempt", attempt))
		a.ensureContainerNameFreeWithin(dockerRunName, preRunRemoveBudget)
		cmdErr = executeApp(runCmd, runEnv)
	}

	// Compose mode: a `docker compose up` can fail not because the user's app is
	// broken but because a container it depends_on (a DB/emulator/broker) crashed
	// during compose's health-wait under CI contention. Compose then aborts with
	// "dependency failed to start: container <dep> exited (N)" BEFORE the app is
	// ever started, and `up` COMMONLY exits non-zero — which keploy would otherwise
	// turn into "user application terminated unexpectedly hence stopping keploy",
	// aborting the recording over a transient infra hiccup. Work-slow-not-fail:
	// retry the bring-up in place, a bounded number of times with a linear
	// backoff (see the loop for why in place, and how keploy's agent is kept).
	//
	// The non-zero exit on a dependency abort is the COMMON case, not guaranteed:
	// empirically ~1 in 20 `up --abort-on-container-exit --exit-code-from app` runs
	// report exit 0 for an app that never started (compose returns the app's
	// exit-code-from value of 0 instead of the dependency-failed error). That race
	// slips past this retry (cmdErr.Err == nil -> the gate's runErr check is false,
	// so no retry). keploy record does not pass it as a clean run: it reads the
	// stopped app's container and fails a project whose app never started though
	// compose reported 0 (record.composeEnd). keploy mock does not read it so yet,
	// and can pass such a run. So the retry catches most-but-not-all of this
	// flake.
	//
	// Gated STRICTLY by isTransientComposeDependencyFailure, which reads docker's
	// own post-mortem state (the abort message is on the up's stderr, which keploy
	// streams rather than captures, so the error string alone can't be trusted):
	// the app service must be in "created" (compose never started it) AND some
	// non-app, non-agent service must have "exited" non-zero. A genuine app
	// failure leaves the app service "exited", never "created", so it can NEVER
	// satisfy the gate — it fails fast with its real error. A dependency that
	// crashes on EVERY attempt is bounded by composeDepFailureRetries and then
	// surfaced; the retry can only DELAY a persistent failure, never hide it.
	var states []composeServiceState
	for attempt := 1; shouldRetryComposeUp(a.kind, attempt, composeDepFailureRetries, ctx.Err(),
		cmdErr.Err, cmdErr.Type, a.composeService,
		func() []composeServiceState {
			states = a.composeServiceStatesWithin(composePsStateBudget)
			return states
		}); attempt++ {
		a.logger.Info("a dependency container failed to start transiently during docker compose up (it crashed before the app could start); retrying the bring-up",
			zap.String("appService", a.composeService),
			zap.Int("attempt", attempt),
			zap.Int("maxAttempts", composeDepFailureRetries))

		// Retry in place, with no teardown: a second `up` starts the crashed
		// dependency afresh (removeCrashedDependencies), and the app once it
		// is healthy. The run has already
		// set keploy's agent up (its mocks, the readiness its healthcheck gates
		// the app on), and that lives in the agent's process. When the abort
		// left the agent running, `up` reuses it; when compose stopped it (every
		// service attached, so the crash was an attached container's exit), or
		// recreates it (a recreate flag), the agent `up` starts has none of it,
		// and retryComposeUp has the agent client set it up again. Bringing
		// the stack down here recreated the agent and nothing set it up: its
		// healthcheck never passed, the app behind it never started, and the run
		// waited out every test's timeout instead of recovering.
		backoff := time.Duration(attempt) * composeDepFailureBaseBackoff
		select {
		case <-ctx.Done():
			a.logger.Debug("context cancelled during compose dependency-failure backoff", zap.Error(ctx.Err()))
			return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: ctx.Err()}
		case <-time.After(backoff):
		}

		a.removeCrashedDependencies(ctx, states)
		if ctx.Err() != nil {
			return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: ctx.Err()}
		}
		cmdErr = a.retryComposeUp(ctx, func() utils.CmdError { return executeApp(runCmd, runEnv) })
	}

	// Under the switch the project is keploy's to take down: an up that
	// returned because keploy stopped the app itself (a set's end, the
	// recording's) has not ended the run (composeAwaitTeardown).
	if a.kind == utils.DockerCompose && composeStopsTheApp() {
		a.composeAwaitTeardown(ctx, composeHaltSeen, composeEnd)
	}

	if cmdErr.Err != nil {
		switch cmdErr.Type {
		case utils.Init:
			return models.AppError{AppErrorType: models.ErrCommandError, Err: cmdErr.Err, AppLogs: a.recentAppLogs(ctx), ExitCode: exitCodeFromErr(cmdErr.Err)}
		case utils.Runtime:
			err = cmdErr.Err
		}
	}

	// Skip on the compose path: cmdCancel's ComposeDown already brought the
	// whole stack down and waitContainersRemoved confirmed the app/agent
	// containers are gone, so waiting again here would only add an inspect loop
	// to the teardown and risk overrunning the drain budget.
	if utils.IsDockerCmd(a.kind) && a.kind != utils.DockerCompose {
		a.waitTillExit()
	}

	select {
	case <-ctx.Done():
		a.logger.Debug("context cancelled, error while waiting for the app to exit", zap.Error(ctx.Err()))
		return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: ctx.Err(), StopKilled: a.takeSetEndKill()}
	default:
		appLogs := a.recentAppLogs(ctx)

		if a.Mode == models.MODE_RECORD && a.EnableTesting {
			a.logger.Info("waiting for some time before returning the error to allow recording of test cases when testing keploy with itself")
			time.Sleep(3 * time.Second)
			a.logger.Debug("test binary stopped", zap.Error(err))
			return models.AppError{AppErrorType: models.ErrTestBinStopped, Err: context.Canceled, AppLogs: appLogs}
		}

		// The app's container exited when keploy had not asked it to: the
		// run ends with the app's exit, as compose's abort ended it.
		if halt := a.composeAppHalt(); halt != nil {
			return halt.appError(appLogs)
		}
		if err != nil {
			return models.AppError{AppErrorType: models.ErrUnExpected, Err: err, AppLogs: appLogs, ExitCode: exitCodeFromErr(err)}
		}
		return models.AppError{AppErrorType: models.ErrAppStopped, Err: nil, AppLogs: appLogs, ExitCode: 0}
	}
}

// exitCodeFromErr extracts the process exit code from a command-wait error.
// It returns the child's exit status for a normal non-zero exit (the case a
// wrapped `pytest`/`go test` failure produces), 128+signal for a signalled
// exit where the runtime exposes it, and -1 when no code can be determined
// (nil error, or an error that is not an *exec.ExitError). The mock
// record/replay flows use this to mirror the wrapped runner's exit status.
func exitCodeFromErr(err error) int {
	if err == nil {
		return -1
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// ExitCode() is -1 when the process was terminated by a signal; in
		// that case fall back to the conventional 128+signal shell encoding
		// so callers still get a non-zero, non-(-1) status to propagate.
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return 1
	}
	// The in-process compose path never runs a child process, so it cannot
	// produce an *exec.ExitError. Compose reports the --exit-code-from status
	// as cli.StatusError instead; without this branch every compose exit code
	// would silently collapse to -1 and the wrapped runner's status would stop
	// propagating.
	var statusErr cli.StatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode
	}
	return -1
}

// composeCommand is a compose subcommand (args: what follows `docker compose`)
// run through the active engine's compose command, for the teardown and probes
// that run where the compose library is not linked.
func composeCommand(ctx context.Context, args ...string) *exec.Cmd {
	compose := engine.Active().Compose
	return exec.CommandContext(ctx, compose[0], append(append([]string{}, compose[1:]...), args...)...)
}

// stopDockerRun is the teardown's stop of a `docker run` app the SIGINT was
// sent to: it gives it graceBudget to exit, then kills its container and the
// docker client. killed says it came to that.
//
// setEnd (the stop at a test set's end, under the proven switch) also takes a
// container docker is removing, or has removed, for one that exited, once
// this stop has seen it: the docker client of a --rm container waits for the
// removal before it exits, so on a loaded daemon the grace can run out with
// the app long gone. It looks at the container once more as the grace runs
// out. A container it never saw (keploy did not name the app's container, or
// named another) says nothing, and its kill is taken for one that reached the
// app; so is a kill that failed other than for a container it saw gone or
// stopped.
func (a *App) stopDockerRun(cmd *exec.Cmd, setEnd bool) (killed bool, err error) {
	// seen: an inspect in this stop found the container. Each read asks the
	// daemon again when it fails, until it answers or ctx (the grace, in it)
	// ends (inspectContainer): with a 2 s budget per read, a loaded daemon's
	// slow answers never came within the grace, and a container that had
	// exited at once was killed, the kill said.
	seen := false
	gone := func(ctx context.Context) bool {
		info, read, notFound := a.inspectContainer(ctx, a.container, fmt.Sprintf("the application's container %q (with no answer, keploy takes it for one still running)", a.container))
		if !read {
			return setEnd && seen && notFound
		}
		seen = true
		switch info.State.Status {
		case "exited", "dead":
			return true
		case "removing":
			return setEnd
		}
		return false
	}
	grace, graceCancel := context.WithTimeout(context.Background(), graceBudget)
	defer graceCancel()
graceLoop:
	for {
		select {
		case <-grace.Done():
			break graceLoop
		case <-time.After(1 * time.Second):
		}

		// Check if the 'docker run' command has exited
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			a.logger.Debug("docker client process exited")
			return false, nil
		}

		// Check the actual container status via Docker API
		// This handles cases where 'docker run' is dead but container is alive.
		if gone(grace) {
			a.logger.Debug("container stopped gracefully")
			return false, nil
		}
	}
	if setEnd && gone(context.Background()) {
		a.logger.Debug("container stopped gracefully")
		return false, nil
	}

	// Force Kill using Docker API
	// We tell the Docker Daemon explicitly to kill this container.
	a.logger.Debug("container did not stop gracefully, killing it forcefully", zap.String("containerID", a.container))

	// "SIGKILL" string is standard for Docker API to force kill.
	// Bounded so a saturated daemon can't wedge the teardown past the
	// record/replay drain budget.
	killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = a.docker.ContainerKill(killCtx, a.container, "SIGKILL")
	killCancel()
	if err != nil {
		warning := fmt.Sprintf("error killing container: %s", err)
		a.logger.Debug(warning)
	}
	// The kill did not reach the app only when the container this stop saw
	// was gone, or no longer running, by then.
	unreached := err != nil && seen && (errdefs.IsNotFound(err) || errdefs.IsConflict(err))
	// Clean up the CLI process as well
	return !unreached, utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGKILL)
}
