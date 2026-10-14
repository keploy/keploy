package http

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"time"

	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A STOPPING AGENT IS KILLED ONLY ONCE IT HAS STOPPED MAKING PROGRESS.
//
// The agent removes what it attached to the kernel as it shuts down, and the
// kernel abandons a uprobe removal whose thread is killed part way ("failed to
// unregister, leaking uprobe"): the breakpoint stays in every process that
// maps the probed library. So the CLI waits for a stopping agent as long as
// its teardown makes progress, which the agent reports
// (utils.ReportTeardownProgress), and kills it only when
//
//   - it has reported everything detached (utils.MarkDetachDone) and is still
//     there a check later: nothing a kill can cut short is left; or
//   - it has taken no teardown step across stallChecks consecutive checks; or
//   - the user presses Ctrl+C while keploy waits for it, once keploy has
//     said that a press stops it (utils.StopNowNotice): a whole
//     stopWatchOffer after the stop's signal was seen. An interrupt before
//     that does not count. One stop is often delivered twice (`sudo pkill -f
//     keploy` signals both sudo and keploy, and sudo hands its signal on), and
//     the copy comes with the first; and a press that ended a recording's
//     drain is no press to kill the agent mid-teardown. A press does what the
//     offer it answers said: the first offer's has the agent die once no
//     teardown work is in progress (as the verdicts below do); only the offer
//     that comes after the agent was told so -- a whole period after the
//     press, or the verdict, that told it -- kills it at once. A copy that
//     sudo was held from handing on until after an offer -- sudo starved, or
//     stopped -- can only be told from a press by its time, which the copy's
//     delay can outlast. Taken for a press, it costs what a stall verdict
//     does: the agent dies between teardown steps, never in one (no uprobe is
//     left half removed), and the steps it had not taken are not taken.
//
// A check is one of the agent's own beats (utils.TeardownBeat), not a tick of
// the CLI's clock: an agent the host does not schedule at all does not beat.
// A beat is still about a second of the agent's own time, so a step must come
// at least every stallChecks beats. What can take longer without one -- the
// removal of what the agent attached to the kernel, a CA tool rewriting a
// trust store -- is reported as work in progress (utils.BeginTeardownWork),
// and no check made during it counts as one without progress. Only an agent
// that says it records the steps of its whole teardown
// (utils.DeclareTeardownSteps) is killed for want of progress; one that does
// not -- an older agent, or one that could not write its report -- is waited
// for until it exits, or Ctrl+C is pressed (as above). So is a frozen agent
// (stopped, or frozen by its cgroup), which beats no more.

// stallChecks is the fixed count of consecutive checks with no teardown step
// after which a stopping agent is taken for one that has stopped making
// progress, and killed.
var stallChecks uint64 = 30

// stopVerdict is what a stopping agent's progress says to do.
type stopVerdict int

const (
	keepWaiting stopVerdict = iota
	// killStalled: no teardown step across stallChecks consecutive checks.
	killStalled
	// killDetached: everything detached a check ago, and still not exited.
	killDetached
)

// stopJudge reads a stopping agent's successive progress reports.
type stopJudge struct {
	seen     bool
	seq      uint64
	beat     uint64
	stalled  uint64 // consecutive checks without a step
	done     bool   // Done reported, at doneBeat
	doneBeat uint64
}

// observe takes the agent's latest report. A report with no new beat is no
// new check: it can only record a step, or the end of the detach. A check
// counts as one without progress only when the agent records its steps
// (Steps) and is not in the middle of work that takes none (Busy): every start
// and end of such work is a step, so a report that shows none since the last
// was not busy in between either.
func (j *stopJudge) observe(p utils.TeardownProgress) stopVerdict {
	if !j.seen {
		j.seen, j.seq, j.beat = true, p.Seq, p.Beat
		if p.Done {
			j.done, j.doneBeat = true, p.Beat
		}
		return keepWaiting
	}
	if p.Seq != j.seq {
		j.seq = p.Seq
		j.stalled = 0
	} else if p.Beat > j.beat && p.Steps && !p.Busy {
		j.stalled += p.Beat - j.beat
	}
	if p.Beat > j.beat {
		j.beat = p.Beat
	}
	if p.Done {
		if !j.done {
			j.done, j.doneBeat = true, p.Beat
			return keepWaiting
		}
		if p.Beat > j.doneBeat {
			return killDetached
		}
		return keepWaiting
	}
	if j.stalled >= stallChecks {
		return killStalled
	}
	return keepWaiting
}

