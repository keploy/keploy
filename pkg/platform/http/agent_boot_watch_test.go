package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// bootLooks drives a judge: each look advances keploy's runnable time by
// step first, then looks at report.
type bootLooks struct {
	t     *testing.T
	clk   *stall.ManualClock
	judge *bootJudge
}

func newBootLooks(t *testing.T) *bootLooks {
	clk := &stall.ManualClock{}
	return &bootLooks{t: t, clk: clk, judge: newBootJudge(clk)}
}

func (b *bootLooks) look(step time.Duration, p utils.BootProgress, ok, running bool) (bootVerdict, string) {
	b.clk.Advance(step)
	return b.judge.look(p, ok, running)
}

// A start that takes steps is waited for however long it takes, on a host
// however slow: the budget of wall time that ended it is gone.
func TestABootThatTakesStepsIsWaitedForHoweverLong(t *testing.T) {
	b := newBootLooks(t)
	for beat := uint64(0); beat < 100*bootStallChecks; beat++ {
		// A step every few beats, an hour of keploy's time between looks.
		p := utils.BootProgress{Beat: beat, Seq: beat / 5}
		if v, why := b.look(time.Hour, p, true, true); v != bootWaiting {
			t.Fatalf("beat %d: %v (%s), want waiting", beat, v, why)
		}
	}
}

// A start that takes no step across bootStallChecks of the agent's own beats,
// with no work in progress making progress, has stopped: a stalled start,
// which a fresh one retries. One beat fewer is still waited for.
func TestABootThatStopsTakingStepsIsStalled(t *testing.T) {
	b := newBootLooks(t)
	b.look(0, utils.BootProgress{Beat: 0, Seq: 3}, true, true)
	for beat := uint64(1); beat < bootStallChecks; beat++ {
		if v, _ := b.look(time.Second, utils.BootProgress{Beat: beat, Seq: 3}, true, true); v != bootWaiting {
			t.Fatalf("beat %d of %d without a step: %v, want waiting", beat, bootStallChecks, v)
		}
	}
	v, why := b.look(time.Second, utils.BootProgress{Beat: bootStallChecks, Seq: 3}, true, true)
	if v != bootStalled || !strings.Contains(why, "took no step") {
		t.Fatalf("%d beats without a step: %v (%s), want stalled", bootStallChecks, v, why)
	}
}

// An agent that counts its beats without progress itself is judged by its
// count, not by the beats keploy read: keploy reads only the latest report, and
// charged every beat since its last read to it. Read at beat 10 (busy) and next
// at beat 40, a report that is not busy at 40 says nothing of beats 11 to 39,
// which the agent may have seen busy; it counts one beat without progress, and
// the start goes on. Its own count reaching bootStallChecks stops it.
func TestAStartIsJudgedByTheAgentsOwnCountOfBeatsWithoutProgress(t *testing.T) {
	b := newBootLooks(t)
	if v, why := b.look(0, utils.BootProgress{Beat: 10, Seq: 5, Busy: true, CountsQuiet: true}, true, true); v != bootWaiting {
		t.Fatalf("a busy beat: %v (%s), want waiting", v, why)
	}
	if v, why := b.look(time.Second, utils.BootProgress{Beat: 10 + bootStallChecks, Seq: 5, Quiet: 1, CountsQuiet: true}, true, true); v != bootWaiting {
		t.Fatalf("%d beats read at once, the agent counting one without progress: %v (%s), want waiting", bootStallChecks, v, why)
	}
	v, why := b.look(time.Second, utils.BootProgress{Beat: 11 + bootStallChecks, Seq: 5, Quiet: bootStallChecks, CountsQuiet: true}, true, true)
	if v != bootStalled || !strings.Contains(why, "took no step") {
		t.Fatalf("the agent counting %d beats without progress: %v (%s), want stalled", bootStallChecks, v, why)
	}

	// An older agent's report, without the count, is judged by keploy's.
	b = newBootLooks(t)
	b.look(0, utils.BootProgress{Beat: 10, Seq: 5, Busy: true}, true, true)
	if v, _ := b.look(time.Second, utils.BootProgress{Beat: 10 + bootStallChecks, Seq: 5}, true, true); v != bootStalled {
		t.Fatalf("an older agent's report: %v, want keploy's own count, stalled", v)
	}
}

// A stopped start says where it was: the last step it took, and each stage in
// progress with its thread's state and the kernel function it sleeps in. That
// is what tells a stage waiting on a lock from one waiting on another
// goroutine, which a bare "took no step" did not.
func TestAStalledStartSaysWhereItWas(t *testing.T) {
	b := newBootLooks(t)
	p := utils.BootProgress{Beat: 41, Seq: 9, Quiet: bootStallChecks, CountsQuiet: true, Step: "the proxy listens",
		Stages: []utils.BootStage{{Name: "the proxy listener's start", State: "D", Wait: "rtnl_lock"}, {Name: "the proxy's start", State: "S"}},
		Errors: 1, Error: "failed to attach the udp4 sendmsg hook"}
	v, why := b.look(0, p, true, true)
	want := "its start took no step across 30 of its beats, with no work in progress making progress; its last step: the proxy listens; in progress: the proxy listener's start (its thread in an uninterruptible wait, in rtnl_lock), the proxy's start (its thread sleeping) (it logged 1 ERROR(s) while starting, the last: failed to attach the udp4 sendmsg hook)"
	if v != bootStalled || why != want {
		t.Fatalf("%v\n got: %s\nwant: %s", v, why, want)
	}
	b = newBootLooks(t)
	_, why = b.look(0, utils.BootProgress{Beat: 31, Seq: 2, Quiet: bootStallChecks, CountsQuiet: true, Step: "the hooks' load ended"}, true, true)
	if !strings.HasSuffix(why, "; its last step: the hooks' load ended; no stage of it in progress") {
		t.Fatalf("a stall between stages: %s", why)
	}
	// Where the agent cannot see a stage's progress (not on Linux) it still
	// names the stage.
	b = newBootLooks(t)
	_, why = b.look(0, utils.BootProgress{Beat: 31, Seq: 4, Quiet: bootStallChecks, CountsQuiet: true, Step: "the proxy's start began",
		Stages: []utils.BootStage{{Name: "the proxy's start", Unseen: true}}}, true, true)
	if !strings.HasSuffix(why, "; in progress: the proxy's start (its progress cannot be seen here)") {
		t.Fatalf("a stall in an unseen stage: %s", why)
	}
}

// Work in progress that makes progress (the eBPF load, its thread running) is
// no beat without progress, however many beats it takes.
func TestBootWorkInProgressIsNoStall(t *testing.T) {
	b := newBootLooks(t)
	for beat := uint64(0); beat < 10*bootStallChecks; beat++ {
		if v, why := b.look(time.Second, utils.BootProgress{Beat: beat, Seq: 1, Busy: true}, true, true); v != bootWaiting {
			t.Fatalf("beat %d of busy work: %v (%s), want waiting", beat, v, why)
		}
	}
}

// Until the agent's process runs (the runtime still creating or starting its
// container) the start is waited for with no bound; once it runs, its first
// report must come within bootSilence of keploy's runnable time.
func TestAContainerTheRuntimeHasNotStartedIsWaitedForWithNoBound(t *testing.T) {
	b := newBootLooks(t)
	for i := 0; i < 1000; i++ {
		if v, _ := b.look(time.Hour, utils.BootProgress{}, false, false); v != bootWaiting {
			t.Fatalf("look %d at a container not running yet: %v, want waiting", i, v)
		}
	}
	if v, _ := b.look(time.Second, utils.BootProgress{}, false, true); v != bootWaiting {
		t.Fatalf("the container has just started: %v, want waiting", v)
	}
	if v, _ := b.look(bootSilence-2*time.Second, utils.BootProgress{}, false, true); v != bootWaiting {
		t.Fatalf("short of bootSilence of runnable time since it runs: %v, want waiting", v)
	}
	if v, why := b.look(2*time.Second, utils.BootProgress{}, false, true); v != bootStalled || !strings.Contains(why, "without reporting its start") {
		t.Fatalf("bootSilence of runnable time since it runs, with no report: %v (%s), want stalled", v, why)
	}

	// Paused before its first report (not running again) it is waited for
	// with no bound, and once it runs again its first report is due within
	// the whole of bootSilence, from then.
	b = newBootLooks(t)
	b.look(0, utils.BootProgress{}, false, true)
	if v, _ := b.look(bootSilence-time.Second, utils.BootProgress{}, false, true); v != bootWaiting {
		t.Fatalf("short of bootSilence since it runs: %v, want waiting", v)
	}
	for i := 0; i < 100; i++ {
		if v, _ := b.look(time.Hour, utils.BootProgress{}, false, false); v != bootWaiting {
			t.Fatalf("look %d at a paused container: %v, want waiting", i, v)
		}
	}
	b.look(0, utils.BootProgress{}, false, true)
	if v, why := b.look(bootSilence-time.Second, utils.BootProgress{}, false, true); v != bootWaiting {
		t.Fatalf("short of bootSilence since it runs again: %v (%s), want waiting", v, why)
	}
	if v, _ := b.look(time.Second, utils.BootProgress{}, false, true); v != bootStalled {
		t.Fatalf("bootSilence since it runs again, with no report: %v, want stalled", v)
	}
}

// From its first report on, the agent's own beats judge its start, not
// keploy's time: an agent that stops beating (frozen, or starved in a VM keploy
// is not) is not given up on for keploy's time passing. Its first report is
// due within bootSilence of keploy's runnable time, not its wall time: a keploy
// starved with it accrues none, and waits.
func TestAnAgentIsJudgedByItsOwnBeatsOnceItHasReported(t *testing.T) {
	b := newBootLooks(t)
	p := utils.BootProgress{Beat: 4, Seq: 2}
	b.look(0, p, true, true)
	for i := 0; i < 1000; i++ {
		if v, why := b.look(time.Hour, p, true, true); v != bootWaiting {
			t.Fatalf("look %d at a report that has not changed: %v (%s), want waiting", i, v, why)
		}
	}
	// Unreadable for a while (a `cp` that fails) is no new sign either.
	if v, why := b.look(time.Hour, utils.BootProgress{}, false, true); v != bootWaiting {
		t.Fatalf("an unreadable report after the first: %v (%s), want waiting", v, why)
	}

	b = newBootLooks(t)
	for i := 0; i < 1000; i++ {
		if v, _ := b.look(0, utils.BootProgress{}, false, true); v != bootWaiting {
			t.Fatalf("look %d with no runnable time passed: %v, want waiting", i, v)
		}
	}
	if v, why := b.look(bootSilence, utils.BootProgress{}, false, true); v != bootStalled || !strings.Contains(why, "without reporting its start") {
		t.Fatalf("bootSilence of runnable time with no first report: %v (%s), want stalled", v, why)
	}
}

// Beats while a prompt waits for the user (Windows' dialog asking to trust the
// CA) are no beats without progress, however many: the user answers in their
// own time.
func TestABootPromptIsNoStall(t *testing.T) {
	b := newBootLooks(t)
	for beat := uint64(0); beat < 10*bootStallChecks; beat++ {
		if v, why := b.look(time.Second, utils.BootProgress{Beat: beat, Seq: 4, Prompt: true}, true, true); v != bootWaiting {
			t.Fatalf("beat %d at the prompt: %v (%s), want waiting", beat, v, why)
		}
	}
}

// An agent that says its server listens while its health check does not
// answer (a port the runtime does not forward) is stalled, once its health
// check has had bootSilence of keploy's runnable time to answer.
func TestAReadyAgentWhoseHealthCheckDoesNotAnswerIsStalled(t *testing.T) {
	b := newBootLooks(t)
	p := utils.BootProgress{Beat: 7, Seq: 9, Ready: true}
	b.look(0, p, true, true)
	if v, _ := b.look(bootSilence-time.Second, p, true, true); v != bootWaiting {
		t.Fatalf("short of bootSilence: %v, want waiting", v)
	}
	if v, why := b.look(time.Second, p, true, true); v != bootStalled || !strings.Contains(why, "has not answered keploy") {
		t.Fatalf("%v (%s), want stalled", v, why)
	}
}

// A start the agent reports failed fails at once, with that failure: a retry
// would fail the same way.
func TestAStartTheAgentReportsFailedFails(t *testing.T) {
	b := newBootLooks(t)
	v, why := b.look(0, utils.BootProgress{Beat: 1, Seq: 2, Failed: true, ExitCode: utils.ExitPrivilegeRequired, Failure: "failed to hook into the app: privileges"}, true, true)
	if v != bootFailed || why != "failed to hook into the app: privileges" {
		t.Fatalf("%v (%s), want failed with the agent's failure", v, why)
	}
}

