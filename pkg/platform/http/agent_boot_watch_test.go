package http

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
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
	if v, why := b.look(time.Second, p, true, true); v != bootStalled || !strings.Contains(why, "health check has not answered") {
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