// stopWatch waits for a stopping agent to exit (gone), checking its teardown
// progress (read) every poll. It kills it (kill) when a stopJudge says so --
// kill(false): the agent dies once it is not in the middle of teardown work
// -- and goes on waiting for the exit; and at the user's Ctrl+C once it has
// said that a press stops the agent (a utils.StopNowNotice, paced on its
// polls: a whole stopWatchOffer after the stop's signal was seen, or after the
// press or verdict before). A press does what the offer it answers said: the
// first kills as a verdict does; one after the agent was told to die so kills
// it at once, kill(true), where the CLI can, after which the watch waits no
// more. reported, when set, is closed at the agent's
// first report: its teardown has begun.
//
// While the agent reports that it records its steps (Steps) and keeps
// beating, no drain gives up on keploy's teardown (utils.BeginAgentStop): its
// stop ends on its own, by its exit or a kill. That hold ends with the watch,
// at a kill decided by the judge, and while no beat has come for
// holdStaleChecks of the agent's beats on the CLI's clock (a frozen agent,
// whose stop would not end): the drains keep their bound then, and keploy
// can exit while the agent finishes on its own. An agent that does not record
// its steps never holds them.
type stopWatch struct {
	logger   *zap.Logger
	gone     <-chan struct{}
	read     func(ctx context.Context) (utils.TeardownProgress, bool)
	kill     func(now bool) error
	poll     time.Duration
	reported chan<- struct{}
	// judged, when set, is told each report once the judge has read it
	// (tests).
	judged func(utils.TeardownProgress)
}

// holdStale is how long, on the CLI's clock, a stepping agent may go without
// a beat before its stop stops holding the drains (stopWatch).
var holdStale = time.Duration(stallChecks) * utils.TeardownBeat

// stopWatchOffer is how long after the stop's signal was seen (or after the
// press or verdict before) a stopWatch says Ctrl+C stops the agent, and from
// when a press does: longer than a copy of that signal (sudo's) takes to come,
// unless sudo is held from handing it on. A copy that late is taken for a
// press, and costs what the first press does: the agent dies between teardown
// steps, as at a stall verdict, never in one.
var stopWatchOffer = 5 * time.Second