// An ERROR the agent logged while starting is no failure of its start: it goes
// on from many on purpose (a hook it does without, a CA store it could not
// write). It is detail for a start that stalls.
func TestAnErrorTheAgentLoggedIsNoFailureOfItsStart(t *testing.T) {
	b := newBootLooks(t)
	b.look(0, utils.BootProgress{Beat: 0, Seq: 2}, true, true)
	for beat := uint64(1); beat < bootStallChecks; beat++ {
		p := utils.BootProgress{Beat: beat, Seq: 2, Errors: 1, Error: "failed to attach the udp4 sendmsg hook"}
		if v, why := b.look(time.Second, p, true, true); v != bootWaiting {
			t.Fatalf("beat %d after an ERROR: %v (%s), want waiting", beat, v, why)
		}
	}
	v, why := b.look(time.Second, utils.BootProgress{Beat: bootStallChecks, Seq: 2, Errors: 2, Error: "failed to set up the CA"}, true, true)
	if v != bootStalled || !strings.Contains(why, "2 ERROR(s)") || !strings.Contains(why, "failed to set up the CA") {
		t.Fatalf("%v (%s), want stalled, naming the last ERROR", v, why)
	}
}

// waitForAgent reads a native agent's report from the file next to its
// teardown progress file, and fails a start the agent reports failed with that
// failure and the cause its exit code names, armed as this process's own, as
// its exit would: not as a stalled start a retry would start again. The agent,
// which may live on after the failure, is stopped.
func TestWaitForAgentFailsAStartItsAgentReportsFailed(t *testing.T) {
	look := bootLook
	bootLook = time.Millisecond
	t.Cleanup(func() { bootLook = look })
	path := filepath.Join(t.TempDir(), utils.BootProgressFileName)
	writeBoot(t, path, utils.BootProgress{Beat: 1, Seq: 2, Failed: true, ExitCode: utils.ExitPrivilegeRequired, Failure: "failed to hook into the app: privileges"})
	a := clientFor(newStandIn(t, drops).url)
	stopped := false
	a.agentCancel = func() { stopped = true }
	a.run = newAgentRun()
	a.run.bootReport = path
	err := waitWithin(t, a, context.Background(), nil)
	if !errors.Is(err, utils.ErrPrivilegeRequired) || errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "failed to hook into the app") {
		t.Fatalf("waitForAgent = %v; want the agent's failure, with the cause its code names", err)
	}
	if utils.ErrCode != utils.ExitPrivilegeRequired {
		t.Fatalf("armed exit code %d, want %d", utils.ErrCode, utils.ExitPrivilegeRequired)
	}
	if !stopped {
		t.Fatal("the agent whose start failed was left running")
	}
}

// An agent that logs an ERROR on its way to exiting for want of privileges (it
// logs "failed to load hooks", tears down, then exits 3) fails the start with
// its exit's cause, however the looks fall: the ERROR read first is no failure.
func TestAnErrorReadBeforeTheAgentsExitLeavesItsExitCode(t *testing.T) {
	look := bootLook
	bootLook = time.Millisecond
	t.Cleanup(func() { bootLook = look })
	path := filepath.Join(t.TempDir(), utils.BootProgressFileName)
	writeBoot(t, path, utils.BootProgress{Beat: 1, Seq: 2, Errors: 1, Error: "failed to load hooks"})
	a := clientFor(newStandIn(t, drops).url)
	a.agentCancel = func() {}
	a.run = newAgentRun()
	a.run.bootReport = path
	exited := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond) // many looks at the ERROR first
		exited <- agentExit(t, utils.ExitPrivilegeRequired)
	}()
	err := waitWithin(t, a, context.Background(), exited)
	if !errors.Is(err, utils.ErrPrivilegeRequired) || utils.ErrCode != utils.ExitPrivilegeRequired {
		t.Fatalf("waitForAgent = %v, armed code %d; want the exit's cause, code %d", err, utils.ErrCode, utils.ExitPrivilegeRequired)
	}
}

// A look that never comes back (inspect says the container runs, and the
// `docker cp` of its report hangs on a wedged daemon) holds up no rule: the
// first report is still due within bootSilence of keploy's runnable time.
func TestAHungLookHoldsUpNoRule(t *testing.T) {
	look, silence, bootLookFn := bootLook, bootSilence, containerBootLook
	bootLook, bootSilence = time.Millisecond, 50*time.Millisecond
	containerBootLook = func(*zap.Logger, *namedContainer) (func(context.Context) (utils.BootProgress, bool), func(context.Context) bool) {
		read := func(ctx context.Context) (utils.BootProgress, bool) {
			<-ctx.Done() // the daemon never answers
			return utils.BootProgress{}, false
		}
		return read, func(context.Context) bool { return true }
	}
	t.Cleanup(func() { bootLook, bootSilence, containerBootLook = look, silence, bootLookFn })
	a := clientFor(newStandIn(t, drops).url)
	a.agentCancel = func() {}
	done := make(chan error, 1)
	go func() { done <- a.waitForAgent(context.Background(), nil, true, models.SetupOptions{}) }()
	select {
	case err := <-done:
		if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "without reporting its start") {
			t.Fatalf("waitForAgent = %v; want a stalled start, its first report not made", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a look that does not come back held up the wait")
	}
}

func writeBoot(t *testing.T, path string, p utils.BootProgress) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// An unanswered question to the container engine's daemon is given up on by
// keploy's runnable time, not its wall time: on a clock that has not moved, a
// bound far shorter than the wall time passed has not run out.
func TestWithRunnableBoundEndsOnKeploysRunnableTime(t *testing.T) {
	clk := &stall.ManualClock{}
	boundClock = clk
	t.Cleanup(func() { boundClock = nil })
	ctx, release := withRunnableBound(context.Background(), 20*time.Millisecond, errDaemonSilent)
	defer release()
	// Each move of the clock once the bound's look at it is armed: one made
	// before would fire no timer, and the bound would wait for another.
	armed := func() {
		for clk.Pending() == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(200 * time.Millisecond)
	armed()
	clk.Pass() // wall time passed, keploy could not run
	select {
	case <-ctx.Done():
		t.Fatalf("the bound ran out on wall time alone: %v", context.Cause(ctx))
	case <-time.After(50 * time.Millisecond):
	}
	armed()
	clk.Advance(20 * time.Millisecond)
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the bound did not run out once keploy had had it of runnable time")
	}
	if !errors.Is(context.Cause(ctx), errDaemonSilent) {
		t.Fatalf("cause %v, want errDaemonSilent", context.Cause(ctx))
	}
	ctx, release = withRunnableBound(context.Background(), time.Hour, errDaemonSilent)
	release()
	<-ctx.Done()
	if errors.Is(context.Cause(ctx), errDaemonSilent) {
		t.Fatal("a released bound ended as one that ran out")
	}
}

// An agent started by a sudo that may prompt for the password has not begun to
// run until sudo runs it: the user answers the prompt in their own time, and
// keploy waits for them with no bound, until they stop it.
func TestWaitForAgentWaitsForSudosPromptWithNoBound(t *testing.T) {
	look, silence := bootLook, bootSilence
	bootLook, bootSilence = time.Millisecond, 0
	t.Cleanup(func() { bootLook, bootSilence = look, silence })
	a := clientFor(newStandIn(t, drops).url)
	a.agentCancel = func() {}
	a.run = newAgentRun()
	a.run.bootPrompt = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.waitForAgent(ctx, nil, false, models.SetupOptions{}) }()
	select {
	case err := <-done:
		t.Fatalf("waitForAgent = %v while sudo's prompt waits for the user; want it waiting", err)
	case <-time.After(200 * time.Millisecond): // many looks, with no silence allowed
	}
	cancel()
	if err := <-done; errors.Is(err, pkg.ErrAgentNotReady) {
		t.Fatalf("waitForAgent = %v; want the user's stop, not a stalled start", err)
	}
}

// A native agent's report is read next to its teardown progress file, and a
// sudo that may prompt in front of it is waited for with no bound.
func TestANativeAgentsStartIsWatchedWhereItReports(t *testing.T) {
	r := newAgentRun()
	r.watchBoot(filepath.Join("/run", "keploy-1", "teardown.json"), true, "/usr/local/bin/keploy")
	if want := filepath.Join("/run", "keploy-1", utils.BootProgressFileName); r.bootReport != want || !r.bootPrompt || r.agentComm != "keploy" {
		t.Fatalf("report %q, prompt %v, name %q; want %q, true, keploy", r.bootReport, r.bootPrompt, r.agentComm, want)
	}
	r = newAgentRun()
	r.watchBoot("", false, "/opt/keploy-enterprise-linux-amd64")
	if r.bootReport != "" || r.bootPrompt || r.agentComm != "keploy-enterpri" {
		t.Fatalf("report %q, prompt %v, name %q; want none, the kernel's 15 bytes of the name", r.bootReport, r.bootPrompt, r.agentComm)
	}
	for _, tc := range []struct {
		elevates, cached bool
		euid             int
		want             bool
	}{
		{true, false, 1000, true}, // plain sudo, on a PTY or not
		{true, true, 1000, false}, // sudo -n
		{true, false, 0, false},   // root: no sudo
		{false, false, 1000, false},
	} {
		if got := sudoMayPrompt(tc.elevates, tc.cached, tc.euid); got != tc.want {
			t.Errorf("sudoMayPrompt(%v, %v, %d) = %v, want %v", tc.elevates, tc.cached, tc.euid, got, tc.want)
		}
	}
}

// An agent that said its server listens, then stopped running (its container
// paused, or one compose starts again), is waited for with no bound while it
// does not run: its health check is due within bootSilence of keploy's
// runnable time once it runs again, from then.
func TestAReadyAgentThatDoesNotRunIsWaitedForWithNoBound(t *testing.T) {
	b := newBootLooks(t)
	ready := utils.BootProgress{Beat: 3, Seq: 9, Ready: true}
	b.look(0, ready, true, true)
	for i := 0; i < 100; i++ {
		if v, why := b.look(time.Hour, ready, true, false); v != bootWaiting {
			t.Fatalf("look %d at a ready agent whose container is paused: %v (%s), want waiting", i, v, why)
		}
	}
	b.look(0, ready, true, true)
	if v, why := b.look(bootSilence-time.Second, ready, true, true); v != bootWaiting {
		t.Fatalf("short of bootSilence since it runs again: %v (%s), want waiting", v, why)
	}
	if v, why := b.look(time.Second, ready, true, true); v != bootStalled || !strings.Contains(why, "has not answered keploy") {
		t.Fatalf("bootSilence since it runs again, its health check silent: %v (%s), want stalled", v, why)
	}
}

// scriptedContainer stands in for the agent's container (containerBootLook,
// containerEnd): what each look finds of it is what the test last set.
type scriptedContainer struct {
	mu      sync.Mutex
	running bool
	p       utils.BootProgress
	ok      bool
	ended   bool
	code    int
	looks   int
}

func (c *scriptedContainer) set(running bool, p utils.BootProgress, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running, c.p, c.ok = running, p, ok
}

func (c *scriptedContainer) exit(code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running, c.ended, c.code = false, true, code
}

func (c *scriptedContainer) looked() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.looks
}

// install makes it the container every look looks at.
func (c *scriptedContainer) install(t *testing.T) {
	t.Helper()
	look, silence, bootLookFn, endFn := bootLook, bootSilence, containerBootLook, containerEnd
	bootLook, bootSilence = time.Millisecond, 50*time.Millisecond
	containerBootLook = func(*zap.Logger, *namedContainer) (func(context.Context) (utils.BootProgress, bool), func(context.Context) bool) {
		read := func(context.Context) (utils.BootProgress, bool) {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.p, c.ok
		}
		running := func(context.Context) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.looks++
			return c.running
		}
		return read, running
	}
	containerEnd = func(*zap.Logger, *namedContainer) func(context.Context) (int, bool) {
		return func(context.Context) (int, bool) {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.code, c.ended
		}
	}
	t.Cleanup(func() { bootLook, bootSilence, containerBootLook, containerEnd = look, silence, bootLookFn, endFn })
}

