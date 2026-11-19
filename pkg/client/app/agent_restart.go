package app

import (
	"context"
	"time"

	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// agentRestartPoll is how often the compose retry looks at keploy's agent
// container while its `up` brings the stack up; agentBaselineTries, how many
// times it asks docker which agent runs before its `up`.
var (
	agentRestartPoll   = 500 * time.Millisecond
	agentBaselineTries = 5
)

// agentStartSkew is how far behind the CLI's clock the docker daemon's may be
// for an agent's start to count as after the retry began: under the retry's
// shortest backoff (composeDepFailureBaseBackoff).
const agentStartSkew = time.Second

// agentIdentity is which process of keploy's agent container runs now: the
// container and when it started, as docker reports them, or "" when none
// runs, and that start. known is false when docker gave no answer (the
// inspect failed, other than for there being no such container), which is not
// "none runs". It needs nothing from the agent itself, so it tells an agent
// compose started again from the one the run set up whatever the agent's
// version.
func (a *App) agentIdentity(ctx context.Context) (id string, started time.Time, known bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := a.docker.ContainerInspect(ctx, a.keployContainer)
	if err != nil {
		return "", time.Time{}, errdefs.IsNotFound(err)
	}
	if info.ContainerJSONBase == nil || info.State == nil {
		return "", time.Time{}, false
	}
	if !info.State.Running {
		return "", time.Time{}, true
	}
	started, _ = time.Parse(time.RFC3339Nano, info.State.StartedAt)
	return info.ID + "@" + info.State.StartedAt, started, true
}

// composeAgentSetupFailed ends the run on err, the failed setup of keploy's
// agent that compose's retry started again: the halt is kept, naming that
// setup, its error and the next step; then the releases of the app the run
// asked for, held or not, end with err (resume), none reaching that agent,
// which has none of the run's mocks or settings and would start the app
// against them; then
// the stack is stopped (end). Releases left held would hold the app back
// until the agent's healthcheck gave up, and let go they would release it.
// A halt kept already stands: its end is under way.
func (a *App) composeAgentSetupFailed(err error, resume func(failed error), end func()) {
	a.composeApp.stopMu.Lock()
	kept := a.composeApp.halted.CompareAndSwap(nil, &composeHalt{exitCode: -1, agentSetup: err, agent: a.keployContainer})
	a.composeApp.stopMu.Unlock()
	resume(err)
	if !kept {
		a.logger.Debug("keploy's agent, started again by docker compose, could not be set up again; the run ends on the halt kept before",
			zap.String("agentContainer", a.keployContainer), zap.Error(err))
		return
	}
	a.logger.Info("keploy's agent, started again by docker compose, could not be set up again: ending the run",
		zap.String("agentContainer", a.keployContainer), zap.Error(err))
	if end != nil {
		end()
	}
}

// appStarted reports whether the app's container runs: compose starts it only
// behind a healthy agent, so the agent then is one the run set up.
func (a *App) appStarted(ctx context.Context) bool {
	if a.container == "" || a.docker == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := a.docker.ContainerInspect(ctx, a.container)
	return err == nil && info.ContainerJSONBase != nil && info.State != nil && info.State.Running
}

// retryComposeUp runs the compose retry's `up` (run) while it watches keploy's
// agent container: compose keeps a running agent, but starts again one it
// stopped and recreates one under a recreate flag, and either has none of
// what the run set up on the one before. When the agent running is not the
// one that ran before the retry, the agent client sets it up again; when the
// app's container starts first, the agent was kept. The watch ends with run.
// When docker cannot say which agent ran before, an agent that started after
// the retry began is taken for one compose started again. A setup of it that
// fails ends the run at once (composeAgentSetupFailed), through end, the
// run's end on a halt (composeHaltEnd); nil: nothing ends it.
func (a *App) retryComposeUp(ctx context.Context, end func(), run func() utils.CmdError) utils.CmdError {
	if a.onAgentRestart == nil || a.keployContainer == "" || a.docker == nil {
		return run()
	}
	before, _, baselineKnown := a.agentIdentity(ctx)
	for try := 1; !baselineKnown && try < agentBaselineTries && ctx.Err() == nil; try++ {
		select {
		case <-ctx.Done():
		case <-time.After(agentRestartPoll):
		}
		before, _, baselineKnown = a.agentIdentity(ctx)
	}
	if !baselineKnown {
		a.logger.Warn("docker could not say which keploy agent ran before the retry of docker compose up; "+
			"an agent that starts after the retry began is taken for one compose started again",
			zap.String("agentContainer", a.keployContainer))
	}
	// An agent that does not run before the retry (keploy's stop of the
	// stack took it) is one the retry's `up` starts again, which answers at
	// the same address with none of what the run set up. A release the run
	// asked for and that this agent took would start the app against it
	// before the agent client set it up again: releases are held from before
	// the `up` until the watch has had the agent set up again, which
	// releases it last, or the watch ends. One kept running is the one the
	// run set up, and a release still owed to it goes on. With no hold on,
	// a failed setup of an agent the watch takes for one compose started
	// again ends every release of the run all the same (refuseAgentReleases):
	// docker not saying which agent ran before, or compose recreating one
	// that ran, never lets a release reach an agent that is not set up.
	resume := func(failed error) {
		if failed != nil && a.refuseAgentReleases != nil {
			a.refuseAgentReleases(failed)
		}
	}
	if baselineKnown && before == "" && a.holdAgentRelease != nil {
		resume = a.holdAgentRelease(ctx)
	}
	// The start docker reports is by the daemon's clock, which can lag the
	// CLI's a little (Docker Desktop's VM); an agent kept from before started
	// at least the retry's backoff before this.
	since := time.Now().Add(-agentStartSkew)
	// restarted reports whether a running agent is not the one that ran
	// before the retry.
	restarted := func(id string, started time.Time) bool {
		if baselineKnown {
			return id != before
		}
		return started.After(since)
	}
	watch, stopWatch := context.WithCancel(ctx)
	g, watch := errgroup.WithContext(watch)
	g.Go(func() error {
		defer utils.Recover(a.logger)
		defer resume(nil)
		tick := time.NewTicker(agentRestartPoll)
		defer tick.Stop()
		for {
			select {
			case <-watch.Done():
				return nil
			case <-tick.C:
			}
			if now, started, known := a.agentIdentity(watch); known && now != "" && restarted(now, started) {
				a.logger.Info("docker compose started keploy's agent again in its retry; setting it up again",
					zap.String("agentContainer", a.keployContainer))
				if err := a.onAgentRestart(watch); err != nil && watch.Err() == nil {
					a.composeAgentSetupFailed(err, resume, end)
				}
				return nil
			}
			if a.appStarted(watch) {
				return nil
			}
		}
	})
	cmdErr := run()
	stopWatch()
	_ = g.Wait()
	return cmdErr
}
