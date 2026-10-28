package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/engine"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type DockerConfigStruct struct {
	DockerImage  string
	Envs         map[string]string
	VolumeMounts []string
}

var DockerConfig = DockerConfigStruct{
	DockerImage: "ghcr.io/keploy/keploy",
}

// shellQuote renders s as a single POSIX shell word.
//
// getAlias builds ONE command string that is later executed through a shell
// (pkg/platform/docker/util_others.go → utils.CommandContext → `sh -c`, or its
// shlex fallback when the image has no /bin/sh — both implement the same
// single-quote rules). Any value interpolated into it that the operator can
// choose has to be quoted, or a space splits it into two arguments and a
// metacharacter runs as a command inside a sudo'd `docker run`.
//
// POSIX single quotes suppress every expansion; the only character that cannot
// appear inside them is the single quote itself, so each one is spliced in as
// close-quote, backslash-quote, reopen-quote.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildUpstreamTLSFlags renders the --upstream-tls-* argv for the containerised
// agent. The host's keploy.yml is not visible inside the agent container, so
// argv is the only propagation channel; the CA path is resolved on the AGENT's
// filesystem, which means the operator must bind-mount the PEM or point at a
// path that already exists in the image.
//
// Both flags are emitted UNCONDITIONALLY in `--flag=value` form so the
// precedence flag > yaml > default resolves identically on docker, on
// docker-compose and on a native run — see proxy.resolveUpstreamTLSConfig for
// why "append only when true" silently broke the off switch.
func buildUpstreamTLSFlags(opts models.SetupOptions) (string, error) {
	flags := fmt.Sprintf(" --upstream-tls-verify=%t", opts.UpstreamTLSVerify)

	if runtime.GOOS == "windows" {
		// The windows runner (util_windows.go) hands the alias to cmd.exe as
		// strings.Split(alias, " ") — there is no quoting layer for any
		// escaping to survive, which is also why GenerateDockerEnvs above
		// skips quoting on windows. Refuse with an actionable message rather
		// than emit a docker run that silently loses half the path.
		if strings.ContainsAny(opts.UpstreamTLSCACert, " \t\"^&|<>") {
			return "", fmt.Errorf("upstream TLS CA certificate path %q cannot be forwarded to the keploy agent container on windows: the docker alias is split on spaces, so the path must contain no spaces or cmd.exe metacharacters — move the PEM somewhere without them, or bake it into the agent image", opts.UpstreamTLSCACert)
		}
		return flags + " --upstream-tls-ca-cert=" + opts.UpstreamTLSCACert, nil
	}

	return flags + " --upstream-tls-ca-cert=" + shellQuote(opts.UpstreamTLSCACert), nil
}

// windowsCmdOperators are the characters cmd.exe acts on in the windows
// docker alias, which the windows runner (util_windows.go) splits on spaces
// and runs through cmd.exe with no quoting layer any escaping would survive:
// & and | end the docker command and start another, < and > redirect it, ^
// escapes the next character, % expands a variable, and a line break ends the
// command. A value holding one would run as cmd.exe syntax.
const windowsCmdOperators = "&|<>^%\r\n"

// aliasWord renders s, a value the agent is handed as one argument (what
// names it in an error), as one word of the docker alias, so it reaches the
// agent as it was given whatever it holds: a space, a quote, JSON, a $ or a
// backtick. Elsewhere than windows it is quoted for the shell the alias runs
// through (shellQuote), so none of it runs as shell code. On windows it goes
// in as it is, and a value holding one of windowsCmdOperators is refused: it
// would run as cmd.exe syntax. (The runner's split on spaces still splits a
// value with a space there, as it always has: only a runner that hands docker
// its arguments as a list, with no cmd.exe, ends that.)
func aliasWord(what, s string) (string, error) {
	if runtime.GOOS != "windows" {
		return shellQuote(s), nil
	}
	if strings.ContainsAny(s, windowsCmdOperators) {
		return "", fmt.Errorf("%s %q cannot be forwarded to the keploy agent container on windows: the docker alias is run by cmd.exe, which would act on its %q", what, s, windowsCmdOperators)
	}
	return s, nil
}

