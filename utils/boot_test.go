package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	keployLog "go.keploy.io/server/v3/utils/log"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// resetBoot is the start's state as a fresh process has it; restored after
// the test.
func resetBoot(t *testing.T) {
	t.Helper()
	reset := func() {
		bootSeq.Store(0)
		bootStep.Store(nil)
		bootErrors.Store(0)
		bootError.Store(nil)
		bootReady.Store(false)
		bootFailed.Store(nil)
		bootPrompts.Store(0)
		bootOver = make(chan struct{})
		bootOverOnce = new(sync.Once)
		select {
		case <-bootKick:
		default:
		}
	}
	reset()
	t.Cleanup(reset)
	every := bootBeat
	bootBeat = time.Millisecond
	t.Cleanup(func() { bootBeat = every })
}

func readBoot(t *testing.T, path string) (BootProgress, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return BootProgress{}, false
	}
	p, err := ParseBootProgress(b)
	if err != nil {
		t.Fatalf("unreadable report: %v", err)
	}
	return p, true
}

// The agent reports its start from the first moment: its steps, its beats,
// the ERRORs it logged (the last one's message), and that its server listens,
// after which the reporter writes no more. A report an earlier agent left is gone before the
// first one.
func TestReportBootProgress(t *testing.T) {
	resetBoot(t)
	path := filepath.Join(t.TempDir(), BootProgressFileName)
	if err := os.WriteFile(path, []byte(`{"seq":99,"ready":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	done := reportBootProgress(context.Background(), zap.NewNop(), pathSink(path), stop)

	BootStep("a step")
	for {
		if p, ok := readBoot(t, path); ok && p.Seq == 1 && p.Beat > 2 && !p.Ready {
			if p.Pid != os.Getpid() {
				t.Fatalf("pid %d, want %d", p.Pid, os.Getpid())
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	NoteBootError("failed to load hooks")
	NoteBootError("a second one")
	MarkBootReady()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reporter did not end once the agent was ready")
	}
	p, _ := readBoot(t, path)
	if !p.Ready || p.Errors != 2 || p.Error != "a second one" || p.Failed {
		t.Fatalf("last report %+v, want ready, 2 errors, the last one's message, not failed", p)
	}
	select {
	case <-BootOver():
	default:
		t.Fatal("the start is not over once the agent is ready")
	}
	NoteBootError("after the start")
	if bootErrors.Load() != 2 {
		t.Fatal("an ERROR after the start was counted as one of the start's")
	}
}

// recordedProbe is a work probe whose answer the test sets: the progress the
// kernel's account of a stage's thread would show (workProbe).
type recordedProbe struct {
	progress bool
	released bool
}

func (p *recordedProbe) moved() bool { return p.progress }
func (p *recordedProbe) release()    { p.released = true }

// Work in progress is a step as it begins and as it ends, and reported busy
// while its probe sees it make progress: not once it stops, nor once it ended,
// when its probe is let go. The probe is the test's, so this holds on every
// platform; the platform's own (Linux: the stage's thread) is watched apart,
// and where there is none (TestBootWorkWhoseProgressCannotBeSeenIsNotBusy) no
// stage is busy.
func TestBootWorkIsReportedBusy(t *testing.T) {
	resetBoot(t)
	probe := &recordedProbe{progress: true}
	was := newWorkProbe
	newWorkProbe = func() workProbe { return probe }
	t.Cleanup(func() { newWorkProbe = was })
	if bootBusy() {
		t.Fatal("busy with no work in progress")
	}
	before := bootSeq.Load()
	end := BeginBootWork("a stage")
	if bootSeq.Load() != before+1 {
		t.Fatal("the work's start is no step")
	}
	if !bootBusy() {
		t.Fatal("work making progress is not busy")
	}
	probe.progress = false
	if bootBusy() {
		t.Fatal("work that stopped making progress is busy")
	}
	probe.progress = true
	end()
	end()
	if bootSeq.Load() != before+2 {
		t.Fatalf("the work's end took %d steps, want one", bootSeq.Load()-before-1)
	}
	if bootBusy() {
		t.Fatal("busy once the work ended")
	}
	if !probe.released {
		t.Fatal("the work's probe was not let go as it ended")
	}
}

// A stopping agent's report ends with its start: its teardown reports from
// then on.
func TestTheBootReporterEndsWithTheAgentsStop(t *testing.T) {
	resetBoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := reportBootProgress(ctx, zap.NewNop(), pathSink(filepath.Join(t.TempDir(), BootProgressFileName)), nil)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reporter did not end with the agent's stop")
	}
}

// A start the agent gives up on is reported failed, with the code it exits
// with and what failed, at once, and the report ends there: its first failure
// is the one reported, and nothing the agent does after it (an ERROR, a
// server that listens after all) changes it.
func TestAFailedStartIsReportedWithItsExitCode(t *testing.T) {
	resetBoot(t)
	path := filepath.Join(t.TempDir(), BootProgressFileName)
	stop := make(chan struct{})
	defer close(stop)
	done := reportBootProgress(context.Background(), zap.NewNop(), pathSink(path), stop)
	NoteBootError("failed to attach the udp4 sendmsg hook") // an ERROR it goes on from
	if p, _ := readBoot(t, path); p.Failed {
		t.Fatal("an ERROR the agent goes on from failed its start")
	}
	FailBoot(fmt.Errorf("failed to hook into the app: %w", ErrPrivilegeRequired))
	FailBoot(errors.New("a second failure"))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reporter did not end once the start had failed")
	}
	select {
	case <-BootOver():
	default:
		t.Fatal("the start is not over once it has failed")
	}
	MarkBootReady()
	NoteBootError("after the failure")
	p, _ := readBoot(t, path)
	if !p.Failed || p.ExitCode != ExitPrivilegeRequired || p.Failure != "failed to hook into the app: "+ErrPrivilegeRequired.Error() {
		t.Fatalf("last report %+v, want the first failure, with exit code %d", p, ExitPrivilegeRequired)
	}
	if p.Errors != 1 || p.Error != "failed to attach the udp4 sendmsg hook" {
		t.Fatalf("last report %+v, want the one ERROR before the failure as its detail", p)
	}
	if bootReady.Load() || bootErrors.Load() != 1 {
		t.Fatal("a failed start was marked ready, or counted an ERROR after its failure")
	}
}

// A start that is over is not failed after it: the agent's later failures
// are no failures of its start.
func TestAFailureAfterTheStartIsNoFailureOfIt(t *testing.T) {
	resetBoot(t)
	MarkBootReady()
	FailBoot(errors.New("failed to setup agent"))
	FailBoot(nil)
	if bootFailed.Load() != nil {
		t.Fatal("a failure after the start was recorded as its failure")
	}
}

// A prompt in front of the user is reported while it is up, and its start and
// end are steps.
func TestABootPromptIsReported(t *testing.T) {
	resetBoot(t)
	before := bootSeq.Load()
	end := BeginBootPrompt("a prompt")
	if p := snapshotBoot(0, false); !p.Prompt || p.Seq != before+1 {
		t.Fatalf("report %+v while the prompt is up, want prompt, one step", p)
	}
	end()
	end()
	if p := snapshotBoot(0, false); p.Prompt || p.Seq != before+2 {
		t.Fatalf("report %+v once the prompt is answered, want no prompt, two steps", p)
	}
}

// Where a stage's progress cannot be seen, it is not reported busy: busy
// throughout, one wedged there would be waited on for ever. Its start and its
// end are still steps, and it is still reported in progress, by its name,
// as unseen: a start that stalls says it was in it.
func TestBootWorkWhoseProgressCannotBeSeenIsNotBusy(t *testing.T) {
	resetBoot(t)
	probe := newWorkProbe
	newWorkProbe = func() workProbe { return nil }
	t.Cleanup(func() { newWorkProbe = probe })
	before := bootSeq.Load()
	end := BeginBootWork("a stage")
	if bootBusy() {
		t.Fatal("work whose progress cannot be seen is busy")
	}
	if _, stages := bootLook(); len(stages) != 1 || stages[0] != (BootStage{Name: "a stage", Unseen: true}) {
		t.Fatalf("stages %+v, want the stage named, unseen", stages)
	}
	end()
	if _, stages := bootLook(); len(stages) != 0 {
		t.Fatalf("stages %+v once the stage ended, want none", stages)
	}
	if bootSeq.Load() != before+2 {
		t.Fatalf("%d steps, want its start and its end", bootSeq.Load()-before)
	}
}

// The agent reports its start next to the teardown progress file its command
// line names, as BootProgressFileName, and leaves that file alone.
func TestReportBootProgressToWritesNextToTheTeardownFile(t *testing.T) {
	resetBoot(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	teardown := filepath.Join(dir, "teardown.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := ReportBootProgressTo(ctx, zap.NewNop(), teardown); err != nil {
		t.Fatal(err)
	}
	BootStep("a step")
	MarkBootReady()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if p, ok := readBoot(t, filepath.Join(dir, BootProgressFileName)); ok && p.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no report that the agent is ready in %s", dir)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(teardown); !os.IsNotExist(err) {
		t.Fatalf("the teardown progress file was written: %v", err)
	}
}

// StopTeardownReporting ends the agent's boot reporters as it ends its
// teardown reporters, and returns once neither writes any more: a test that
// ran the agent command stops them all before it removes the directory they
// write in. A boot reporter it left running wrote its next report (a temporary
// file, then the rename) into that directory while the test's cleanup removed
// it, which failed the cleanup with "directory not empty".
func TestStopTeardownReportingEndsTheBootReporters(t *testing.T) {
	resetBoot(t)
	ctx, cancel := context.WithCancel(context.Background()) // the agent never stops
	defer cancel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ReportBootProgressTo(ctx, zap.NewNop(), filepath.Join(dir, "teardown.json")); err != nil {
		t.Fatal(err)
	}
	exited := ReportBootProgress(ctx, zap.NewNop(), filepath.Join(t.TempDir(), BootProgressFileName))
	if n := reporting.live.Load(); n != 2 {
		t.Fatalf("%d reporters StopTeardownReporting would end, want both boot reporters", n)
	}
	BootStep("a step") // neither reporter's start is over

	stopped := make(chan struct{})
	go func() { StopTeardownReporting(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("StopTeardownReporting never returned: a boot reporter it should have ended still runs")
	}
	select {
	case <-exited:
	default:
		t.Fatal("a boot reporter still runs after StopTeardownReporting returned")
	}
	if n := reporting.live.Load(); n != 0 {
		t.Fatalf("%d reporters still running after StopTeardownReporting returned", n)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("a stopped boot reporter's directory does not come away: %v", err)
	}
}

// redactingSecret is a redactor that replaces "hunter2".
type redactingSecret struct{}

func (redactingSecret) RedactEntry(*zapcore.Entry) {}
func (redactingSecret) RedactField(*zapcore.Field) {}
func (redactingSecret) RedactEncoded(text string) string {
	return strings.ReplaceAll(text, "hunter2", "[REDACTED]")
}

// A start's failure leaves the agent in its report, so it is redacted as the
// log line that says it is.
func TestAFailedStartsReportIsRedacted(t *testing.T) {
	resetBoot(t)
	keployLog.SetRedactor(redactingSecret{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })
	FailBoot(errors.New("dial postgres://app:hunter2@db:5432: refused"))
	if p := snapshotBoot(0, false); p.Failure != "dial postgres://app:[REDACTED]@db:5432: refused" {
		t.Fatalf("reported %q; want it redacted", p.Failure)
	}
}

// manualBootClock has the reporter beat on clk, the agent's runnable time as
// the test drives it, for the rest of the test.
func manualBootClock(t *testing.T) *stall.ManualClock {
	t.Helper()
	clk := &stall.ManualClock{}
	was := bootClock
	bootClock = func() (stall.Clock, func()) { return clk, func() {} }
	t.Cleanup(func() { bootClock = was })
	return clk
}

// awaitBootReport returns the first report at path that want accepts.
func awaitBootReport(t *testing.T, path string, want func(BootProgress) bool) BootProgress {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for {
		if p, ok := readBoot(t, path); ok && want(p) {
			return p
		}
		if time.Now().After(deadline) {
			p, _ := readBoot(t, path)
			t.Fatalf("no such report; the last: %+v", p)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitPending returns once the reporter waits on clk again: whatever it was
// woken for is done.
func awaitPending(t *testing.T, clk *stall.ManualClock) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for clk.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the reporter never waited on its clock again")
		}
		time.Sleep(time.Millisecond)
	}
}

// A beat is BootBeat of the agent's own runnable time, not of wall time: an
// agent the host starves beats only as fast as it gets to run, so its start,
// which runs no faster, is not charged beats it could not spend. Wall time
// passing alone (the clock's timers firing, the agent not run) is no beat,
// however often; each BootBeat it could run is one, and so is each of several
// it ran between two looks.
func TestBootBeatsAreTheAgentsRunnableTime(t *testing.T) {
	resetBoot(t)
	clk := manualBootClock(t)
	bootBeat = time.Second
	path := filepath.Join(t.TempDir(), BootProgressFileName)
	stop := make(chan struct{})
	reported := reportBootProgress(context.Background(), zap.NewNop(), pathSink(path), stop)
	// Gone before the next test: a reporter left running takes its kicks.
	t.Cleanup(func() { close(stop); <-reported })
	awaitBootReport(t, path, func(BootProgress) bool { return true })
	for i := 0; i < 100; i++ {
		awaitPending(t, clk)
		clk.Pass()
	}
	awaitPending(t, clk)
	if p, _ := readBoot(t, path); p.Beat != 0 {
		t.Fatalf("beat %d after wall time alone, want none: a starved agent was charged beats it could not run", p.Beat)
	}
	clk.Advance(time.Second / 2)
	awaitPending(t, clk)
	if p, _ := readBoot(t, path); p.Beat != 0 {
		t.Fatalf("beat %d after half a beat of runnable time, want none", p.Beat)
	}
	clk.Advance(time.Second / 2)
	awaitBootReport(t, path, func(p BootProgress) bool { return p.Beat == 1 })
	awaitPending(t, clk)
	clk.Advance(3 * time.Second)
	awaitBootReport(t, path, func(p BootProgress) bool { return p.Beat == 4 })
	// What it ran past its last beat counts toward the next.
	awaitPending(t, clk)
	clk.Advance(1500 * time.Millisecond)
	awaitBootReport(t, path, func(p BootProgress) bool { return p.Beat == 5 })
	awaitPending(t, clk)
	clk.Advance(time.Second / 2)
	awaitBootReport(t, path, func(p BootProgress) bool { return p.Beat == 6 })
}

// The agent counts its beats without progress itself, beat by beat: the beats
// since its start's last step at which no stage of it had made progress and no
// prompt was up (BootProgress.Quiet). A step starts the count again, and so
// does a beat at which a stage made progress. The CLI, which reads only the
// latest report, gives the start up by this count, not by what it read.
func TestTheAgentCountsItsOwnBeatsWithoutProgress(t *testing.T) {
	resetBoot(t)
	clk := manualBootClock(t)
	bootBeat = time.Second
	probe := &recordedProbe{}
	was := newWorkProbe
	newWorkProbe = func() workProbe { return probe }
	t.Cleanup(func() { newWorkProbe = was })
	path := filepath.Join(t.TempDir(), BootProgressFileName)
	stop := make(chan struct{})
	reported := reportBootProgress(context.Background(), zap.NewNop(), pathSink(path), stop)
	// The reporter is gone before the probe is put back.
	t.Cleanup(func() { close(stop); <-reported })
	BootStep("the agent's process runs")
	awaitBootReport(t, path, func(p BootProgress) bool { return p.Seq == 1 && p.CountsQuiet })
	// beat has the agent run one more beat, its stage's probe answering
	// progress at it. The probe is set once the reporter waits on its clock
	// again: it is done reading it.
	beat := func(progress bool, want func(BootProgress) bool) BootProgress {
		t.Helper()
		awaitPending(t, clk)
		probe.progress = progress
		clk.Advance(time.Second)
		return awaitBootReport(t, path, want)
	}
	for q := uint64(1); q <= 3; q++ {
		beat(false, func(p BootProgress) bool { return p.Beat == q && p.Quiet == q })
	}
	BootStep("the CA's install is over")
	if p := awaitBootReport(t, path, func(p BootProgress) bool { return p.Seq == 2 }); p.Quiet != 0 || p.Step != "the CA's install is over" {
		t.Fatalf("report %+v after a step: want no beat without progress, and the step named", p)
	}
	end := BeginBootWork("the proxy's start")
	beat(true, func(p BootProgress) bool { return p.Beat == 4 })
	p := beat(true, func(p BootProgress) bool { return p.Beat == 5 })
	if p.Quiet != 0 || !p.Busy || len(p.Stages) != 1 || p.Stages[0].Name != "the proxy's start" || !p.Stages[0].Progressing {
		t.Fatalf("report %+v while a stage makes progress: want no beat without progress, the stage named and progressing", p)
	}
	beat(false, func(p BootProgress) bool { return p.Beat == 6 && p.Quiet == 1 })
	p = beat(false, func(p BootProgress) bool { return p.Beat == 7 })
	if p.Quiet != 2 || p.Busy || len(p.Stages) != 1 || p.Stages[0].Progressing || p.Step != "the proxy's start began" {
		t.Fatalf("report %+v while a stage makes none: want 2 beats without progress, the stage named, not progressing, and its start the last step", p)
	}
	beat(true, func(p BootProgress) bool { return p.Beat == 8 && p.Quiet == 0 })
	awaitPending(t, clk)
	end()
}
