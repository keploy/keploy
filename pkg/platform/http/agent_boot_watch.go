package http

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// AN AGENT'S START IS GIVEN UP ON ONLY ONCE IT HAS STOPPED MAKING PROGRESS.
//
// keploy waits for the agent it started to answer its health check. It waited
// a fixed budget of wall time for that (330 s, KEPLOY_AGENT_READY_TIMEOUT), so a
// host loaded enough to run the agent slowly lost a start that was making
// progress, and a start that had stopped making any was waited on for the
// whole budget before a retry began again from scratch. The agent now reports
// its start's progress (utils.ReportBootProgress), and keploy waits while that
// shows progress (waitForAgent, bootJudge, which judges once a bootLook by the
// latest of what it has seen):
//
//   - The agent's exit ends the wait at once, with the exit's reason, as before.
//   - A start the agent reports failed (utils.FailBoot, where the agent gives
//     up on it) ends it at once, with that failure and the code the agent exits
//     with for it, as its exit would: a retry would fail the same way. The
//     ERRORs the agent logs while it starts are no failure (it goes on from
//     many on purpose); they are detail for a start that stalls.
//   - Until the agent's process runs (a container the runtime is still creating
//     or starting, or one that is paused: docker inspect's status is not
//     running; a native agent
//     sudo has not run yet, its password prompt in front of it) nothing on
//     keploy's side shows progress of the start, and keploy waits for it however
//     long it takes, until `docker run` exits or the user stops keploy: a busy
//     daemon starts a container in its own time, and the user answers a prompt
//     in theirs.
//   - Once it runs (natively, once started, or once sudo has run it; in a
//     container, once the container runs), its first report is due within
//     bootSilence of keploy's own runnable time (stall.Process: keploy, starved
//     with the agent, accrues it no faster than it gets to run). An agent that
//     cannot report (one older than this keploy, or one whose report cannot be
//     read) is held to the same bound: its start is given up on once it has run
//     that long without one.
//   - From its first report on, the agent's own beats judge it: it beats once a
//     second of its own time, and a start that takes no step across
//     bootStallChecks of them, with no work in progress making progress (the
//     eBPF load, the proxy's start, which the agent sees through their thread)
//     and no prompt in front of the user, has stopped. An agent the host does
//     not run does not beat, and is not judged.
//   - One that says its server listens is due to answer its health check
//     within bootSilence of keploy's runnable time (a port the runtime does not
//     forward does not answer).
//
// A stopped start is pkg.ErrAgentNotReady, which a fresh start retries
// (pkg.RetryAgentSetup).

// bootStallChecks is the count of consecutive beats of a starting agent with no
// step of its start and no work in progress making progress after which its
// start is taken for one that has stopped: the teardown's count
// (utils.TeardownStallChecks).
const bootStallChecks = utils.TeardownStallChecks

// bootSilence is how long of keploy's own runnable time keploy waits, once the
// agent's process runs, for its first report, and, once it says its server
// listens, for its health check's answer. A variable for tests.
var bootSilence = 60 * time.Second

// bootLook is how often keploy looks at a starting agent, and judges its start.
var bootLook = time.Second

// bootVerdict is what one judgement of a starting agent decides.
type bootVerdict int

const (
	// bootWaiting: the start is making progress, or has not begun to run.
	bootWaiting bootVerdict = iota
	// bootStalled: the start has stopped making progress (ErrAgentNotReady).
	bootStalled
	// bootFailed: the agent reports its start failed (utils.FailBoot).
	bootFailed
)

// bootJudge decides, judgement by judgement, whether a starting agent's start
// is making progress (see the top of this file). Not safe for concurrent use.
type bootJudge struct {
	clk stall.Clock
	// awaiting: the agent's process runs, and has made no report, since
	// awaitingSince (runnable time).
	awaiting      bool
	awaitingSince time.Duration
	// ready: it says its server listens, since readySince (runnable time).
	ready      bool
	readySince time.Duration
	// seen: a report has been read; lastBeat and lastSeq are the last one's.
	seen              bool
	lastBeat, lastSeq uint64
	// quiet is the agent's beats since its start last took a step, with no
	// work in progress making progress and no prompt up.
	quiet uint64
}

func newBootJudge(clk stall.Clock) *bootJudge { return &bootJudge{clk: clk} }