// awaitLooks waits until the watch has looked at c n more times, so a test
// knows the watch has seen what it set.
func (c *scriptedContainer) awaitLooks(t *testing.T, n int) {
	t.Helper()
	want := c.looked() + n
	deadline := time.Now().Add(30 * time.Second)
	for c.looked() < want {
		if time.Now().After(deadline) {
			t.Fatalf("the watch looked %d times, want %d", c.looked(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A compose wait watches the start of the agent docker compose starts, by its
// container (WatchAgentStart): nothing while the engine has not run the
// container, however many looks that takes, nor while a running agent's start
// takes steps; the agent's own failure, with the cause its exit code names,
// once it reports one, armed as this process's code; a stalled start once it
// stops taking steps.
func TestWatchAgentStartFollowsTheStartOfAComposeAgent(t *testing.T) {
	keepCode := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = keepCode })
	for _, tc := range []struct {
		name  string
		end   func(c *scriptedContainer)
		want  func(error) bool
		armed int
	}{
		{"failed", func(c *scriptedContainer) {
			c.set(true, utils.BootProgress{Beat: 9, Seq: 4, Failed: true, ExitCode: utils.ExitPrivilegeRequired, Failure: "failed to load eBPF"}, true)
		}, func(err error) bool {
			return errors.Is(err, utils.ErrPrivilegeRequired) && strings.Contains(err.Error(), "failed to load eBPF")
		}, utils.ExitPrivilegeRequired},
		{"stalled", func(c *scriptedContainer) {
			c.set(true, utils.BootProgress{Beat: 9 + bootStallChecks, Seq: 4}, true)
		}, func(err error) bool {
			return errors.Is(err, pkg.ErrAgentNotReady) && strings.Contains(err.Error(), "took no step")
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			utils.ErrCode = 0
			c := &scriptedContainer{}
			c.install(t)
			a := clientFor("http://127.0.0.1:1")
			a.agentContainer = &namedContainer{name: "keploy-v3-test"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errs := a.WatchAgentStart(ctx)
			// Not created yet, then not started: many looks, and a long
			// time, with nothing said.
			c.awaitLooks(t, 200)
			select {
			case err := <-errs:
				t.Fatalf("a container the engine has not run ended the wait: %v", err)
			default:
			}
			for beat := uint64(0); beat < 3*bootStallChecks; beat++ {
				c.set(true, utils.BootProgress{Beat: beat, Seq: beat}, true)
				c.awaitLooks(t, 1)
			}
			c.set(true, utils.BootProgress{Beat: 9, Seq: 4}, true)
			c.awaitLooks(t, 2)
			select {
			case err := <-errs:
				t.Fatalf("a start taking steps ended the wait: %v", err)
			default:
			}
			tc.end(c)
			select {
			case err := <-errs:
				if !tc.want(err) {
					t.Fatalf("the watch said %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the watch said nothing")
			}
			if utils.ErrCode != tc.armed {
				t.Fatalf("armed exit code %d, want %d", utils.ErrCode, tc.armed)
			}
		})
	}
	// No agent at all, not even a URI: nothing to watch, a channel that
	// never says.
	if errs := clientFor("").WatchAgentStart(context.Background()); errs != nil {
		t.Fatal("a client with no agent watched one")
	}
}

// steppingClock is a runnable clock that moves by a second at every read: a
// keploy that runs flat out between looks.
type steppingClock struct{ stall.ManualClock }

func (c *steppingClock) Runnable() time.Duration {
	c.Advance(time.Second)
	return c.ManualClock.Runnable()
}

// fastURILooks makes the looks at an agent known only by its URI quick, and
// keploy's runnable time move a second at each.
func fastURILooks(t *testing.T) {
	t.Helper()
	look, silence, clk, every := bootLook, bootSilence, uriClock, stillWaitingLooks
	bootLook, bootSilence, uriClock, stillWaitingLooks = time.Millisecond, 5*time.Second, &steppingClock{}, 5
	t.Cleanup(func() { bootLook, bootSilence, uriClock, stillWaitingLooks = look, silence, clk, every })
}

// An agent this client did not set up (a sidecar another process started) is
// watched at its URI. Where nothing takes a connection, no agent is there:
// once nothing has for bootSilence of keploy's runnable time, the wait ends,
// saying so, as a start that made no progress; never a wait that goes on in
// silence.
func TestAnAgentURIWhereNothingAnswersIsGivenUpOn(t *testing.T) {
	fastURILooks(t)
	uri := "http://" + testnet.RefusedAddr(t, "tcp") + "/agent"
	core, logs := observer.New(zapcore.InfoLevel)
	a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	select {
	case err := <-a.WatchAgentStart(context.Background()):
		if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "nothing answers at "+uri) {
			t.Fatalf("the watch said %v; want that nothing answers at the URI", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a URI where nothing answers was waited on")
	}
	if logs.FilterMessageSnippet("still waiting for the keploy agent at "+uri).Len() == 0 {
		t.Fatal("the wait said nothing while it waited")
	}
}

// Where something takes connections at the URI (the agent, whose health check
// answers only once its start is over, or a forward in front of it), keploy
// waits on however long that takes, past bootSilence many times over, and
// says now and then that it still waits, and what it found.
func TestAnAgentURIThatTakesConnectionsIsWaitedFor(t *testing.T) {
	fastURILooks(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	uri := "http://" + ln.Addr().String() + "/agent"
	core, logs := observer.New(zapcore.InfoLevel)
	a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := a.WatchAgentStart(ctx)
	deadline := time.Now().Add(30 * time.Second)
	// 20 lines, 5 looks apart: 100 looks, 100 s of keploy's runnable time,
	// 20 times bootSilence.
	for logs.FilterMessageSnippet("still waiting for the keploy agent at "+uri).Len() < 20 {
		select {
		case err := <-errs:
			t.Fatalf("an agent that takes connections was given up on: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("said it still waits %d times, want 20", logs.FilterMessageSnippet("still waiting").Len())
		}
		time.Sleep(time.Millisecond)
	}
	line := logs.FilterMessageSnippet("still waiting for the keploy agent at " + uri).All()[0]
	if !strings.Contains(line.Message, "something takes connections there") {
		t.Fatalf("the line says %q; want what the look found", line.Message)
	}
}

// A compose wait does not end on the agent's container having exited:
// compose's own exit says why (its project aborts over a dependency that
// failed to start). The release of the app (watchAgent's endOnExit) does: a
// container that stopped answers no more.
func TestOnlyTheReleaseEndsOnTheAgentsContainerHavingExited(t *testing.T) {
	for _, endOnExit := range []bool{false, true} {
		c := &scriptedContainer{}
		c.install(t)
		a := clientFor("http://127.0.0.1:1")
		a.agentContainer = &namedContainer{name: "keploy-v3-test"}
		ctx, cancel := context.WithCancel(context.Background())
		errs, _ := a.watchAgent(ctx, endOnExit)
		c.set(true, utils.BootProgress{Beat: 1, Seq: 1, Ready: true}, true)
		c.awaitLooks(t, 2)
		c.exit(137)
		if !endOnExit {
			c.awaitLooks(t, 200)
			select {
			case err := <-errs:
				t.Fatalf("a compose wait ended on the container's exit: %v", err)
			default:
			}
		} else {
			select {
			case err := <-errs:
				if !strings.Contains(err.Error(), "exited with code 137") {
					t.Fatalf("the release ended with %v, want the container's exit", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the release did not end on the container's exit")
			}
		}
		cancel()
	}
}

// The release of the app ends at once on the CA setup failure the agent
// latches, with its words: no wait changes it.
func TestTheReleaseEndsAtOnceOnALatchedCASetupFailure(t *testing.T) {
	var readies atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent/ready") {
			readies.Add(1)
			http.Error(w, "CA setup failed: open /tmp/keploy-tls/ca.crt: read-only file system", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KEPLOY_PROVEN", "")
	a := clientFor(srv.URL)
	err := a.MakeAgentReadyForDockerCompose(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read-only file system") || readies.Load() != 1 {
		t.Fatalf("MakeAgentReadyForDockerCompose = %v after %d asks; want the latched failure, after one", err, readies.Load())
	}
}

// The release ends on the watch's verdict even while a request to
// /agent/ready is in flight: an agent that takes the connection and never
// answers holds the request, not the release.
func TestTheReleaseEndsOnTheWatchWhileAReadyRequestHangs(t *testing.T) {
	c := &scriptedContainer{}
	c.install(t)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent/ready") {
			select {
			case <-block:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })
	t.Setenv("KEPLOY_PROVEN", "")
	a := clientFor(srv.URL)
	a.agentContainer = &namedContainer{name: "keploy-v3-test"}
	c.set(true, utils.BootProgress{Beat: 1, Seq: 1, Ready: true}, true)
	done := make(chan error, 1)
	go func() { done <- a.MakeAgentReadyForDockerCompose(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "did not take its release") {
			t.Fatalf("the release ended with %v; want the watch's verdict", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the release is still blocked; the watch looked %d times", c.looked())
	}
}

// The release ends once the agent's container has exited, as that exit, not
// as a start that stalled. One whose last report said its start failed ends
// with that failure, and the cause its code names.
func TestTheReleaseEndsOnTheAgentsContainerHavingExited(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report utils.BootProgress
		code   int
		want   func(error) bool
	}{
		{"ready, then exited", utils.BootProgress{Beat: 1, Seq: 1, Ready: true}, 137, func(err error) bool {
			return !errors.Is(err, pkg.ErrAgentNotReady) && strings.Contains(err.Error(), "the keploy agent's container exited with code 137") &&
				!strings.Contains(err.Error(), "could not start")
		}},
		// A docker run --rm agent that exited: the engine no longer knows
		// its container.
		{"ready, then removed", utils.BootProgress{Beat: 1, Seq: 1, Ready: true}, containerGone, func(err error) bool {
			return !errors.Is(err, pkg.ErrAgentNotReady) && strings.Contains(err.Error(), "the keploy agent's container is gone (removed)") &&
				!strings.Contains(err.Error(), "could not start")
		}},
		{"its start failed, then exited", utils.BootProgress{Beat: 1, Seq: 1, Failed: true, ExitCode: utils.ExitPrivilegeRequired, Failure: "failed to load eBPF"}, 137, func(err error) bool {
			return errors.Is(err, utils.ErrPrivilegeRequired) && strings.Contains(err.Error(), "failed to load eBPF")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keepCode := utils.ErrCode
			t.Cleanup(func() { utils.ErrCode = keepCode })
			c := &scriptedContainer{}
			c.install(t)
			// The agent takes no release: its /agent/ready answers 503, as
			// one still installing its CA.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(srv.Close)
			t.Setenv("KEPLOY_PROVEN", "")
			a := clientFor(srv.URL)
			a.agentContainer = &namedContainer{name: "keploy-v3-test"}
			c.set(true, tc.report, true)
			c.exit(tc.code)
			done := make(chan error, 1)
			go func() { done <- a.MakeAgentReadyForDockerCompose(context.Background()) }()
			select {
			case err := <-done:
				if !tc.want(err) {
					t.Fatalf("the release ended with %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the release did not end on the container's exit")
			}
		})
	}
}

// A stale report that says the agent's server listens (a container compose
// started again, its last run's report still there) does not latch: once the
// new run's beats begin again, the ready rule's bound counts from the new
// run's own ready report, not the stale one's.
func TestAStaleReadyReportDoesNotLatch(t *testing.T) {
	b := newBootLooks(t)
	b.look(0, utils.BootProgress{Beat: 40, Seq: 12, Ready: true, CountsQuiet: true}, true, true)
	for i := uint64(0); i < 70; i++ {
		if v, why := b.look(time.Second, utils.BootProgress{Beat: i, Seq: i, CountsQuiet: true}, true, true); v != bootWaiting {
			t.Fatalf("beat %d of a start taking a step every beat: %v (%s)", i, v, why)
		}
	}
	if v, why := b.look(0, utils.BootProgress{Beat: 70, Seq: 70, Ready: true, CountsQuiet: true}, true, true); v != bootWaiting {
		t.Fatalf("the new run says its server listens this very look, and is given up on: %v (%s)", v, why)
	}
}

// A URI where nothing takes a connection is given up on only once nothing
// has, without a break, for bootSilence of keploy's runnable time: a
// connection taken in between starts the count again.
func TestAnAgentURIsSilenceCountsFromTheLastTakenConnection(t *testing.T) {
	fastURILooks(t)
	// 3 looks refused, 1 taken, 3 refused, then taken for good: never 5
	// refused in a row, at a second of runnable time a look.
	script := []bool{false, false, false, true, false, false, false}
	var n atomic.Int32
	prev := lookAtAgent
	lookAtAgent = func(context.Context, string) (string, bool) {
		i := int(n.Add(1)) - 1
		if i < len(script) {
			return "scripted", script[i]
		}
		return "scripted", true
	}
	t.Cleanup(func() { lookAtAgent = prev })
	a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: "http://127.0.0.1:1/agent"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := a.WatchAgentStart(ctx)
	deadline := time.Now().Add(30 * time.Second)
	for n.Load() < int32(len(script))+20 {
		select {
		case err := <-errs:
			t.Fatalf("the URI was given up on after %d looks: %v", n.Load(), err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the watch looked %d times", n.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

// The wait for an agent compose started again follows its start: a start
// that fails ends it, with that failure, where its health check never
// answers.
func TestTheWaitForARestartedAgentEndsOnItsFailedStart(t *testing.T) {
	keepCode := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = keepCode })
	c := &scriptedContainer{}
	c.install(t)
	uri := "http://" + testnet.RefusedAddr(t, "tcp") + "/agent"
	a := clientFor(uri)
	a.agentContainer = &namedContainer{name: "keploy-v3-test"}
	c.set(true, utils.BootProgress{Beat: 2, Seq: 2, Failed: true, ExitCode: utils.ExitPrivilegeRequired, Failure: "failed to load eBPF"}, true)
	done := make(chan error, 1)
	go func() { done <- a.awaitAgentAnswers(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, utils.ErrPrivilegeRequired) || !strings.Contains(err.Error(), "did not come up") {
			t.Fatalf("awaitAgentAnswers = %v; want the agent's failed start", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the wait for the restarted agent did not end on its failed start")
	}
}

// While the engine does not run the agent's container (compose has not
// started it yet, it is paused, or it stopped under a command that goes on),
// no rule bounds the wait: keploy says every stillWaitingLooks ticks that it
// still waits, and what its latest look found, never a wait that goes on in
// silence. A container that runs is no report of the agent's start: the count
// goes on through it, and a line said while it runs says so. The row runs in
// a bubble (inBubble): each look is answered as it begins, so each tick
// begins one, and the watch takes its answer before the next tick.
func TestAWaitOnAContainerTheEngineDoesNotRunSaysSo(t *testing.T) {
	look, every, silence := bootLook, stillWaitingLooks, bootSilence
	bootLook, stillWaitingLooks, bootSilence = time.Second, 5, time.Hour
	t.Cleanup(func() { bootLook, stillWaitingLooks, bootSilence = look, every, silence })
	inBubble(t, func(t *testing.T) {
		const notRun = "still waiting for the keploy agent: the container engine does not run its container"
		const runsUnread = "still waiting for the keploy agent: its container runs, and keploy has read no report of its start from it yet"
		var runs atomic.Bool
		read := func(context.Context) (utils.BootProgress, bool) { return utils.BootProgress{}, false }
		running := func(context.Context) bool { return runs.Load() }
		core, logs := observer.New(zapcore.InfoLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		judged := watchStart(ctx, zap.New(core), read, running, nil, true, false)
		start := time.Now()
		for i, tick := range []int{5, 10, 15} {
			tickTo(start, tick)
			saidBy(t, logs, judged, tick, i+1, zapcore.InfoLevel, notRun, "with the container not run")
		}
		// It runs from the 16th tick's look on, and reports nothing: said at
		// the 20th, the count going on.
		runs.Store(true)
		tickTo(start, 20)
		saidBy(t, logs, judged, 20, 4, zapcore.InfoLevel, runsUnread, "with the container run since the 16th tick")
		// The engine runs it no more from the 21st tick's look on.
		runs.Store(false)
		tickTo(start, 25)
		saidBy(t, logs, judged, 25, 5, zapcore.InfoLevel, notRun, "with the container not run since the 21st tick")
		// One line at each line's tick, counted on from the watch's start.
		checkStartLineTicks(t, logs.All(), 5*bootLook)
	})
}

// saidBy fails unless the watch has judged nothing and has said n lines by
// the tick, the nth at level and beginning want, and returns that line. why
// says what the row expects of it.
func saidBy(t *testing.T, logs *observer.ObservedLogs, judged <-chan startJudgment, tick, n int, level zapcore.Level, want, why string) observer.LoggedEntry {
	t.Helper()
	notJudged(t, judged)
	lines := logs.All()
	if len(lines) != n {
		t.Fatalf("%d lines said by tick %d; want %d", len(lines), tick, n)
	}
	e := lines[n-1]
	if !strings.HasPrefix(e.Message, want) || e.Level != level {
		t.Fatalf("line %d, at tick %d, %s, says %s %q; want %s %q", n-1, tick, why, e.Level.CapitalString(), e.Message, level.CapitalString(), want)
	}
	return e
}

// saysExactly fails unless line e, said by the tick, says want word for word.
func saysExactly(t *testing.T, e observer.LoggedEntry, tick int, want string) {
	t.Helper()
	if e.Message != want {
		t.Fatalf("the line at tick %d says %q; want %q", tick, e.Message, want)
	}
}

// waitedOf is the wait a line says it has gone on for.
func waitedOf(e observer.LoggedEntry) time.Duration {
	d, _ := e.ContextMap()["waited"].(time.Duration)
	return d
}

// checkStartLineTicks fails unless the lines are one at each line's tick of
// the wait: the ith says it has waited i+1 times every.
func checkStartLineTicks(t *testing.T, lines []observer.LoggedEntry, every time.Duration) {
	t.Helper()
	for i, e := range lines {
		if got, want := waitedOf(e), time.Duration(i+1)*every; got != want {
			t.Fatalf("line %d (%q) says it waited %v; want %v: one line at each line's tick", i, e.Message, got, want)
		}
	}
}

// A report that says the agent's server does not listen ends what an earlier
// one said of it, even when its beats did not go backwards (the last run's
// report read, then the new run's once its beats had passed it): the new
// agent's "ready" is judged from when it says it, not from the stale one.
func TestAReportThatDoesNotListenEndsAnEarlierReady(t *testing.T) {
	b := newBootLooks(t)
	b.look(0, utils.BootProgress{Beat: 40, Seq: 12, Ready: true, CountsQuiet: true}, true, true)
	for i := uint64(50); i < 120; i++ {
		if v, why := b.look(time.Second, utils.BootProgress{Beat: i, Seq: i, CountsQuiet: true}, true, true); v != bootWaiting {
			t.Fatalf("beat %d of a start taking a step every beat: %v (%s)", i, v, why)
		}
	}
	if v, why := b.look(0, utils.BootProgress{Beat: 120, Seq: 120, Ready: true, CountsQuiet: true}, true, true); v != bootWaiting {
		t.Fatalf("the new run says its server listens this very look, and is given up on: %v (%s)", v, why)
	}
}

// Beats that go backwards are another run of the agent: what was seen of the
// last run says nothing of it, a "ready" included, though both say ready (the
// last run's report read, then, after looks that read none, the new run's).
func TestBeatsThatGoBackwardsBeginTheJudgementAgain(t *testing.T) {
	b := newBootLooks(t)
	b.look(0, utils.BootProgress{Beat: 40, Seq: 12, Ready: true, CountsQuiet: true}, true, true)
	if v, why := b.look(70*time.Second, utils.BootProgress{}, false, true); v != bootWaiting {
		t.Fatalf("a look that read no report: %v (%s)", v, why)
	}
	if v, why := b.look(0, utils.BootProgress{Beat: 1, Seq: 1, Ready: true, CountsQuiet: true}, true, true); v != bootWaiting {
		t.Fatalf("the new run says its server listens this very look, and is given up on: %v (%s)", v, why)
	}
}

// WatchContainerAgentStart serves a wait that makes no health check of its
// own (compose's healthcheck is what enterprise's gate waits on): once the
// agent says its server listens, it ends, with no judgment, never the ready
// rule's "has not answered keploy" for a check it does not make.
func TestTheContainerWatchEndsOnceTheAgentSaysItListens(t *testing.T) {
	look, lookFn := bootLook, containerBootLook
	bootLook = time.Millisecond
	t.Cleanup(func() { bootLook, containerBootLook = look, lookFn })
	reads := make(chan struct{}, 16)
	containerBootLook = func(*zap.Logger, *namedContainer) (func(context.Context) (utils.BootProgress, bool), func(context.Context) bool) {
		read := func(context.Context) (utils.BootProgress, bool) {
			select {
			case reads <- struct{}{}:
			default:
			}
			return utils.BootProgress{Beat: 3, Seq: 3, Ready: true, CountsQuiet: true}, true
		}
		return read, func(context.Context) bool { return true }
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := WatchContainerAgentStart(ctx, zap.NewNop(), "keploy-v3-test")
	select {
	case <-reads:
	case <-time.After(30 * time.Second):
		t.Fatal("the agent's report was never read")
	}
	// Ended, the watch reads no more, and judges nothing: at a look a
	// millisecond, a watch that went on would have read again at once (a
	// look begins only once the last is in, so the first read is the
	// watch's only one).
	select {
	case <-reads:
		t.Fatal("the watch went on looking once the agent said its server listens")
	case err := <-errs:
		t.Fatalf("the watch judged a start whose agent said its server listens: %v", err)
	case <-time.After(2 * time.Second):
	}
}

// Whether the engine runs the agent's container is what the engine last
// said: an inspect it does not answer (a busy or restarting daemon) keeps the
// last answer, so that a daemon that errs now and then does not reset the
// judge's bounds look by look; a container that is not there does not run.
func TestWhetherTheContainerRunsIsWhatTheEngineLastSaid(t *testing.T) {
	type answer struct {
		out  string
		err  error
		want bool
	}
	script := []answer{
		{"created\n", nil, false},
		{"", errors.New("Cannot connect to the Docker daemon"), false},
		{"running\n", nil, true},
		{"", errors.New("context deadline exceeded"), true},
		{"", errors.New("error during connect"), true},
		{"paused\n", nil, false},
		{"", errors.New("i/o timeout"), false},
		{"running\n", nil, true},
		{"Error: No such object: keploy-v3-test\n", errors.New("exit status 1"), false},
		{"running\n", nil, true},
		// What the docker CLI answers of a removed container: an empty
		// line on stdout, the message on stderr, lowercase in Docker 29.
		{"\n", &exec.ExitError{Stderr: []byte("error: no such object: keploy-v3-test\n")}, false},
	}
	i := 0
	running := runningByStatus(func(context.Context) ([]byte, error) {
		a := script[i]
		return []byte(a.out), a.err
	})
	for ; i < len(script); i++ {
		if got := running(context.Background()); got != script[i].want {
			t.Fatalf("answer %d (%q, %v): running = %v, want %v", i, script[i].out, script[i].err, got, script[i].want)
		}
	}
}

// engineSaysGone reads the engine's "not there" wherever its CLI puts it:
// on stderr (an *exec.ExitError's), as Docker 29 says it, lowercase, or on
// stdout, as older ones and Podman do; any other failure is no such answer.
func TestEngineSaysGoneReadsTheEnginesNotThereWhereverItIsSaid(t *testing.T) {
	for _, c := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"docker 29, on stderr", "\n", &exec.ExitError{Stderr: []byte("error: no such object: keploy-v3-x\n")}, true},
		{"older docker, on stdout", "Error: No such object: keploy-v3-x\n", errors.New("exit status 1"), true},
		{"podman", "", &exec.ExitError{Stderr: []byte("Error: no such container keploy-v3-x\n")}, true},
		{"daemon down", "", &exec.ExitError{Stderr: []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock\n")}, false},
		{"timed out", "", context.DeadlineExceeded, false},
		{"running", "running\n", nil, false},
	} {
		if got := engineSaysGone([]byte(c.out), c.err); got != c.want {
			t.Errorf("%s: engineSaysGone = %v, want %v", c.name, got, c.want)
		}
	}
}

// Against the real engine CLI, where there is one: a container it does not
// know does not run, whatever the CLI's version says of it, and wherever it
// says it. Nor has it ended while it is one docker compose has not created
// yet: keploy has never known it to be there, and the run that is to create
// it is not over. It has ended once keploy has known it to be there
// (containerGone: removed), and, never known, once that run is over
// (containerNeverThere). Each run of the test names the container afresh,
// through a client's Setup naming (nameAgentContainer), and forgets it at its
// end: nothing it knew is left for the next (-count, -shuffle). A naming of
// the same name again is a new entry: nothing the last one knew is known.
func TestARealEnginesUnknownContainerDoesNotRunAndHasEnded(t *testing.T) {
	cli := newEngineCLI(zap.NewNop())
	if _, err := cli.stdout(context.Background(), "version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skipf("no container engine to ask here: %v", err)
	}
	name := fmt.Sprintf("keploy-v3-no-such-%d", os.Getpid())
	a := clientFor("http://127.0.0.1:1/agent")
	t.Cleanup(a.forgetAgentContainer)
	a.nameAgentContainer(name)
	c := a.containerNamed(name)
	n := 0
	running := runningByStatus(func(ctx context.Context) ([]byte, error) {
		if n++; n == 1 {
			return []byte("running\n"), nil
		}
		return cli.stdout(ctx, "inspect", "-f", "{{.State.Status}}", name)
	})
	if !running(context.Background()) {
		t.Fatal("setup: the first answer says it runs")
	}
	if running(context.Background()) {
		t.Fatal("the engine does not know the container, and it is still taken to run")
	}
	exited := containerExited(cli, c)
	if code, ended := exited(context.Background()); ended {
		t.Fatalf("containerExited = %d, %v of a container keploy never knew to be there, whose run is not over; want not ended", code, ended)
	}
	c.noteRun(false)
	if code, ended := exited(context.Background()); ended {
		t.Fatalf("containerExited = %d, %v of a container whose run has begun; want not ended", code, ended)
	}
	c.noteRun(true)
	if code, ended := exited(context.Background()); !ended || code != containerNeverThere {
		t.Fatalf("containerExited = %d, %v once the run that was to create it is over; want containerNeverThere, ended", code, ended)
	}
	a.nameAgentContainer(name)
	c = a.containerNamed(name)
	exited = containerExited(cli, c)
	if code, ended := exited(context.Background()); ended {
		t.Fatalf("containerExited = %d, %v of a container named again, whose run is not over; want not ended", code, ended)
	}
	c.know()
	if code, ended := exited(context.Background()); !ended || code != containerGone {
		t.Fatalf("containerExited = %d, %v of a container keploy knew; want containerGone, ended", code, ended)
	}
	c.noteRun(true)
	if code, ended := exited(context.Background()); !ended || code != containerGone {
		t.Fatalf("containerExited = %d, %v of a container keploy knew, whose run is over; want containerGone, ended", code, ended)
	}
}

// A native agent's process that has not begun to run is sudo's, waiting on
// its password prompt: the wait says so, not that a container engine does
// not run a container. The row runs in a bubble (inBubble): whether it runs
// is answered as each look begins, and the watch takes the answer before the
// next tick.
func TestANativeAgentThatHasNotBegunToRunIsSaidToWaitOnItsStart(t *testing.T) {
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	bootLook, bootSilence, stillWaitingLooks = time.Second, time.Hour, 3
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks = look, silence, every })
	inBubble(t, func(t *testing.T) {
		core, logs := observer.New(zapcore.InfoLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		read := func(context.Context) (utils.BootProgress, bool) { return utils.BootProgress{}, false }
		judged := watchStart(ctx, zap.New(core), read, func(context.Context) bool { return false }, nil, false, false)
		start := time.Now()
		tickTo(start, 3)
		e := saidBy(t, logs, judged, 3, 1, zapcore.InfoLevel, "still waiting for the keploy agent: its process has not begun to run", "with the process not begun to run")
		if strings.Contains(e.Message, "container") {
			t.Fatalf("the line says %q; want a process that has not begun to run, and no container", e.Message)
		}
	})
}

// A look at the agent's container that the engine does not answer is a
// container engine still creating or starting it: docker's inspect waits on
// the lock its start holds. The wait says so every stillWaitingLooks ticks,
// long before the silence bound. Once that look comes back the count goes on,
// and the lines say what the latest look found: the container that does not
// run, until the next look has been out a whole line's worth of ticks. The
// row runs in a bubble (inBubble): it answers the first look between the 7th
// tick and the 8th, and the watch takes that answer before the 8th.
func TestALookTheEngineDoesNotAnswerIsSaidWhileItIsOut(t *testing.T) {
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	bootLook, bootSilence, stillWaitingLooks = time.Second, time.Hour, 3
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks = look, silence, every })
	for _, inContainer := range []bool{true, false} {
		t.Run(fmt.Sprintf("inContainer=%v", inContainer), func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				core, logs := observer.New(zapcore.InfoLevel)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// The first look is out until the row answers it, "not
				// running"; the second is out for good.
				answer := make(chan struct{})
				var looks atomic.Int32
				running := func(ctx context.Context) bool {
					if looks.Add(1) == 1 {
						select {
						case <-answer:
						case <-ctx.Done():
						}
					} else {
						<-ctx.Done()
					}
					return false
				}
				read := func(context.Context) (utils.BootProgress, bool) { return utils.BootProgress{}, false }
				judged := watchStart(ctx, zap.New(core), read, running, nil, inContainer, false)
				start := time.Now()
				notAnswered := "waiting on the keploy agent's start: keploy's look at it has not come back"
				notRun := "still waiting for the keploy agent: its process has not begun to run"
				if inContainer {
					notAnswered = "waiting on the container engine: it has not answered keploy's look at the keploy agent's container"
					notRun = "still waiting for the keploy agent: the container engine does not run its container"
				}
				// The first look, out from the 1st tick: said at the 3rd
				// and the 6th.
				for i, tick := range []int{3, 6} {
					tickTo(start, tick)
					saidBy(t, logs, judged, tick, i+1, zapcore.InfoLevel, notAnswered, "with the first look out for all the line's 3 ticks")
				}
				tickTo(start, 7)
				if n := looks.Load(); n != 1 {
					t.Fatalf("%d looks begun by the 7th tick, with the first out; want 1", n)
				}
				// It comes back; the second begins at the 8th tick.
				close(answer)
				tickTo(start, 8)
				if n := looks.Load(); n != 2 {
					t.Fatalf("%d looks begun by the 8th tick; want 2: the second at the tick after the first came back", n)
				}
				// At the 9th the second has been out 2 of the line's 3
				// ticks: the line says what the first found.
				tickTo(start, 9)
				saidBy(t, logs, judged, 9, 3, zapcore.InfoLevel, notRun, "with the second look out for 2 of the line's 3 ticks")
				tickTo(start, 12)
				saidBy(t, logs, judged, 12, 4, zapcore.InfoLevel, notAnswered, "with the second look out for all the line's 3 ticks")
				checkStartLineTicks(t, logs.All(), 3*bootLook)
			})
		})
	}
}

// A watch ends only once its look out has: however it ends, with its
// caller's ctx or with its judgment, it cancels the look out, and closes its
// channel only once that look has come back. The look here is cancelled, as
// an engine command is killed with its ctx, and comes back only when the row
// lets it, as that command's wait ends only once the command has (a look's
// funcs wait for the engine commands they run: containerEngineCommand). The
// row runs in a bubble (inBubble): what is not done yet once every goroutine
// there is blocked is waiting on the row.
func TestAWatchEndsOnlyOnceItsLookOutHasEnded(t *testing.T) {
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks = look, silence, every })
	for _, byJudgment := range []bool{false, true} {
		t.Run(map[bool]string{false: "ended by its caller", true: "ended by its judgment"}[byJudgment], func(t *testing.T) {
			// With the silence bound at 0, the start of an agent whose
			// process runs, with no report read, is given up on at the
			// first tick that knows it runs: the 2nd, the look out.
			bootLook, bootSilence, stillWaitingLooks = time.Second, time.Hour, 1000
			if byJudgment {
				bootSilence = 0
			}
			inBubble(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// The look says the process runs, then reads its report
				// until it is cancelled, and comes back once let go.
				var looks, cancelled, back atomic.Int32
				letGo, let := heldBack(t)
				running := func(context.Context) bool {
					looks.Add(1)
					return true
				}
				read := func(ctx context.Context) (utils.BootProgress, bool) {
					<-ctx.Done()
					cancelled.Add(1)
					<-letGo
					back.Add(1)
					return utils.BootProgress{}, false
				}
				judged := watchStart(ctx, zap.NewNop(), read, running, nil, true, false)
				start := time.Now()
				tickTo(start, 2)
				if byJudgment {
					select {
					case j, ok := <-judged:
						if !ok || j.verdict != bootStalled {
							t.Fatalf("the watch sent %v (%s), open: %v; want the start given up on at the 2nd tick", j.verdict, j.why, ok)
						}
					default:
						t.Fatal("the watch has not given up on the start by the 2nd tick")
					}
				} else {
					notJudged(t, judged)
					cancel()
					synctest.Wait()
				}
				if n := looks.Load(); n != 1 {
					t.Fatalf("%d looks begun; want 1, out from the 1st tick", n)
				}
				if cancelled.Load() != 1 {
					t.Fatal("the watch has ended, and has not cancelled its look out")
				}
				select {
				case _, ok := <-judged:
					t.Fatalf("the watch's channel says it has ended (open: %v), with its look out", ok)
				default:
				}
				let()
				synctest.Wait()
				select {
				case j, ok := <-judged:
					if ok {
						t.Fatalf("the watch sent %v (%s) once its look came back; want its end", j.verdict, j.why)
					}
				default:
					t.Fatal("the watch's look came back, and the watch has not ended")
				}
				if back.Load() != 1 {
					t.Fatal("the watch ended before its look came back")
				}
			})
		})
	}
}

// A wait on the agent that watches its start returns only once its watch
// has ended, and so once the watch's look out has come back: the release of
// the app, the wait for the agent keploy started and the wait for an agent
// docker compose started again. A look's command that ran on past the
// release wrote to a test's directory as it was removed ("TempDir RemoveAll
// cleanup: ...: directory not empty"). Each wait here ends with its
// caller's ctx while a look is out; the look comes back only once the row
// lets it. The row runs in a bubble (inBubble), with no network: the agent
// has no address, and the client's requests fail at once.
func TestAWaitOnTheAgentEndsOnlyOnceItsWatchHas(t *testing.T) {
	look, lookFn, endFn := bootLook, containerBootLook, containerEnd
	t.Cleanup(func() { bootLook, containerBootLook, containerEnd = look, lookFn, endFn })
	bootLook = time.Second
	for _, tc := range []struct {
		name string
		wait func(ctx context.Context, a *AgentClient, container string) error
	}{
		{"the release of the app", func(ctx context.Context, a *AgentClient, _ string) error {
			return a.MakeAgentReadyForDockerCompose(ctx)
		}},
		{"the wait for the agent keploy started", func(ctx context.Context, a *AgentClient, container string) error {
			return a.waitForAgent(ctx, nil, true, models.SetupOptions{KeployContainer: container})
		}},
		{"the wait for an agent compose started again", func(ctx context.Context, a *AgentClient, _ string) error {
			return a.awaitAgentAnswers(ctx)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				// The first look is out until it is cancelled, and comes
				// back once let go; none other begins while it is out.
				var looks, cancelled, back atomic.Int32
				letGo, let := heldBack(t)
				containerBootLook = func(*zap.Logger, *namedContainer) (func(context.Context) (utils.BootProgress, bool), func(context.Context) bool) {
					read := func(context.Context) (utils.BootProgress, bool) { return utils.BootProgress{}, false }
					return read, func(ctx context.Context) bool {
						looks.Add(1)
						<-ctx.Done()
						cancelled.Add(1)
						<-letGo
						back.Add(1)
						return false
					}
				}
				containerEnd = func(*zap.Logger, *namedContainer) func(context.Context) (int, bool) {
					return func(context.Context) (int, bool) { return 0, false }
				}
				a := clientFor("")
				a.client.Transport = failingTransport{}
				const container = "keploy-v3-held"
				a.nameAgentContainer(container)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- tc.wait(ctx, a, container) }()
				time.Sleep(2 * bootLook)
				synctest.Wait()
				if n := looks.Load(); n != 1 {
					t.Fatalf("%d looks begun by the 2nd tick; want 1, out from the 1st", n)
				}
				cancel()
				synctest.Wait()
				if cancelled.Load() != 1 {
					t.Fatal("the wait's ctx ended, and its look out was not cancelled")
				}
				select {
				case err := <-done:
					t.Fatalf("the wait returned (%v) with its look out", err)
				default:
				}
				let()
				synctest.Wait()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("the wait, ended by its ctx, returned nil")
					}
				default:
					t.Fatal("the wait's look came back, and the wait has not returned")
				}
				if back.Load() != 1 {
					t.Fatal("the wait returned before its look came back")
				}
			})
		})
	}
}

// An engine command keploy may not signal (a sudo before 1.9.4, which makes
// itself root in full so that the user who ran it cannot kill it) is left to
// end by itself once its context has ended: its run returns at once, with
// that context's cause, so the look that ran it, the watch and the wait that
// owns the watch end (runEngine). One keploy may signal is waited for until
// it has ended. The row runs in a bubble, the command's run held until the
// row lets it go.
func TestAnEngineCommandKeployMayNotSignalIsLeftToEndByItself(t *testing.T) {
	for _, mayNot := range []bool{false, true} {
		t.Run(map[bool]string{false: "one keploy may signal", true: "one keploy may not signal"}[mayNot], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				letGo, let := heldBack(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				unsignalled := make(chan struct{})
				var back atomic.Bool
				output := func(*exec.Cmd) ([]byte, error) {
					<-letGo
					back.Store(true)
					return []byte("running\n"), nil
				}
				done := make(chan error, 1)
				go func() {
					_, err := runEngine(ctx, &exec.Cmd{Args: []string{"docker", "inspect", "keploy-v3"}}, unsignalled, output)
					done <- err
				}()
				cancel()
				if mayNot {
					close(unsignalled)
				}
				synctest.Wait()
				select {
				case err := <-done:
					if !mayNot {
						t.Fatalf("the run returned (%v) before its command ended", err)
					}
					if !errors.Is(err, context.Canceled) || back.Load() {
						t.Fatalf("the run returned %v, its command back: %v; want its context's cause, with the command left running", err, back.Load())
					}
				default:
					if mayNot {
						t.Fatal("the context of a command keploy may not signal ended, and its run has not returned")
					}
				}
				let()
				synctest.Wait()
				if !mayNot {
					select {
					case err := <-done:
						if err != nil {
							t.Fatalf("the run of a command that ended returned %v; want its output's", err)
						}
					default:
						t.Fatal("the command ended, and its run has not returned")
					}
				}
			})
		})
	}
}

// failingTransport fails every request at once, as one to an agent that is
// not there yet would: a client's requests with no network.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no agent there yet")
}

// heldBack is a held look's hold, for a row in a bubble: letGo is closed
// once let is called, or once the row ends, however it ends, so that a row
// that fails with a look held leaves none blocked in its bubble.
func heldBack(t *testing.T) (letGo <-chan struct{}, let func()) {
	ch := make(chan struct{})
	var once sync.Once
	let = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(let)
	return ch, let
}

// inBubble runs f in a synctest bubble, whose clock moves only once every
// goroutine there is durably blocked: a row there moves the clock to the
// ticks it checks (tickTo), every tick on the way coming in turn, and decides
// when each look comes back, whatever the host's load. keploy's process
// clock is held from outside the bubble meanwhile, so a watch's hold of it in
// the bubble neither starts nor stops its sampling, which goes on outside,
// on real time, as it does for every other hold. Were no other hold out when
// the watch takes its own, it would start the sampler in the bubble, and a
// hold taken from outside while that sampler ran, and released after the
// watch's, would fail the run: released while the row runs, it closes the
// sampler's stop channel, made in the bubble, from outside, a fatal error
// that ends the test binary; still held when the row ends, it leaves the
// sampler running in the bubble, synctest's deadlock panic. The process
// clock, sampled outside, is the one real clock the rows leave: the silence
// bound runs on it, and the rows set that to an hour, past any run, or to 0,
// which every look is past at once; the rows that place the warning, or a
// judgment, against the bound give the watch a clock they drive instead
// (startClock).
func inBubble(t *testing.T, f func(t *testing.T)) {
	t.Helper()
	defer stall.Hold()()
	synctest.Test(t, f)
}

// tickTo moves a bubble's clock (inBubble) to half a tick (bootLook/2) past
// the nth tick of the watch that began at start, and waits until every other
// goroutine there is durably blocked again. The watch has then taken each
// tick up to the nth, and each look that came back before it or as the clock
// got here; the next tick has not come. start is the bubble's time when
// watchStart returned, which is when the watch's ticks began: the clock does
// not move while the row's goroutine runs, nor before the watch's goroutine
// has made its ticker and blocks on it.
func tickTo(start time.Time, n int) {
	time.Sleep(time.Until(start.Add(time.Duration(n)*bootLook + bootLook/2)))
	synctest.Wait()
}

// notJudged fails if the watch has judged the start.
func notJudged(t *testing.T, judged <-chan startJudgment) {
	t.Helper()
	select {
	case j := <-judged:
		t.Fatalf("the start was judged: %v (%s)", j.verdict, j.why)
	default:
	}
}

// Looks the engine answers, each slower than a tick, still have the wait said
// every stillWaitingLooks ticks: the lines count the wait's ticks, not the
// looks that came back, nor the ticks one look is out. Each look here comes
// back "not running" two and a half ticks after it began, and the next
// begins at the tick after that: at the 1st tick, the 4th, the 7th and so on.
// So a look is out at each line, at the 10th, 20th and 30th tick, begun then,
// a tick before, or two: the line says what the look before it found, that
// the container does not run, not that the engine has not answered. The row
// runs in a bubble (inBubble): each look comes back at its time, half way
// between two ticks, and the watch takes it before the next tick.
func TestLooksTheEngineAnswersSlowlyAreSaidAsTheWaitGoesOn(t *testing.T) {
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	bootLook, bootSilence, stillWaitingLooks = time.Second, time.Hour, 10
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks = look, silence, every })
	inBubble(t, func(t *testing.T) {
		const notRun = "still waiting for the keploy agent: the container engine does not run its container"
		core, logs := observer.New(zapcore.InfoLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var looks atomic.Int32
		running := func(ctx context.Context) bool {
			looks.Add(1)
			select {
			case <-time.After(bootLook * 5 / 2):
			case <-ctx.Done():
			}
			return false
		}
		read := func(context.Context) (utils.BootProgress, bool) { return utils.BootProgress{}, false }
		judged := watchStart(ctx, zap.New(core), read, running, nil, true, false)
		start := time.Now()
		for i, at := range []struct {
			tick  int
			looks int32
		}{{10, 4}, {20, 7}, {30, 10}} {
			tickTo(start, at.tick)
			if n := looks.Load(); n != at.looks {
				t.Fatalf("%d looks begun by tick %d; want %d, one every 3 ticks from the 1st", n, at.tick, at.looks)
			}
			saidBy(t, logs, judged, at.tick, i+1, zapcore.InfoLevel, notRun, "with a look out for less than a line")
		}
		checkStartLineTicks(t, logs.All(), 10*bootLook)
	})
}

