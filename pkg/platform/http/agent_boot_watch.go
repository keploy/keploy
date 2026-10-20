package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// AN AGENT'S START IS GIVEN UP ON ONLY ONCE IT HAS STOPPED MAKING PROGRESS.
//
// keploy waits for the agent it started to answer its health check. It waited
// a fixed budget of wall time for that (330 s, KEPLOY_AGENT_READY_TIMEOUT, both
// since removed), so a host loaded enough to run the agent slowly lost a start
// that was making progress, and a start that had stopped making any was waited
// on for the whole budget before a retry began again from scratch. The agent now reports
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
//   - While the agent's process does not run (a container the runtime is still
//     creating or starting, or one that is paused, before its start or during
//     it: docker inspect's status is not running; a native agent
//     sudo has not run yet, its password prompt in front of it) nothing on
//     keploy's side shows progress of the start, and keploy waits for it however
//     long it takes, until `docker run` (or the docker compose project) exits or
//     the user stops keploy: a busy daemon starts a container in its own time,
//     and the user answers a prompt in theirs.
//   - Once it runs (natively, once started, or once sudo has run it; in a
//     container, once the container runs), its first report is due within
//     bootSilence of keploy's own runnable time (stall.Process: keploy, starved
//     with the agent, accrues it no faster than it gets to run). An agent that
//     cannot report (one older than this keploy, or one whose report cannot be
//     read) is held to the same bound: its start is given up on once it has run
//     that long without one.
//   - From its first report on, the agent's own beats judge it. It beats once
//     a second of its own runnable time: an agent the host freezes, stops or
//     throttles beats only as fast as it gets to run, and one it does not run
//     does not beat, and is not judged. A start that takes no step across
//     bootStallChecks of them, with no work in progress making progress (the
//     eBPF load, the proxy's start, its listener's start and its DNS
//     servers' binds, which the agent sees through their threads) and no
//     prompt in front of the user, has stopped. The agent counts those beats
//     itself, as it saw each of them (BootProgress.Quiet): keploy reads only
//     the latest report, and charged every beat since its last read to it,
//     so a keploy that read seldom counted beats of work in progress as beats
//     without. An older agent's report, without the count, is judged by
//     keploy's own count as before. A stopped start says the last step it
//     took and the stages it had in progress, with their threads' state and
//     wait point.
//   - One that says its server listens is due to answer its health check
//     within bootSilence of keploy's runnable time (a port the runtime does not
//     forward does not answer).
//
// A stopped start is pkg.ErrAgentNotReady, which a fresh start retries
// (pkg.RetryAgentSetup).
//
// The waits for an agent keploy did not start itself follow its start the
// same way: docker compose starts it as a service of the project keploy wrote
// it into, and keploy record, test and mock wait for it to answer
// (WatchAgentStart, pkg.WatchAgentStart; WatchContainerAgentStart for a wait
// that knows only the agent's container). They end at once on a start the
// agent reports failed or one that has stopped making progress, and wait on
// however long a start that makes progress takes, with no budget of wall time.

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
	// bootExited: the agent's container has exited, and will not run again
	// unless something starts it again (watchStart's ended).
	bootExited
)

