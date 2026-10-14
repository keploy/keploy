package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// The agent's teardown progress, for the CLI that stops it.
//
// A keploy agent removes what it attached to the kernel as it shuts down, and
// an agent killed part way through removing a TLS uprobe leaks it: the
// breakpoint stays in every process that maps the probed library. So the CLI
// kills a stopping agent only once it has stopped making progress, or has
// detached everything and still not exited (pkg/platform/http, stopWatch).
// What it judges by is what ReportTeardownProgress writes here.

// TeardownProgress is what an agent reports of its teardown once it has begun.
type TeardownProgress struct {
	// Seq counts the steps of the teardown taken so far (TeardownStep): it
	// moves while the teardown makes progress.
	Seq uint64 `json:"seq"`
	// Beat counts the reporter's beats since the teardown began, one every
	// TeardownBeat: each is one check of its progress. An agent the host does
	// not schedule at all does not beat; one it does beats about once a
	// second however slowly its teardown goes, so a stage that can take long
	// without a step is reported as work in progress (BeginTeardownWork).
	Beat uint64 `json:"beat"`
	// Busy says a stage the agent must not be killed in the middle of is in
	// progress (BeginTeardownWork): no check counts as one without progress.
	Busy bool `json:"busy"`
	// Steps says the agent records its teardown's steps (DeclareTeardownSteps),
	// so a teardown that takes none is one that has stopped: only such an
	// agent is killed for want of progress.
	Steps bool `json:"steps"`
	// Done says the agent has detached everything it attached
	// (MarkDetachDone): nothing a kill can cut short is left.
	Done bool `json:"done"`
}

// TeardownBeat is how often the reporter beats once the teardown has begun.
const TeardownBeat = time.Second

// teardownBeat is TeardownBeat, which tests shorten.
var teardownBeat = TeardownBeat

var (
	teardownSeq atomic.Uint64
	// teardownWorkMu orders the start of teardown work against the
	// escalation's check that none is in progress (DieOnEscalation).
	teardownWorkMu sync.Mutex
	teardownWork   atomic.Int64
	teardownSteps  atomic.Bool
	teardownDone   atomic.Bool
	// teardownKick wakes the reporter to write a step or the end at once.
	teardownKick = make(chan struct{}, 1)
)

func kickTeardownReport() {
	select {
	case teardownKick <- struct{}{}:
	default:
	}
}

// TeardownStep records a step of the agent's teardown: something detached,
// closed or stopped. A teardown that takes no step for the CLI's fixed count
// of beats is taken for one that has stopped making progress, so a stage
// that can take long should record each of its parts.
func TeardownStep() {
	teardownSeq.Add(1)
	kickTeardownReport()
}

// BeginTeardownWork records the start of a stage of the teardown the agent
// must not be killed in the middle of, and which can take long without a
// step: removing what it attached to the kernel, a CA tool rewriting a trust
// store. While one is in progress the CLI counts no check as one without
// progress (TeardownProgress.Busy). The stage's start and its end -- the
// returned func, which ends it once however often it is called -- are each a
// step.
func BeginTeardownWork() (end func()) { return beginTeardownWork(nil) }

// beginTeardownWork is BeginTeardownWork, whose end runs between (tests; nil
// otherwise) after its step and before the work is over.
func beginTeardownWork(between func()) (end func()) {
	teardownWorkMu.Lock()
	teardownWork.Add(1)
	teardownWorkMu.Unlock()
	TeardownStep()
	var once sync.Once
	return func() {
		once.Do(func() {
			// The step before the work is over: a report, which loads Busy
			// before Seq (snapshotTeardown), that reads the work as over
			// reads this step too.
			TeardownStep()
			if between != nil {
				between()
			}
			teardownWork.Add(-1)
		})
	}
}

// DeclareTeardownSteps says this agent records the steps of its whole
// teardown (TeardownStep, BeginTeardownWork) through to MarkDetachDone, so
// the CLI may take a teardown that takes none for one that has stopped, and
// kill it. An agent that does not say so is never killed for want of
// progress. It is for main to call, which knows everything the process tears
// down -- including what it removes after the command has returned.
func DeclareTeardownSteps() {
	teardownSteps.Store(true)
	kickTeardownReport()
}

