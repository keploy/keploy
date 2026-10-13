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
//   - the user presses Ctrl+C again (utils.InterruptedAgain).
//
// A check is one of the agent's own beats (utils.TeardownBeat), not a tick of
// the CLI's clock: an agent the host does not schedule does not beat, so a
// loaded host only slows the checks down, and nothing is decided by how long
// the teardown takes. An agent that reports nothing -- one that predates the
// report, or could not write it -- is never killed for want of progress.

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
// new check: it can only record a step, or the end of the detach.
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
	} else if p.Beat > j.beat {
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
// progress (read) every poll, and kills it (kill) only when a stopJudge says
// so or the user presses Ctrl+C again. reported, when set, is closed at the
// agent's first report: its teardown has begun.
type stopWatch struct {
	logger   *zap.Logger
	gone     <-chan struct{}
	read     func() (utils.TeardownProgress, bool)
	kill     func() error
	poll     time.Duration
	reported chan<- struct{}
}

// run returns once the agent has exited, or once it has been killed (true);
// it does not wait for a killed agent's exit.
func (w stopWatch) run() bool {
	w.logger.Info("Waiting for the keploy agent to finish shutting down: it removes what it attached to the kernel before it exits, and is stopped by force only if it stops making progress (a second Ctrl+C stops it now)")
	var judge stopJudge
	reported := false
	tick := time.NewTicker(w.poll)
	defer tick.Stop()
	for {
		select {
		case <-w.gone:
			return false
		case <-utils.InterruptedAgain():
			w.force("Ctrl+C was pressed again")
			return true
		case <-tick.C:
		}
		p, ok := w.read()
		if !ok {
			continue
		}
		if !reported && w.reported != nil {
			reported = true
			close(w.reported)
		}
		switch judge.observe(p) {
		case killStalled:
			w.force("its teardown has made no progress across the agent's last checks")
			return true
		case killDetached:
			w.force("it has detached everything it attached but has not exited")
			return true
		}
	}
}

// force kills the agent, saying why.
func (w stopWatch) force(why string) {
	select {
	case <-w.gone:
		return // exited meanwhile: nothing to kill
	default:
	}
	w.logger.Warn("Stopping the keploy agent by force: "+why, zap.Uint64("stallChecks", stallChecks))
	if err := w.kill(); err != nil {
		w.logger.Debug("failed to kill the keploy agent", zap.Error(err))
	}
}

// readProgressFile reads the teardown progress a native agent reports at path
// (its --teardown-progress-file).
func readProgressFile(path string) func() (utils.TeardownProgress, bool) {
	return func() (utils.TeardownProgress, bool) {
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
func readContainerProgress(ctx context.Context, logger *zap.Logger, name string) func() (utils.TeardownProgress, bool) {
	return func() (utils.TeardownProgress, bool) {
		out, err := agentContainerOutput(ctx, logger, "cp", name+":"+kdocker.AgentTeardownProgressFile, "-")
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