func GenerateDockerEnvs(config DockerConfigStruct) string {
	var envs []string
	for key, value := range config.Envs {
		if runtime.GOOS == "windows" {
			envs = append(envs, fmt.Sprintf("-e %s=%s", key, value))
		} else {
			envs = append(envs, fmt.Sprintf("-e %s='%s'", key, value))
		}
	}
	return strings.Join(envs, " ")
}

// AgentTokenEnv is what a docker or docker compose client that starts a keploy
// agent must have in its environment, on top of what it inherits: the session
// token under token.Env. The `docker run` alias (`-e KEPLOY_AGENT_TOKEN`) and
// the generated compose service (`environment: [KEPLOY_AGENT_TOKEN]`) name the
// variable without a value, and the client fills it in from here. Nil when
// there is no token.
func AgentTokenEnv() []string {
	tok := token.Session()
	if tok == "" {
		return nil
	}
	return []string{token.Env + "=" + tok}
}

// linuxDockerClient is how the linux `docker run` alias invokes the docker
// client that starts the agent container: as root, with the control-plane
// token in its environment when passToken is set (see getAlias).
//
// A process that is already root runs docker itself, and that is every process
// that normally gets here: utils.ReexecWithSudo re-executes keploy under sudo
// for a docker command, and the agent client's docker-mode privilege check
// refuses a process without the capabilities this alias grants. A sudo on top
// would add only its env_reset, which drops the token before docker can copy
// it, and a dependency on whatever `sudo` is on PATH: a pass-through stand-in
// execs --preserve-env as if it were the command, and a sudoers rule without
// SETENV refuses the whole command. Run directly, docker also sees the same
// environment, and so the same daemon, as the app's own `docker run`
// (utils.ExecuteCommand), the `docker stop` that tears this container down and
// the Engine API client (FromEnv), none of which go through sudo.
//
// Anything else goes through sudo, told to keep that one variable and no other
// (sudoKeepingToken). PrepareDockerCommand refuses a sudo that would not.
func linuxDockerClient(root, passToken bool) string {
	cli := engine.Active().CLI
	switch {
	case root:
		return cli
	case passToken:
		return sudoKeepingToken + " " + cli
	default:
		return "sudo " + cli
	}
}

// agentSecurityOptFlags are the engine's --security-opt flags for keploy's
// agent container, each with a leading space.
func agentSecurityOptFlags() string {
	var b strings.Builder
	for _, opt := range engine.Active().AgentSecurityOpts {
		b.WriteString(" --security-opt " + opt)
	}
	return b.String()
}

func GetKeployDockerAlias(ctx context.Context, logger *zap.Logger, conf *config.Config, opts models.SetupOptions) (keployAlias string, err error) {
	// Preserves your environment variable setup
	if DockerConfig.Envs == nil {
		DockerConfig.Envs = map[string]string{
			"INSTALLATION_ID": conf.InstallationID,
		}
	} else {
		DockerConfig.Envs["INSTALLATION_ID"] = conf.InstallationID
	}

	// Preserves your Docker client initialization and setup
	client, err := New(logger, conf)
	if err != nil {
		return "", fmt.Errorf("failed to initialise docker: %w", err)
	}

	err = client.CreateVolume(ctx, "debugfs", true, map[string]string{
		"type":   "debugfs",
		"device": "debugfs",
	})
	if err != nil {
		// Log the error but don't fail, consistent with original logic.
		utils.LogError(logger, err, "failed to create debugfs volume")
	}

	// Preserves the alias generation
	keployalias, err := getAlias(ctx, logger, opts, conf)
	if err != nil {
		return "", err
	}

	return keployalias, nil
}