// TeardownBusy reports whether a stage of the teardown begun with
// BeginTeardownWork is in progress: what the reporter writes as Busy.
func TeardownBusy() bool { return teardownWork.Load() > 0 }

// TeardownSteps returns how many teardown steps have been recorded
// (TeardownStep): what the reporter writes as Seq.
func TeardownSteps() uint64 { return teardownSeq.Load() }

// MarkDetachDone records that the agent has detached everything it attached:
// all that is left is to exit, and the CLI may kill it if it does not. It is
// called as late as the process allows -- by main, once the command has
// returned and anything that main itself removes is gone.
func MarkDetachDone() {
	teardownDone.Store(true)
	kickTeardownReport()
}

// TeardownProgressFileName is the name a CLI gives the file it asks an agent
// to report its teardown progress in (ReportTeardownProgressTo).
const TeardownProgressFileName = "teardown.json"

// progressSink is where a reporter writes: it clears what an earlier agent
// left, and replaces the report whole (a rename), so a reader never sees half
// of one.
type progressSink interface {
	clear() error
	write(b []byte) error
}

// reporting is the reporters ReportTeardownProgress and
// ReportTeardownProgressTo started, which run until the process exits unless
// StopTeardownReporting ends them: stop is closed then and cleared, and the
// next reporter started makes a new one.
var reporting struct {
	mu      sync.Mutex
	stop    chan struct{}
	running sync.WaitGroup
	live    atomic.Int32 // how many are running (tests read it)
}

// startReporting starts a reporter on sink that StopTeardownReporting ends.
func startReporting(ctx context.Context, logger *zap.Logger, sink progressSink) {
	reporting.mu.Lock()
	defer reporting.mu.Unlock()
	if reporting.stop == nil {
		reporting.stop = make(chan struct{})
	}
	reporting.running.Add(1)
	reporting.live.Add(1)
	done := reportTeardownProgress(ctx, logger, sink, reporting.stop, nil)
	go func() {
		<-done
		reporting.live.Add(-1)
		reporting.running.Done()
	}()
}

// StopTeardownReporting ends every reporter ReportTeardownProgress and
// ReportTeardownProgressTo started, and returns once none writes any more. An
// agent's reporter runs until its process exits; this is for a test that
// starts one (through the agent command) and must stop it before the
// directory it writes in is removed. Tests only: an agent whose reports stop
// is one its CLI never kills for want of progress. It ends the reporters of
// the whole process, so a test that calls it must not run in parallel with
// another that starts one.
func StopTeardownReporting() {
	reporting.mu.Lock()
	defer reporting.mu.Unlock()
	if reporting.stop != nil {
		close(reporting.stop)
		reporting.stop = nil
	}
	reporting.running.Wait()
}

// ReportTeardownProgress writes the agent's teardown progress to path, a path
// the agent chose itself (a containerised agent's, in its own container): see
// reportTeardownProgress.
func ReportTeardownProgress(ctx context.Context, logger *zap.Logger, path string) {
	if path == "" {
		return
	}
	startReporting(ctx, logger, pathSink(path))
}

// ReportTeardownProgressTo writes the agent's teardown progress to path, a
// path its command line gave (the CLI's --teardown-progress-file): see
// reportTeardownProgress. An agent elevated by sudo writes there as root, so
// the path is held to what a keploy CLI gives -- TeardownProgressFileName, in
// a directory that is not a symlink, that no one but its owner may write to,
// and that root, this user or the user sudo ran this agent for owns -- and
// every write goes through that directory as it was opened, never through the
// path again: whatever the command line says, the agent touches no other file.
// It refuses a path that is not such, and reports nothing then.
func ReportTeardownProgressTo(ctx context.Context, logger *zap.Logger, path string) error {
	if path == "" {
		return nil
	}
	sink, err := openCommandLineSink(path)
	if err != nil {
		return err
	}
	startReporting(ctx, logger, sink)
	return nil
}