// A look out for fewer ticks than a line's worth when the line is due has not
// been out since the line before: the line says what the look before it
// found, here that the container runs with no report read, not that the
// engine has not answered. A line with the look out for all its ticks says
// the engine has not answered. The row runs in a bubble (inBubble): it moves
// the clock to the ticks it checks, answers the first look between the 6th
// tick and the 7th, and the watch takes that answer before the 7th; the
// second look is never answered.
func TestALookOutForLessThanALineIsSaidByWhatTheLookBeforeFound(t *testing.T) {
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	bootLook, bootSilence, stillWaitingLooks = time.Second, time.Hour, 5
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks = look, silence, every })
	inBubble(t, func(t *testing.T) {
		const notAnswered = "waiting on the container engine: it has not answered keploy's look at the keploy agent's container"
		const runsUnread = "still waiting for the keploy agent: its container runs, and keploy has read no report of its start from it yet"
		core, logs := observer.New(zapcore.InfoLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The first look is out until the row answers it: the container
		// runs, and its read finds no report. The second is out for good.
		answer := make(chan struct{})
		var looks atomic.Int32
		running := func(ctx context.Context) bool {
			if looks.Add(1) == 1 {
				select {
				case <-answer:
					return true
				case <-ctx.Done():
					return false
				}
			}
			<-ctx.Done()
			return false
		}
		read := func(context.Context) (utils.BootProgress, bool) { return utils.BootProgress{}, false }
		judged := watchStart(ctx, zap.New(core), read, running, nil, true, false)
		start := time.Now()
		said := func(tick, n int, want, why string) {
			t.Helper()
			saidBy(t, logs, judged, tick, n, zapcore.InfoLevel, want, why)
		}
		begun := func(tick int, want int32, why string) {
			t.Helper()
			if n := looks.Load(); n != want {
				t.Fatalf("%d looks begun by tick %d; want %d: %s", n, tick, want, why)
			}
		}
		// The first look begins at the 1st tick: at the 5th it has been
		// out for all the line's 5 ticks.
		tickTo(start, 5)
		said(5, 1, notAnswered, "with the first look out for all the line's 5 ticks")
		// It comes back between the 6th tick and the 7th, where the second
		// begins.
		tickTo(start, 6)
		begun(6, 1, "one look at a time")
		close(answer)
		synctest.Wait()
		tickTo(start, 7)
		begun(7, 2, "the second at the tick after the first came back")
		// At the 10th, the second has been out for 4 of the line's 5
		// ticks, the 7th to the 10th: the line says what the first found.
		tickTo(start, 10)
		said(10, 2, runsUnread, "with the second look out for 4 of the line's 5 ticks")
		tickTo(start, 15)
		said(15, 3, notAnswered, "with the second look out for all the line's 5 ticks (9 in all)")
		checkStartLineTicks(t, logs.All(), 5*bootLook)
	})
}