// getAlias constructs the docker run command.
// Note: The container runs with --cap-add=NET_ADMIN to configure its own private network namespace.
// Due to Docker's network isolation (namespaces), this capability is strictly confined to the container
// and CANNOT access or modify the Host System's network interfaces or configuration.
// agentPortPublish is the ONE place the agent's control-plane port is published
// to the host. Every getAlias platform arm must call it rather than building
// its own "-p" fragment.
//
// The scoping is load-bearing, not cosmetic. The control-plane server carries
// serious capability — /agent/pcap/keylog streams live TLS session keys,
// /agent/stop kills the session and /agent/storemocks injects mock data — and
// while a bearer token now guards each route (see routes.Authenticate), the
// port should still not be offered to the whole network. In
// docker mode the in-container listener deliberately stays on every interface
// (see agentBindAddr) so docker can forward the published port in, which makes
// this host-IP scoping the only thing keeping that control plane off every
// host interface.
//
// It is also untestable by behaviour: "-p <port>:<port>" binds 0.0.0.0, a
// strict superset of 127.0.0.1, so the agent stays reachable over loopback
// either way and every lane passes identically whether the scoping is here or
// not. Five hand-written copies of this fragment were therefore five silent
// single points of failure - hence one function, asserted directly.
func agentPortPublish(agentPort uint32) string {
	return fmt.Sprintf(" -p 127.0.0.1:%d:%d", agentPort, agentPort)
}

