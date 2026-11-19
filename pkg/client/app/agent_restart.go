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
//
// A setup that failed while keploy's own stop of the stack is under way
// (agentStopping, or a dependency's crash kept for the next bring-up) is a
// setup that stop cut, decided under stopMu as an exit is: the stop took the
// agent it talked to. It keeps no halt, ends nothing and refuses no release:
// the watch ends, and the hold is let go with nil. The run's loop then takes
// the next bring-up, whose agent is not the last one set up, so it is set up
// again before any release goes to it.
//
// With no such stop under way, a setup whose agent no longer runs (replaced:
// compose recreated it, or it is gone) is one compose cut, not a failed one:
// it keeps no halt and ends nothing, and the watch goes on (goOn) to set up
// the agent that runs in its place.
func (a *App) composeAgentSetupFailed(err error, replaced bool, resume func(failed error), end func()) (goOn bool) {
	a.composeApp.stopMu.Lock()
	if a.composeApp.agentStopping.Load() || a.composeApp.depCrash.Load() != nil {
		a.composeApp.stopMu.Unlock()
		a.logger.Debug("the setup of keploy's agent, started again by docker compose, ended while keploy stopped the stack: keploy's own stop cut it, and the next bring-up sets the agent up again",
			zap.String("agentContainer", a.keployContainer), zap.Error(err))
		return false
	}
	if replaced {
		a.composeApp.stopMu.Unlock()
		a.logger.Debug("the setup of keploy's agent, started again by docker compose, ended once that agent no longer ran: compose replaced it, and keploy sets up the one in its place",
			zap.String("agentContainer", a.keployContainer), zap.Error(err))
		return true
	}
	kept := a.composeApp.halted.CompareAndSwap(nil, &composeHalt{exitCode: -1, agentSetup: err, agent: a.keployContainer})
	a.composeApp.stopMu.Unlock()
	resume(err)
	if !kept {
		a.logger.Debug("keploy's agent, started again by docker compose, could not be set up again; the run ends on the halt kept before",
			zap.String("agentContainer", a.keployContainer), zap.Error(err))
		return false
	}
	a.logger.Info("keploy's agent, started again by docker compose, could not be set up again: ending the run",
		zap.String("agentContainer", a.keployContainer), zap.Error(err))
	if end != nil {
		end()
	}
	return false
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
// what the run set up on the one before. Agents are told apart by identity
// (agentIdentity), against the last one whose setup completed (agentSetUp):
// until the app's container starts or run returns, the agent client sets up
// every running agent that is not that one, each in turn, whether or not
// docker said which agent ran before the retry. When the app's container
// starts first, the agent was kept. While no identity is known yet (docker
// said nothing at every retry so far), an agent that started after the first
// such retry began is taken for one compose started again. A setup that
// fails while its agent still runs ends the run at once
// (composeAgentSetupFailed), through end, the run's end on a halt
// (composeHaltEnd); nil: nothing ends it.
func (a *App) retryComposeUp(ctx context.Context, end func(), run func() utils.CmdError) utils.CmdError {
	if a.onAgentRestart == nil || a.keployContainer == "" || a.docker == nil {
		return run()
	}
	before, beforeStarted, baselineKnown := a.agentIdentity(ctx)
	for try := 1; !baselineKnown && try < agentBaselineTries && ctx.Err() == nil; try++ {
		select {
		case <-ctx.Done():
		case <-time.After(agentRestartPoll):
		}
		before, beforeStarted, baselineKnown = a.agentIdentity(ctx)
	}
	// The start docker reports is by the daemon's clock, which can lag the
	// CLI's a little (Docker Desktop's VM); an agent kept from before started
	// at least the retry's backoff before this.
	since := time.Now().Add(-agentStartSkew)
	if !baselineKnown {
		a.logger.Warn("docker could not say which keploy agent ran before the retry of docker compose up; "+
			"an agent that starts after the retry began is taken for one compose started again",
			zap.String("agentContainer", a.keployContainer))
	}
	// The run's first agent is the one that ran before the bring-up's first
	// retry: the run set it up. Once docker names the agent before a retry,
	// that is it, unless it started after a retry docker said nothing at had
	// begun (agentBlindSince): compose started that one, and no setup of it
	// completed, so none of the agents set up runs ("").
	if !a.agentSetUpKnown {
		switch {
		case baselineKnown:
			a.agentSetUp, a.agentSetUpKnown = before, true
			if !a.agentBlindSince.IsZero() && beforeStarted.After(a.agentBlindSince) {
				a.agentSetUp = ""
			}
		case a.agentBlindSince.IsZero():
			a.agentBlindSince = since
		}
	}
	last, lastKnown, blindSince := a.agentSetUp, a.agentSetUpKnown, a.agentBlindSince
	// restarted reports whether a running agent is not the last one set up.
	restarted := func(id string, started time.Time) bool {
		if lastKnown {
			return id != last
		}
		return started.After(blindSince)
	}
	// No agent running before the retry (keploy's stop of the stack took
	// it), or one that is not the last set up, means the agent at the
	// agent's address while the `up` runs is one no setup has completed on.
	// A release the run asked for and that it took would start the app
	// against it before the agent client set it up: releases are held from
	// before the `up` until the watch ends, and the agent client lets none
	// go to an agent no setup has completed on, whatever the hold's end
	// (SetAgentReleaseHold); each setup releases its agent last. The last
	// agent set up, kept running, is the one the run set up, and a release
	// still owed to it goes on. With no hold on, a failed setup of an agent
	// the watch takes for one compose started again ends every release of
	// the run all the same (refuseAgentReleases): docker not saying which
	// agent ran before, or compose recreating one that ran, never lets a
	// release reach an agent that is not set up.
	resume := func(failed error) {
		if failed != nil && a.refuseAgentReleases != nil {
			a.refuseAgentReleases(failed)
		}
	}
	if baselineKnown && (before == "" || before != last) && a.holdAgentRelease != nil {
		resume = a.holdAgentRelease(ctx)
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
			case <-tick.C:
			}
			if watch.Err() != nil {
				// run returned. An agent it left running that is not the
				// last one set up, where the app has not started (the up
				// returned before the watch set it up, or compose replaced
				// the one it set up): the releases are held for it and the
				// hold let go, so none goes to it until a setup of it
				// completes (the next retry's) or the run ends.
				if a.holdAgentRelease != nil && !a.appStarted(ctx) {
					if now, started, _ := a.agentIdentity(ctx); now != "" && restarted(now, started) {
						a.holdAgentRelease(ctx)(nil)
					}
				}
				return nil
			}
			now, started, _ := a.agentIdentity(watch)
			if now == "" || !restarted(now, started) {
				if a.appStarted(watch) {
					return nil
				}
				continue
			}
			a.logger.Info("docker compose started keploy's agent again in its retry; setting it up again",
				zap.String("agentContainer", a.keployContainer))
			if err := a.onAgentRestart(watch); err != nil {
				// A setup whose agent no longer runs, or runs as another
				// process, was cut by compose replacing it.
				after, _, afterKnown := a.agentIdentity(watch)
				if watch.Err() != nil {
					return nil // run's end cut the setup, which marked its agent not set up
				}
				if !a.composeAgentSetupFailed(err, afterKnown && after != now, resume, end) {
					return nil
				}
				continue
			}
			last, lastKnown = now, true
		}
	})
	cmdErr := run()
	stopWatch()
	_ = g.Wait()
	a.agentSetUp, a.agentSetUpKnown = last, lastKnown
	return cmdErr
}