// run returns once the agent has exited (false), or once it has been killed
// at once (true). The reads run on a goroutine of their own, under a context
// run cancels before it joins them: a slow one (an engine's `cp`) never holds
// up the exit or Ctrl+C.
func (w stopWatch) run() bool {
	reports := make(chan utils.TeardownProgress, 1)
	readCtx, stopReading := context.WithCancel(context.Background())
	read := make(chan struct{})
	defer func() { stopReading(); <-read }()
	go func() {
		defer close(read)
		tick := time.NewTicker(w.poll)
		defer tick.Stop()
		for {
			select {
			case <-readCtx.Done():
				return
			case <-tick.C:
			}
			if readCtx.Err() != nil {
				return
			}
			p, ok := w.read(readCtx)
			if !ok {
				continue
			}
			select { // the latest report replaces one not yet judged
			case <-reports:
			default:
			}
			reports <- p
		}
	}()

	release := func() {}
	holding := false
	hold := func(on bool) {
		switch {
		case on && !holding:
			release, holding = utils.BeginAgentStop(), true
		case !on && holding:
			release()
			holding = false
		}
	}
	defer hold(false)
	stale := time.NewTimer(holdStale)
	defer stale.Stop()

	// A press stops the agent once the watch has said so (offering).
	notice := utils.NewStopNowNotice(stopWatchOffer)
	defer func() { notice.Withdraw() }()
	ticks := time.NewTicker(w.poll)
	defer ticks.Stop()
	offering := false

	var judge stopJudge
	reported, killed, beaten := false, false, false
	var lastBeat uint64
	for {
		var p utils.TeardownProgress
		select {
		case <-w.gone:
			return false
		case <-notice.Interrupt():
			// This offer is answered: the next press needs one of its own,
			// a whole period from now.
			notice.Withdraw()
			notice, offering = utils.NewStopNowNotice(stopWatchOffer), false
			if !killed {
				// The first press: the agent dies once no teardown work is
				// in progress, as at a verdict.
				if !w.force("Ctrl+C was pressed: it stops once no teardown work is in progress", false) {
					return false
				}
				killed = true
				hold(false)
				continue
			}
			if !w.force("Ctrl+C was pressed again", true) {
				return false // it had exited: nothing was killed
			}
			return true
		case <-ticks.C:
			if !offering && notice.Next() {
				offering = true
				if killed {
					w.logger.Info("Press Ctrl+C again to stop the keploy agent at once: what it has not yet removed from the kernel may stay attached")
				} else {
					w.logger.Info("Press Ctrl+C again to stop the keploy agent: it then stops once no teardown work is in progress")
				}
			}
			continue
		case <-stale.C:
			hold(false) // no beat for a while: a stop that may not end
			continue
		case p = <-reports:
		}
		if !reported {
			reported = true
			w.logger.Info("Waiting for the keploy agent to finish shutting down: it removes what it attached to the kernel before it exits, and is stopped by force only if it stops making progress")
			if w.reported != nil {
				close(w.reported)
			}
		}
		if !beaten || p.Beat != lastBeat { // a beat: the agent is not frozen
			beaten, lastBeat = true, p.Beat
			stale.Reset(holdStale)
			hold(p.Steps && !killed)
		}
		verdict := judge.observe(p)
		if w.judged != nil {
			w.judged(p)
		}
		if killed {
			continue
		}
		switch verdict {
		case killStalled:
			w.force("its teardown has made no progress across the agent's last checks", false)
		case killDetached:
			w.force("it has detached everything it attached but has not exited", false)
		default:
			continue
		}
		// It dies once no teardown work is in progress; a press kills it at
		// once -- one that answers an offer saying so: a standing offer said
		// a press would have it die once idle, so it is withdrawn, and the
		// next comes a whole period from now, saying what a press does now.
		killed = true
		hold(false)
		if offering {
			notice.Withdraw()
			notice, offering = utils.NewStopNowNotice(stopWatchOffer), false
		}
	}
}

// force kills the agent, saying why; now: at once where the CLI can. It
// reports false, killing nothing, when the agent has exited.
func (w stopWatch) force(why string, now bool) (killed bool) {
	select {
	case <-w.gone:
		return false // exited meanwhile: nothing to kill
	default:
	}
	w.logger.Warn("Stopping the keploy agent by force: "+why, zap.Uint64("stallChecks", stallChecks))
	if err := w.kill(now); err != nil {
		w.logger.Debug("failed to kill the keploy agent", zap.Error(err))
	}
	return true
}

// readProgressFile reads the teardown progress a native agent reports at path
// (its --teardown-progress-file).
func readProgressFile(path string) func(context.Context) (utils.TeardownProgress, bool) {
	return func(context.Context) (utils.TeardownProgress, bool) {
		if path == "" {
			return utils.TeardownProgress{}, false
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return utils.TeardownProgress{}, false
		}
		p, err := utils.ParseTeardownProgress(b)
		return p, err == nil
	}
}

// readContainerProgress reads the teardown progress the agent in container
// name reports (kdocker.AgentTeardownProgressFile), through the engine's
// `cp`, which writes the file as a tar stream: nothing is run inside the
// container.
func readContainerProgress(cli *engineCLI, name string) func(context.Context) (utils.TeardownProgress, bool) {
	return func(ctx context.Context) (utils.TeardownProgress, bool) {
		out, err := cli.stdout(ctx, "cp", name+":"+kdocker.AgentTeardownProgressFile, "-")
		if err != nil {
			return utils.TeardownProgress{}, false
		}
		b, err := firstTarFile(out)
		if err != nil {
			return utils.TeardownProgress{}, false
		}
		p, err := utils.ParseTeardownProgress(b)
		return p, err == nil
	}
}

// firstTarFile returns the contents of the first regular file in a tar stream.
func firstTarFile(stream []byte) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(stream))
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 1<<16))
		}
	}
}
