// Package engine names the container engine a docker-mode command runs its
// containers on, and holds what keploy needs to drive that engine itself: the
// CLI for its own container commands, the compose command, the Engine API
// endpoint, and the security options its agent container needs there.
//
// Docker is built in. Any other engine is supported only when a build
// registers it (Register); this build registers none, and a command for an
// engine that is not registered is refused (Unsupported) instead of being run
// as a native process, where keploy would capture nothing from it.
package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// Engine names.
const (
	Docker = "docker"
	Podman = "podman"
)

// Runtime is what keploy needs to drive one container engine.
type Runtime struct {
	// Name is the engine's name (Docker, Podman).
	Name string
	// CLI is the binary keploy runs its own container commands with: the
	// agent container, and the diagnostics and stop around it.
	CLI string
	// Compose is the command keploy runs its own compose commands with, where
	// the compose library is not linked: {"docker", "compose"}.
	Compose []string
	// Host is the Engine API endpoint, exported as DOCKER_HOST so the Docker
	// SDK client and the compose library both reach it. Empty keeps the
	// environment's own (DOCKER_HOST, else the Docker socket).
	Host string
	// AgentSecurityOpts are the --security-opt values keploy's agent
	// container needs on this engine, for `run` and for the compose service.
	AgentSecurityOpts []string
}

// Preparer readies an engine for one keploy run and says how to drive it. It
// may start what the run needs (an API service); whatever it starts must not
// outlive the keploy process.
type Preparer func(ctx context.Context, logger *zap.Logger) (Runtime, error)

// dockerRuntime is Docker's. The agent loads eBPF programs, which an SELinux
// policy denies to a container by default (container_t may not call bpf(2)),
// so on an SELinux host the agent never starts. label=disable lifts SELinux
// confinement for the agent container alone, and Docker accepts it where
// SELinux is off.
var dockerRuntime = Runtime{
	Name:              Docker,
	CLI:               "docker",
	Compose:           []string{"docker", "compose"},
	AgentSecurityOpts: []string{"label=disable"},
}

var (
	mu        sync.RWMutex
	preparers = map[string]Preparer{}
	active    = dockerRuntime
)

// Register makes an engine available to docker-mode commands. Call it from an
// init function, before the CLI runs.
func Register(name string, p Preparer) {
	mu.Lock()
	defer mu.Unlock()
	preparers[name] = p
}

// Supported reports whether this build can drive the engine.
func Supported(name string) bool {
	if name == Docker {
		return true
	}
	mu.RLock()
	defer mu.RUnlock()
	_, ok := preparers[name]
	return ok
}

// Prepare readies the engine for this run and makes it the active one. For
// an engine other than Docker it exports the engine's API endpoint as
// DOCKER_HOST, which every Engine API client keploy builds reads.
func Prepare(ctx context.Context, logger *zap.Logger, name string) error {
	if name == "" || name == Docker {
		mu.Lock()
		active = dockerRuntime
		mu.Unlock()
		return nil
	}
	mu.RLock()
	p, ok := preparers[name]
	mu.RUnlock()
	if !ok {
		return Unsupported(name)
	}
	rt, err := p(ctx, logger)
	if err != nil {
		return fmt.Errorf("failed to prepare %s: %w", name, err)
	}
	// What keploy runs its own commands with must never be empty.
	if rt.Name == "" {
		rt.Name = name
	}
	if rt.CLI == "" {
		rt.CLI = name
	}
	if len(rt.Compose) == 0 {
		rt.Compose = []string{rt.CLI, "compose"}
	}
	if rt.Host != "" {
		if err := os.Setenv("DOCKER_HOST", rt.Host); err != nil {
			return fmt.Errorf("failed to point the Engine API client at %s: %w", name, err)
		}
	}
	mu.Lock()
	active = rt
	mu.Unlock()
	return nil
}

// Active is the engine this run drives; Docker until Prepare says otherwise.
func Active() Runtime {
	mu.RLock()
	defer mu.RUnlock()
	return active
}

// Unsupported is the error for a command on an engine this build cannot
// drive.
func Unsupported(name string) error {
	if name == Podman {
		return fmt.Errorf("this keploy build cannot record or test applications that run in Podman. " +
			"Keploy from keploy.io (free with an account) can: " +
			"curl --silent -O -L https://keploy.io/install.sh && source install.sh")
	}
	return fmt.Errorf("container engine %q is not supported", name)
}

// Invocation finds the container engine a command invokes: the first
// docker-compose or podman-compose, or the first docker or podman followed,
// past its own global flags, by a subcommand that launches containers (run,
// start, compose, container, and Podman's kube and pod). Wrappers in front
// (sudo -u alice, timeout 600, env FOO=bar, a full path) do not matter. A bare
// name is not enough: `npm run podman` runs a script.
//
// It names the engine, for choosing and refusing one. Whether keploy can
// rewrite the command is a stricter question (the CLI's mentionsDockerBinary).
func Invocation(command string) (name string, ok bool) {
	fields := strings.Fields(strings.ToLower(command))
	for i, field := range fields {
		base := field
		if j := strings.LastIndex(base, "/"); j >= 0 {
			base = base[j+1:]
		}
		switch base {
		case "docker-compose":
			return Docker, true
		case "podman-compose":
			return Podman, true
		case Docker, Podman:
			if sub, ok := subcommand(fields[i+1:]); ok && launches(sub) {
				return base, true
			}
		}
	}
	return "", false
}

// subcommand is the first argument after an engine's global flags. A flag
// that takes its value as the next argument (podman --connection m run,
// docker --context prod run) consumes it.
func subcommand(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a, true
		}
		if !strings.Contains(a, "=") && globalFlagsWithValue[a] {
			i++
		}
	}
	return "", false
}

// globalFlagsWithValue are the docker and podman global flags whose value can
// be the next argument.
var globalFlagsWithValue = map[string]bool{
	// docker
	"--config": true, "--context": true, "-c": true, "--host": true, "-h": true, // -H, lowercased with the rest
	"--log-level": true, "-l": true, "--tlscacert": true, "--tlscert": true, "--tlskey": true,
	// podman (-c and --log-level are shared)
	"--connection": true, "--url": true, "--identity": true, "--root": true, "--runroot": true,
	"--storage-driver": true, "--storage-opt": true, "--cgroup-manager": true, "--runtime": true,
	"--runtime-flag": true, "--module": true, "--ssh": true, "--tmpdir": true,
	"--events-backend": true, "--conmon": true, "--network-cmd-path": true,
	"--network-config-dir": true, "--hooks-dir": true, "--imagestore": true, "--volumepath": true,
	"--registries-conf": true, "--cdi-spec-dir": true, "--out": true, "--db-backend": true,
}

// launches reports whether an engine subcommand starts containers.
func launches(sub string) bool {
	switch sub {
	case "run", "start", "compose", "container", "kube", "pod":
		return true
	}
	return false
}

// Detect names the engine a docker-mode command runs on: the one it invokes,
// and Docker when it invokes none, which is what a wrapper (make up, a
// script) given --cmd-type has always meant.
func Detect(command string) string {
	if name, ok := Invocation(command); ok {
		return name
	}
	return Docker
}