// getAlias's conf is the CLI's config, whose root flags the agent container
// shares: --debug, and --disable-tele (teleSuffix).
func getAlias(ctx context.Context, logger *zap.Logger, opts models.SetupOptions, conf *config.Config) (string, error) {
	// Get the name of the operating system.
	osName := runtime.GOOS
	debug := conf.Debug
	//TODO: configure the hardcoded port mapping
	img := DockerConfig.DockerImage + ":v" + utils.Version
	logger.Info("Starting keploy in docker with image", zap.String("image:", img))
	// envParts is assembled and joined rather than concatenated: the alias is
	// split on single spaces on windows (util_windows.go), so an empty piece
	// would become an empty argv entry.
	var envParts []string
	if base := GenerateDockerEnvs(DockerConfig); base != "" {
		envParts = append(envParts, base)
	}
	// The control-plane token, by name only. `-e KEPLOY_AGENT_TOKEN` with no
	// value makes the docker client copy the variable from its own
	// environment, and AgentTokenEnv is what PrepareDockerCommand puts there,
	// on the one process that runs this alias. So the value is in neither
	// argv — where /proc/<pid>/cmdline and `ps` would show it to every user
	// on the box, the ones the token exists to keep out of the control plane
	// — nor in any file the docker client has to open. The latter is where
	// --env-file fell down: a strictly confined docker (the snap) runs in a
	// mount namespace with a private /tmp, where a host temp file does not
	// exist, and it refused to start the agent at all. The environment
	// reaches it on every platform, windows' cmd.exe and macOS included, and
	// a bare name has no quoting or path to survive the windows arm's split
	// on spaces.
	//
	// Once docker has it the token is in the container's Config.Env and
	// shows up in `docker inspect`. That is not a boundary this could hold
	// anyway: membership of the docker group is equivalent to root.
	//
	// No token is not fatal: the agent starts unauthenticated and says so,
	// which is how it behaved before this existed, rather than leaving the
	// user unable to record at all. The launch is recorded either way, so the
	// client still holds this agent to the token (see token.RecordLaunch).
	token.RecordLaunch(opts.AgentURI)
	passToken := token.Session() != ""
	if passToken {
		envParts = append(envParts, "-e "+token.Env)
	} else {
		logger.Warn("no control-plane token to hand to the agent container; it will start without authentication")
	}
	envs := strings.Join(envParts, " ")
	if envs != "" {
		envs = envs + " "
	}

	appPortsStr := ""
	if len(opts.AppPorts) > 0 {
		appPortsStr = " " + strings.Join(opts.AppPorts, " ")
	}
	appNetworkStr := ""
	if len(opts.AppNetworks) > 0 {
		for _, network := range opts.AppNetworks {
			appNetworkStr += " --network " + network
		}
	}

	// The app's own host entries, and host.docker.internal at host-gateway:
	// the app joins this container's network namespace and reads its
	// /etc/hosts (extra_hosts.go).
	appHostsStr, err := agentHostFlags(opts.AppExtraHosts, opts.HostGateway)
	if err != nil {
		utils.LogError(logger, err, "failed to build the keploy agent docker alias")
		return "", err
	}

	tlsVolumeMount := fmt.Sprintf("-v %s:%s ", KeployTLSVolumeName, KeployTLSMountPath)
	agentMemoryLimitFlag := ""
	if opts.MemoryLimit > 0 {
		agentMemoryLimitFlag = " --memory-limit " + strconv.FormatUint(opts.MemoryLimit, 10)
	}

	// Forward record-buffer tuning to the containerised agent.
	// Host's keploy.yml is not bind-mounted into the agent container, so
	// argv is the only propagation channel for these. Empty when the
	// orchestrator hasn't overridden defaults — the agent then falls
	// back to its built-in defaults via relay.withDefaults().
	recordBufferFlags := ""
	if opts.RecordBufferMaxMemoryPerConn > 0 {
		recordBufferFlags += " --max-memory-per-conn " + strconv.FormatUint(opts.RecordBufferMaxMemoryPerConn, 10)
	}
	if opts.RecordBufferQueueSize > 0 {
		recordBufferFlags += " --queue-size " + strconv.Itoa(opts.RecordBufferQueueSize)
	}
	if opts.RecordBufferConsumerStallGrace > 0 {
		recordBufferFlags += " --consumer-stall-grace " + opts.RecordBufferConsumerStallGrace.String()
	}
	// != 0, not > 0: a NEGATIVE half-close grace is the documented way to
	// disable half-close, so it has to reach the agent.
	if opts.RecordBufferHalfCloseGrace != 0 {
		recordBufferFlags += " --half-close-grace " + opts.RecordBufferHalfCloseGrace.String()
	}

	// Forward upstream TLS verification to the containerised agent — same
	// argv-only propagation channel as recordBufferFlags above.
	upstreamTLSFlags, err := buildUpstreamTLSFlags(opts)
	if err != nil {
		utils.LogError(logger, err, "failed to build the keploy agent docker alias")
		return "", err
	}

	Volumes := ""
	for i, volume := range DockerConfig.VolumeMounts {
		if i != 0 {
			Volumes = Volumes + "-v " + volume
		} else {
			Volumes = "-v " + volume
		}
		if i == len(DockerConfig.VolumeMounts)-1 {
			Volumes = Volumes + " "
		}
	}

	Volumes = Volumes + tlsVolumeMount

	// The agent's config path and its own arguments a caller hands it
	// (ExtraArgs: the enterprise startup hook's, JSON among them), each one
	// word of the alias as it was given (aliasWord). Every arm appends both
	// (TestGetAlias_EveryPlatformBranchHandsTheAgentItsArgumentsAsWords).
	configPathFlag := ""
	if opts.ConfigPath != "" && opts.ConfigPath != "." {
		if w, err := aliasWord("the config path", opts.ConfigPath); err == nil {
			configPathFlag = " --config-path " + w
		} else {
			// The agent container reads no file at a host path (no arm
			// mounts the project), so leaving it out changes nothing the
			// agent does, where refusing would stop the run.
			logger.Debug("not handing the keploy agent container the config path, which cmd.exe would act on", zap.String("configPath", opts.ConfigPath), zap.Error(err))
		}
	}
	extraArgWords := ""
	for _, a := range opts.ExtraArgs {
		w, err := aliasWord("the keploy agent argument", a)
		if err != nil {
			utils.LogError(logger, err, "failed to build the keploy agent docker alias")
			return "", err
		}
		extraArgWords += " " + w
	}
	// Skip publishing the proxy port when it's zero. Docker rejects
	// `-p 0:0` outright, so a caller that intentionally sets
	// ProxyPort=0 (e.g. to run the agent without a listening proxy
	// socket) would fail the whole docker-run alias build otherwise.
	//
	// Published to the host's loopback only, like the agent port: the
	// application reaches the proxy through the agent's network namespace
	// (`--network=container:<keploy container>`, see pkg/client/app), not
	// through this publish, so there is nothing for the other host interfaces
	// to serve but the interception point itself.
	proxyPortStr := ""
	if opts.ProxyPort != 0 {
		proxyPortStr = " -p 127.0.0.1:" + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort)
	}
	switch osName {
	case "linux":

		alias := linuxDockerClient(os.Geteuid() == 0, passToken) + " container run --name " + opts.KeployContainer + appNetworkStr + appHostsStr + " " + envs + "-e BINARY_TO_DOCKER=true" +
			agentPortPublish(opts.AgentPort) +
			proxyPortStr + appPortsStr +
			" --cap-add=BPF --cap-add=PERFMON --cap-add=NET_ADMIN --cap-add=SYS_RESOURCE --cap-add=SYS_PTRACE" + agentSecurityOptFlags() + " " + Volumes +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf " +
			" --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) + clientPIDNSSuffix() + " --mode " + string(opts.Mode) + " --dns-port " + fmt.Sprintf("%d", opts.DnsPort) + " --is-docker" + mockModeSuffix(opts) + settleReadsSuffix(opts) + provenSuffix() + teleSuffix(conf)

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)
		if opts.GlobalPassthrough {
			alias += " --global-passthrough"
		}
		if opts.DisableHandshakeHold {
			alias += " --disable-handshake-hold"
		}
		if opts.CapturePackets {
			alias += " --capture-packets"
		}
		if opts.OpportunisticTLSIntercept {
			alias += " --opportunistic-tls-intercept"
		}
		if opts.ChannelBindingShim {
			alias += " --channel-binding-shim"
		}
		if opts.BuildDelay > 0 {
			alias += fmt.Sprintf(" --build-delay %d", opts.BuildDelay)
		}
		alias += agentMemoryLimitFlag
		alias += recordBufferFlags
		alias += upstreamTLSFlags
		if models.IsAnsiDisabled {
			alias += " --disable-ansi"
		}
		if len(opts.PassThroughPorts) > 0 {
			portStrings := make([]string, len(opts.PassThroughPorts))
			for i, port := range opts.PassThroughPorts {
				portStrings[i] = strconv.Itoa(int(port))
			}
			// Note the "=" sign, which is good practice for docker run
			alias += fmt.Sprintf(" --pass-through-ports=%s", strings.Join(portStrings, ","))
		}
		if debug {
			alias += " --debug"
		}
		alias += configPathFlag
		if opts.Synchronous {
			alias += " --sync"
		}
		if opts.EnableSampling > 0 {
			alias += fmt.Sprintf(" --enable-sampling=%d", opts.EnableSampling)
		}
		alias += extraArgWords
		return alias, nil
	case "windows":

		cmd := exec.CommandContext(ctx, "docker", "context", "ls", "--format", "{{.Name}}\t{{.Current}}")
		out, err := cmd.Output()
		if err != nil {
			utils.LogError(logger, err, "failed to get the current docker context")
			return "", errors.New("failed to get alias")
		}
		dockerContext := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
		if len(dockerContext) == 0 {
			utils.LogError(logger, nil, "failed to get the current docker context")
			return "", errors.New("failed to get alias")
		}
		dockerContext = strings.Split(dockerContext, "\n")[0]
		if dockerContext == "colima" {
			logger.Info("Starting keploy in docker with colima context, as that is the current context.")
			// alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + "-v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
			// return alias, nil

			alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + appHostsStr + " " + envs + "-e BINARY_TO_DOCKER=true" +
				agentPortPublish(opts.AgentPort) +
				proxyPortStr + appPortsStr +
				" --cap-add=BPF --cap-add=PERFMON --cap-add=NET_ADMIN --cap-add=SYS_RESOURCE --cap-add=SYS_PTRACE " + Volumes +
				" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf " +
				" --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) + clientPIDNSSuffix() +
				" --mode " + string(opts.Mode) + " --dns-port " + fmt.Sprintf("%d", opts.DnsPort) + " --is-docker" + mockModeSuffix(opts) + settleReadsSuffix(opts) + provenSuffix() + teleSuffix(conf)

			if opts.EnableTesting {
				alias += " --enable-testing"
			}
			alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
			alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

			if opts.GlobalPassthrough {
				alias += " --global-passthrough"
			}
			if opts.DisableHandshakeHold {
				alias += " --disable-handshake-hold"
			}
			if opts.CapturePackets {
				alias += " --capture-packets"
			}
			if opts.OpportunisticTLSIntercept {
				alias += " --opportunistic-tls-intercept"
			}
			if opts.ChannelBindingShim {
				alias += " --channel-binding-shim"
			}
			if opts.BuildDelay > 0 {
				alias += fmt.Sprintf(" --build-delay %d", opts.BuildDelay)
			}
			alias += agentMemoryLimitFlag
			alias += recordBufferFlags
			alias += upstreamTLSFlags
			if models.IsAnsiDisabled {
				alias += " --disable-ansi"
			}
			if len(opts.PassThroughPorts) > 0 {
				portStrings := make([]string, len(opts.PassThroughPorts))
				for i, port := range opts.PassThroughPorts {
					portStrings[i] = strconv.Itoa(int(port))
				}
				// Note the "=" sign, which is good practice for docker run
				alias += fmt.Sprintf(" --pass-through-ports=%s", strings.Join(portStrings, ","))
			}
			if debug {
				alias += " --debug"
			}
			alias += configPathFlag
			if opts.Synchronous {
				alias += " --sync"
			}
			if opts.EnableSampling > 0 {
				alias += fmt.Sprintf(" --enable-sampling=%d", opts.EnableSampling)
			}
			alias += extraArgWords
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		// alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + "-v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
		alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + appHostsStr + " " + envs + "-e BINARY_TO_DOCKER=true" +
			agentPortPublish(opts.AgentPort) +
			proxyPortStr + appPortsStr +
			" --cap-add=BPF --cap-add=PERFMON --cap-add=NET_ADMIN --cap-add=SYS_RESOURCE --cap-add=SYS_PTRACE " + Volumes +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf " +
			" --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) + clientPIDNSSuffix() +
			" --mode " + string(opts.Mode) + " --dns-port " + fmt.Sprintf("%d", opts.DnsPort) + " --is-docker" + mockModeSuffix(opts) + settleReadsSuffix(opts) + provenSuffix() + teleSuffix(conf)

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

		if opts.GlobalPassthrough {
			alias += " --global-passthrough"
		}
		if opts.DisableHandshakeHold {
			alias += " --disable-handshake-hold"
		}
		if opts.CapturePackets {
			alias += " --capture-packets"
		}
		if opts.OpportunisticTLSIntercept {
			alias += " --opportunistic-tls-intercept"
		}
		if opts.ChannelBindingShim {
			alias += " --channel-binding-shim"
		}
		if opts.BuildDelay > 0 {
			alias += fmt.Sprintf(" --build-delay %d", opts.BuildDelay)
		}
		alias += agentMemoryLimitFlag
		alias += recordBufferFlags
		alias += upstreamTLSFlags
		if models.IsAnsiDisabled {
			alias += " --disable-ansi"
		}
		if len(opts.PassThroughPorts) > 0 {
			portStrings := make([]string, len(opts.PassThroughPorts))
			for i, port := range opts.PassThroughPorts {
				portStrings[i] = strconv.Itoa(int(port))
			}
			// Note the "=" sign, which is good practice for docker run
			alias += fmt.Sprintf(" --pass-through-ports=%s", strings.Join(portStrings, ","))
		}
		if debug {
			alias += " --debug"
		}
		alias += configPathFlag
		if opts.Synchronous {
			alias += " --sync"
		}
		if opts.EnableSampling > 0 {
			alias += fmt.Sprintf(" --enable-sampling=%d", opts.EnableSampling)
		}
		alias += extraArgWords
		return alias, nil

	case "darwin":
		cmd := exec.CommandContext(ctx, "docker", "context", "ls", "--format", "{{.Name}}\t{{.Current}}")
		out, err := cmd.Output()
		if err != nil {
			utils.LogError(logger, err, "failed to get the current docker context")
			return "", errors.New("failed to get alias")
		}
		dockerContext := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
		if len(dockerContext) == 0 {
			utils.LogError(logger, nil, "failed to get the current docker context")
			return "", errors.New("failed to get alias")
		}
		dockerContext = strings.Split(dockerContext, "\n")[0]
		if dockerContext == "colima" {
			logger.Info("Starting keploy in docker with colima context, as that is the current context.")
			// alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
			// return alias, nil
			logger.Info("Starting keploy in docker with colima context, as that is the current context.")
			alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + appHostsStr + " " + envs + "-e BINARY_TO_DOCKER=true" +
				agentPortPublish(opts.AgentPort) +
				proxyPortStr + appPortsStr +
				" --cap-add=BPF --cap-add=PERFMON --cap-add=NET_ADMIN --cap-add=SYS_RESOURCE --cap-add=SYS_PTRACE " + Volumes +
				" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf " +
				" --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) + clientPIDNSSuffix() +
				" --mode " + string(opts.Mode) + " --dns-port " + fmt.Sprintf("%d", opts.DnsPort) + " --is-docker" + mockModeSuffix(opts) + settleReadsSuffix(opts) + provenSuffix() + teleSuffix(conf)

			if opts.EnableTesting {
				alias += " --enable-testing"
			}
			alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
			alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

			if opts.GlobalPassthrough {
				alias += " --global-passthrough"
			}
			if opts.DisableHandshakeHold {
				alias += " --disable-handshake-hold"
			}
			if opts.CapturePackets {
				alias += " --capture-packets"
			}
			if opts.OpportunisticTLSIntercept {
				alias += " --opportunistic-tls-intercept"
			}
			if opts.ChannelBindingShim {
				alias += " --channel-binding-shim"
			}
			if opts.BuildDelay > 0 {
				alias += fmt.Sprintf(" --build-delay %d", opts.BuildDelay)
			}
			alias += agentMemoryLimitFlag
			alias += recordBufferFlags
			alias += upstreamTLSFlags
			if models.IsAnsiDisabled {
				alias += " --disable-ansi"
			}
			if len(opts.PassThroughPorts) > 0 {
				portStrings := make([]string, len(opts.PassThroughPorts))
				for i, port := range opts.PassThroughPorts {
					portStrings[i] = strconv.Itoa(int(port))
				}
				// Note the "=" sign, which is good practice for docker run
				alias += fmt.Sprintf(" --pass-through-ports=%s", strings.Join(portStrings, ","))
			}
			if debug {
				alias += " --debug"
			}
			alias += configPathFlag
			if opts.Synchronous {
				alias += " --sync"
			}
			if opts.EnableSampling > 0 {
				alias += fmt.Sprintf(" --enable-sampling=%d", opts.EnableSampling)
			}
			alias += extraArgWords
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + appHostsStr + " " + envs + "-e BINARY_TO_DOCKER=true" +
			agentPortPublish(opts.AgentPort) +
			proxyPortStr + appPortsStr +
			" --cap-add=BPF --cap-add=PERFMON --cap-add=NET_ADMIN --cap-add=SYS_RESOURCE --cap-add=SYS_PTRACE " + Volumes +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf " +
			" --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) + clientPIDNSSuffix() +
			" --mode " + string(opts.Mode) + " --dns-port " + fmt.Sprintf("%d", opts.DnsPort) + " --is-docker" + mockModeSuffix(opts) + settleReadsSuffix(opts) + provenSuffix() + teleSuffix(conf)

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

		if opts.GlobalPassthrough {
			alias += " --global-passthrough"
		}
		if opts.DisableHandshakeHold {
			alias += " --disable-handshake-hold"
		}
		if opts.CapturePackets {
			alias += " --capture-packets"
		}
		if opts.OpportunisticTLSIntercept {
			alias += " --opportunistic-tls-intercept"
		}
		if opts.ChannelBindingShim {
			alias += " --channel-binding-shim"
		}
		if opts.BuildDelay > 0 {
			alias += fmt.Sprintf(" --build-delay %d", opts.BuildDelay)
		}
		alias += agentMemoryLimitFlag
		alias += recordBufferFlags
		alias += upstreamTLSFlags
		if models.IsAnsiDisabled {
			alias += " --disable-ansi"
		}
		if len(opts.PassThroughPorts) > 0 {
			portStrings := make([]string, len(opts.PassThroughPorts))
			for i, port := range opts.PassThroughPorts {
				portStrings[i] = strconv.Itoa(int(port))
			}
			// Note the "=" sign, which is good practice for docker run
			alias += fmt.Sprintf(" --pass-through-ports=%s", strings.Join(portStrings, ","))
		}
		if debug {
			alias += " --debug"
		}
		alias += configPathFlag
		if opts.Synchronous {
			alias += " --sync"
		}
		if opts.EnableSampling > 0 {
			alias += fmt.Sprintf(" --enable-sampling=%d", opts.EnableSampling)
		}
		alias += extraArgWords
		return alias, nil
	}
	return "", errors.New("failed to get alias")
}