// A container that runs, whose report keploy has not read (the read is out,
// or finds none), is said to run with no report: not an engine that has not
// answered, which said whether it runs. The line names the agent's container,
// or its process. The row runs in a bubble (inBubble): whether it runs is
// answered as each look begins, or, for the first look where it is answered
// late, between the 5th tick and the 6th, and the watch takes the answer
// before the next tick. A read that is out stays out; one that finds none is
// the end of its look, and the next tick begins another.
func TestAStartThatRunsWithNoReportReadIsSaidToRun(t *testing.T) {
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	bootLook, bootSilence, stillWaitingLooks = time.Second, time.Hour, 5
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks = look, silence, every })
	for _, tc := range []struct {
		name                        string
		inContainer, readOut, asked bool
		want                        string
	}{
		{"container, read out", true, true, false, "still waiting for the keploy agent: its container runs, and keploy has read no report of its start from it yet"},
		{"process, read out", false, true, false, "still waiting for the keploy agent: its process runs, and keploy has read no report of its start from it yet"},
		{"container, no report", true, false, false, "still waiting for the keploy agent: its container runs, and keploy has read no report of its start from it yet"},
		// Whether it runs is answered only after a line said it was not:
		// the read that follows is out.
		{"container, answered late, read out", true, true, true, "still waiting for the keploy agent: its container runs, and keploy has read no report of its start from it yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				core, logs := observer.New(zapcore.InfoLevel)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				answer := make(chan struct{})
				running := func(ctx context.Context) bool {
					if tc.asked {
						select {
						case <-answer:
						case <-ctx.Done():
						}
					}
					return true
				}
				read := func(ctx context.Context) (utils.BootProgress, bool) {
					if tc.readOut {
						<-ctx.Done()
					}
					return utils.BootProgress{}, false
				}
				judged := watchStart(ctx, zap.New(core), read, running, nil, tc.inContainer, false)
				start := time.Now()
				for i, tick := range []int{5, 10, 15} {
					tickTo(start, tick)
					if tc.asked && i == 0 {
						saidBy(t, logs, judged, tick, 1, zapcore.InfoLevel, "waiting on the container engine: it has not answered keploy's look at the keploy agent's container", "with whether it runs not answered for all the line's 5 ticks")
						close(answer)
						continue
					}
					if e := saidBy(t, logs, judged, tick, i+1, zapcore.InfoLevel, tc.want, "with it said to run and no report read"); e.Message != tc.want {
						t.Fatalf("line %d says %q; want %q", i, e.Message, tc.want)
					}
				}
				checkStartLineTicks(t, logs.All(), 5*bootLook)
			})
		})
	}
}

