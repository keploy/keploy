package utils

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"

	keployLog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// The agent's boot progress, for the CLI that waits for it to be ready.
//
// A CLI that starts an agent waits for it to answer its health check before it
// goes on. It waited a fixed budget of wall time for that (330 s, or
// KEPLOY_AGENT_READY_TIMEOUT), so a host loaded enough to run the agent slowly
// tore down a start that was making progress, and a start that had stopped
// making any was waited on for the whole budget. So the agent reports its
// start's progress here (ReportBootProgressTo, ReportBootProgress), from the
// moment its process starts until its control-plane server listens, and the
// CLI judges by it (pkg/platform/http, bootJudge): it waits while the start
// takes steps, does work that makes progress, or waits for the user, fails at
// once on a start the agent itself says has failed (FailBoot), and gives up on
// a start that has stopped making progress, by the agent's own beats (an agent
// the host does not run does not beat).

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
}

// BootProgressFileName is the name of the file an agent reports its start's
// progress in: next to the one the CLI asks it to report its teardown's in
// (--teardown-progress-file), in the same directory.
const BootProgressFileName = "boot.json"

// BootBeat is how often the reporter beats during the start.
const BootBeat = time.Second

// bootBeat is BootBeat, which tests shorten.
var bootBeat = BootBeat

// bootFailure is a start's failure (FailBoot).
type bootFailure struct {
	code int
	msg  string
}

var (
	bootSeq     atomic.Uint64
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

// BootStep records a step of the agent's start: something loaded, started or
// set up. A start that takes no step for the CLI's count of beats, with no
// work in progress making progress, is taken for one that has stopped.
func BootStep() {
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

// BeginBootPrompt records that the agent's start waits for the user, at a
// prompt the agent puts in front of them (Windows' dialog asking whether to
// trust keploy's CA): the CLI that waits for it waits however long the user
// takes (BootProgress.Prompt). Its start and its end (the returned func, which
// ends it once) are steps.
func BeginBootPrompt() (end func()) {
	bootPrompts.Add(1)
	BootStep()
	var once sync.Once
	return func() {
		once.Do(func() {
			bootPrompts.Add(-1)
			BootStep()
		})
	}
}

// bootWorkItem is one stage of the start in progress.
type bootWorkItem struct {
	probe workProbe
}

// BeginBootWork records the start of a stage of the agent's start that can
// take long without a step: loading the eBPF programs (the kernel's verifier),
// starting the proxy (its CA's install runs keytool, update-ca-certificates
// and the JVM a trust-store hook starts). While it makes progress, which the
// agent sees through its thread and the processes that thread runs (Linux:
// scheduled since the last beat), the CLI counts no beat as one without
// progress (BootProgress.Busy); its start and its end (the returned func,
// which ends it once) are steps. As BeginTeardownWork, the calling goroutine
// stays on its thread until it ends the stage, and must be the one that ends
// it: work it waits for on another goroutine is no progress of the stage.
//
// Where a stage's progress cannot be seen (not on Linux: TeardownProgressSeen)
// it is not marked busy, only its start and its end are steps: busy
// throughout, a stage wedged there would be waited on for ever, which is why
// the start is not marked teardown work there either.
func BeginBootWork() (end func()) {
	w := &bootWorkItem{probe: newWorkProbe()}
	if w.probe != nil {
		bootWorkMu.Lock()
		bootWorks[w] = struct{}{}
		bootWorkMu.Unlock()
	}
	BootStep()
	var once sync.Once
	return func() {
		once.Do(func() {
			BootStep()
			if w.probe == nil {
				return
			}
			bootWorkMu.Lock()
			delete(bootWorks, w)
			bootWorkMu.Unlock()
			w.probe.release()
		})
	}
}

// bootBusy reports whether a stage of the start in progress made progress
// since the last beat.
func bootBusy() bool {
	bootWorkMu.Lock()
	defer bootWorkMu.Unlock()
	busy := false
	for w := range bootWorks {
		// Every probe is asked, so each one's next answer is about one beat.
		if w.probe.moved() {
			busy = true
		}
	}
	return busy
}

// snapshotBoot is the report for beat.
func snapshotBoot(beat uint64, busy bool) BootProgress {
	p := BootProgress{Seq: bootSeq.Load(), Beat: beat, Busy: busy, Prompt: bootPrompts.Load() > 0,
		Errors: bootErrors.Load(), Ready: bootReady.Load(), Pid: os.Getpid()}
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
// (BootBeat), until it has written the report that says the agent is ready
// or that its start has failed, ctx is done (the agent is stopping: its
// teardown reports from there), or stop is closed (tests; nil: never).
// Whatever an earlier agent left is cleared first, so a CLI never reads it as
// this agent's. A failed write is only logged: a CLI that reads no report
// gives the agent's process bootSilence of its runnable time to make its
// first (bootJudge). The returned channel is closed once the reporter has
// returned.
func reportBootProgress(ctx context.Context, logger *zap.Logger, sink progressSink, stop <-chan struct{}) (exited <-chan struct{}) {
	if err := sink.clear(); err != nil && !os.IsNotExist(err) && logger != nil {
		logger.Debug("failed to remove a stale boot progress report", zap.Error(err))
	}
	every := bootBeat
	done := make(chan struct{})
	go func() {
		defer close(done)
		if c, ok := sink.(interface{ close() }); ok {
			defer c.close()
		}
		beats := time.NewTicker(every)
		defer beats.Stop()
		var beat uint64
		busy := bootBusy()
		logged := false
		for {
			p := snapshotBoot(beat, busy)
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
			select {
			case <-beats.C:
				beat++
				busy = bootBusy()
			case <-bootKick:
			case <-ctx.Done():
				return
			case <-stop:
				return
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
