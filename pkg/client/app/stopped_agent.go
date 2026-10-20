package app

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
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
	// Container is the agent's container name.
	Container string
	// Agent and App are how the agent's container and the app's ended, as
	// docker reports them; nil when docker could not say.
	Agent *container.State
	App   *container.State
	// Outcome is the models.MockOutcome JSON a replaying agent writes as it
	// stops (docker.AgentOutcomeFile). OutcomeErr says why there is none.
	Outcome    []byte
	OutcomeErr error
}

// AppStarted reports whether the app's container ever started. Compose starts
// it only once the agent's healthcheck passes, so an app that never started is
// one the agent never let through.
func (s StoppedAgent) AppStarted() bool {
	if s.App == nil {
		return false
	}
	started, err := time.Parse(time.RFC3339Nano, s.App.StartedAt)
	return err == nil && !started.IsZero()
}

// AgentFailed reports whether the agent's container ended the run: it stopped
// on its own while the app still needed it -- died, or was killed -- rather
// than being stopped by compose once the app had exited, which is how every
// compose run ends. Compose's stop is graceful, and the agent exits 0 from it.
//
// Timestamps cannot tell the two apart. The app runs in the agent's PID
// namespace (pkg/platform/docker, modifyAppService), so when the agent dies
// the kernel kills the app along with it, and the two end within the same
// instant, in an order docker does not reliably record. What does tell them
// apart is how the APP ended: killed by a signal -- the namespace going away
// (137), or compose stopping it over the dead agent (143) -- rather than
// exiting with a code of its own. An agent that exits non-zero after the app
// has returned its own code is one compose killed for not stopping in time:
// the app's code still stands. So does an app the kernel killed for running
// out of its own memory: that 137 is the app's, whatever became of the agent
// afterwards.
//
// When docker could not say how the app ended, the agent's own exit is all
// there is.
func (s StoppedAgent) AgentFailed() bool {
	if s.Agent == nil || s.Agent.Running || s.Agent.ExitCode == 0 {
		return false
	}
	if s.App == nil || !s.AppStarted() || s.App.Running {
		return true
	}
	if s.App.OOMKilled {
		return false
	}
	return s.App.ExitCode == 128+int(syscall.SIGKILL) || s.App.ExitCode == 128+int(syscall.SIGTERM)
}

// AgentDiedAfterReady is AgentFailed for a run keploy saw its agent ready in
// (keploy record's, once its recording is under way): the agent crashed
// (agentCrashed) and so stopped the project -- before compose ever started
// the app (its container never started: no exit of its own), or under the
// app, taking it with it (the app killed by a signal, 137 or 143, not ended
// with a code of its own or for running out of its own memory). appExit is
// the app's exit as keploy got it, which decides when docker could not say
// how the app's container ended: unlike AgentFailed, an unknown app is never
// read as one that never started.
//
// Only a crash counts, not compose's own stop of the agent (0, or 143 / 137
// from its TERM and its KILL after the grace period): compose stops the agent
// when the user stops the project, and when another of the project's
// services fails, and neither is the agent's failure. The price: an agent
// something else SIGKILLed after it was ready leaves the app's own end to
// decide.
func (s StoppedAgent) AgentDiedAfterReady(appExit int) bool {
	if !s.agentCrashed() {
		return false
	}
	if s.AppNeverStarted() {
		return true
	}
	if s.App != nil && !s.App.Running {
		// The app's own end, as docker reports it, decides: killed with the
		// agent, or a code of its own, after which the agent crashed.
		if s.App.OOMKilled {
			return false
		}
		appExit = s.App.ExitCode
	}
	return appExit == 128+int(syscall.SIGKILL) || appExit == 128+int(syscall.SIGTERM)
}

// AppNeverStarted reports whether docker says the app's container never
// started: its state is still "created", compose having provisioned it and
// stopped the project before starting it (it starts the app only once the
// agent's healthcheck passes). An app docker could not report is not one.
func (s StoppedAgent) AppNeverStarted() bool {
	return s.App != nil && s.App.Status == "created"
}

// agentCrashed reports whether the agent's container ended on its own rather
// than being stopped by compose: an exit other than 0, compose's TERM (143) or
// its KILL (137) -- the rule isTransientComposeDependencyFailure reads
// compose's stop of the agent by -- plus a kill for running out of memory,
// which that rule (made for a retry) leaves out.
func (s StoppedAgent) agentCrashed() bool {
	a := s.Agent
	if a == nil || a.Running || a.ExitCode == 0 {
		return false
	}
	if a.OOMKilled {
		return true
	}
	return a.ExitCode != 128+int(syscall.SIGTERM) && a.ExitCode != 128+int(syscall.SIGKILL)
}

