package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cerrdefs "github.com/containerd/errdefs"
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
// An agent known only by its URI (another process started it) shows its
// progress only in what its health check answers, and is judged by that
// (watchAgentAt): a wait on one that never answers healthy ends, saying what
// it answered.

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
	writeStages(&b, p)
	return b.String()
}

// startStepLine is what a start's wait says while the agent reports as it
// goes (its beats move) and its start takes no new step (watchStart): its
// last step, and the stages in progress at its last beat, each with whether
// it made progress and its thread's state, so that a start slowed or stuck in
// one stage names it while keploy waits. "" when the latest look did not find
// the process running with a report read, or found one that says the start is
// over (its server listens, or it failed): other lines, or none, say those.
func startStepLine(cur bootLookResult) string {
	if !cur.running || !cur.ok || cur.p.Ready || cur.p.Failed {
		return ""
	}
	var b strings.Builder
	b.WriteString("still waiting for the keploy agent's start: it runs, and has taken no new step for a while")
	if cur.p.Step != "" {
		fmt.Fprintf(&b, "; its last step: %s", cur.p.Step)
	}
	writeStages(&b, cur.p)
	return b.String()
}

// writeStages says the stages of a start in progress at its last beat
// (p.Stages): each one's name, whether it made progress since the beat
// before, and its thread's state and wait point where they can be read; or,
// for a report that counts its beats without progress, that none is.
func writeStages(b *strings.Builder, p utils.BootProgress) {
	if len(p.Stages) == 0 {
		if p.CountsQuiet {
			b.WriteString("; no stage of it in progress")
		}
		return
	}
	b.WriteString("; in progress: ")
	for i, st := range p.Stages {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(st.Name)
		var said []string
		switch {
		case st.Unseen:
			said = append(said, "its progress cannot be seen here")
		case st.Progressing:
			said = append(said, "making progress")
		}
		// A stage whose progress cannot be seen has no thread described
		// (utils.BootStage.Unseen).
		switch {
		case st.State != "" && st.Wait != "":
			said = append(said, fmt.Sprintf("its thread %s, in %s", threadState(st.State), st.Wait))
		case st.State != "":
			said = append(said, "its thread "+threadState(st.State))
		}
		if len(said) > 0 {
			fmt.Fprintf(b, " (%s)", strings.Join(said, "; "))
		}
	}
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

// readContainerBoot reads the start's progress the agent in container c
// reports (kdocker.AgentBootProgressFile), copied out of it through the
// engine's API, or its `cp` (containerLook). A copy out of it notes, on c,
// that it is there.
func readContainerBoot(look *containerLook, c *namedContainer) func(context.Context) (utils.BootProgress, bool) {
	return func(ctx context.Context) (utils.BootProgress, bool) {
		out, err := look.copyOut(ctx, c.name, kdocker.AgentBootProgressFile)
		if err != nil {
			return utils.BootProgress{}, false
		}
		c.know()
		b, err := firstTarFile(out)
		if err != nil {
			return utils.BootProgress{}, false
		}
		p, err := utils.ParseBootProgress(b)
		return p, err == nil
	}
}

// containerRunning reports whether the engine runs container c: its status
// is running. False while it is still being created or started, while it is
// paused (its processes frozen: the engine says Running of a paused one too),
// restarting, or once it has exited, and when it cannot be told. An inspect
// the engine answers (through its API, or its CLI: containerLook) notes, on
// c, that it is there.
//
// A look the engine does not answer (an error, other than the container not
// being there) is no answer: it keeps what the last answer said, so that a
// daemon that errs now and then does not reset the judge's bounds look by
// look.
func containerRunning(look *containerLook, c *namedContainer) func(context.Context) bool {
	return runningByStatus(func(ctx context.Context) ([]byte, error) {
		out, err := look.inspect(ctx, c.name, false)
		if err == nil {
			c.know()
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

// engineSaysGone reports whether the engine answered a question about a
// container (out, err) with its not being there: its API's not-found, or its
// CLI's "No such container" or "No such object", as Docker and Podman say it,
// in either case, on stderr (which an *exec.ExitError carries) or on stdout.
func engineSaysGone(out []byte, err error) bool {
	if err != nil && cerrdefs.IsNotFound(err) {
		return true
	}
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
// remove (it failed, or was cut by its bound). The run whose agent it was to
// serve is over (see below).
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
// not remove it (the down failed, or was cut by its bound). The release is
// of the application that run ran, and that run is over: there is nothing
// left for the agent to release, whatever its container does next
// (containerLeftNotRunning). A restarting one is no exception: it is the
// daemon that restarts a container (by its restart policy, which the agent's
// compose service does not set), not the run, and what it would restart is
// the agent of a run that is over. One that runs is not ended by the run's
// end: its agent can still answer, and the release ends with its answer.
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
//
// What is seen of a container is noted on the Setup's entry of it, never
// found by its name. The Setup that names the container makes the entry
// (namedContainer), and its client keeps it (AgentClient's agentContainer):
// that client's Run notes its run on it, and its watches look at the
// container through it (containerBootLook, containerEnd), as the answers at
// the address the Setup gave the agent do (containerAt). A name is two random
// bytes (agentUtils.GenerateRandomContainerName), and a process can make one
// client after another (enterprise's sandbox makes one for each command it
// runs), or have two at once, that draw the same name. By name, what one saw
// was noted of the other's container: an earlier client's run's end ended a
// later client's release, asked before that client's run began, with "the
// run ended without the keploy agent's container", and an earlier client's
// look at its own container, still there, made the later client's, not
// created yet, known, so that it read as "gone (removed)".

// namedContainer is what keploy has seen of the agent container a Setup named
// (nameAgentContainer), kept by the client whose Setup it was. The looks at a
// container need one; Run and the notes take a nil one, of a client that set
// up no container (a native agent's), and note nothing on it.
type namedContainer struct {
	// name is the container's name.
	name string
	// known: keploy has known it to be there since it was named.
	known atomic.Bool
	// runEnded: the latest run of the application since it was named (Run)
	// is over, and no other has begun since.
	runEnded atomic.Bool
}

// know notes that the container is there.
func (c *namedContainer) know() {
	if c != nil {
		c.known.Store(true)
	}
}

// isKnown reports whether the container has been known to be there since it
// was named.
func (c *namedContainer) isKnown() bool { return c != nil && c.known.Load() }

// noteRun notes that a run of the application of the agent in the container
// begins (ended false), or is over (ended true).
func (c *namedContainer) noteRun(ended bool) {
	if c != nil {
		c.runEnded.Store(ended)
	}
}

// runOver reports whether the latest run of the application of the agent in
// the container is over, with no other begun since.
func (c *namedContainer) runOver() bool { return c != nil && c.runEnded.Load() }

// containerAt is the agent container each agent address (host:port) is
// served from, as the Setup that named it set it up there: its entry, so that
// an answer there is noted on what its client keeps, and an answer of an
// earlier client's agent says nothing of a later client's container that took
// the same name. A client forgets its own when it sets up another agent
// (forgetAgentContainer), and only its own: one a later Setup set up at the
// same address is that Setup's. Nothing tells a client's end, so the last of
// each client that ran a docker Setup is kept for the process's lifetime, a
// few bytes each (enterprise's sandbox makes one client for each command it
// runs, not for each session).
var containerAt sync.Map // host:port -> *namedContainer

// answeredAt notes an answer of the agent at host (host:port): its container,
// if keploy named the one served there, is there.
func answeredAt(host string) {
	if v, ok := containerAt.Load(host); ok {
		v.(*namedContainer).know()
	}
}

// nameAgentContainer makes name the container of the agent this client sets
// up, at its agent address: a new entry of it, which the client keeps, with
// nothing known of it yet and no run of it ended, and an answer at that
// address is an answer from it. What the client kept of the container it
// named before is forgotten.
func (a *AgentClient) nameAgentContainer(name string) {
	a.forgetAgentContainer()
	c := &namedContainer{name: name}
	a.mu.Lock()
	a.agentContainer, a.agentContainerAt = c, ""
	if uri := a.conf.Agent.AgentURI; uri != "" {
		a.agentContainerAt = agentHost(uri)
	}
	at := a.agentContainerAt
	a.mu.Unlock()
	if at != "" {
		containerAt.Store(at, c)
	}
}

// forgetAgentContainer forgets the agent container this client named last,
// and its address: the client sets up another agent (Setup, which names that
// one's container, when it runs in one, after this). Only what this client
// kept: an entry another client's Setup has set up since at the same address
// is that client's.
func (a *AgentClient) forgetAgentContainer() {
	a.mu.Lock()
	c, at := a.agentContainer, a.agentContainerAt
	a.agentContainer, a.agentContainerAt = nil, ""
	a.mu.Unlock()
	if c != nil && at != "" {
		containerAt.CompareAndDelete(at, c)
	}
}

// containerNamed is this client's entry of container name: the one it named
// last (nameAgentContainer), or, for a name it did not name, an entry of its
// own, which no run, nor any other look, shares.
func (a *AgentClient) containerNamed(name string) *namedContainer {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c := a.agentContainer; c != nil && c.name == name {
		return c
	}
	return &namedContainer{name: name}
}

// containerExited reports whether container c has ended (docker inspect's
// status is exited or dead: it will not run again unless something starts it
// again), and its exit code. For one the engine says is not there, it has
// ended once keploy has known it to be there (containerGone), or once the run
// that was to create it is over (containerNeverThere); before either, it is
// one the engine has not created yet, which has not ended. One that is there
// and does not run, and has not exited (created, paused, restarting), has
// ended once that run is over (containerLeftNotRunning): the run it was to
// serve is over. One being removed is gone. One that runs has not ended.
// What it knows of c, and what it notes, is c's: the entry of the Setup that
// named it.
func containerExited(look *containerLook, c *namedContainer) func(context.Context) (int, bool) {
	return func(ctx context.Context) (int, bool) {
		out, err := look.inspect(ctx, c.name, true)
		if err != nil {
			if engineSaysGone(out, err) {
				// Known first: a container keploy saw, which the run's
				// down then removed, is gone, not one never there.
				switch {
				case c.isKnown():
					return containerGone, true
				case c.runOver():
					return containerNeverThere, true
				}
			}
			return 0, false
		}
		c.know()
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
		if len(f) >= 1 && f[0] != "running" && c.runOver() {
			look.cli.logger.Debug("the run of the application is over, and the keploy agent's container is there, not running: the release has no run left to release",
				zap.String("container", c.name), zap.String("status", f[0]))
			return containerLeftNotRunning, true
		}
		return 0, false
	}
}

// containerEnd is how keploy asks whether the agent's container c has ended
// (containerExited), through the engine's API api (nil: its CLI alone). A
// variable for tests.
var containerEnd = func(logger *zap.Logger, api containerAPI, c *namedContainer) func(context.Context) (int, bool) {
	return containerExited(newContainerLook(logger, api), c)
}

// containerBootLook is how keploy looks at the start of the agent in container
// c: its report, read out of the container, and whether the engine runs it,
// through the engine's API api (nil: its CLI alone). A variable for tests.
var containerBootLook = func(logger *zap.Logger, api containerAPI, c *namedContainer) (read func(context.Context) (utils.BootProgress, bool), running func(context.Context) bool) {
	look := newContainerLook(logger, api)
	return readContainerBoot(look, c), containerRunning(look, c)
}

// alwaysRunning is a native agent's: its process runs once it has started.
func alwaysRunning(context.Context) bool { return true }

// bootLookResult is one look at a starting agent: whether its process runs,
// and, once it does, its report (ok: one was read).
type bootLookResult struct {
	p       utils.BootProgress
	ok      bool
	running bool
	// whole: the look is done. A look says whether the process runs first,
	// on its own (whole false), and then all it found.
	whole bool
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

// startClock is watchStart's clock: nil, keploy's own runnable time
// (stall.Process). Tests set one they drive.
var startClock stall.Clock

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
// Its channel is closed once it has ended, and its look with it: the look
// out as it ends is cancelled, and waited for until the engine command it
// runs has been killed and waited for, unless keploy may not signal that
// command, which is left to end by itself (containerEngineCommand,
// runEngine).
// Each look asks first whether the process runs, then, once it does, reads
// its report, one look at a time, off the caller's goroutine: a look into a
// container goes through the engine's CLI, which a busy daemon answers
// slowly, or not at all. A look that does not come back holds up no rule: its
// silence is judged as any other. A wait that goes on is said every
// stillWaitingLooks ticks since the agent's last new report (or since the
// watch began), with what the latest look found: the look has not said
// whether the process runs, it does not run, or it runs with no report, or
// no new one, read. While its reports move (it beats) but its start takes no
// new step, the wait is said every stepWaitLooks ticks since its last step
// up to stepWaitBackOff of them, then every stepWaitLaterLooks
// (stepLineDue), naming that step and the stages in progress
// (startStepLine): a start slowed or stuck in one stage would otherwise be
// waited on in silence, as long as that stage makes progress. Such a line is
// said only while its reports still move: a new one read within the last
// stepWaitLooks ticks, and since its last step. An agent whose reports stop
// moving (a frozen process, or a stuck reporter) has no step line once
// stepWaitLooks ticks pass with no new report, only the lines that say
// keploy has read no new one. A look out past the silence bound is said once,
// at WARN, in place of the next of those lines, with what it is out on and
// what keploy does next (lookOutWarning). inContainer says, for those lines,
// whether the agent runs in a container.
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
	clk, release := startClock, func() {}
	if clk == nil {
		clk, release = stall.Process()
	}
	judge := newBootJudge(clk)
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	stepEvery, stepUntil, stepLater := stepWaitLooks, stepWaitBackOff, stepWaitLaterLooks
	go func() {
		// The look out ends with the watch, however the watch ends, and the
		// watch ends only once it has: a look's funcs return only once the
		// engine command each ran on lookCtx has been killed, with the
		// processes still in its process group that keploy may signal, and
		// waited for, unless keploy may not signal it (containerEngineCommand,
		// runEngine). Its channel closes then.
		lookCtx, endLook := context.WithCancel(ctx)
		var lookOut sync.WaitGroup
		defer func() {
			endLook()
			lookOut.Wait()
			release()
			close(out)
		}()
		// A look's two words come on the one channel, in the order it
		// says them: one look is out at a time, so neither is ever taken
		// for another look's.
		looks := make(chan bootLookResult, 2)
		lookNow := func() {
			lookOut.Go(func() {
				r := running(lookCtx)
				select {
				case looks <- bootLookResult{running: r}:
				case <-lookCtx.Done(): // the watch is over
					return
				}
				l := bootLookResult{running: r, whole: true}
				switch {
				case r:
					l.p, l.ok = read(lookCtx)
				case ended != nil:
					// Its last report, read out of the container that ended.
					if l.code, l.ended = ended(lookCtx); l.ended {
						l.p, l.ok = read(lookCtx)
					}
				}
				select {
				case looks <- l:
				case <-lookCtx.Done():
				}
			})
		}
		var cur bootLookResult
		tick := time.NewTicker(look)
		defer tick.Stop()
		looking := false
		// lookSince is when (runnable time) the look out began; lookSaid,
		// that it has been said, at WARN, to be out for long.
		var lookSince time.Duration
		lookSaid := false
		// asking: the look out has not said yet whether the process runs.
		// asked: the ticks that look has been out, the one it began at
		// included.
		asking, asked := false, 0
		// waited is the ticks since the agent last made progress (a report
		// of its start keploy had not read before), or since the watch
		// began. The wait is said every `every` of them, with what the
		// latest look found, however long each look takes to come back.
		waited := 0
		// stepWaited is the ticks since the agent's start last took a step
		// (a report whose step count keploy had not read before), or since
		// the watch began. It runs ahead of waited only once a report has
		// moved by its beats alone. The start is said by startStepLine only
		// while it beats: a report moved since its last step (waited <
		// stepWaited) and within the last stepEvery ticks (waited <
		// stepEvery). A report that does not move at all, or has stopped
		// moving for stepEvery ticks, is said by startWaitLine alone: its
		// last report's stages say nothing of what it does now.
		stepWaited := 0
		for {
			select {
			case <-ctx.Done():
				return
			case l := <-looks:
				cur.running, asking = l.running, false
				if !l.whole {
					// Whether it runs, while the look reads on.
					continue
				}
				looking = false
				if l.ok {
					if !cur.ok || l.p.Beat != cur.p.Beat || l.p.Seq != cur.p.Seq {
						waited = 0
					}
					if !cur.ok || l.p.Seq != cur.p.Seq {
						stepWaited = 0
					}
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
			case <-tick.C:
				if !looking {
					looking, lookSince, lookSaid = true, clk.Runnable(), false
					asking, asked = true, 0
					lookNow()
				}
				asked++
				waited++
				stepWaited++
				lineDue := every > 0 && waited%every == 0
				if !lineDue && stepLineDue(stepWaited, stepEvery, stepUntil, stepLater) && waited < stepWaited && waited < stepEvery {
					if line := startStepLine(cur); line != "" {
						logger.Info(line, zap.Duration("waited", time.Duration(stepWaited)*look))
					}
				}
				if lineDue {
					if line := startWaitLine(cur, asking && asked >= every, inContainer, silence); line != "" {
						if !lookSaid && clk.Runnable()-lookSince >= silence {
							// A look out past the silence bound is said
							// once at WARN, in place of the line: the
							// agent's beats, once its first report is in,
							// are what judge its start, and a look that
							// does not come back brings no more of them.
							lookSaid = true
							logger.Warn(lookOutWarning(cur, asking, ended != nil, inContainer, silence),
								zap.Duration("runnable_time", silence), zap.Bool("in_a_container", inContainer), zap.Duration("waited", time.Duration(waited)*look))
						} else {
							logger.Info(line, zap.Duration("waited", time.Duration(waited)*look))
						}
					}
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

// startWaitLine is what a start's wait that has gone on says: what the latest
// look found (cur). notAnswered: the look out has not said whether the
// process runs for a whole line's worth of ticks (a container's look asks the
// engine, which may not answer while it is still creating or starting the
// container); the line then says what keploy does next (startWaitNext;
// silence: the silence bound). "" once a report says the start failed: the
// watch ends the wait with that failure at the same tick (bootJudge.look
// judges a failure first). "" too when the latest look found the process
// running and its report says its server listens (the ready rule bounds that
// wait), unless notAnswered.
func startWaitLine(cur bootLookResult, notAnswered, inContainer bool, silence time.Duration) string {
	what := "its process"
	if inContainer {
		what = "its container"
	}
	switch {
	case cur.ok && cur.p.Failed:
		return ""
	case notAnswered && inContainer:
		return "waiting on the container engine: it has not answered keploy's look at the keploy agent's container, as an engine still creating or starting a container may not until that is done; " + startWaitNext(cur, inContainer, silence)
	case notAnswered:
		return "waiting on the keploy agent's start: keploy's look at it has not come back; " + startWaitNext(cur, inContainer, silence)
	case !cur.running && inContainer:
		return "still waiting for the keploy agent: the container engine does not run its container (not started yet, paused, or stopped), and keploy waits for it to run however long that takes"
	case !cur.running:
		return "still waiting for the keploy agent: its process has not begun to run (sudo may be asking for a password), and keploy waits for it however long that takes"
	case !cur.ok:
		return "still waiting for the keploy agent: " + what + " runs, and keploy has read no report of its start from it yet"
	case cur.p.Ready:
		return ""
	default:
		return "still waiting for the keploy agent: " + what + " runs, and keploy has read no new report of its start since its last"
	}
}

// lookOutWarning is the silence bound's warning of a look out (watchStart):
// what the look has not come back with, by what it has said so far, then what
// keploy does next (startWaitNext). asking: it has not said whether the
// process runs. Else it has (cur.running): for one that runs, it reads the
// report of its start; for a container that does not run, it asks whether
// the container has ended, and reads its last report if it has (ending: the
// watch asks that); else it has nothing left to say. (The look's result may
// also be in already and not yet taken, when a tick that came at the same
// time is taken first; the warning then says what the look had said before
// that result.) A container's look goes through the engine's CLI, a native
// agent's through keploy's own reads of its process and its report's file:
// only a container's names the engine.
func lookOutWarning(cur bootLookResult, asking, ending, inContainer bool, silence time.Duration) string {
	var out string
	switch {
	case asking && inContainer:
		out = "the container engine has not answered whether it runs the agent's container"
	case asking:
		out = "keploy has not yet found out whether the agent's process runs"
	case cur.running && inContainer:
		out = "the agent's container runs, and the container engine has not yet given keploy the report of its start from inside it"
	case cur.running:
		out = "the agent's process runs, and keploy's read of the report of its start has not come back"
	case ending && inContainer:
		out = "the container engine does not run the agent's container, and has not yet said whether the container has ended or, if it has, given keploy its last report from inside it"
	case inContainer:
		out = "the container engine does not run the agent's container"
	default:
		out = "the agent's process does not run"
	}
	return "keploy's look at the agent's start has not come back for a while: " + out + "; " + startWaitNext(cur, inContainer, silence)
}

// startWaitNext is what keploy does next in a start's wait while a look is
// out, by what the latest look found (cur), as bootJudge.look judges the wait
// with no new report: no time limit while the process is not known to run; an
// agent found running with no report read is given up on once it has been
// found running for the silence bound, and one that says its server listens
// once it has not answered keploy for that bound; otherwise only a new report
// can show that its start has stalled, and keploy reads none until the look
// comes back. (A report that says the start failed has no line at all:
// startWaitLine.)
func startWaitNext(cur bootLookResult, inContainer bool, silence time.Duration) string {
	what := "process"
	if inContainer {
		what = "container"
	}
	switch {
	case !cur.running:
		return "keploy sets no time limit on the wait while the agent's " + what + " is not known to run, and waits on"
	case !cur.ok:
		return fmt.Sprintf("keploy gives up on the start once it has found the agent's %s running, with no report of its start read, for %s of keploy's runnable time", what, silence)
	case cur.p.Ready:
		return fmt.Sprintf("keploy gives up on the start once the agent, which says its server listens, has not answered keploy for %s of keploy's runnable time", silence)
	default:
		return "keploy waits on: the agent has reported, so only a new report from it can show that its start has stalled, and keploy reads none until this look comes back"
	}
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
// agent's health check answering, the project's exit). It knows the container
// by its name only, which another client's Setup may have drawn too: what its
// looks see is noted on an entry of its own, never on a client's.
func WatchContainerAgentStart(ctx context.Context, logger *zap.Logger, name string) <-chan error {
	api, closeAPI := envContainerAPI(logger)
	read, running := containerBootLook(logger, api, &namedContainer{name: name})
	errs, over := startErrors(ctx, watchStart(ctx, logger, read, running, nil, true, true), func(j startJudgment) error {
		return startFailure(j.p.ExitCode, j.why)
	})
	// The looks read through api until the watch is over, and its look
	// with it (watchStart): its client is closed then.
	go func() {
		<-over
		closeAPI()
	}()
	return errs
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
// One is sent, once; none once ctx ends. over is closed once the watch that
// judges (watchStart, on ctx) has ended: judged is closed.
func startErrors(ctx context.Context, judged <-chan startJudgment, failed func(startJudgment) error) (errs <-chan error, over <-chan struct{}) {
	out, ends := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(ends)
		select {
		case <-ctx.Done():
		case j, ok := <-judged:
			switch {
			case !ok:
				// The watch ended with no judgment (untilReady).
				return
			case j.verdict == bootFailed || j.verdict == bootExited:
				out <- failed(j)
			default:
				out <- fmt.Errorf("%w: %s", pkg.ErrAgentNotReady, j.why)
			}
		}
		// The watch ends with ctx too, and has ended, or is ending, once
		// it has judged.
		for range judged {
		}
	}()
	return out, ends
}

// WatchAgentStart implements pkg.AgentStartWatcher: it watches the start of
// the agent this client's Setup set up (WatchContainerAgentStart, for one in a
// container; the report a native one it started writes, otherwise). A start
// the agent reports failed arms the exit code its cause calls for, as
// waitForAgent's does. An agent Setup did not set up (another process started
// it: a Kubernetes sidecar) is watched at its URI (watchAgentAt). nil, which
// never sends, for a client with no agent at all.
func (a *AgentClient) WatchAgentStart(ctx context.Context) <-chan error {
	errs, _ := a.watchAgent(ctx, false)
	return errs
}

// watchAgent is WatchAgentStart, ending also, with endOnExit, once the agent's
// container has exited, is gone once it was there, or was never there by the
// end of the run that was to create it (watchStart's ended, containerExited):
// a wait the agent's answer ends (the release of the app), which a container
// that stopped, or never came, will not give. It can begin before docker
// compose has created that container, and waits for it then, until the run
// of the application ends. It watches the container this client's Setup
// named, through the entry the client keeps of it, not by its name: what it
// sees is noted there, and what it asks (known, the run over) is asked there.
// over is closed once the watch has ended, with ctx or its verdict, and its
// look with it (watchStart, watchAgentAt). Both are nil where there is no
// watch: a client with no agent.
func (a *AgentClient) watchAgent(ctx context.Context, endOnExit bool) (errs <-chan error, over <-chan struct{}) {
	a.mu.Lock()
	container, run := a.agentContainer, a.run
	a.mu.Unlock()
	var read func(context.Context) (utils.BootProgress, bool)
	var ended func(context.Context) (int, bool)
	running := alwaysRunning
	switch {
	case container != nil:
		read, running = containerBootLook(a.logger, a.containerAPI(), container)
		if endOnExit {
			ended = containerEnd(a.logger, a.containerAPI(), container)
		}
	case run != nil:
		read = readBootFile(run.bootReport)
		if run.bootPrompt {
			running = run.sudoRanAgent
		}
	case a.conf != nil && a.conf.Agent.AgentURI != "":
		return a.watchAgentAt(ctx, a.conf.Agent.AgentURI)
	default:
		return nil, nil
	}
	inContainer := container != nil
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
				return errors.New("the run of the application (docker compose up) ended with the keploy agent's container not running: compose created it and did not start it, or it is paused or restarting, and the down after the run did not remove it (docker inspect says its state; the run's error logged above, or compose's output, says why); the run is over, so its agent has no run left to release")
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
// still waits for an agent known only by its URI (watchAgentAt), and how many
// ticks of a start's wait with no new report from the agent pass between
// those that say what the wait is on (watchStart). A variable for tests.
var stillWaitingLooks = 30

// stepWaitLooks is how many ticks of a start's wait, while the agent reports
// as it goes and its start takes no new step, pass between the lines that
// name its last step and the stages in progress (watchStart, startStepLine).
// A start takes a step every few seconds of a healthy host's time, its CA's
// install the longest: ten ticks without one is a start slowed, or stuck, in
// one stage, worth saying long before the wait is over. It is also how
// recent the agent's last new report must be for that line to be said: an
// agent beats once a second of its own runnable time, so ten ticks with no
// new report is an agent that has stopped beating, or that the host has all
// but stopped running, not one that runs slowly. A variable for tests; never
// 0.
var stepWaitLooks = 10

// stepWaitBackOff is how many ticks with no new step the lines of
// stepWaitLooks cover; from there on they come every stepWaitLaterLooks
// ticks (stepLineDue). The first minute of a stage that does not end says
// where it is often; a stage that goes on for minutes is said less often, so
// that a three-minute stall in one stage says about ten lines, not
// eighteen. Variables for tests; stepWaitLaterLooks never 0.
var stepWaitBackOff, stepWaitLaterLooks = 60, 30

// stepLineDue says whether the line for a start that beats with no new step
// is due at the tick that makes n ticks since its last step: every `every`
// ticks up to `until` of them, then every `later` ticks from there.
func stepLineDue(n, every, until, later int) bool {
	if n <= until {
		return n%every == 0
	}
	return (n-until)%later == 0
}

// agentLookFirstWait is how long the first look at an agent known only by its
// URI waits for its health check's answer, and agentLookMaxWait how long a
// run of looks that got none can grow that wait to: twice the last after a
// look that ran out of time (nextAgentLookWait), as keploy's own health check
// does (pkg.AgentHealthTicker), so an agent behind a slow path (a port
// forward over a far link) is found answering, not taken for one that holds
// the connection without an answer. A look's wait is its own length, not a
// bound on the watch's wait, which counts what the looks find. The first is a
// variable for tests.
var agentLookFirstWait = time.Second

const agentLookMaxWait = 30 * time.Second

// nextAgentLookWait is the wait of the look after one that waited wait:
// twice that, up to agentLookMaxWait, when it ran out of time (held), and the
// same otherwise.
func nextAgentLookWait(wait time.Duration, held bool) time.Duration {
	if !held {
		return wait
	}
	if wait >= agentLookMaxWait/2 {
		return agentLookMaxWait
	}
	return 2 * wait
}

// agentLookKind is what one look at an agent known only by its URI found.
type agentLookKind int

const (
	// agentRefused: nothing took the look's connection (refused, or not
	// taken within the look's wait).
	agentRefused agentLookKind = iota
	// agentClosed: something took the connection and closed it, or broke
	// it, with no answer to the health check: a forward in front of a port
	// nothing listens on.
	agentClosed
	// agentHeld: something took the connection and held it, with no answer
	// to the health check, for the whole of the look's wait: an agent that
	// is frozen or stopped (the kernel takes connections for a process that
	// does not run), or a server that hangs.
	agentHeld
	// agentAnswered: an answer to the health check other than healthy.
	agentAnswered
	// agentHealthy: the health check answered healthy (200).
	agentHealthy
)

// agentLook is what one look at an agent known only by its URI found
// (lookAtAgentHealth).
type agentLook struct {
	kind agentLookKind
	// status is the answer's HTTP status, for agentAnswered and
	// agentHealthy.
	status int
	// found says what the look found, in words, with its detail: the error
	// of a look that got no answer, the status and the start of the body of
	// one that did.
	found string
}

// agentState is what a look found, as far as it can be a step of the
// agent's start: the look's kind, and its answer's status. A look that finds
// a state no look of the wait found before is the progress an agent known
// only by its URI shows from outside its host (a forward that comes up, a
// server that begins to answer, an answer of a status it never gave); one
// that finds a state found before shows none, whatever its detail (a port of
// the connection, a time in the body), and however many other states came
// in between: an agent whose answers go back and forth (503, 502, 503...)
// makes no progress. A wait has only so many states to find, so steps cannot
// put off its end for good.
type agentState struct {
	kind   agentLookKind
	status int
}

func (l agentLook) state() agentState { return agentState{kind: l.kind, status: l.status} }

// agentBodyShown is how much of an answer's body a look keeps to say it.
const agentBodyShown = 200

// watchAgentAt watches an agent this client did not set up, known only by its
// URI: another process started it (a Kubernetes sidecar, a pod's agent), and
// its report of its start's progress is on its own host, where keploy cannot
// read it. Once a bootLook, a look asks the agent's health check
// (lookAtAgent), and finds one of: nothing takes a connection there; something
// takes it and closes it, or holds it, with no answer; an answer other than
// healthy; a healthy one (agentLookKind).
//
// What the looks find is the agent's progress, as far as it shows from
// outside its host: a look that finds what no look of this wait found before
// (another kind, another status: agentState) is a step. The wait is given up
// on (pkg.ErrAgentNotReady) once the looks have found no new state, and the
// last is not healthy, for bootSilence of keploy's runnable time: an agent
// whose start makes progress is waited for however long that takes, as long
// as each step comes within that bound, and one that never answers healthy (a
// forward in front of nothing, an agent frozen, one that answers but never
// healthy, or one whose answers go back and forth) is given up on, saying
// why, as is one that stops (a look finds nothing takes a connection where
// something took one before). keploy's runnable time accrues no faster than
// keploy gets to run, so a box too loaded to run keploy does not run out the
// bound either. A healthy answer is never given up on: the wait ends on it
// (its own health check finds it).
//
// It says every stillWaitingLooks looks that it still waits, and what it
// found. Why it gives up is said once, by the error it sends: each caller
// ends its run with that error, which says it.
//
// ctx ends it; so does the wait, once the agent's health check answers.
// over is closed once it has ended, its look with it (each look is on the
// watch's own goroutine). A URI its health check cannot be asked at (one
// that names no host, or no scheme keploy can send on) is looked at all the
// same: nothing takes a connection there, and the wait is given up on, saying
// why, as for one where no agent is.
func (a *AgentClient) watchAgentAt(ctx context.Context, uri string) (errs <-chan error, over <-chan struct{}) {
	out, ends := make(chan error, 1), make(chan struct{})
	// Read here, on the caller's goroutine: the watch's outlives the wait.
	clk, release := uriClock, func() {}
	if clk == nil {
		clk, release = stall.Process()
	}
	look, silence, every, lookAt, wait := bootLook, bootSilence, stillWaitingLooks, lookAtAgent, agentLookFirstWait
	// Each look on a connection of its own, as keploy's own health check
	// sends it (the default transport, with its proxy settings), and in a
	// scheme the embedder registered on it (k8s-proxy's keploy-agent://)
	// through the round tripper it registered (ownConnections).
	client := &http.Client{Transport: ownConnections(http.DefaultTransport)}
	healthURL, shown := uri+"/health", pkg.ShownAgentURI(uri)
	go func() {
		defer close(ends)
		defer release()
		tick := time.NewTicker(look)
		defer tick.Stop()
		var (
			// seen: what the looks found (agentSeen).
			seen  = agentSeen{states: map[agentState]bool{}}
			since time.Duration
			looks int
		)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			looks++
			l := lookAt(ctx, client, healthURL, wait)
			if ctx.Err() != nil {
				return
			}
			wait = nextAgentLookWait(wait, l.kind == agentHeld)
			now := clk.Runnable()
			if !seen.states[l.state()] {
				since = now
			}
			seen.note(l)
			if l.kind != agentHealthy && now-since >= silence {
				out <- seen.givenUp(shown, silence)
				return
			}
			if looks%every == 0 {
				a.logger.Info("still waiting for the keploy agent at "+shown+": "+l.found,
					zap.Bool("something_answers", l.kind != agentRefused))
			}
		}
	}()
	return out, ends
}

// agentSeen is what the looks at an agent known only by its URI have found:
// the last look's, the last that found something take its connection, and
// the last that got an answer; whether any found those, or a healthy answer;
// and each state any found (agentState).
type agentSeen struct {
	last, lastTaken, lastAnswer agentLook
	taken, answered, healthy    bool
	states                      map[agentState]bool
}

// note adds the look l to what has been seen; s.states must not be nil.
func (s *agentSeen) note(l agentLook) {
	s.states[l.state()] = true
	s.last = l
	if l.kind != agentRefused {
		s.taken, s.lastTaken = true, l
	}
	if l.kind == agentAnswered || l.kind == agentHealthy {
		s.answered, s.lastAnswer = true, l
	}
	if l.kind == agentHealthy {
		s.healthy = true
	}
}

// givenUp is why the wait for the agent at uri is given up on, the looks
// having found no new state for silence of keploy's runnable time.
func (s *agentSeen) givenUp(uri string, silence time.Duration) error {
	var why string
	switch {
	case s.last.kind == agentRefused && !s.taken:
		return fmt.Errorf("%w: nothing answers at %s: no keploy agent is there, or it has stopped; no connection was taken for %s of keploy's runnable time (%s); start the agent there, or point keploy at the one that runs (its agent URI)",
			pkg.ErrAgentNotReady, uri, silence, s.last.found)
	case s.last.kind == agentRefused:
		why = fmt.Sprintf("the keploy agent at %s has stopped: something took connections there, and none was taken for %s of keploy's runnable time since (%s); the last look that found something there: %s",
			uri, silence, s.last.found, s.lastTaken.found)
	default:
		did := "never answered its health check healthy"
		if s.healthy {
			did = "no longer answers its health check healthy"
		}
		why = fmt.Sprintf("the keploy agent at %s %s: something takes connections there, and keploy found nothing new there for %s of keploy's runnable time: %s",
			uri, did, silence, s.last.found)
	}
	// The last answer, unless the reason said it already: the last look
	// that found something take its connection got it.
	if s.answered && s.lastAnswer != s.lastTaken {
		why += "; its last answer: " + s.lastAnswer.found
	}
	return fmt.Errorf("%w: %s", pkg.ErrAgentNotReady, why)
}

// lookAtAgent is how watchAgentAt looks (lookAtAgentHealth). A variable for
// tests.
var lookAtAgent = lookAtAgentHealth

// lookAtAgentHealth is one look at an agent known only by its URI: a request
// to its health check (healthURL) on client, waiting wait for its answer, and
// what it found (agentLook). A connection is taken once the request's dial
// connects (httptrace), whatever round tripper sends it.
func lookAtAgentHealth(ctx context.Context, client *http.Client, healthURL string, wait time.Duration) agentLook {
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				connected.Store(true)
			}
		},
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	}
	lookCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, trace), wait)
	defer cancel()
	req, err := http.NewRequestWithContext(lookCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return agentLook{kind: agentRefused, found: fmt.Sprintf("its health check cannot be asked (%v)", pkg.ShownAgentURIError(err))}
	}
	res, err := client.Do(req)
	switch {
	case err != nil && !connected.Load():
		return agentLook{kind: agentRefused, found: fmt.Sprintf("nothing takes a connection there yet (%v)", err)}
	case err != nil && errors.Is(lookCtx.Err(), context.DeadlineExceeded):
		return agentLook{kind: agentHeld, found: fmt.Sprintf("something takes connections there and holds them with no answer to its health check for %s", wait)}
	case err != nil:
		return agentLook{kind: agentClosed, found: fmt.Sprintf("something takes connections there and closes them with no answer to its health check (%v)", err)}
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, agentBodyShown))
	if res.StatusCode == http.StatusOK {
		return agentLook{kind: agentHealthy, status: res.StatusCode, found: "its health check answers healthy"}
	}
	return agentLook{kind: agentAnswered, status: res.StatusCode, found: fmt.Sprintf("its health check answers %d, not healthy: %q", res.StatusCode, strings.TrimSpace(string(body)))}
}
