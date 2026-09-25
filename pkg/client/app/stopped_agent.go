package app

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// stoppedAgentReadBudget bounds everything readStoppedAgent asks the docker
// daemon. Not on the Ctrl+C path -- readStoppedAgent skips an interrupted run
// -- so it is sized for a busy daemon rather than for the teardown drain.
const stoppedAgentReadBudget = 10 * time.Second

// maxAgentOutcomeBytes caps the outcome file keploy reads back from the agent's
// container. It lists every mock served and every call missed; a suite with
// tens of thousands of dependency calls stays far under this.
const maxAgentOutcomeBytes = 64 << 20

// StoppedAgent is what keploy read from the keploy-agent compose service's
// container after compose stopped it, and before keploy's own teardown
// removed it.
//
// Under compose the agent is a service in the project, and compose stops
// every service the moment the app exits. So by the time keploy has the app's
// exit, there is no agent left to ask anything: whatever keploy needs to know
// about the agent's end of the run has to be read from the container it left.
type StoppedAgent struct {
	// Outcome is the models.MockOutcome JSON a replaying agent writes as it
	// stops (docker.AgentOutcomeFile). OutcomeErr says why there is none.
	Outcome    []byte
	OutcomeErr error
}

// stoppedAgent holds the last StoppedAgent read. Guarded because the App runs
// on the runner goroutine and is read from the caller's.
type stoppedAgent struct {
	mu   sync.Mutex
	read *StoppedAgent
}

// StoppedAgent returns what keploy read from the stopped keploy-agent compose
// service, and false when nothing was read: not a compose run, not a mock
// replay, or a run that was interrupted.
func (a *App) StoppedAgent() (StoppedAgent, bool) {
	a.stopped.mu.Lock()
	defer a.stopped.mu.Unlock()
	if a.stopped.read == nil {
		return StoppedAgent{}, false
	}
	return *a.stopped.read, true
}

// readStoppedAgent reads the agent's end of a compose mock replay out of its
// stopped container. It must run after `docker compose up` has returned and
// before ComposeDown removes the container: run() defers it after the
// teardown, so it runs first.
func (a *App) readStoppedAgent(ctx context.Context) {
	if a.kind != utils.DockerCompose || !a.opts.MockMode || a.opts.Mode != models.MODE_TEST || a.keployContainer == "" {
		return
	}
	if ctx.Err() != nil {
		// Interrupted: cmdCancel has already brought the project down, and an
		// interrupted replay reports no outcome.
		return
	}
	readCtx, cancel := context.WithTimeout(context.Background(), stoppedAgentReadBudget)
	defer cancel()
	read := &StoppedAgent{}
	read.Outcome, read.OutcomeErr = readContainerFile(readCtx, a.docker, a.keployContainer, docker.AgentOutcomeFile, maxAgentOutcomeBytes)
	if read.OutcomeErr != nil {
		a.logger.Debug("could not read the replay outcome the keploy-agent container left",
			zap.String("container", a.keployContainer), zap.Error(read.OutcomeErr))
	}
	a.stopped.mu.Lock()
	a.stopped.read = read
	a.stopped.mu.Unlock()
}

// readContainerFile returns the regular file at path in the container name,
// which may be stopped, refusing one larger than limit.
func readContainerFile(ctx context.Context, client docker.Client, name, path string, limit int64) ([]byte, error) {
	rc, _, err := client.CopyFromContainer(ctx, name, path)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("the keploy-agent container %s left no %s", name, path)
		}
		return nil, fmt.Errorf("failed to read %s from the keploy-agent container %s: %w", path, name, err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("failed to read %s from the keploy-agent container %s: %w", path, name, err)
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("%s in the keploy-agent container %s is not a regular file", path, name)
	}
	body, err := io.ReadAll(io.LimitReader(tr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s from the keploy-agent container %s: %w", path, name, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s in the keploy-agent container %s is over the %d bytes keploy reads", path, name, limit)
	}
	return body, nil
}
