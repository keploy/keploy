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
	"time"

	"go.keploy.io/server/v3/config"
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
		Stages: []utils.BootStage{{Name: "the UDP DNS server's bind", State: "D", Wait: "rtnl_lock"}, {Name: "the proxy's start", State: "S"}},
		Errors: 1, Error: "failed to attach the udp4 sendmsg hook"}
	v, why := b.look(0, p, true, true)
	want := "its start took no step across 30 of its beats, with no work in progress making progress; its last step: the proxy listens; in progress: the UDP DNS server's bind (its thread in an uninterruptible wait, in rtnl_lock), the proxy's start (its thread sleeping) (it logged 1 ERROR(s) while starting, the last: failed to attach the udp4 sendmsg hook)"
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
	containerBootLook = func(*zap.Logger, string) (func(context.Context) (utils.BootProgress, bool), func(context.Context) bool) {
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
	containerBootLook = func(*zap.Logger, string) (func(context.Context) (utils.BootProgress, bool), func(context.Context) bool) {
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
	containerEnd = func(*zap.Logger, string) func(context.Context) (int, bool) {
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
			a.agentContainer = "keploy-v3-test"
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
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uri := "http://" + closed.Addr().String() + "/agent"
	_ = closed.Close()
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
		a.agentContainer = "keploy-v3-test"
		ctx, cancel := context.WithCancel(context.Background())
		errs := a.watchAgent(ctx, endOnExit)
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
	a.agentContainer = "keploy-v3-test"
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
			a.agentContainer = "keploy-v3-test"
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
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uri := "http://" + closed.Addr().String() + "/agent"
	_ = closed.Close()
	a := clientFor(uri)
	a.agentContainer = "keploy-v3-test"
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
// no rule bounds the wait: keploy says now and then that it still waits, and
// on what, never a wait that goes on in silence. A look that finds it running
// begins the count again.
func TestAWaitOnAContainerTheEngineDoesNotRunSaysSo(t *testing.T) {
	look, every := bootLook, stillWaitingLooks
	bootLook, stillWaitingLooks = time.Millisecond, 5
	t.Cleanup(func() { bootLook, stillWaitingLooks = look, every })
	var runs atomic.Bool
	ran := make(chan struct{}, 1)
	read := func(context.Context) (utils.BootProgress, bool) { return utils.BootProgress{}, false }
	running := func(context.Context) bool {
		r := runs.Load()
		if r {
			ran <- struct{}{}
		}
		return r
	}
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	judged := watchStart(ctx, zap.New(core), read, running, nil, true, false)
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for logs.FilterMessageSnippet("the container engine does not run its container").Len() < n {
			select {
			case j := <-judged:
				t.Fatalf("a container the engine does not run was judged: %v (%s)", j.verdict, j.why)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("said it still waits %d times, want %d", logs.FilterMessageSnippet("the container engine does not run").Len(), n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor(3)
	line := logs.FilterMessageSnippet("the container engine does not run its container").All()[2]
	if got := line.ContextMap()["looks"]; got != int64(15) {
		t.Fatalf("the third line counts %v looks, want 15", got)
	}
	// It runs, and reports nothing yet: a look begins only once the last is
	// in, so the second look that finds it running is past the first. Then
	// the engine stops running it again, and the count begins again.
	runs.Store(true)
	<-ran
	<-ran
	runs.Store(false)
	waitFor(4)
	if got := logs.FilterMessageSnippet("the container engine does not run its container").All()[3].ContextMap()["looks"]; got != int64(5) {
		t.Fatalf("the line after the container ran again counts %v looks, want 5: the count did not begin again", got)
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
	containerBootLook = func(*zap.Logger, string) (func(context.Context) (utils.BootProgress, bool), func(context.Context) bool) {
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
// know does not run, and has ended (containerGone), whatever the CLI's
// version says of it, and wherever it says it.
func TestARealEnginesUnknownContainerDoesNotRunAndHasEnded(t *testing.T) {
	cli := newEngineCLI(zap.NewNop())
	if _, err := cli.stdout(context.Background(), "version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skipf("no container engine to ask here: %v", err)
	}
	name := fmt.Sprintf("keploy-v3-no-such-%d", os.Getpid())
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
	if code, ended := containerExited(cli, name)(context.Background()); !ended || code != containerGone {
		t.Fatalf("containerExited = %d, %v; want containerGone, ended", code, ended)
	}
}

// A native agent's process that has not begun to run is sudo's, waiting on
// its password prompt: the wait says so, not that a container engine does
// not run a container.
func TestANativeAgentThatHasNotBegunToRunIsSaidToWaitOnItsStart(t *testing.T) {
	look, every := bootLook, stillWaitingLooks
	bootLook, stillWaitingLooks = time.Millisecond, 3
	t.Cleanup(func() { bootLook, stillWaitingLooks = look, every })
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	read := func(context.Context) (utils.BootProgress, bool) { return utils.BootProgress{}, false }
	_ = watchStart(ctx, zap.New(core), read, func(context.Context) bool { return false }, nil, false, false)
	deadline := time.Now().Add(30 * time.Second)
	for logs.FilterMessageSnippet("still waiting for the keploy agent").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the wait said nothing")
		}
		time.Sleep(time.Millisecond)
	}
	if msg := logs.FilterMessageSnippet("still waiting for the keploy agent").All()[0].Message; !strings.Contains(msg, "its process has not begun to run") || strings.Contains(msg, "container") {
		t.Fatalf("the line says %q; want a process that has not begun to run, and no container", msg)
	}
}