// bootJudge decides, judgement by judgement, whether a starting agent's start
// is making progress (see the top of this file). Not safe for concurrent use.
type bootJudge struct {
	clk stall.Clock
	// silence is bootSilence, as it was when the judge was made.
	silence time.Duration
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

// newBootJudge is a judge on clk, its bounds bootSilence as it is now: read on
// the caller's goroutine, never on the watch's.
func newBootJudge(clk stall.Clock) *bootJudge { return &bootJudge{clk: clk, silence: bootSilence} }

// look decides from the latest report read of the agent's start (ok: one has
// been), and whether its process runs, and says why when it is not waiting.
// A judgement that finds the same report as the last is no new sign, and
// counts nothing: the agent's beats are its time.
func (j *bootJudge) look(p utils.BootProgress, ok, running bool) (bootVerdict, string) {
	now := j.clk.Runnable()
	if ok && p.Failed {
		return bootFailed, p.Failure
	}
	if !running {
		// The runtime does not run its process (not yet, or no longer: a
		// container paused, or one compose starts again): no bound. A report
		// read while it ran says nothing of how long it has run since.
		j.awaiting, j.ready = false, false
		return bootWaiting, ""
	}
	if !ok {
		if j.seen {
			return bootWaiting, ""
		}
		if !j.awaiting {
			j.awaiting, j.awaitingSince = true, now
		}
		if now-j.awaitingSince >= j.silence {
			return bootStalled, fmt.Sprintf("its process has run for %s of keploy's runnable time without reporting its start, which keploy gives an agent that long to begin to", j.silence)
		}
		return bootWaiting, ""
	}
	if j.seen && p.Beat < j.lastBeat {
		// The agent's beats begin again: another run of it reports (one
		// compose starts again), and what was seen of the last says nothing
		// of this one.
		j.seen, j.quiet, j.ready = false, 0, false
	}
	if !j.seen || p.Beat != j.lastBeat || p.Seq != j.lastSeq {
		switch {
		case p.CountsQuiet:
			// The agent's own count, beat by beat.
			j.quiet = p.Quiet
		case j.seen && p.Seq == j.lastSeq && !p.Busy && !p.Prompt && p.Beat > j.lastBeat:
			// An older agent's: the beats since the last report read, as
			// this one says they went.
			j.quiet += p.Beat - j.lastBeat
		default:
			j.quiet = 0
		}
		j.seen, j.lastBeat, j.lastSeq = true, p.Beat, p.Seq
		if j.quiet >= bootStallChecks {
			return bootStalled, withBootErrors(stalledStart(j.quiet, p), p)
		}
	}
	if !p.Ready {
		// A report that says its server does not listen yet ends whatever
		// an earlier one said of it.
		j.ready = false
	}
	if p.Ready {
		if !j.ready {
			j.ready, j.readySince = true, now
		}
		if now-j.readySince >= j.silence {
			return bootStalled, withBootErrors(fmt.Sprintf("it says its server listens, and has not answered keploy for %s of keploy's runnable time", j.silence), p)
		}
	}
	return bootWaiting, ""
}

// stalledStart is why a start that took no step across quiet of its beats has
// stopped: with the last step it took, and the stages it had in progress, each
// with its thread's state and wait point, where the agent says them -- where a
// start that stalls was, which nothing else keploy shows says.
func stalledStart(quiet uint64, p utils.BootProgress) string {
	var b strings.Builder
	fmt.Fprintf(&b, "its start took no step across %d of its beats, with no work in progress making progress", quiet)
	if p.Step != "" {
		fmt.Fprintf(&b, "; its last step: %s", p.Step)
	}
	if len(p.Stages) == 0 {
		if p.CountsQuiet {
			b.WriteString("; no stage of it in progress")
		}
		return b.String()
	}
	b.WriteString("; in progress: ")
	for i, st := range p.Stages {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(st.Name)
		switch {
		case st.Unseen:
			b.WriteString(" (its progress cannot be seen here)")
		case st.State != "" && st.Wait != "":
			fmt.Fprintf(&b, " (its thread %s, in %s)", threadState(st.State), st.Wait)
		case st.State != "":
			fmt.Fprintf(&b, " (its thread %s)", threadState(st.State))
		}
	}
	return b.String()
}

// threadState says the kernel's state of a thread (its stat letter) in words.
func threadState(st string) string {
	switch st {
	case "R":
		return "running or waiting to run"
	case "S":
		return "sleeping"
	case "D":
		return "in an uninterruptible wait"
	case "T", "t":
		return "stopped"
	case "Z":
		return "a zombie"
	case "I":
		return "idle"
	default:
		return "in state " + st
	}
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
		knowContainer(name)
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
//
// A look the engine does not answer (an error, other than the container not
// being there) is no answer: it keeps what the last answer said, so that a
// daemon that errs now and then does not reset the judge's bounds look by
// look.
func containerRunning(cli *engineCLI, name string) func(context.Context) bool {
	return runningByStatus(func(ctx context.Context) ([]byte, error) {
		out, err := cli.stdout(ctx, "inspect", "-f", "{{.State.Status}}", name)
		if err == nil {
			knowContainer(name)
		}
		return out, err
	})
}

// runningByStatus is containerRunning by the answers of inspect, which asks
// the engine for the container's status.
func runningByStatus(inspect func(context.Context) ([]byte, error)) func(context.Context) bool {
	var last atomic.Bool
	return func(ctx context.Context) bool {
		out, err := inspect(ctx)
		switch {
		case err == nil:
			last.Store(strings.TrimSpace(string(out)) == "running")
		case engineSaysGone(out, err):
			last.Store(false)
		}
		return last.Load()
	}
}

// engineSaysGone reports whether the engine's CLI answered a question about
// a container (out, err) with its not being there: "No such container" or
// "No such object", as Docker and Podman say it, in either case, on stderr
// (which an *exec.ExitError carries) or on stdout.
func engineSaysGone(out []byte, err error) bool {
	said := string(out)
	if err != nil {
		said += "\n" + err.Error()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			said += "\n" + string(exit.Stderr)
		}
	}
	said = strings.ToLower(said)
	return strings.Contains(said, "no such container") || strings.Contains(said, "no such object")
}

// containerGone is containerExited's code for a container that is no longer
// there at all (removed: a docker run --rm agent that exited, a compose down):
// it has ended, with a code no one can read any more.
const containerGone = -1

// containerNeverThere is containerExited's code for an agent container the run
// of the application that was to create it ended without: the engine has no
// such container, and keploy never knew it to be there (see below).
const containerNeverThere = -2

// containerLeftNotRunning is containerExited's code for an agent container the
// run of the application ended with still there, and not running: created and
// never started, paused, or restarting, which the down after the run did not
// remove (it failed, or was cut by its bound). Nothing starts it once the run
// is over (see below).
const containerLeftNotRunning = -3

// AN AGENT'S CONTAINER THE ENGINE DOES NOT KNOW HAS ENDED ONLY ONCE IT WAS
// THERE, OR ONCE THE RUN THAT WAS TO CREATE IT IS OVER; ONE THAT DOES NOT RUN,
// ONCE IT HAS EXITED, OR ONCE THAT RUN IS OVER.
//
// The engine says the same of a container that is not there yet as of one
// that is not there any more: `docker inspect` of a container docker compose
// has not created, and of one removed, both answer "No such object". Taking
// the first for the second ended the release of the app (watchAgent's
// endOnExit) with "the keploy agent's container is gone (removed)" at its
// first look, for an agent whose container compose was still creating:
// enterprise's headless test and replay stack ask for the release as soon as
// they have started the project, and a loaded host takes more than a look to
// create its containers (atg-with-mocks, pipeline 12070). So a container the
// engine does not know has ended in two cases only:
//
//   - keploy has known it to be there since the Setup that named it
//     (nameAgentContainer): the engine answered a look at it (an inspect, a
//     `cp` out of it), or the agent answered from it (an answer at the
//     address that Setup gave it, which only its container serves). It is
//     gone, removed (containerGone).
//   - keploy never knew it to be there, and the run of the application that
//     was to create it is over (Run has returned: after docker compose's `up`
//     and the down after it returned, or before the app's run could begin).
//     Compose ended without creating it (the agent's image did not pull, a
//     hook wrote a file compose rejects, a service created before it hit a
//     name conflict), or removed it before a look got to it; either way
//     nothing will start it (containerNeverThere).
//
// The run's end ends, in the same way, the wait for a container the engine
// has that does not run and has not exited: compose created it and did not
// start it, or it is paused, or restarting, and the down after the run did
// not remove it (the down failed, or was cut by its bound). It is the run that
// starts it, and the run is over: nothing will start it now, whatever its
// state (containerLeftNotRunning). One that runs is not ended by the run's
// end: its agent can still answer.
//
// Until then it is one still to come, and the release waits for it, however
// long compose takes to create it, as any wait for an agent's start waits for
// a container the engine does not run yet. The run's end is what ends that
// wait, not the release's ctx: enterprise's callers (the headless test, the
// replay stack) start the run on a goroutine of its own and ask for the
// release with a ctx only a signal ends, which does not carry the run's end.
//
// A run is judged by what Run notes as it begins and as it ends. A release
// asked for before Run has begun the first run since the Setup waits, as for
// a container still to come: no run has ended. One asked for a run to begin
// again, after an earlier run of the same Setup ended without the container,
// is judged by that earlier run until Run has begun the next. No caller asks
// so: the headless test and the replay stack ask once, for the one run they
// start, and a replay that runs the project again for each test set asks only
// once the agent of that set's run has answered (its container known).

// namedContainer is what keploy has seen of an agent container a Setup named.
type namedContainer struct {
	// known: keploy has known it to be there since it was named.
	known atomic.Bool
	// runEnded: the latest run of the application since it was named (Run)
	// is over, and no other has begun since.
	runEnded atomic.Bool
}

// namedContainers is the agent containers this process's Setups named, by
// name. A name is a Setup's own (agentUtils.GenerateRandomContainerName), and
// its entry is new when a Setup names it, of any client, so what is known of
// one is of its container, not of an earlier container of that name. A name
// no Setup named (a look at another container) notes nothing.
//
// What is kept is at most one entry for each AgentClient that ran a docker
// Setup in this process, a few bytes each: the client forgets its last one
// when it names another (nameAgentContainer), and only its own, so a later
// client's entry of the same name is kept. Nothing tells a client's end, so
// the last entry of each is kept for the process's lifetime: a process that
// makes one client after another keeps one for each (enterprise's sandbox
// makes one for each command it runs, not for each session).
var namedContainers sync.Map // name -> *namedContainer

// containerAt is the agent container each agent address (host:port) is
// served from, as the Setup that named it set it up there: its entry, not its
// name, so that an answer of an earlier client's agent says nothing of a later
// client's container that took the same name. At most one for each
// AgentClient too.
var containerAt sync.Map // host:port -> *namedContainer

// namedContainerOf is what keploy has seen of container name; nil for one no
// Setup named.
func namedContainerOf(name string) *namedContainer {
	if v, ok := namedContainers.Load(name); ok {
		return v.(*namedContainer)
	}
	return nil
}

// knowContainer notes that container name is there.
func knowContainer(name string) {
	if c := namedContainerOf(name); c != nil {
		c.known.Store(true)
	}
}

// containerKnown reports whether container name has been known to be there
// since it was named.
func containerKnown(name string) bool {
	c := namedContainerOf(name)
	return c != nil && c.known.Load()
}

// runOfContainer notes that a run of the application of the agent in
// container name begins (ended false), or is over (ended true).
func runOfContainer(name string, ended bool) {
	if c := namedContainerOf(name); c != nil {
		c.runEnded.Store(ended)
	}
}

// containerRunEnded reports whether the latest run of the application of the
// agent in container name is over, with no other begun since.
func containerRunEnded(name string) bool {
	c := namedContainerOf(name)
	return c != nil && c.runEnded.Load()
}

// answeredAt notes an answer of the agent at host (host:port): its container,
// if keploy named the one served there, is there.
func answeredAt(host string) {
	if v, ok := containerAt.Load(host); ok {
		v.(*namedContainer).known.Store(true)
	}
}

// nameAgentContainer makes name the container of the agent this client sets
// up, at its agent address: nothing is known of it yet, no run of it has
// ended, and an answer at that address is an answer from it. What the client
// kept of the container it named before is forgotten.
func (a *AgentClient) nameAgentContainer(name string) {
	a.forgetAgentContainer()
	c := &namedContainer{}
	namedContainers.Store(name, c)
	a.mu.Lock()
	a.agentContainer, a.agentContainerEntry, a.agentContainerAt = name, c, ""
	if uri := a.conf.Agent.AgentURI; uri != "" {
		a.agentContainerAt = agentHost(uri)
	}
	at := a.agentContainerAt
	a.mu.Unlock()
	if at != "" {
		containerAt.Store(at, c)
	}
}

// forgetAgentContainer forgets what keploy kept of the agent container this
// client named last, and its address: the client sets up another, or none.
// Only what this client kept: an entry another client has made since, of the
// same name or at the same address, is that client's.
func (a *AgentClient) forgetAgentContainer() {
	a.mu.Lock()
	name, c, at := a.agentContainer, a.agentContainerEntry, a.agentContainerAt
	a.agentContainerEntry, a.agentContainerAt = nil, ""
	a.mu.Unlock()
	if c == nil {
		return
	}
	namedContainers.CompareAndDelete(name, c)
	if at != "" {
		containerAt.CompareAndDelete(at, c)
	}
}

// containerExited reports whether container name has ended (docker inspect's
// status is exited or dead: it will not run again unless something starts it
// again), and its exit code. For one the engine says is not there, it has
// ended once keploy has known it to be there (containerGone), or once the run
// that was to create it is over (containerNeverThere); before either, it is
// one the engine has not created yet, which has not ended. One that is there
// and does not run, and has not exited (created, paused, restarting), has
// ended once that run is over (containerLeftNotRunning): it is the run that
// starts it. One being removed is gone. One that runs has not ended.
func containerExited(cli *engineCLI, name string) func(context.Context) (int, bool) {
	return func(ctx context.Context) (int, bool) {
		out, err := cli.stdout(ctx, "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", name)
		if err != nil {
			if engineSaysGone(out, err) {
				// Known first: a container keploy saw, which the run's
				// down then removed, is gone, not one never there.
				switch {
				case containerKnown(name):
					return containerGone, true
				case containerRunEnded(name):
					return containerNeverThere, true
				}
			}
			return 0, false
		}
		knowContainer(name)
		f := strings.Fields(string(out))
		if len(f) >= 1 && f[0] == "removing" {
			// Removed as keploy looks: the next look finds it gone.
			return containerGone, true
		}
		if len(f) == 2 && (f[0] == "exited" || f[0] == "dead") {
			if code, err := strconv.Atoi(f[1]); err == nil {
				return code, true
			}
		}
		if len(f) >= 1 && f[0] != "running" && containerRunEnded(name) {
			cli.logger.Debug("the run of the application is over, and the keploy agent's container is there, not running; nothing will start it",
				zap.String("container", name), zap.String("status", f[0]))
			return containerLeftNotRunning, true
		}
		return 0, false
	}
}

// containerEnd is how keploy asks whether the agent's container name has
// ended (containerExited). A variable for tests.
var containerEnd = func(logger *zap.Logger, name string) func(context.Context) (int, bool) {
	return containerExited(newEngineCLI(logger), name)
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
	// ended: the process does not run, and has ended for good, with code
	// (watchStart's ended).
	ended bool
	code  int
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

// startJudgment is watchStart's word on a start it gave up on: its verdict
// (bootFailed or bootStalled), why, and the last report read.
type startJudgment struct {
	verdict bootVerdict
	why     string
	p       utils.BootProgress
}

// watchStart judges an agent's start once a bootLook, by the latest of what
// it has seen (bootJudge): whether its process runs (running), and its last
// report (read). It sends one judgment when the start has failed or stopped
// making progress, and nothing while it makes progress; it ends with ctx.
// Each look asks first whether the process runs, then, once it does, reads
// its report, one look at a time, off the caller's goroutine: a look into a
// container goes through the engine's CLI, which a busy daemon answers
// slowly, or not at all. A look that does not come back holds up no rule: its
// silence is judged as any other, and a look out for long is said once.
// inContainer says, for that line, whether the agent runs in a container.
// ended, when set, is asked of a process that does not run whether it has
// ended for good (its container exited), and with what code: the judgment is
// then bootExited, or bootFailed for an agent whose last report says its start
// failed. nil waits on a process that does not run however long it takes.
// untilReady ends the watch, with no judgment, once a report says the agent's
// server listens: for a wait that makes no health check of its own, which
// the ready rule (its health check due within bootSilence) would misjudge.
func watchStart(ctx context.Context, logger *zap.Logger, read func(context.Context) (utils.BootProgress, bool), running func(context.Context) bool, ended func(context.Context) (int, bool), inContainer, untilReady bool) <-chan startJudgment {
	out := make(chan startJudgment, 1)
	// What the watch's goroutine reads of the package's settings is read
	// here, on the caller's: the goroutine outlives the wait it serves.
	clk, release := stall.Process()
	judge := newBootJudge(clk)
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	go func() {
		defer release()
		runs := make(chan bool, 1)
		looks := make(chan bootLookResult, 1)
		lookNow := func() {
			go func() {
				r := running(ctx)
				select {
				case runs <- r:
				case <-ctx.Done(): // the wait is over
					return
				}
				l := bootLookResult{running: r}
				switch {
				case r:
					l.p, l.ok = read(ctx)
				case ended != nil:
					// Its last report, read out of the container that ended.
					if l.code, l.ended = ended(ctx); l.ended {
						l.p, l.ok = read(ctx)
					}
				}
				select {
				case looks <- l:
				case <-ctx.Done():
				}
			}()
		}
		var cur bootLookResult
		tick := time.NewTicker(look)
		defer tick.Stop()
		looking := false
		// lookSince is when (runnable time) the look out began; lookSaid,
		// that it has been said to be out for long.
		var lookSince time.Duration
		lookSaid := false
		// idle is the looks in a row that found the process not running:
		// a wait no rule bounds, said every `every` of them.
		idle := 0
		for {
			select {
			case <-ctx.Done():
				return
			case r := <-runs:
				cur.running = r
			case l := <-looks:
				looking = false
				cur.running = l.running
				if l.ok {
					cur.p, cur.ok = l.p, true
				}
				if untilReady && cur.ok && cur.p.Ready && !cur.p.Failed {
					return
				}
				if l.ended {
					if cur.ok && cur.p.Failed {
						out <- startJudgment{verdict: bootFailed, why: cur.p.Failure, p: cur.p}
						return
					}
					p := cur.p
					p.ExitCode = l.code
					why := fmt.Sprintf("its container exited with code %d", l.code)
					switch l.code {
					case containerGone:
						why = "its container is gone (removed)"
					case containerNeverThere:
						why = "the application's run ended without its container"
					case containerLeftNotRunning:
						why = "the application's run ended with its container not running"
					}
					out <- startJudgment{verdict: bootExited, why: why, p: p}
					return
				}
				if l.running {
					idle = 0
				} else {
					idle++
					if every > 0 && idle%every == 0 {
						if inContainer {
							logger.Info("still waiting for the keploy agent: the container engine does not run its container (not started yet, paused, or stopped), and keploy waits for it to run however long that takes",
								zap.Int("looks", idle))
						} else {
							logger.Info("still waiting for the keploy agent: its process has not begun to run (sudo may be asking for a password), and keploy waits for it however long that takes",
								zap.Int("looks", idle))
						}
					}
				}
			case <-tick.C:
				if !looking {
					looking, lookSince, lookSaid = true, clk.Runnable(), false
					lookNow()
				} else if !lookSaid && clk.Runnable()-lookSince >= silence {
					// Once its first report is in, a start is judged by the
					// agent's beats, which a look that does not come back
					// brings no more of: said once, so a wait that seems to
					// hang says what it waits on.
					lookSaid = true
					logger.Warn("keploy's look at the agent's start has not come back for a while; the container engine may not be answering, and keploy waits on",
						zap.Duration("runnable_time", silence), zap.Bool("in_a_container", inContainer))
				}
				if verdict, why := judge.look(cur.p, cur.ok, cur.running); verdict != bootWaiting {
					out <- startJudgment{verdict: verdict, why: why, p: cur.p}
					return
				}
			}
		}
	}()
	return out
}

// WatchContainerAgentStart watches the start of the keploy agent in container
// name as keploy watches one it starts itself (waitForAgent; see the top of
// this file), for a wait that did not start the agent: docker compose starts
// it as a service of the project keploy wrote it into. It sends one error once
// the start has failed (the agent says so: its failure, wrapping the cause its
// exit code names) or has stopped making progress (pkg.ErrAgentNotReady), and
// nothing while it makes progress, nor while the engine does not run the
// container: one docker compose has not started yet, one being created, one
// paused. ctx ends it. The wait ends with whatever else it waits on (the
// agent's health check answering, the project's exit).
func WatchContainerAgentStart(ctx context.Context, logger *zap.Logger, name string) <-chan error {
	read, running := containerBootLook(logger, name)
	return startErrors(ctx, watchStart(ctx, logger, read, running, nil, true, true), func(j startJudgment) error {
		return startFailure(j.p.ExitCode, j.why)
	})
}

// startFailure is the error of a start the agent reports failed (failure),
// wrapping the cause its exit code names, if any (agentExitCause).
func startFailure(code int, failure string) error {
	if cause := agentExitCause(code); cause != nil {
		return fmt.Errorf("%w: the keploy agent could not start: %s", cause, failure)
	}
	return fmt.Errorf("the keploy agent could not start: %s", failure)
}

// startErrors is judged's judgment as an error: failed's for a start the agent
// reports failed or whose container exited, pkg.ErrAgentNotReady for one that
// stopped making progress.
// One is sent, once; none once ctx ends.
func startErrors(ctx context.Context, judged <-chan startJudgment, failed func(startJudgment) error) <-chan error {
	out := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
		case j := <-judged:
			if j.verdict == bootFailed || j.verdict == bootExited {
				out <- failed(j)
				return
			}
			out <- fmt.Errorf("%w: %s", pkg.ErrAgentNotReady, j.why)
		}
	}()
	return out
}