// ParseDockerCmd returns the container and network names a docker command
// refers to. Both flags are accepted in either spelling docker allows -
// "--name foo" and "--name=foo", "--network"/"--net" likewise - because a
// parser that understands only the spaced form reads nothing at all from a
// valid command, and the caller then has no container to instrument.
func ParseDockerCmd(cmd string, kind utils.CmdType, idc Client) (string, string, error) {
	if kind == utils.DockerStart {
		containerNameRegex := regexp.MustCompile(`start\s+(?:-[^\s]+\s+)*([^\s]*)`)
		containerNameMatches := containerNameRegex.FindStringSubmatch(cmd)
		if len(containerNameMatches) < 2 {
			return "", "", fmt.Errorf("failed to parse container name")
		}
		containerName := containerNameMatches[1]

		networks, err := idc.ExtractNetworksForContainer(containerName)
		if err != nil {
			return containerName, "", err
		}
		for i := range networks {
			return containerName, i, nil
		}
		return containerName, "", fmt.Errorf("failed to parse network name")
	}

	containerName := utils.ContainerNameFromDockerRun(cmd)
	if containerName == "" {
		return "", "", fmt.Errorf("failed to parse container name")
	}

	networkName := utils.NetworkNameFromDockerRun(cmd)
	if networkName == "" {
		return containerName, "", fmt.Errorf("failed to parse network name")
	}

	return containerName, networkName, nil
}