// A report keploy has read before is no progress: an agent that runs, whose
// report does not move, has the wait said as one that runs with no new
// report, counted from its first report, the agent's last progress, which
// may be the agent's first beat with no step yet taken. The row runs in a
// bubble (inBubble): each look is answered as it begins, and the watch takes
// its answer before the next tick.
func TestAStartWhoseReportDoesNotMoveIsSaidAsTheWaitGoesOn(t *testing.T) {
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	bootLook, bootSilence, stillWaitingLooks = time.Second, time.Hour, 10
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks = look, silence, every })
	inBubble(t, func(t *testing.T) {
		const unread = "still waiting for the keploy agent: its container runs, and keploy has read no report of its start from it yet"
		const still = "still waiting for the keploy agent: its container runs, and keploy has read no new report of its start since its last"
		core, logs := observer.New(zapcore.InfoLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var reports atomic.Bool
		read := func(context.Context) (utils.BootProgress, bool) {
			if !reports.Load() {
				return utils.BootProgress{}, false
			}
			return utils.BootProgress{Step: "loading eBPF", CountsQuiet: true}, true
		}
		judged := watchStart(ctx, zap.New(core), read, alwaysRunning, nil, true, false)
		start := time.Now()
		tickTo(start, 10)
		if e := saidBy(t, logs, judged, 10, 1, zapcore.InfoLevel, unread, "with no report read"); e.Message != unread || waitedOf(e) != 10*bootLook {
			t.Fatalf("the first line says %q, waited %v; want %q, 10s", e.Message, waitedOf(e), unread)
		}
		// The 11th tick's look reads the first report: the count begins
		// again from it, so no line comes at the 20th tick.
		reports.Store(true)
		tickTo(start, 20)
		saidBy(t, logs, judged, 20, 1, zapcore.InfoLevel, unread, "with the count begun again at the 11th tick's report")
		for i, tick := range []int{21, 31} {
			tickTo(start, tick)
			e := saidBy(t, logs, judged, tick, i+2, zapcore.InfoLevel, still, "with the report not moved since the 11th tick")
			if want := time.Duration(i+1) * 10 * bootLook; e.Message != still || waitedOf(e) != want {
				t.Fatalf("line %d says %q, waited %v; want %q, %v: counted from its first report", i+1, e.Message, waitedOf(e), still, want)
			}
		}
	})
}