// reportTeardownProgress writes the agent's teardown progress
// (TeardownProgress, as JSON) to sink once ctx is done -- the root context,
// whose cancel begins the teardown: at once, on every step and on
// MarkDetachDone, and on every beat (TeardownBeat) until the process exits, or
// stop is closed (StopTeardownReporting, and tests; nil: never). Whatever an
// earlier agent left is cleared now, so a CLI never reads it as this agent's.
// A failed write is only logged: the CLI that reads no progress never kills
// for want of it.
//
// Each report is taken by snapshotTeardown, which runs between (tests; nil
// otherwise) between its loads of Busy and Seq. The returned channel is
// closed once the reporter has returned, which it does some time after stop
// is closed: closing stop alone does not end a write in flight, and a beat or
// kick ready with it can win the select for another write. A test waits on
// the channel before it removes the directory, or a write puts the report
// (and its temporary file) back in it; once the channel is closed, the
// reporter writes nothing more.
func reportTeardownProgress(ctx context.Context, logger *zap.Logger, sink progressSink, stop <-chan struct{}, between func()) (exited <-chan struct{}) {
	if err := sink.clear(); err != nil && !os.IsNotExist(err) && logger != nil {
		logger.Debug("failed to remove a stale teardown progress report", zap.Error(err))
	}
	// Read now: a test restores it before the goroutine has returned.
	every := teardownBeat
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A sink that holds something open lets it go once its reporter has
		// stopped (the agent's never does: it reports until it exits).
		if c, ok := sink.(interface{ close() }); ok {
			defer c.close()
		}
		select {
		case <-ctx.Done():
		case <-stop:
			return
		}
		beats := time.NewTicker(every)
		defer beats.Stop()
		var beat uint64
		logged := false
		for {
			b, err := json.Marshal(snapshotTeardown(beat, between))
			if err == nil {
				err = sink.write(b)
			}
			if err != nil && !logged && logger != nil {
				logged = true
				logger.Debug("failed to write the agent's teardown progress", zap.Error(err))
			}
			select {
			case <-beats.C:
				beat++
			case <-teardownKick:
			case <-stop:
				return
			}
		}
	}()
	return done
}

// snapshotTeardown is the report for beat. It is never torn -- a step from
// before a stage of teardown work ended, paired with an agent no longer busy,
// which the CLI would count as checks without progress for every beat since
// the last report it read, up to the whole stage -- because of two orders:
// the end of the work records its step before the work is over
// (BeginTeardownWork), and the report loads Busy before Seq. So a report that
// reads the work as over reads the step that ended it too. It runs between
// (tests; nil otherwise) after its load of Busy and before its load of Seq.
func snapshotTeardown(beat uint64, between func()) TeardownProgress {
	busy := teardownWork.Load() > 0
	if between != nil {
		between()
	}
	return TeardownProgress{Seq: teardownSeq.Load(), Beat: beat, Busy: busy,
		Steps: teardownSteps.Load(), Done: teardownDone.Load()}
}

// pathSink writes to a path the agent chose itself.
type pathSink string

func (p pathSink) clear() error { return os.Remove(string(p)) }

// write replaces the file at p with b.
func (p pathSink) write(b []byte) error {
	path := string(p)
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// checkCommandLinePath checks what can be checked of a command-line path
// without opening anything: absolute, and naming TeardownProgressFileName.
func checkCommandLinePath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("the teardown progress file %q is not an absolute path", path)
	}
	if filepath.Base(path) != TeardownProgressFileName {
		return fmt.Errorf("the teardown progress file %q is not named %s", path, TeardownProgressFileName)
	}
	return nil
}

// ParseTeardownProgress reads what ReportTeardownProgress wrote.
func ParseTeardownProgress(b []byte) (TeardownProgress, error) {
	var p TeardownProgress
	err := json.Unmarshal(b, &p)
	return p, err
}