// clientPIDNSSuffix says, beside --client-pid, the pid namespace that pid is
// this CLI's in (utils.OwnPIDNamespace: digits, a boot id's hex and dashes,
// or "none", so nothing to quote). The agent container runs in a pid
// namespace of its own, so it never takes the pid for a process of its own
// (utils.ClientPIDHere).
func clientPIDNSSuffix() string {
	if ns := utils.OwnPIDNamespace(); ns != "" {
		return " --client-pid-ns " + ns
	}
	return ""
}

// provenSuffix carries the proven model's staging switch into the agent
// container, which is not given KEPLOY_PROVEN: " --proven" when the switch is
// on, "" otherwise, so the alias is byte-identical to before with it off.
func provenSuffix() string {
	args := provenswitch.AgentArgs()
	if len(args) == 0 {
		return ""
	}
	return " " + strings.Join(args, " ")
}

// teleSuffix carries the CLI's --disable-tele into the agent container,
// whatever its value. The agent pings telemetry at its start from its own
// config (provider.GetService) and the container sees no keploy.yml, so
// without it `keploy record --disable-tele` (or keploy.yml's disableTele)
// still resolves and contacts the telemetry host from the agent.
func teleSuffix(conf *config.Config) string {
	return fmt.Sprintf(" --disable-tele=%t", conf.DisableTele)
}

// settleReadsSuffix returns " --settle-reads" for a run that learns noise
// (models.SetupOptions.SettleReads), "" otherwise, so the alias is
// byte-identical to before for every other run.
func settleReadsSuffix(opts models.SetupOptions) string {
	if !opts.SettleReads {
		return ""
	}
	return " --settle-reads"
}

// mockModeSuffix returns " --mock-mode" when the session is a `keploy mock`
// record/replay run, so the containerised agent skips ingress/bind relocation.
func mockModeSuffix(opts models.SetupOptions) string {
	if !opts.MockMode {
		return ""
	}
	if opts.RecordRequests {
		return " --mock-mode --record-requests"
	}
	return " --mock-mode"
}
