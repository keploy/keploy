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
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// resetBoot is the start's state as a fresh process has it; restored after
// the test.
func resetBoot(t *testing.T) {
	t.Helper()
	reset := func() {
		bootSeq.Store(0)
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

	BootStep()
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

// Work in progress is a step as it begins and as it ends, and reported busy
// while it makes progress, or cannot be seen not to.
func TestBootWorkIsReportedBusy(t *testing.T) {
	resetBoot(t)
	if bootBusy() {
		t.Fatal("busy with no work in progress")
	}
	before := bootSeq.Load()
	end := BeginBootWork()
	if bootSeq.Load() != before+1 {
		t.Fatal("the work's start is no step")
	}
	if !bootBusy() {
		t.Fatal("work just begun is not busy")
	}
	end()
	end()
	if bootSeq.Load() != before+2 {
		t.Fatalf("the work's end took %d steps, want one", bootSeq.Load()-before-1)
	}
	if bootBusy() {
		t.Fatal("busy once the work ended")
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
	end := BeginBootPrompt()
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
// end are still steps.
func TestBootWorkWhoseProgressCannotBeSeenIsNotBusy(t *testing.T) {
	resetBoot(t)
	probe := newWorkProbe
	newWorkProbe = func() workProbe { return nil }
	t.Cleanup(func() { newWorkProbe = probe })
	before := bootSeq.Load()
	end := BeginBootWork()
	if bootBusy() {
		t.Fatal("work whose progress cannot be seen is busy")
	}
	end()
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
	BootStep()
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
