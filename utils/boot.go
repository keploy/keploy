package utils

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	keployLog "go.keploy.io/server/v3/utils/log"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// The agent's boot progress, for the CLI that waits for it to be ready.
//
// A CLI that starts an agent waits for it to answer its health check before it
// goes on. It waited a fixed budget of wall time for that (330 s, or
// KEPLOY_AGENT_READY_TIMEOUT, both since removed), so a host loaded enough to
// run the agent slowly tore down a start that was making progress, and a
// start that had stopped making any was waited on for the whole budget. So the agent reports its
// start's progress here (ReportBootProgressTo, ReportBootProgress), from the
// moment its process starts until its control-plane server listens, and the
// CLI judges by it (pkg/platform/http, bootJudge): it waits while the start
// takes steps, does work that makes progress, or waits for the user, fails at
// once on a start the agent itself says has failed (FailBoot), and gives up on
// a start that has stopped making progress, by the agent's own beats.
//
// A beat is BootBeat of the agent's own runnable time (stall.Process), not of
// wall time: an agent the host freezes, stops or throttles to its CPU limit
// beats only as fast as it gets to run, so its start, which runs no faster,
// is not charged beats it could not spend; one the host does not run at all
// does not beat. (Where the host only makes it wait behind other work, the
// clock's sampler still wakes on time and the clock runs near wall time:
// there, work on a stage's own thread reads as progress while it waits to
// run, and so does the work a stage waits for on another goroutine, a stage
// of its own.) And the agent counts its beats without progress itself
// (BootProgress.Quiet), as it saw each of them: the CLI, which reads only the
// latest report, cannot tell how the beats between two of its reads went.