// look decides from the latest report read of the agent's start (ok: one has
// been), and whether its process runs, and says why when it is not waiting.
// A judgement that finds the same report as the last is no new sign, and
// counts nothing: the agent's beats are its time.
func (j *bootJudge) look(p utils.BootProgress, ok, running bool) (bootVerdict, string) {
	now := j.clk.Runnable()
	if ok && p.Failed {
		return bootFailed, p.Failure
	}
	if !ok {
		if j.seen {
			return bootWaiting, ""
		}
		if !running {
			// The runtime has not run its process: no bound.
			j.awaiting = false
			return bootWaiting, ""
		}
		if !j.awaiting {
			j.awaiting, j.awaitingSince = true, now
		}
		if now-j.awaitingSince >= bootSilence {
			return bootStalled, fmt.Sprintf("its process has run for %s of keploy's runnable time without reporting its start, which keploy gives an agent that long to begin to", bootSilence)
		}
		return bootWaiting, ""
	}
	if !j.seen || p.Beat != j.lastBeat || p.Seq != j.lastSeq {
		if j.seen && p.Seq == j.lastSeq && !p.Busy && !p.Prompt && p.Beat > j.lastBeat {
			j.quiet += p.Beat - j.lastBeat
		} else {
			j.quiet = 0
		}
		j.seen, j.lastBeat, j.lastSeq = true, p.Beat, p.Seq
		if j.quiet >= bootStallChecks {
			return bootStalled, withBootErrors(fmt.Sprintf("its start took no step across %d of its beats, with no work in progress making progress", j.quiet), p)
		}
	}
	if p.Ready {
		if !j.ready {
			j.ready, j.readySince = true, now
		}
		if now-j.readySince >= bootSilence {
			return bootStalled, withBootErrors(fmt.Sprintf("it says its server listens, and its health check has not answered for %s of keploy's runnable time", bootSilence), p)
		}
	}
	return bootWaiting, ""
}

// withBootErrors is why, with the last ERROR the agent logged while starting,
// when it logged one: what a user looks for first in a stalled start.
func withBootErrors(why string, p utils.BootProgress) string {
	if p.Errors == 0 || p.Error == "" {
		return why
	}
	return fmt.Sprintf("%s (it logged %d ERROR(s) while starting, the last: %s)", why, p.Errors, p.Error)
}

// readBootFile reads the start's progress a native agent reports at path (next
// to its --teardown-progress-file); false while there is none.
func readBootFile(path string) func(context.Context) (utils.BootProgress, bool) {
	return func(context.Context) (utils.BootProgress, bool) {
		if path == "" {
			return utils.BootProgress{}, false
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return utils.BootProgress{}, false
		}
		p, err := utils.ParseBootProgress(b)
		return p, err == nil
	}
}

// readContainerBoot reads the start's progress the agent in container name
// reports (kdocker.AgentBootProgressFile), through the engine's `cp`, as its
// teardown's is read (readContainerProgress).
func readContainerBoot(cli *engineCLI, name string) func(context.Context) (utils.BootProgress, bool) {
	return func(ctx context.Context) (utils.BootProgress, bool) {
		out, err := cli.stdout(ctx, "cp", name+":"+kdocker.AgentBootProgressFile, "-")
		if err != nil {
			return utils.BootProgress{}, false
		}
		b, err := firstTarFile(out)
		if err != nil {
			return utils.BootProgress{}, false
		}
		p, err := utils.ParseBootProgress(b)
		return p, err == nil
	}
}

// containerRunning reports whether the engine runs container name: its status
// is running. False while it is still being created or started, while it is
// paused (its processes frozen: the engine says Running of a paused one too),
// restarting, or once it has exited, and when it cannot be told.
func containerRunning(cli *engineCLI, name string) func(context.Context) bool {
	return func(ctx context.Context) bool {
		out, err := cli.stdout(ctx, "inspect", "-f", "{{.State.Status}}", name)
		return err == nil && strings.TrimSpace(string(out)) == "running"
	}
}

// containerBootLook is how keploy looks at the start of the agent in container
// name: its report, read out of the container, and whether the engine runs
// it. A variable for tests.
var containerBootLook = func(logger *zap.Logger, name string) (read func(context.Context) (utils.BootProgress, bool), running func(context.Context) bool) {
	cli := newEngineCLI(logger)
	return readContainerBoot(cli, name), containerRunning(cli, name)
}

// alwaysRunning is a native agent's: its process runs once it has started.
func alwaysRunning(context.Context) bool { return true }

// bootLookResult is one look at a starting agent: whether its process runs,
// and, once it does, its report (ok: one was read).
type bootLookResult struct {
	p       utils.BootProgress
	ok      bool
	running bool
}

// daemonAnswerStall is how long of keploy's own runnable time keploy waits for
// the container engine's daemon to answer a question it must answer before the
// agent can start (its version, Setup): an answer that does not come is no
// progress. A variable for tests.
var daemonAnswerStall = 60 * time.Second

// errDaemonSilent is the cause of a question to the daemon that went
// unanswered for daemonAnswerStall.
var errDaemonSilent = errors.New("no answer from the container engine's daemon")

// boundClock is withRunnableBound's clock: nil, keploy's own runnable time
// (stall.Process). Tests set one they drive.
var boundClock stall.Clock

// withRunnableBound returns ctx cancelled with cause once keploy has had d of
// its own runnable time since now (stall.Process: a starved keploy accrues it
// no faster than it gets to run), and the func that releases it.
func withRunnableBound(ctx context.Context, d time.Duration, cause error) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	m := stall.NewMeter(boundClock, d)
	go func() {
		defer m.Stop()
		for !m.Expired() {
			select {
			case <-ctx.Done():
				return
			case <-m.Wait():
			}
		}
		cancel(cause)
	}()
	return ctx, func() { cancel(context.Canceled) }
}