// WatchAgentStart implements pkg.AgentStartWatcher: it watches the start of
// the agent this client's Setup set up (WatchContainerAgentStart, for one in a
// container; the report a native one it started writes, otherwise). A start
// the agent reports failed arms the exit code its cause calls for, as
// waitForAgent's does. An agent Setup did not set up (another process started
// it: a Kubernetes sidecar) is watched at its URI (watchAgentAt). nil, which
// never sends, for a client with no agent at all.
func (a *AgentClient) WatchAgentStart(ctx context.Context) <-chan error {
	return a.watchAgent(ctx, false)
}

// watchAgent is WatchAgentStart, ending also, with endOnExit, once the agent's
// container has exited, is gone once it was there, or was never there by the
// end of the run that was to create it (watchStart's ended, containerExited):
// a wait the agent's answer ends (the release of the app), which a container
// that stopped, or never came, will not give. It can begin before docker
// compose has created that container, and waits for it then, until the run
// of the application ends.
func (a *AgentClient) watchAgent(ctx context.Context, endOnExit bool) <-chan error {
	a.mu.Lock()
	container, run := a.agentContainer, a.run
	a.mu.Unlock()
	var read func(context.Context) (utils.BootProgress, bool)
	var ended func(context.Context) (int, bool)
	running := alwaysRunning
	switch {
	case container != "":
		read, running = containerBootLook(a.logger, container)
		if endOnExit {
			ended = containerEnd(a.logger, container)
		}
	case run != nil:
		read = readBootFile(run.bootReport)
		if run.bootPrompt {
			running = run.sudoRanAgent
		}
	case a.conf != nil && a.conf.Agent.AgentURI != "":
		return a.watchAgentAt(ctx, a.conf.Agent.AgentURI)
	default:
		return nil
	}
	inContainer := container != ""
	return startErrors(ctx, watchStart(ctx, a.logger, read, running, ended, inContainer, false), func(j startJudgment) error {
		if j.verdict == bootExited {
			// The agent's process is gone, whatever it had said of its
			// start: no failure of the start, unless its code names one.
			switch j.p.ExitCode {
			case containerGone:
				return errors.New("the keploy agent's container is gone (removed), and will not answer again")
			case containerNeverThere:
				return errors.New("the run of the application (docker compose up) ended without the keploy agent's container, which keploy never saw: compose did not create it (its output, or the run's error logged above, says why), or removed it before keploy saw it; the agent will not answer")
			case containerLeftNotRunning:
				return errors.New("the run of the application (docker compose up) ended with the keploy agent's container not running: compose created it and did not start it, or it is paused or restarting, and the down after the run did not remove it (docker inspect says its state; the run's error logged above, or compose's output, says why); nothing will start it now, and the agent will not answer")
			}
			if cause := agentExitCause(j.p.ExitCode); cause != nil {
				return fmt.Errorf("%w: the keploy agent's container exited with code %d", cause, j.p.ExitCode)
			}
			return fmt.Errorf("the keploy agent's container exited with code %d", j.p.ExitCode)
		}
		return a.agentReportedStartFailure(j.p.ExitCode, j.why, inContainer)
	})
}