// BootProgress is what an agent reports of its start.
type BootProgress struct {
	// Seq counts the steps of the start taken so far (BootStep): it moves
	// while the start makes progress.
	Seq uint64 `json:"seq"`
	// Beat counts the reporter's beats since the start began, one every
	// BootBeat: each is one check of its progress.
	Beat uint64 `json:"beat"`
	// Busy says a stage of the start that can take long without a step is in
	// progress and making progress (BeginBootWork): loading the eBPF
	// programs, starting the proxy and installing its CA. Where the agent
	// cannot see a stage's progress (not on Linux) no stage is busy.
	Busy bool `json:"busy"`
	// Prompt says the start waits for the user (BeginBootPrompt): a prompt
	// the agent put in front of them, answered in their own time.
	Prompt bool `json:"prompt,omitempty"`
	// Errors counts the ERROR entries the agent logged during its start
	// (NoteBootError), and Error is the last one's message. They are detail,
	// for a start the CLI gives up on: no failure of the start, which the
	// agent says itself (Failed). It goes on from many an ERROR on purpose (a
	// hook it does without, a CA store it could not write).
	Errors uint64 `json:"errors,omitempty"`
	Error  string `json:"error,omitempty"`
	// Failed says the agent's start has failed, as the agent decided where it
	// gave up on it (FailBoot): it exits with ExitCode (ExitCodeFor), or, for a
	// failure it lives on after, does not start its server. Failure is what
	// failed.
	Failed   bool   `json:"failed,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	Failure  string `json:"failure,omitempty"`
	// Ready says the agent's control-plane server listens (MarkBootReady):
	// its start is over, and its health check answers from now on.
	Ready bool `json:"ready"`
	// Pid is the agent's own process id.
	Pid int `json:"pid,omitempty"`
	// Quiet counts the consecutive beats, since the start's last step, at
	// which no stage of it in progress had made progress and no prompt was
	// up; a step, a beat at which a stage made progress, and a prompt start
	// it again. It is the agent's own count, beat by beat (CountsQuiet),
	// which the CLI gives the start up by.
	Quiet uint64 `json:"quiet,omitempty"`
	// CountsQuiet says the agent counts Quiet. A report without it is an
	// older agent's, which the CLI judges by the beats it read itself.
	CountsQuiet bool `json:"countsQuiet,omitempty"`
	// Step names the last step the start took (BootStep).
	Step string `json:"step,omitempty"`
	// Stages are the stages of the start in progress at the last beat
	// (BeginBootWork), what a start that stalls was doing.
	Stages []BootStage `json:"stages,omitempty"`
}

// BootStage is one stage of the agent's start in progress, as the last beat
// saw it.
type BootStage struct {
	// Name says what the stage does (BeginBootWork).
	Name string `json:"name"`
	// Progressing says the stage made progress since the beat before.
	Progressing bool `json:"progressing"`
	// Unseen says the stage's progress cannot be seen here (not on Linux):
	// it is never progressing, nor is its thread described.
	Unseen bool `json:"unseen,omitempty"`
	// State is the kernel's state of the stage's thread (R running or
	// waiting to run, S sleeping, D in an uninterruptible wait, ...), and
	// Wait the kernel function it sleeps in (its wchan), where they can be
	// read.
	State string `json:"state,omitempty"`
	Wait  string `json:"wait,omitempty"`
}

// BootProgressFileName is the name of the file an agent reports its start's
// progress in: next to the one the CLI asks it to report its teardown's in
// (--teardown-progress-file), in the same directory.
const BootProgressFileName = "boot.json"

// BootBeat is how much of the agent's own runnable time each beat of the
// reporter stands for during the start.
const BootBeat = time.Second

// bootBeat is BootBeat, which tests shorten.
var bootBeat = BootBeat

// bootClock is the clock the reporter beats on, and the func that releases
// it: the agent's own runnable time (stall.Process). Tests drive one.
var bootClock = stall.Process

// bootFailure is a start's failure (FailBoot).
type bootFailure struct {
	code int
	msg  string
}

var (
	bootSeq     atomic.Uint64
	bootStep    atomic.Pointer[string] // the last step's name
	bootErrors  atomic.Uint64
	bootError   atomic.Pointer[string]
	bootReady   atomic.Bool
	bootFailed  atomic.Pointer[bootFailure]
	bootPrompts atomic.Int64
	// bootKick wakes the reporter to write a step, an error or the end at
	// once.
	bootKick = make(chan struct{}, 1)
	// bootOver is closed once the start is over: ready, or failed.
	bootOver     = make(chan struct{})
	bootOverOnce = new(sync.Once)

	bootWorkMu sync.Mutex
	bootWorks  = map[*bootWorkItem]struct{}{}
)

// endBoot records that the start is over.
func endBoot() { bootOverOnce.Do(func() { close(bootOver) }) }

// BootOver is closed once the agent's start is over: its server listens
// (MarkBootReady), or it has failed (FailBoot).
func BootOver() <-chan struct{} { return bootOver }

// bootEnded reports whether the start is over.
func bootEnded() bool { return bootReady.Load() || bootFailed.Load() != nil }

func kickBootReport() {
	select {
	case bootKick <- struct{}{}:
	default:
	}
}

// BootStep records a step of the agent's start, name saying what it was:
// something loaded, started or set up. A start that takes no step for the
// CLI's count of beats, with no work in progress making progress, is taken
// for one that has stopped, and the last step's name says how far it got.
func BootStep(name string) {
	// The name before the count: a report that counts the step names it.
	bootStep.Store(&name)
	bootSeq.Add(1)
	kickBootReport()
}

// NoteBootError records an ERROR the agent logged before its start was over,
// as detail for a start the CLI gives up on (BootProgress.Errors). A no-op
// once the start is over.
func NoteBootError(msg string) {
	if bootEnded() {
		return
	}
	// The message before the count: a report that counts an ERROR has its
	// message (snapshotBoot loads the count first).
	bootError.Store(&msg)
	bootErrors.Add(1)
	kickBootReport()
}

// FailBoot records that the agent's start has failed with err, where the
// agent gives up on it: the CLI that waits for it fails the start with err,
// and with the code the agent exits with for it (ExitCodeFor), at once. Only
// the first failure is recorded; a no-op once the agent is ready, or for a nil
// err.
func FailBoot(err error) {
	if err == nil || bootReady.Load() {
		return
	}
	// Redacted as the log line that says it is: the report leaves the agent.
	if bootFailed.CompareAndSwap(nil, &bootFailure{code: ExitCodeFor(err), msg: keployLog.RedactText(err.Error())}) {
		endBoot()
		kickBootReport()
	}
}

// MarkBootReady records that the agent's control-plane server listens: its
// start is over, and the reporter writes its last report. A DaemonSet agent,
// which starts no server, marks its start over once it would have.
func MarkBootReady() {
	if bootFailed.Load() != nil {
		return // its start has failed already
	}
	bootReady.Store(true)
	endBoot()
	kickBootReport()
}

// BeginBootPrompt records that the agent's start waits for the user, at the
// prompt name says the agent put in front of them (Windows' dialog asking
// whether to trust keploy's CA): the CLI that waits for it waits however long
// the user takes (BootProgress.Prompt). Its start and its end (the returned
// func, which ends it once) are steps.
func BeginBootPrompt(name string) (end func()) {
	bootPrompts.Add(1)
	BootStep(name + " is up")
	var once sync.Once
	return func() {
		once.Do(func() {
			bootPrompts.Add(-1)
			BootStep(name + " was answered")
		})
	}
}

// bootWorkItem is one stage of the start in progress.
type bootWorkItem struct {
	name  string
	probe workProbe
}

// BeginBootWork records the start of a stage of the agent's start that can
// take long without a step, name saying what it does: loading the eBPF
// programs (the kernel's verifier), starting the proxy (its CA's install runs
// keytool, update-ca-certificates and the JVM a trust-store hook starts).
// While it makes progress, which the agent sees through its thread and the
// processes that thread runs (Linux: scheduled since the last beat), the CLI
// counts no beat as one without progress (BootProgress.Busy); its start and
// its end (the returned func, which ends it once) are steps. As
// BeginTeardownWork, the calling goroutine stays on its thread until it ends
// the stage, and must be the one that ends it: work it waits for on another
// goroutine is no progress of the stage, so that work begins a stage of its
// own, on its own goroutine (the proxy's listener, its DNS servers' binds).
// Stages may overlap: the start makes progress while any of them does.
//
// Where a stage's progress cannot be seen (not on Linux: TeardownProgressSeen)
// it is not marked busy, only its start and its end are steps: busy
// throughout, a stage wedged there would be waited on for ever, which is why
// the start is not marked teardown work there either.
func BeginBootWork(name string) (end func()) {
	// Recorded even where its progress cannot be seen: a start that stalls
	// still says it was in it.
	w := &bootWorkItem{name: name, probe: newWorkProbe()}
	bootWorkMu.Lock()
	bootWorks[w] = struct{}{}
	bootWorkMu.Unlock()
	BootStep(name + " began")
	var once sync.Once
	return func() {
		once.Do(func() {
			BootStep(name + " ended")
			bootWorkMu.Lock()
			delete(bootWorks, w)
			bootWorkMu.Unlock()
			if w.probe != nil {
				w.probe.release()
			}
		})
	}
}

// bootBusy reports whether a stage of the start in progress made progress
// since the last beat (bootLook).
func bootBusy() bool {
	busy, _ := bootLook()
	return busy
}

// bootLook is one beat's look at the stages of the start in progress: whether
// any made progress since the last look, and each one as it found it, in the
// order of their names.
func bootLook() (busy bool, stages []BootStage) {
	bootWorkMu.Lock()
	defer bootWorkMu.Unlock()
	for w := range bootWorks {
		st := BootStage{Name: w.name, Unseen: w.probe == nil}
		if w.probe != nil {
			// Every probe is asked, so each one's next answer is about one
			// beat.
			st.Progressing = w.probe.moved()
			if d, ok := w.probe.(describedProbe); ok {
				st.State, st.Wait = d.describe()
			}
		}
		busy = busy || st.Progressing
		stages = append(stages, st)
	}
	sort.Slice(stages, func(i, j int) bool { return stages[i].Name < stages[j].Name })
	return busy, stages
}

// snapshotBoot is the report for beat.
func snapshotBoot(beat uint64, busy bool) BootProgress {
	p := BootProgress{Seq: bootSeq.Load(), Beat: beat, Busy: busy, Prompt: bootPrompts.Load() > 0,
		Errors: bootErrors.Load(), Ready: bootReady.Load(), Pid: os.Getpid()}
	if st := bootStep.Load(); st != nil {
		p.Step = *st
	}
	if p.Errors > 0 {
		if e := bootError.Load(); e != nil {
			p.Error = *e
		}
	}
	if f := bootFailed.Load(); f != nil {
		p.Failed, p.ExitCode, p.Failure = true, f.code, f.msg
	}
	return p
}

// ReportBootProgress writes the agent's start's progress to path, a path the
// agent chose itself (a containerised agent's, in its own container): see
// reportBootProgress. The returned channel is closed once the reporter has
// returned, and so written its last report (at once for an empty path).
func ReportBootProgress(ctx context.Context, logger *zap.Logger, path string) (exited <-chan struct{}) {
	if path == "" {
		done := make(chan struct{})
		close(done)
		return done
	}
	return reportBootProgress(ctx, logger, pathSink(path), nil)
}

// ReportBootProgressTo writes the agent's start's progress next to the
// teardown progress file its command line gave (--teardown-progress-file), as
// BootProgressFileName, through that file's directory, checked as
// ReportTeardownProgressTo checks it: an agent elevated by sudo writes there
// as root, and touches no other file.
func ReportBootProgressTo(ctx context.Context, logger *zap.Logger, teardownPath string) error {
	if teardownPath == "" {
		return nil
	}
	sink, err := openCommandLineSinkNamed(teardownPath, BootProgressFileName)
	if err != nil {
		return err
	}
	reportBootProgress(ctx, logger, sink, nil)
	return nil
}

// reportBootProgress writes the agent's start's progress (BootProgress, as
// JSON) to sink: at once, on every step, error and the end, and on every beat
// (BootBeat of the agent's own runnable time, bootClock), until it has written
// the report that says the agent is ready or that its start has failed, ctx is
// done (the agent is stopping: its teardown reports from there), or stop is
// closed (tests; nil: never). Whatever an earlier agent left is cleared first,
// so a CLI never reads it as this agent's. A failed write is only logged: a
// CLI that reads no report gives the agent's process bootSilence of its
// runnable time to make its first (bootJudge). The returned channel is closed
// once the reporter has returned.
func reportBootProgress(ctx context.Context, logger *zap.Logger, sink progressSink, stop <-chan struct{}) (exited <-chan struct{}) {
	if err := sink.clear(); err != nil && !os.IsNotExist(err) && logger != nil {
		logger.Debug("failed to remove a stale boot progress report", zap.Error(err))
	}
	every := bootBeat
	clk, release := bootClock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer release()
		if c, ok := sink.(interface{ close() }); ok {
			defer c.close()
		}
		beats := stall.NewMeter(clk, every)
		defer beats.Stop()
		var beat uint64
		busy, stages := bootLook()
		// quiet is the beats since the step quietSeq counts at which no stage
		// made progress and no prompt was up.
		var quiet uint64
		quietSeq := bootSeq.Load()
		var next <-chan time.Time // the meter's pending look
		logged := false
		for {
			if s := bootSeq.Load(); s != quietSeq {
				quiet, quietSeq = 0, s // a step: progress
			}
			p := snapshotBoot(beat, busy)
			p.Quiet, p.CountsQuiet, p.Stages = quiet, true, stages
			b, err := json.Marshal(p)
			if err == nil {
				err = sink.write(b)
			}
			if err != nil && !logged && logger != nil {
				logged = true
				logger.Debug("failed to write the agent's boot progress", zap.Error(err))
			}
			if p.Ready || p.Failed {
				return
			}
			for waiting := true; waiting; {
				if next == nil {
					next = beats.Wait()
				}
				select {
				case <-next:
					next = nil
					// One beat for each BootBeat the agent could run since the
					// last; none yet, however much wall time passed.
					n := uint64(beats.Stalled() / every)
					if n == 0 {
						continue
					}
					beats.ResetBy(time.Duration(n) * every) // what it ran past them counts toward the next
					beat += n
					busy, stages = bootLook()
					if s := bootSeq.Load(); s != quietSeq || busy || bootPrompts.Load() > 0 {
						quiet, quietSeq = 0, s
					} else {
						quiet += n
					}
					waiting = false
				case <-bootKick:
					waiting = false
				case <-ctx.Done():
					return
				case <-stop:
					return
				}
			}
		}
	}()
	return done
}

// ParseBootProgress reads what ReportBootProgress wrote.
func ParseBootProgress(b []byte) (BootProgress, error) {
	var p BootProgress
	err := json.Unmarshal(b, &p)
	return p, err
}
