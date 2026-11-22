package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// composeAgentMoves bounds how many times a run moves keploy's agent to
// another port when `docker compose up` could not publish the one it had
// (upMovingTheAgent): three ups in all, as the agent client starts an agent
// three times at most (pkg.RetryAgentSetup).
const composeAgentMoves = 2

// composeUpFailed is the code `docker compose up` exits with when it fails,
// a container it could not start among the reasons.
const composeUpFailed = 1

// upOutputSize is how much of what an up printed last is kept (upOutput):
// compose's refusal to start a container is its last line.
const upOutputSize = 8 << 10

// upOutput keeps the last upOutputSize bytes a `docker compose up` printed,
// as keploy's console got them.
type upOutput struct {
	mu sync.Mutex
	b  []byte
}

func (o *upOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.b = append(o.b, p...)
	if over := len(o.b) - upOutputSize; over > 0 {
		o.b = append(o.b[:0], o.b[over:]...)
	}
	return len(p), nil
}

func (o *upOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.b)
}

// SetAgentAddress sets where the application client reads keploy's agent's
// address: the agent client's (config's AgentURI), which a move of the
// agent's port changes (SetOnAgentPortRefused). Unset, it is the address
// Setup handed (opts.AgentURI).
func (a *App) SetAgentAddress(f func() string) {
	a.agentAddressOf = f
}

// SetOnAgentPortRefused sets what the run asks, with what a failed `docker
// compose up` said, whether compose refused to publish keploy's agent's port
// on the host because another socket took it: the port the agent client
// moved the agent to, or 0 where the up's failure is another one, or the
// error of one keploy does not move the agent for.
func (a *App) SetOnAgentPortRefused(f func(ctx context.Context, said string) (uint32, error)) {
	a.onAgentPortRefused = f
}

// agentAddress is keploy's agent's address, as the agent client has it now.
func (a *App) agentAddress() string {
	if a.agentAddressOf != nil {
		return a.agentAddressOf()
	}
	return a.opts.AgentURI
}

// upMovingTheAgent runs up, a `docker compose up` that starts keploy's
// agent, and while it fails because compose could not publish the agent's
// port on the host, another socket having taken it after Setup drew it
// (onAgentPortRefused), moves the agent's service in the run's compose
// document to the port the agent client drew instead (moveComposeAgent), and
// runs up again, with the agent's launch recorded at its new address
// (redo): at most composeAgentMoves times, each said. The agent's port is
// moved before compose started the app, which runs in the agent's network
// namespace, so before any of the run's traffic. An up that fails otherwise,
// or that named the agent's address in words keploy does not know for a
// taken port (fail closed: its error shows the line), is returned as it is.
func (a *App) upMovingTheAgent(ctx context.Context, up func() utils.CmdError, redo func() utils.CmdError) utils.CmdError {
	if a.kind != utils.DockerCompose || a.onAgentPortRefused == nil {
		return up()
	}
	for moves := 0; ; moves++ {
		said := &upOutput{}
		a.upSaid.Store(said)
		cmdErr := up()
		a.upSaid.Store(nil)
		if cmdErr.Err == nil || ctx.Err() != nil || !composeUpEnded(cmdErr, a.useComposeLibrary()) {
			return cmdErr
		}
		if moves == composeAgentMoves {
			a.logger.Error("docker compose up failed again after keploy moved its agent to another port; not moving it again",
				zap.Int("moves", moves), zap.Error(cmdErr.Err))
			return cmdErr
		}
		to, err := a.onAgentPortRefused(ctx, said.String()+"\n"+cmdErr.Err.Error())
		switch {
		case err != nil:
			cmdErr.Err = fmt.Errorf("%w; %v", cmdErr.Err, err)
			return cmdErr
		case to == 0:
			return cmdErr
		}
		from := a.opts.AgentPort
		if err := a.moveComposeAgent(from, to); err != nil {
			cmdErr.Err = fmt.Errorf("%w; keploy could not move its agent to port %d in the compose document it runs: %v", cmdErr.Err, to, err)
			return cmdErr
		}
		if a.lastLaunchURI != "" {
			token.ForgetLaunch(a.lastLaunchURI, a.lastLaunchSeq)
			a.lastLaunchURI, a.lastLaunchSeq = "", 0
		}
		a.logger.Warn("running docker compose up again, with keploy's agent on another port",
			zap.Uint32("from", from), zap.Uint32("to", to), zap.Int("move", moves+1), zap.Int("of", composeAgentMoves))
		up = redo
	}
}

// composeUpEnded reports whether cmdErr is the end of a `docker compose up`
// that failed (composeUpFailed), or of the compose library's up (inProcess),
// whose error is what the engine said: not one keploy's start of the command
// failed, nor a signal's.
func composeUpEnded(cmdErr utils.CmdError, inProcess bool) bool {
	if cmdErr.Type != utils.Runtime {
		return false
	}
	if inProcess {
		return true
	}
	var ee interface{ ExitCode() int }
	return errors.As(cmdErr.Err, &ee) && ee.ExitCode() == composeUpFailed
}

// moveComposeAgent moves keploy's agent's service, in the run's compose
// document, from port from to port to (docker.MoveComposeAgentPort), and puts
// the document where the next up reads it: the file keploy wrote, or the
// document it hands compose (setComposeSource, which starts the compose
// library's runner and the containers' output tails of that document anew).
func (a *App) moveComposeAgent(from, to uint32) error {
	if a.docker == nil {
		return errors.New("keploy has no container engine client to write the compose document with")
	}
	if err := docker.MoveComposeAgentPort(a.composeDoc, from, to); err != nil {
		return err
	}
	if a.composeFile != "" {
		if err := a.docker.WriteComposeFile(a.composeDoc, a.composeFile); err != nil {
			return fmt.Errorf("failed to write the compose file %s: %w", a.composeFile, err)
		}
	} else {
		content, err := a.docker.MarshalCompose(a.composeDoc)
		if err != nil {
			return fmt.Errorf("failed to serialise the compose document: %w", err)
		}
		a.setComposeSource("", content, a.composeDoc)
	}
	a.opts.AgentPort = to
	return nil
}
