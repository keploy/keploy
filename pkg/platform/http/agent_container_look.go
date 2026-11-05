package http

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

// containerAPI is the part of the container engine's API that the wait for an
// agent's start reads the agent's container through: its state, and the
// report of its start it writes in it.
type containerAPI interface {
	ContainerInspect(ctx context.Context, container string) (container.InspectResponse, error)
	CopyFromContainer(ctx context.Context, container, srcPath string) (io.ReadCloser, container.PathStat, error)
}

// containerLook reads an agent's container for the wait for its start: through
// the engine's API, with the client keploy already holds, and through the
// engine's CLI (engineCLI, which tries sudo -n where it is refused) only where
// there is no client, or the client cannot reach the engine's socket. Read
// through the CLI alone, a keploy with no `docker` binary beside it (the
// runner image a cluster's replay runs keploy in) could read neither the
// container's state nor its report, and waited on the agent's health check
// alone.
//
// Each read answers what the CLI prints for it, so one reading of an answer
// serves both.
type containerLook struct {
	api containerAPI
	cli *engineCLI
	// viaCLI says the API's socket could not be reached: the CLI reads from
	// then on, as it did before the API read at all.
	viaCLI atomic.Bool
}

// newContainerLook is a containerLook through api (nil: the CLI alone).
func newContainerLook(logger *zap.Logger, api containerAPI) *containerLook {
	return &containerLook{api: api, cli: newEngineCLI(logger)}
}

// useAPI reports whether a read goes to the API first.
func (l *containerLook) useAPI() bool { return l.api != nil && !l.viaCLI.Load() }

// unreachable reports whether err is the API client's failure to reach the
// engine's socket (not there, refused, or not permitted), after which the
// CLI reads: an answer of the engine, a not-found included, is no such
// failure.
func (l *containerLook) unreachable(err error) bool {
	if !client.IsErrConnectionFailed(err) {
		return false
	}
	if l.viaCLI.CompareAndSwap(false, true) {
		l.cli.logger.Debug("keploy cannot reach the container engine's API socket; it reads the keploy agent's container through the engine's CLI",
			zap.Error(err))
	}
	return true
}

// inspect answers what `inspect -f '{{.State.Status}}'` prints of container
// name or, withCode, `inspect -f '{{.State.Status}} {{.State.ExitCode}}'`.
// An engine with no such container answers the API's not-found, or the CLI's
// words for it (engineSaysGone reads both).
func (l *containerLook) inspect(ctx context.Context, name string, withCode bool) ([]byte, error) {
	if l.useAPI() {
		resp, err := l.api.ContainerInspect(ctx, name)
		switch {
		case err == nil && (resp.ContainerJSONBase == nil || resp.State == nil):
			return nil, fmt.Errorf("the container engine's inspect of %s carries no state", name)
		case err == nil && withCode:
			return fmt.Appendf(nil, "%s %d\n", resp.State.Status, resp.State.ExitCode), nil
		case err == nil:
			return fmt.Appendf(nil, "%s\n", resp.State.Status), nil
		case !l.unreachable(err):
			return nil, err
		}
	}
	format := "{{.State.Status}}"
	if withCode {
		format = "{{.State.Status}} {{.State.ExitCode}}"
	}
	return l.cli.stdout(ctx, "inspect", "-f", format, name)
}

// bootReportMax bounds what a read of the agent's report of its start takes
// from the engine: the report is a few hundred bytes, and firstTarFile keeps
// no more than 64 KiB of it.
const bootReportMax = 1 << 20

// copyOut answers what `cp name:path -` prints: path, out of container name,
// as a tar stream.
func (l *containerLook) copyOut(ctx context.Context, name, path string) ([]byte, error) {
	if l.useAPI() {
		rc, _, err := l.api.CopyFromContainer(ctx, name, path)
		if err == nil {
			defer func() { _ = rc.Close() }()
			return io.ReadAll(io.LimitReader(rc, bootReportMax))
		}
		if !l.unreachable(err) {
			return nil, err
		}
	}
	return l.cli.stdout(ctx, "cp", name+":"+path, "-")
}

// containerAPI is the engine API client a's container looks read through:
// the client keploy runs the agent's container with (nil: none, and the
// CLI reads).
func (a *AgentClient) containerAPI() containerAPI {
	// A nil client is a nil containerAPI.
	return a.dockerClient
}

// envContainerAPI is an engine API client for a wait with no client of its
// own (WatchContainerAgentStart), at the endpoint the environment names
// (DOCKER_HOST, which keploy sets to the active engine's), and the func that
// closes it once the wait is over, so no wait leaves a connection to the
// engine's socket open behind it: nil, and the CLI reads, when none can be
// made. Making one connects to nothing. It negotiates the API's version with
// the engine on its first read, so an engine older than this client's API
// answers it too.
func envContainerAPI(logger *zap.Logger) (api containerAPI, closeAPI func()) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Debug("keploy could not make a container engine API client; it reads the keploy agent's container through the engine's CLI",
			zap.Error(err))
		return nil, func() {}
	}
	return c, func() {
		if err := c.Close(); err != nil {
			logger.Debug("keploy could not close its container engine API client", zap.Error(err))
		}
	}
}