// A report that moves (a new beat, or a new step) is the agent's progress:
// the count begins again with each, so an agent that reports as it goes has
// no wait said, nor one that says its server listens, which the ready rule
// bounds. The row runs in a bubble (inBubble): each look is answered as it
// begins, so each tick begins one and reads one report, and the watch takes
// it before the next tick.
func TestAStartThatReportsAsItGoesHasNoWaitSaid(t *testing.T) {
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	bootLook, bootSilence, stillWaitingLooks = time.Second, time.Hour, 10
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks = look, silence, every })
	for _, tc := range []struct {
		name   string
		report func(n uint64) utils.BootProgress
	}{
		{"beats", func(n uint64) utils.BootProgress { return utils.BootProgress{Beat: n, CountsQuiet: true} }},
		{"steps", func(n uint64) utils.BootProgress { return utils.BootProgress{Seq: n, CountsQuiet: true} }},
		{"listens", func(uint64) utils.BootProgress {
			return utils.BootProgress{Seq: 1, Beat: 1, Ready: true, CountsQuiet: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				core, logs := observer.New(zapcore.InfoLevel)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var reads atomic.Uint64
				read := func(context.Context) (utils.BootProgress, bool) { return tc.report(reads.Add(1)), true }
				judged := watchStart(ctx, zap.New(core), read, alwaysRunning, nil, true, false)
				start := time.Now()
				// Five lines' worth of ticks.
				tickTo(start, 5*10)
				notJudged(t, judged)
				if n := reads.Load(); n != 5*10 {
					t.Fatalf("%d reports read by the 50th tick; want 50, one a tick", n)
				}
				if n := logs.Len(); n != 0 {
					t.Fatalf("an agent that reports as it goes had %d lines said, the first %q", n, logs.All()[0].Message)
				}
			})
		})
	}
}

// A look out past the silence bound is said once, at WARN, in place of the
// line due then: one line at each line's tick, never two, the rest at INFO.
// Each look out that long is said so once: a look that begins after one that
// was warned of is warned of in its turn. The bound is keploy's own runnable
// time, which the bubble (inBubble) does not move, so the row sets it to 0: a
// look out is past it at its first line due. The bound at 0 bounds the
// judge's waits too, for a first report and for a health check once the agent
// listens; neither applies here, since no look says the process runs. Where
// the warning comes against the bound, the next row pins.
func TestALookOutPastTheSilenceBoundIsWarnedOnce(t *testing.T) {
	look, silence, every := bootLook, bootSilence, stillWaitingLooks
	bootLook, bootSilence, stillWaitingLooks = time.Second, 0, 3
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks = look, silence, every })
	inBubble(t, func(t *testing.T) {
		const warning = "keploy's look at the agent's start has not come back for a while"
		// Each look warned of is out on whether the container runs, and no
		// look has found it running: nothing bounds that wait.
		const warned = warning + ": the container engine has not answered whether it runs the agent's container; keploy sets no time limit on the wait while the agent's container is not known to run, and waits on"
		const notAnswered = "waiting on the container engine: it has not answered"
		core, logs := observer.New(zapcore.InfoLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The first look is out until the row answers it, "not running";
		// the second is out for good.
		answer := make(chan struct{})
		var looks atomic.Int32
		running := func(ctx context.Context) bool {
			if looks.Add(1) == 1 {
				select {
				case <-answer:
				case <-ctx.Done():
				}
			} else {
				<-ctx.Done()
			}
			return false
		}
		read := func(context.Context) (utils.BootProgress, bool) { return utils.BootProgress{}, false }
		judged := watchStart(ctx, zap.New(core), read, running, nil, true, false)
		start := time.Now()
		// The first look, out from the 1st tick: warned of at the 3rd, its
		// first line, then said at INFO at the 6th.
		tickTo(start, 3)
		first := saidBy(t, logs, judged, 3, 1, zapcore.WarnLevel, warning, "the first look's first line")
		if got := first.ContextMap()["runnable_time"]; got != time.Duration(0) {
			t.Fatalf("the warning says runnable_time %v; want the silence bound, 0s", got)
		}
		saysExactly(t, first, 3, warned)
		tickTo(start, 6)
		saidBy(t, logs, judged, 6, 2, zapcore.InfoLevel, notAnswered, "the first look, warned of already")
		// It comes back; the second begins at the 7th tick: warned of at
		// the 9th, its first line, then said at INFO.
		close(answer)
		tickTo(start, 9)
		saysExactly(t, saidBy(t, logs, judged, 9, 3, zapcore.WarnLevel, warning, "the second look's first line"), 9, warned)
		tickTo(start, 12)
		saidBy(t, logs, judged, 12, 4, zapcore.InfoLevel, notAnswered, "the second look, warned of already")
		if n := looks.Load(); n != 2 {
			t.Fatalf("%d looks begun by tick 12; want 2", n)
		}
		checkStartLineTicks(t, logs.All(), 3*bootLook)
	})
}

// The silence bound's warning comes at a look's first line due once the look
// has been out for the bound of keploy's own runnable time, counted from the
// look's own start, and not before; it names the bound, and whether the agent
// runs in a container, and says what the look is out on and what keploy does
// next: only the warning for an agent in a container names the container
// engine, which a native agent's looks do not ask. Here the first look's
// first line, a nanosecond short of the bound, is said at INFO, its next, at
// the bound, is the warning, and the one after is INFO again; that look is
// out on whether the process runs.
// It comes back that the process runs, with a report, and the second look
// begins a quarter of the bound later and is out at its read of the next
// report: its first line, a nanosecond short of the bound into it, is INFO,
// its next, half the bound after that line, is the warning, and the one after,
// the bound later again, is INFO, nothing judged (keploy waits on). The row
// gives the watch a clock it drives (startClock), moved only while every
// other goroutine in the bubble (inBubble) is blocked, for an agent in a
// container and a native one.
func TestTheSilenceWarningComesAtTheFirstLineOnceTheBoundIsReached(t *testing.T) {
	const bound = 10 * time.Second
	look, silence, every, sclk := bootLook, bootSilence, stillWaitingLooks, startClock
	bootLook, bootSilence, stillWaitingLooks = time.Second, bound, 3
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks, startClock = look, silence, every, sclk })
	for _, inContainer := range []bool{true, false} {
		t.Run(fmt.Sprintf("inContainer=%v", inContainer), func(t *testing.T) {
			clk := &stall.ManualClock{}
			startClock = clk
			inBubble(t, func(t *testing.T) {
				const warning = "keploy's look at the agent's start has not come back for a while"
				notAnswered := "waiting on the keploy agent's start: keploy's look at it has not come back"
				noNewReport := "still waiting for the keploy agent: its process runs, and keploy has read no new report"
				// The first warning's look is out on whether the process
				// runs, which no look has said: nothing bounds the wait.
				// The second's is out at its read, the process running and
				// its first report read: its beats judge it.
				outOnRuns := warning + ": keploy has not yet found out whether the agent's process runs; keploy sets no time limit on the wait while the agent's process is not known to run, and waits on"
				outAtRead := warning + ": the agent's process runs, and keploy's read of the report of its start has not come back; keploy waits on: the agent has reported, so only a new report from it can show that its start has stalled, and keploy reads none until this look comes back"
				if inContainer {
					notAnswered = "waiting on the container engine: it has not answered"
					noNewReport = "still waiting for the keploy agent: its container runs, and keploy has read no new report"
					outOnRuns = warning + ": the container engine has not answered whether it runs the agent's container; keploy sets no time limit on the wait while the agent's container is not known to run, and waits on"
					outAtRead = warning + ": the agent's container runs, and the container engine has not yet given keploy the report of its start from inside it; keploy waits on: the agent has reported, so only a new report from it can show that its start has stalled, and keploy reads none until this look comes back"
				}
				core, logs := observer.New(zapcore.InfoLevel)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// The first look is out on whether the process runs until
				// the row answers it: it runs, and its report is read. The
				// second says at once that it runs, and its read is out for
				// good.
				answer := make(chan struct{})
				var looks atomic.Int32
				running := func(ctx context.Context) bool {
					if looks.Add(1) == 1 {
						select {
						case <-answer:
						case <-ctx.Done():
						}
					}
					return true
				}
				read := func(ctx context.Context) (utils.BootProgress, bool) {
					if looks.Load() == 1 {
						return utils.BootProgress{Step: "loading eBPF", CountsQuiet: true}, true
					}
					<-ctx.Done()
					return utils.BootProgress{}, false
				}
				judged := watchStart(ctx, zap.New(core), read, running, nil, inContainer, false)
				start := time.Now()
				// warnedAt fails unless the nth line, by the tick, is the
				// warning, want word for word, naming the container engine
				// only for an agent in a container, and the bound and where
				// the agent runs.
				warnedAt := func(tick, n int, why, want string) {
					t.Helper()
					e := saidBy(t, logs, judged, tick, n, zapcore.WarnLevel, warning, why)
					if says := strings.Contains(e.Message, "container engine"); says != inContainer {
						t.Fatalf("the warning at tick %d names the container engine: %v, for an agent in a container: %v; it says %q", tick, says, inContainer, e.Message)
					}
					saysExactly(t, e, tick, want)
					if got := e.ContextMap()["runnable_time"]; got != bound {
						t.Fatalf("the warning at tick %d says runnable_time %v; want the silence bound, %v", tick, got, bound)
					}
					if got := e.ContextMap()["in_a_container"]; got != inContainer {
						t.Fatalf("the warning at tick %d says in_a_container %v; want %v", tick, got, inContainer)
					}
				}
				// The first look begins at the 1st tick, the clock at 0.
				tickTo(start, 1)
				clk.Advance(bound - time.Nanosecond)
				tickTo(start, 3)
				saidBy(t, logs, judged, 3, 1, zapcore.InfoLevel, notAnswered, "the first look's first line, a nanosecond short of the bound")
				clk.Advance(time.Nanosecond)
				tickTo(start, 6)
				warnedAt(6, 2, "the first look's second line, at the bound", outOnRuns)
				clk.Advance(bound)
				tickTo(start, 9)
				saidBy(t, logs, judged, 9, 3, zapcore.InfoLevel, notAnswered, "the first look's third line, warned of already")
				// It comes back, the clock at twice the bound, with the
				// agent's first report: progress, so the wait's count
				// begins again. The clock moves a quarter of the bound,
				// and the second look begins at the 10th tick.
				close(answer)
				synctest.Wait()
				clk.Advance(bound / 4)
				tickTo(start, 10)
				clk.Advance(bound - time.Nanosecond)
				tickTo(start, 12)
				saidBy(t, logs, judged, 12, 4, zapcore.InfoLevel, noNewReport, "the second look's first line, a nanosecond short of the bound into it")
				clk.Advance(bound / 2)
				tickTo(start, 15)
				warnedAt(15, 5, "the second look's second line, half the bound after its first", outAtRead)
				// keploy waits on, as the warning said: the bound passes
				// again, and the next line is INFO, nothing judged.
				clk.Advance(bound)
				tickTo(start, 18)
				saidBy(t, logs, judged, 18, 6, zapcore.InfoLevel, noNewReport, "the second look's third line, warned of already, the bound passed again")
				if n := looks.Load(); n != 2 {
					t.Fatalf("%d looks begun by tick 18; want 2", n)
				}
				// Each look's lines are counted from the wait's last
				// progress: the watch's start, then the first report.
				lines := logs.All()
				checkStartLineTicks(t, lines[:3], 3*bootLook)
				checkStartLineTicks(t, lines[3:], 3*bootLook)
			})
		})
	}
}