var _ pkg.AgentStartWatcher = (*AgentClient)(nil)

// uriClock is watchAgentAt's clock: nil, keploy's own runnable time
// (stall.Process). Tests set one they drive.
var uriClock stall.Clock

// stillWaitingLooks is how many looks pass between the lines that say keploy
// still waits for an agent no rule bounds the wait for: one known only by its
// URI (watchAgentAt), and one whose container the engine does not run
// (watchStart). A variable for tests.
var stillWaitingLooks = 30

// agentDialWait is how long one look at an agent known only by its URI waits
// for its connection to be taken: the look's own length, not a bound on the
// wait, which counts what looks find (watchAgentAt).
const agentDialWait = time.Second

// watchAgentAt watches an agent this client did not set up, known only by its
// URI: another process started it (a Kubernetes sidecar, a pod's agent), and
// its report of its start's progress is on its own host, where keploy cannot
// read it. Once a bootLook, a look tells two cases apart:
//
//   - Nothing takes a connection at the URI (refused, or not taken): no agent
//     is there. Once nothing has for bootSilence of keploy's runnable time,
//     the wait is given up on (pkg.ErrAgentNotReady), saying so: a URI that
//     names no agent.
//   - Something takes it (the agent, whose health check answers OK only once
//     its start is over, or a port forward in front of it): keploy waits on,
//     however long the start takes, and says every stillWaitingLooks looks
//     that it still waits, and what it found.
//
// ctx ends it; so does the wait, once the agent's health check answers.
func (a *AgentClient) watchAgentAt(ctx context.Context, uri string) <-chan error {
	out := make(chan error, 1)
	addr := agentAddr(uri)
	if addr == "" {
		return nil
	}
	// Read here, on the caller's goroutine: the watch's outlives the wait.
	clk, release := uriClock, func() {}
	if clk == nil {
		clk, release = stall.Process()
	}
	look, silence, every, lookAt := bootLook, bootSilence, stillWaitingLooks, lookAtAgent
	go func() {
		defer release()
		tick := time.NewTicker(look)
		defer tick.Stop()
		refusing := false
		var refusedSince time.Duration
		looks := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			looks++
			found, taken := lookAt(ctx, addr)
			if ctx.Err() != nil {
				return
			}
			now := clk.Runnable()
			if taken {
				refusing = false
			} else {
				if !refusing {
					refusing, refusedSince = true, now
				}
				if now-refusedSince >= silence {
					out <- fmt.Errorf("%w: nothing answers at %s: no keploy agent is there, or it has stopped; no connection was taken for %s of keploy's runnable time (%s); start the agent there, or point keploy at the one that runs (its agent URI)",
						pkg.ErrAgentNotReady, uri, silence, found)
					return
				}
			}
			if looks%every == 0 {
				a.logger.Info("still waiting for the keploy agent at "+uri+": "+found,
					zap.Bool("something_answers", taken))
			}
		}
	}()
	return out
}

// agentAddr is the host:port an agent URI names; "" for one that names none.
func agentAddr(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() != "" {
		return u.Host
	}
	switch u.Scheme {
	case "https":
		return net.JoinHostPort(u.Hostname(), "443")
	default:
		return net.JoinHostPort(u.Hostname(), "80")
	}
}

// lookAtAgent is how watchAgentAt looks (lookAtAgentAddr). A variable for
// tests.
var lookAtAgent = lookAtAgentAddr

// lookAtAgentAddr is one look at an agent known only by its address: whether
// a connection to it is taken, and what the look found, in words.
func lookAtAgentAddr(ctx context.Context, addr string) (found string, taken bool) {
	d := net.Dialer{Timeout: agentDialWait}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Sprintf("nothing takes a connection there yet (%v)", err), false
	}
	_ = conn.Close()
	return "something takes connections there, and has not answered keploy yet: an agent another process started reports its start's steps only on its own host", true
}