// MockOutcome is what the agent served and missed in a mock replay, decoded
// from the account it left as compose stopped it -- or why there is none. An
// account keploy did not get is never an empty one: read as "served nothing,
// missed nothing", it would prove a replay nobody saw.
func (s StoppedAgent) MockOutcome() (models.MockOutcome, error) {
	if s.OutcomeErr != nil {
		return models.MockOutcome{}, s.OutcomeErr
	}
	if s.Outcome == nil {
		return models.MockOutcome{}, errors.New("keploy did not read the replay outcome the keploy-agent container left")
	}
	var outcome models.MockOutcome
	if err := json.Unmarshal(s.Outcome, &outcome); err != nil {
		return models.MockOutcome{}, fmt.Errorf("failed to decode the replay outcome the keploy-agent container left: %w", err)
	}
	// Its misses are read as the agent client reads a live agent's: a report
	// carries their content only when both this CLI and the agent opted in.
	outcome.Missed = models.ReportContent(outcome.Missed)
	return outcome, nil
}

// stoppedAgent holds the last StoppedAgent read. Guarded because the App runs
// on the runner goroutine and is read from the caller's.
type stoppedAgent struct {
	mu   sync.Mutex
	read *StoppedAgent
}

// StoppedAgent returns what keploy read from the stopped keploy-agent compose
// service, and false when nothing was read: not a compose mock run or compose
// recording, or a run that was interrupted.
func (a *App) StoppedAgent() (StoppedAgent, bool) {
	a.stopped.mu.Lock()
	defer a.stopped.mu.Unlock()
	if a.stopped.read == nil {
		return StoppedAgent{}, false
	}
	return *a.stopped.read, true
}

// readStoppedAgent reads the agent's end of a compose mock run, or of a
// compose recording, out of its stopped container: how it ended beside how
// the app did (which says whether the agent is what stopped the project:
// AgentFailed), and -- for a mock replay -- what it served and missed. It
// must run after `docker compose up` has returned and before ComposeDown
// removes the container: run() defers it after the teardown, so it runs
// first.
func (a *App) readStoppedAgent(ctx context.Context) {
	if a.kind != utils.DockerCompose || a.keployContainer == "" || !(a.opts.MockMode || a.opts.Mode == models.MODE_RECORD) {
		return
	}
	if ctx.Err() != nil {
		// Interrupted: cmdCancel has already brought the project down, and an
		// interrupted run reports neither an outcome nor a failure.
		return
	}
	readCtx, cancel := context.WithTimeout(context.Background(), stoppedAgentReadBudget)
	defer cancel()
	read := &StoppedAgent{Container: a.keployContainer}
	var agentErr error
	read.Agent, agentErr = a.containerState(readCtx, a.keployContainer)
	if a.container != "" {
		read.App, _ = a.containerState(readCtx, a.container)
	}
	if a.opts.MockMode && a.opts.Mode == models.MODE_TEST {
		if agentErr != nil {
			// Docker answers "not found" for a container it does not know
			// exactly as for a file the container lacks, and that is not
			// what happened: the agent's container is not on the daemon
			// keploy asks, or that daemon could not be reached.
			read.OutcomeErr = fmt.Errorf("keploy could not find the keploy-agent container %s on the docker daemon it talks to: %w", a.keployContainer, agentErr)
		} else {
			read.Outcome, read.OutcomeErr = readContainerFile(readCtx, a.docker, a.keployContainer, docker.AgentOutcomeFile, maxAgentOutcomeBytes)
		}
		if read.OutcomeErr != nil {
			a.logger.Debug("could not read the replay outcome the keploy-agent container left",
				zap.String("container", a.keployContainer), zap.Error(read.OutcomeErr))
		}
	}
	a.stopped.mu.Lock()
	a.stopped.read = read
	a.stopped.mu.Unlock()
}

// containerState is how name ended, or why docker could not say.
func (a *App) containerState(ctx context.Context, name string) (*container.State, error) {
	info, err := a.docker.ContainerInspect(ctx, name)
	if err == nil && (info.ContainerJSONBase == nil || info.State == nil) {
		err = errors.New("docker reported no state for it")
	}
	if err != nil {
		a.logger.Debug("could not read how a compose container ended", zap.String("container", name), zap.Error(err))
		return nil, err
	}
	return info.State, nil
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