// An agent that has said its server listens still has its wait said once the
// latest look found its process not running (a container paused, or one
// docker compose starts again), and then a look out past the silence bound is
// warned of; the ready rule bounds no wait on a process that does not run.
// While its process runs, its wait is said only once a look has gone a whole
// line without saying whether it runs. Each case runs on a clock the row
// drives (startClock), in a bubble (inBubble), for an agent in a container
// and, where its process runs on, a native one: a native agent's process
// that has run does not stop and go on, its exit ends the wait. The first
// look finds the process running and reads that its server listens, and the
// count of the wait begins again there. Each line with a look out says what
// keploy does next, true to what it then does: on a process it does not know
// to run it waits on, long past the bound; on one that says its server
// listens it gives up once it has not answered keploy for the bound.
func TestAnAgentThatSaidItListensHasItsWaitSaidWhenItsLooksStopSayingSo(t *testing.T) {
	const bound = 10 * time.Second
	look, silence, every, sclk := bootLook, bootSilence, stillWaitingLooks, startClock
	bootLook, bootSilence, stillWaitingLooks = time.Second, bound, 3
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks, startClock = look, silence, every, sclk })
	for _, inContainer := range []bool{true, false} {
		for _, stops := range []bool{true, false} {
			if stops && !inContainer {
				continue
			}
			t.Run(fmt.Sprintf("inContainer=%v,stops=%v", inContainer, stops), func(t *testing.T) {
				clk := &stall.ManualClock{}
				startClock = clk
				inBubble(t, func(t *testing.T) {
					const listens = "keploy gives up on the start once the agent, which says its server listens, has not answered keploy for 10s of keploy's runnable time"
					notAnswered := "waiting on the keploy agent's start: keploy's look at it has not come back"
					notRun := "still waiting for the keploy agent: its process has not begun to run"
					if inContainer {
						notAnswered = "waiting on the container engine: it has not answered"
						notRun = "still waiting for the keploy agent: the container engine does not run its container"
					}
					core, logs := observer.New(zapcore.InfoLevel)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					// The first look finds the process running; the second
					// finds it not running where it stops, else running; the
					// third is out on whether it runs for good. Each read
					// finds the report that says the server listens.
					var looks atomic.Int32
					running := func(ctx context.Context) bool {
						switch looks.Add(1) {
						case 1:
							return true
						case 2:
							return !stops
						}
						<-ctx.Done()
						return false
					}
					read := func(context.Context) (utils.BootProgress, bool) {
						return utils.BootProgress{Seq: 1, Beat: 1, Ready: true, CountsQuiet: true}, true
					}
					judged := watchStart(ctx, zap.New(core), read, running, nil, inContainer, false)
					start := time.Now()
					// The count begins again at the 1st tick's report: the
					// line at the 4th tick has the third look out 2 ticks.
					tickTo(start, 4)
					if !stops {
						if n := logs.Len(); n != 0 {
							t.Fatalf("%d lines by tick 4, with the process running and its server listening; want none, the first %q", n, logs.All()[0].Message)
						}
						notJudged(t, judged)
						tickTo(start, 7)
						e := saidBy(t, logs, judged, 7, 1, zapcore.InfoLevel, notAnswered, "with the third look out on whether the process runs for a whole line")
						want := "waiting on the keploy agent's start: keploy's look at it has not come back; " + listens
						if inContainer {
							want = "waiting on the container engine: it has not answered keploy's look at the keploy agent's container, as an engine still creating or starting a container may not until that is done; " + listens
						}
						saysExactly(t, e, 7, want)
						checkStartLineTicks(t, logs.All(), 6*bootLook)
						// The bound passes since the judge first saw the
						// report that the server listens, at the 2nd tick:
						// the start is given up on at the next tick, as the
						// line said.
						clk.Advance(bound)
						tickTo(start, 8)
						select {
						case j := <-judged:
							if j.verdict != bootStalled || !strings.HasPrefix(j.why, "it says its server listens, and has not answered keploy for 10s") {
								t.Fatalf("by tick 8 the start is judged %v (%s); want it given up on, its server listening and not answering keploy for the bound", j.verdict, j.why)
							}
						default:
							t.Fatal("by tick 8, the bound passed since the agent said its server listens, the start is not judged; the line at tick 7 said keploy gives up on it then")
						}
						if n := logs.Len(); n != 1 {
							t.Fatalf("%d lines by tick 8; want the one at tick 7", n)
						}
						return
					}
					saidBy(t, logs, judged, 4, 1, zapcore.InfoLevel, notRun, "with the second look's process not running")
					clk.Advance(bound)
					tickTo(start, 7)
					e := saidBy(t, logs, judged, 7, 2, zapcore.WarnLevel, "keploy's look at the agent's start has not come back for a while", "with the third look out for the bound")
					if got := e.ContextMap()["runnable_time"]; got != bound {
						t.Fatalf("the warning says runnable_time %v; want the silence bound, %v", got, bound)
					}
					// The look before found the container not running:
					// nothing bounds the wait, whatever its report said.
					saysExactly(t, e, 7, "keploy's look at the agent's start has not come back for a while: the container engine has not answered whether it runs the agent's container; keploy sets no time limit on the wait while the agent's container is not known to run, and waits on")
					// keploy waits on, as the warning said: long past the
					// bound, the next line is INFO, nothing judged.
					clk.Advance(10 * bound)
					tickTo(start, 10)
					saidBy(t, logs, judged, 10, 3, zapcore.InfoLevel, notAnswered, "with the third look out, warned of already, long past the bound")
					checkStartLineTicks(t, logs.All(), 3*bootLook)
				})
			})
		}
	}
}

// The silence bound's warning says what the look out has not come back with
// and what keploy does next, true to what the watch then does. An agent whose
// process runs, and whose report's read is out with none read before, is
// given up on once it has been found running for the bound with none read;
// one that says its server listens, whose next look is out on whether its
// process runs, once it has not answered keploy for the bound: here each at
// the warning's tick. A container the engine does not run, whose look is out
// on whether it has ended, is waited on, long past the bound (a container's
// look only). A report that says the start failed has no line (and so no
// warning): the wait ends with the failure at the tick it is first judged,
// a line's tick here. These run on a clock the row drives (startClock), in
// a bubble (inBubble), for an agent in a container and, but for the end's
// look, a native one. A look that has said its process does not run and has
// nothing left to say is out only until the watch takes its result, which in
// the bubble is before the next tick, so that warning is asked of
// lookOutWarning directly.
func TestAWarnedLookSaysWhatItIsOutOnAndWhatKeployDoesNext(t *testing.T) {
	const bound = 10 * time.Second
	const warning = "keploy's look at the agent's start has not come back for a while: "
	look, silence, every, sclk := bootLook, bootSilence, stillWaitingLooks, startClock
	bootLook, bootSilence, stillWaitingLooks = time.Second, bound, 3
	t.Cleanup(func() { bootLook, bootSilence, stillWaitingLooks, startClock = look, silence, every, sclk })
	// stalledAt fails unless, by the tick, the one line said is the warning,
	// want word for word, and the start has then been given up on for a
	// reason that begins with why.
	stalledAt := func(t *testing.T, logs *observer.ObservedLogs, judged <-chan startJudgment, tick int, want, why string) {
		t.Helper()
		lines := logs.All()
		if len(lines) != 1 || lines[0].Level != zapcore.WarnLevel {
			t.Fatalf("%d lines by tick %d; want the one warning", len(lines), tick)
		}
		saysExactly(t, lines[0], tick, want)
		select {
		case j := <-judged:
			if j.verdict != bootStalled || !strings.HasPrefix(j.why, why) {
				t.Fatalf("by tick %d the start is judged %v (%s); want it given up on: %s", tick, j.verdict, j.why, why)
			}
		default:
			t.Fatalf("by tick %d the start is not judged; want it given up on, as the warning said: %s", tick, why)
		}
	}
	for _, inContainer := range []bool{true, false} {
		what := "process"
		if inContainer {
			what = "container"
		}
		t.Run(fmt.Sprintf("no report read,inContainer=%v", inContainer), func(t *testing.T) {
			clk := &stall.ManualClock{}
			startClock = clk
			inBubble(t, func(t *testing.T) {
				core, logs := observer.New(zapcore.InfoLevel)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// The look says at once that the process runs; its read is
				// out for good.
				running := func(context.Context) bool { return true }
				read := func(ctx context.Context) (utils.BootProgress, bool) {
					<-ctx.Done()
					return utils.BootProgress{}, false
				}
				judged := watchStart(ctx, zap.New(core), read, running, nil, inContainer, false)
				start := time.Now()
				// The look begins at the 1st tick, the clock at 0; the
				// judge first sees the process run at the 2nd, the clock
				// at 0, and the bound has passed since both at the 3rd.
				tickTo(start, 2)
				clk.Advance(bound)
				tickTo(start, 3)
				out := "the agent's process runs, and keploy's read of the report of its start has not come back"
				if inContainer {
					out = "the agent's container runs, and the container engine has not yet given keploy the report of its start from inside it"
				}
				stalledAt(t, logs, judged, 3, warning+out+"; keploy gives up on the start once it has found the agent's "+what+" running, with no report of its start read, for 10s of keploy's runnable time",
					"its process has run for 10s of keploy's runnable time without reporting its start")
			})
		})
		t.Run(fmt.Sprintf("listens,inContainer=%v", inContainer), func(t *testing.T) {
			clk := &stall.ManualClock{}
			startClock = clk
			inBubble(t, func(t *testing.T) {
				core, logs := observer.New(zapcore.InfoLevel)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// The first look finds the process running and reads that
				// its server listens; the second is out on whether it runs
				// for good.
				var looks atomic.Int32
				running := func(ctx context.Context) bool {
					if looks.Add(1) == 1 {
						return true
					}
					<-ctx.Done()
					return false
				}
				read := func(context.Context) (utils.BootProgress, bool) {
					return utils.BootProgress{Seq: 1, Beat: 1, Ready: true, CountsQuiet: true}, true
				}
				judged := watchStart(ctx, zap.New(core), read, running, nil, inContainer, false)
				start := time.Now()
				// The count begins again at the 1st tick's report. The
				// second look begins at the 2nd tick, the clock at 0, where
				// the judge first sees that the server listens; at the 4th
				// it has been out a whole line, and the bound has passed.
				tickTo(start, 3)
				clk.Advance(bound)
				tickTo(start, 4)
				out := "keploy has not yet found out whether the agent's process runs"
				if inContainer {
					out = "the container engine has not answered whether it runs the agent's container"
				}
				stalledAt(t, logs, judged, 4, warning+out+"; keploy gives up on the start once the agent, which says its server listens, has not answered keploy for 10s of keploy's runnable time",
					"it says its server listens, and has not answered keploy for 10s of keploy's runnable time")
				if n := looks.Load(); n != 2 {
					t.Fatalf("%d looks begun by tick 4; want 2", n)
				}
			})
		})
	}
	t.Run("exit not answered", func(t *testing.T) {
		clk := &stall.ManualClock{}
		startClock = clk
		inBubble(t, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// The look says at once that the engine does not run the
			// container; whether it has ended is out for good.
			running := func(context.Context) bool { return false }
			read := func(context.Context) (utils.BootProgress, bool) { return utils.BootProgress{}, false }
			ended := func(ctx context.Context) (int, bool) {
				<-ctx.Done()
				return 0, false
			}
			judged := watchStart(ctx, zap.New(core), read, running, ended, true, false)
			start := time.Now()
			// The look begins at the 1st tick, the clock at 0.
			tickTo(start, 1)
			clk.Advance(bound)
			tickTo(start, 3)
			e := saidBy(t, logs, judged, 3, 1, zapcore.WarnLevel, warning, "with the look out on whether the container has ended for the bound")
			saysExactly(t, e, 3, warning+"the container engine does not run the agent's container, and has not yet said whether the container has ended or, if it has, given keploy its last report from inside it; keploy sets no time limit on the wait while the agent's container is not known to run, and waits on")
			// keploy waits on, as the warning said: long past the bound,
			// the next line is INFO, nothing judged.
			clk.Advance(10 * bound)
			tickTo(start, 6)
			saidBy(t, logs, judged, 6, 2, zapcore.InfoLevel, "still waiting for the keploy agent: the container engine does not run its container", "with the look out on whether the container has ended, warned of already, long past the bound")
		})
	})
	for _, inContainer := range []bool{true, false} {
		t.Run(fmt.Sprintf("failed report,inContainer=%v", inContainer), func(t *testing.T) {
			clk := &stall.ManualClock{}
			startClock = clk
			inBubble(t, func(t *testing.T) {
				core, logs := observer.New(zapcore.InfoLevel)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// Each look finds the process running. The first read finds
				// a report; the second is out until the row answers it, with
				// the same report saying the start failed (an agent's failure
				// kicks a report of the same beat and step); the reads after
				// find that too.
				answer := make(chan struct{})
				var reads atomic.Int32
				running := func(context.Context) bool { return true }
				read := func(ctx context.Context) (utils.BootProgress, bool) {
					p := utils.BootProgress{Seq: 1, Beat: 1, CountsQuiet: true}
					switch reads.Add(1) {
					case 1:
						return p, true
					case 2:
						select {
						case <-answer:
						case <-ctx.Done():
						}
					}
					p.Failed, p.Failure = true, "the eBPF load failed"
					return p, true
				}
				judged := watchStart(ctx, zap.New(core), read, running, nil, inContainer, false)
				start := time.Now()
				// The count begins again at the 1st tick's report, so the
				// 4th is a line's tick. The second look, begun at the 2nd,
				// comes back after the 3rd with the failure: at the 4th
				// the start is judged failed, with no line before it.
				tickTo(start, 3)
				close(answer)
				tickTo(start, 4)
				if n := logs.Len(); n != 0 {
					t.Fatalf("%d lines by tick 4, with the start reported failed; want none, the first %q", n, logs.All()[0].Message)
				}
				select {
				case j := <-judged:
					if j.verdict != bootFailed || j.why != "the eBPF load failed" {
						t.Fatalf("by tick 4 the start is judged %v (%s); want it failed, as the agent reports", j.verdict, j.why)
					}
				default:
					t.Fatal("by tick 4 the start is not judged; want it failed, as the agent reports")
				}
			})
		})
	}
	t.Run("nothing left to say", func(t *testing.T) {
		for _, tc := range []struct {
			inContainer bool
			want        string
		}{
			{true, warning + "the container engine does not run the agent's container; keploy sets no time limit on the wait while the agent's container is not known to run, and waits on"},
			{false, warning + "the agent's process does not run; keploy sets no time limit on the wait while the agent's process is not known to run, and waits on"},
		} {
			if got := lookOutWarning(bootLookResult{}, false, false, tc.inContainer, bound); got != tc.want {
				t.Errorf("a look that said its process does not run, inContainer=%v, is warned of as %q; want %q", tc.inContainer, got, tc.want)
			}
		}
	})
}
