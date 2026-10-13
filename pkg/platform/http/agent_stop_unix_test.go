//go:build linux || darwin

package http

// A RUNNING AGENT IS STOPPED BY ASKING IT, AND WAITING FOR IT TO EXIT.
//
// The agent removes its TLS uprobes as it shuts down. The kernel removes a
// uprobe in the thread that closes it, and abandons the removal when that
// thread is killed part way ("failed to unregister, leaking uprobe"): the
// breakpoint stays in every process that maps the probed library until they
// restart. The CLI killed a running agent three ways: SIGKILL 8 s after a
// SIGTERM (StopCommand, now removed), `docker stop`'s SIGKILL 10 s after its
// SIGTERM, and a hang-up from closing the agent's terminal at once on stop.
// Each landed wherever the agent's shutdown happened to be (agentRun).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	agentUtils "go.keploy.io/server/v3/pkg/platform/http/utils"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// hangBound turns a regression that would hang a test into a failure. It is
// not part of any assertion: every positive assertion waits on its event, and
// this bound is only how long a test gives that event before saying it never
// came.
const hangBound = 2 * time.Minute

// within runs f and fails the test if it has not returned within hangBound.
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(hangBound):
		t.Fatalf("%s did not return within %v", what, hangBound)
	}
}

// returnsWithin reports whether done is closed within d. It asserts only that
// something has NOT happened yet; every positive assertion waits on the event.
func returnsWithin(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// terminateAgent signals the agent once and returns once it has exited, not
// before; it never signals one that is already gone (its pid may be another
// process's by then).
func TestTerminateAgentSignalsOnceAndWaitsForTheExit(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	a := &AgentClient{logger: zap.NewNop()}
	run := newAgentRun()
	t.Cleanup(run.exited)
	var signals atomic.Int32
	signalled := make(chan struct{}, 1)
	terminate := func() error {
		signals.Add(1)
		signalled <- struct{}{}
		return nil
	}
	returned := make(chan struct{})
	go func() { a.terminateAgent(terminate, run, 1); close(returned) }()

	select {
	case <-signalled:
	case <-time.After(hangBound):
		t.Fatal("terminateAgent never signalled the agent")
	}
	if returnsWithin(returned, 200*time.Millisecond) {
		t.Fatal("terminateAgent returned while the agent was still running")
	}
	run.exited()
	<-returned
	if n := signals.Load(); n != 1 {
		t.Fatalf("signalled the agent %d times, want 1", n)
	}

	gone := newAgentRun()
	gone.exited()
	a.terminateAgent(func() error { t.Error("signalled an agent that had already exited"); return nil }, gone, 1)
}

// closedAgentURL is the URL of an agent API nothing listens on: a stop
// request to it is refused, as one to an agent that cannot be asked is.
func closedAgentURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

// startPTYSleeper starts a long sleep on a PTY, as startNativeAgentWithPTY
// starts sudo and the agent, and reaps it when it exits.
func startPTYSleeper(t *testing.T) (*agentUtils.PTYHandle, *exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	agentUtils.ConfigureCommandForPTY(cmd)
	h, err := agentUtils.StartCommandWithPTY(cmd, zap.NewNop())
	if err != nil {
		t.Skipf("no PTY here: %v", err)
	}
	gone := make(chan struct{})
	go func() { _ = h.Wait(); close(gone) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-gone
	})
	return h, cmd, gone
}

// stopAgent never hangs up the agent, answered or not: closing its PTY
// delivers SIGHUP, which kills it where it stands. Its stop goroutine ends it
// with SIGTERM (which also ends sudo at its password prompt). stopAgent gives
// the user's terminal back at once.
func TestStopAgentNeverHangsUpTheAgentsTerminal(t *testing.T) {
	for _, ready := range []bool{true, false} {
		h, cmd, gone := startPTYSleeper(t)
		run := newAgentRun()
		run.ready.Store(ready)
		a := &AgentClient{logger: zap.NewNop(), agentCmd: cmd, agentPTY: h, run: run}

		a.stopAgent()
		if returnsWithin(gone, 300*time.Millisecond) {
			t.Fatalf("ready=%v: stopAgent hung up the agent", ready)
		}
		if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
			t.Fatalf("ready=%v: the agent is gone after stopAgent: %v", ready, err)
		}
		if !agentUtils.StdinOff(h) {
			t.Fatalf("ready=%v: stopAgent did not give the user's terminal back", ready)
		}
	}
}

// stopNativeAgent, the native stop goroutine's call: when the agent cannot be
// asked, its leader gets SIGTERM and is waited for, and nothing kills it --
// answered or not.
func TestStopNativeAgentSignalsAndWaits(t *testing.T) {
	// A refused stop request from an agent that answered is one going away,
	// given the grace before its signal; the grace is not what this tests.
	defer func(g time.Duration) { agentStoppingGrace = g }(agentStoppingGrace)
	agentStoppingGrace = time.Millisecond
	for _, ready := range []bool{true, false} {
		dir := t.TempDir()
		readyFile, got := filepath.Join(dir, "ready"), filepath.Join(dir, "got-term")
		cmd := exec.Command("sh", "-c", `trap 'touch "$GOT"; sleep 0.3; exit 0' TERM; touch "$READY"; while :; do sleep 0.05; done`)
		cmd.Env = append(os.Environ(), "READY="+readyFile, "GOT="+got)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() }) // safe after the reap, unlike a group kill
		run := newAgentRun()
		run.ready.Store(ready)
		waited := make(chan error, 1)
		go func() { err := cmd.Wait(); run.exited(); waited <- err }()
		for {
			if _, err := os.Stat(readyFile); err == nil {
				break
			}
			if returnsWithin(run.gone, 10*time.Millisecond) {
				t.Fatalf("ready=%v: the agent exited before it was ready: %v", ready, <-waited)
			}
		}
		a := clientFor(closedAgentURL(t)) // the API cannot be asked
		within(t, "stopNativeAgent", func() { a.stopNativeAgent(run, cmd.Process.Pid, cmd) })
		if !run.isGone() {
			t.Fatalf("ready=%v: stopNativeAgent returned before the agent exited", ready)
		}
		if err := <-waited; err != nil {
			t.Fatalf("ready=%v: the agent did not exit on its own (%v): it was killed", ready, err)
		}
		if _, err := os.Stat(got); err != nil {
			t.Fatalf("ready=%v: the agent never got SIGTERM", ready)
		}
	}
}

// stopPTYAgent, the PTY stop goroutine's call: the terminal is given back at
// once, and when the agent cannot be asked, sudo (here the PTY's command) gets
// SIGTERM, with the PTY left open, and is waited for.
func TestStopPTYAgentRestoresTheTerminalAndSignals(t *testing.T) {
	h, cmd, gone := startPTYSleeper(t)
	run := newAgentRun()
	go func() { <-gone; run.exited() }()
	a := clientFor(closedAgentURL(t))
	within(t, "stopPTYAgent", func() { a.stopPTYAgent(run, cmd.Process.Pid, h) })
	if !run.isGone() {
		t.Fatal("stopPTYAgent returned before the agent exited")
	}
	if !agentUtils.StdinOff(h) {
		t.Fatal("stopPTYAgent did not give the user's terminal back")
	}
	if st, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !st.Signaled() || st.Signal() != syscall.SIGTERM {
		t.Fatalf("the PTY's command ended with %v, want SIGTERM", cmd.ProcessState)
	}
}

// dockerAgentCancel, the `docker run` command's Cancel: the container is sent
// SIGTERM and waited for, answered or not, never `stop`, and then the client
// is killed.
func TestDockerAgentCancel(t *testing.T) {
	for _, ready := range []bool{true, false} {
		log, release := fakeEngine(t)
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		client := exec.Command("sleep", "300")
		if err := client.Start(); err != nil {
			t.Fatal(err)
		}
		run := newAgentRun()
		run.ready.Store(ready)
		if err := dockerAgentCancel(zap.NewNop(), client, run, stopTestContainer)(); err != nil {
			t.Fatalf("ready=%v: cancel: %v", ready, err)
		}
		if err := client.Wait(); err == nil {
			t.Fatalf("ready=%v: the docker client was not killed", ready)
		}
		requireSignalledAndWaited(t, engineLines(t, log))
	}
}

// requireSignalledAndWaited fails the test unless the engine calls are a
// SIGTERM to the container first, a wait for it, and nothing else but more of
// the same SIGTERM: never `stop`, whose grace period ends in a SIGKILL.
func requireSignalledAndWaited(t *testing.T, calls []string) {
	t.Helper()
	kill, wait := "kill --signal SIGTERM "+stopTestContainer, "wait "+stopTestContainer
	if len(calls) == 0 || calls[0] != kill {
		t.Fatalf("engine calls %q do not start with %q", calls, kill)
	}
	waited := false
	for _, c := range calls {
		switch c {
		case kill:
		case wait:
			waited = true
		default:
			t.Fatalf("engine calls %q include %q", calls, c)
		}
	}
	if !waited {
		t.Fatalf("engine calls %q never wait for the container", calls)
	}
}

// engineLines returns the engine invocations fakeEngine logged, one per call.
func engineLines(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var calls []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			calls = append(calls, l)
		}
	}
	return calls
}

// awaitEngineWait returns once the stop under test is waiting for the
// container (the fake engine has logged the `wait`), and fails the test if the
// stop returned first, or never gets there.
func awaitEngineWait(t *testing.T, log string, returned <-chan struct{}) {
	t.Helper()
	hung := time.After(hangBound)
	for count(engineLines(t, log), "wait "+stopTestContainer) == 0 {
		if returnsWithin(returned, 5*time.Millisecond) {
			t.Fatalf("the stop returned without waiting for the container; calls: %q", engineLines(t, log))
		}
		select {
		case <-hung:
			t.Fatalf("the stop never waited for the container; calls: %q", engineLines(t, log))
		default:
		}
	}
}

// count returns how many of calls are call.
func count(calls []string, call string) int {
	n := 0
	for _, c := range calls {
		if c == call {
			n++
		}
	}
	return n
}

// stopTestContainer is a container name no engine on this host has: should the
// fake engine ever fail, the sudo -n fallback (whose secure_path skips the
// fake) must not reach a real container.
var stopTestContainer = fmt.Sprintf("keploy-stop-test-no-such-container-%d", os.Getpid())

// fakeEngine puts a `docker` first on PATH that logs each invocation's
// arguments and, for `wait`, blocks until the test releases it.
func fakeEngine(t *testing.T) (log, release string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	release = filepath.Join(dir, "release")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
if [ "$1" = wait ]; then
  while [ ! -e "` + release + `" ]; do sleep 0.05; done
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644) })
	return log, release
}

func engineCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", "|"))
}

// A running agent's container gets SIGTERM and is waited for, however long
// it takes -- never `stop`, whose grace period ends in a SIGKILL.
func TestStopAgentContainerSignalsAndWaitsWithoutStop(t *testing.T) {
	log, release := fakeEngine(t)
	returned := make(chan struct{})
	go func() { stopAgentContainer(zap.NewNop(), stopTestContainer, false); close(returned) }()

	awaitEngineWait(t, log, returned)
	if returnsWithin(returned, 200*time.Millisecond) {
		t.Fatal("stopAgentContainer returned while the container was still running")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	<-returned
	got := strings.Join(engineCalls(t, log), " ")
	if want := "kill --signal SIGTERM " + stopTestContainer + "|wait " + stopTestContainer; got != want {
		t.Fatalf("engine calls %q, want %q", got, want)
	}
}

// An agent that never answered is sent SIGTERM again while it is waited for:
// its PID 1 may still have been the OSS image's entrypoint, which drops the
// first. It is never sent `stop`. An agent that answered is past the
// entrypoint, and is sent SIGTERM once.
func TestAnUnansweredAgentContainerIsSignalledAgainWhileWaited(t *testing.T) {
	defer func(f, m time.Duration) { sigtermResendFirst, sigtermResendMax = f, m }(sigtermResendFirst, sigtermResendMax)
	sigtermResendFirst, sigtermResendMax = time.Millisecond, 2*time.Millisecond
	kill := "kill --signal SIGTERM " + stopTestContainer

	log, release := fakeEngine(t)
	returned := make(chan struct{})
	go func() { stopAgentContainer(zap.NewNop(), stopTestContainer, true); close(returned) }()
	hung := time.After(hangBound)
	for count(engineLines(t, log), kill) < 3 {
		if returnsWithin(returned, 5*time.Millisecond) {
			t.Fatalf("stopAgentContainer returned while the container was running; calls: %q", engineLines(t, log))
		}
		select {
		case <-hung:
			t.Fatalf("an unanswered agent's container was not signalled again while waited for; calls: %q", engineLines(t, log))
		default:
		}
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	within(t, "stopAgentContainer", func() { <-returned })
	requireSignalledAndWaited(t, engineLines(t, log))

	log, release = fakeEngine(t)
	returned = make(chan struct{})
	go func() { stopAgentContainer(zap.NewNop(), stopTestContainer, false); close(returned) }()
	awaitEngineWait(t, log, returned)
	// Only that it has NOT been signalled again: under load, too short a
	// look can only miss a wrong resend, never fail a right stop.
	_ = returnsWithin(returned, 100*time.Millisecond)
	if n := count(engineLines(t, log), kill); n != 1 {
		t.Fatalf("an agent that answered was sent SIGTERM %d times, want once", n)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	within(t, "stopAgentContainer", func() { <-returned })
}

// The user's second interrupt ends terminateAgent's wait, not the agent: it
// was signalled once, and nothing else is sent.
func TestTerminateAgentStopsWaitingOnASecondInterrupt(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	a := &AgentClient{logger: zap.NewNop()}
	run := newAgentRun()
	t.Cleanup(run.exited)
	var signals atomic.Int32
	returned := make(chan struct{})
	go func() {
		a.terminateAgent(func() error { signals.Add(1); return nil }, run, 1)
		close(returned)
	}()
	if returnsWithin(returned, 200*time.Millisecond) {
		t.Fatal("terminateAgent returned while the agent was still running")
	}
	utils.MarkInterruptedAgain()
	<-returned
	if n := signals.Load(); n != 1 {
		t.Fatalf("signalled the agent %d times, want 1", n)
	}
}

// The docker stop signals and waits for the container whether or not its
// agent answered: the agent attaches its uprobes before it answers.
func TestStopAgentContainerForSignalsAndWaitsAnsweredOrNot(t *testing.T) {
	for _, ready := range []bool{true, false} {
		log, release := fakeEngine(t)
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		run := newAgentRun()
		run.ready.Store(ready)
		within(t, "stopAgentContainerFor", func() { stopAgentContainerFor(zap.NewNop(), run, stopTestContainer) })
		requireSignalledAndWaited(t, engineLines(t, log))
	}
}

// The user's second interrupt ends the wait for a running agent's container
// (the `wait` client), not the container.
func TestStopAgentContainerStopsWaitingOnASecondInterrupt(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	log, _ := fakeEngine(t)
	ready := newAgentRun()
	ready.ready.Store(true)
	returned := make(chan struct{})
	go func() { stopAgentContainerFor(zap.NewNop(), ready, stopTestContainer); close(returned) }()
	awaitEngineWait(t, log, returned)
	utils.MarkInterruptedAgain()
	<-returned
	if got, want := strings.Join(engineCalls(t, log), " "), "kill --signal SIGTERM "+stopTestContainer+"|wait "+stopTestContainer; got != want {
		t.Fatalf("engine calls %q, want only %q: a second interrupt ends the wait, it sends the container nothing", got, want)
	}
}

// awaitAgentExit: the group waits for the agent's exit, and once the stop has
// begun the user's second interrupt lets the group go, not the agent.
func TestAwaitAgentExitLetsGoOnASecondInterruptOnceStopping(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	a := &AgentClient{logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	reaped := make(chan error, 1)
	returned := make(chan struct{})
	var ok atomic.Bool
	go func() { _, o := a.awaitAgentExit(ctx, reaped, 1); ok.Store(o); close(returned) }()
	cancel()
	if returnsWithin(returned, 200*time.Millisecond) {
		t.Fatal("the group stopped waiting on the stop alone")
	}
	utils.MarkInterruptedAgain()
	<-returned
	if ok.Load() {
		t.Fatal("awaitAgentExit reported an exit it did not see")
	}

	utils.ResetInterruptedAgain()
	reaped2 := make(chan error, 1)
	reaped2 <- nil
	if _, o := a.awaitAgentExit(context.Background(), reaped2, 1); !o {
		t.Fatal("awaitAgentExit missed the exit")
	}
}

// A retried start waits for the agent it replaces to exit.
func TestAwaitPreviousAgent(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	a := &AgentClient{logger: zap.NewNop()}
	if err := a.awaitPreviousAgent(context.Background()); err != nil {
		t.Fatalf("no previous agent: %v", err)
	}
	prev := newAgentRun()
	a.run = prev
	returned := make(chan error, 1)
	go func() { returned <- a.awaitPreviousAgent(context.Background()) }()
	select {
	case err := <-returned:
		t.Fatalf("returned (%v) while the previous agent was still running", err)
	case <-time.After(200 * time.Millisecond):
	}
	prev.exited()
	if err := <-returned; err != nil {
		t.Fatalf("after the previous agent exited: %v", err)
	}
}

// A second interrupt already given before the docker stop begins ends only the
// waiting: the container is still sent its SIGTERM before the call returns --
// or it would run on with its hooks and the app's ports.
func TestStopAgentContainerForSignalsEvenAfterASecondInterrupt(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	utils.MarkInterruptedAgain()
	for _, ready := range []bool{true, false} {
		log, release := fakeEngine(t)
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		run := newAgentRun()
		run.ready.Store(ready)
		within(t, "stopAgentContainerFor", func() { stopAgentContainerFor(zap.NewNop(), run, stopTestContainer) })
		want := "kill --signal SIGTERM " + stopTestContainer
		// Sent synchronously: it has gone out when the call returns, so it
		// has before keploy can exit.
		if got := engineLines(t, log); count(got, want) == 0 {
			t.Fatalf("ready=%v: engine calls %q, want %q sent before the call returned", ready, got, want)
		}
	}
}

// awaitPreviousAgent also ends on the caller's context, and on the user's
// second interrupt, with an error: the new agent is not started then.
func TestAwaitPreviousAgentEndsOnContextOrInterrupt(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	a := &AgentClient{logger: zap.NewNop()}
	a.run = newAgentRun() // never exits

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.awaitPreviousAgent(ctx); err == nil {
		t.Fatal("awaitPreviousAgent ignored the caller's context")
	}

	utils.MarkInterruptedAgain()
	if err := a.awaitPreviousAgent(context.Background()); err == nil {
		t.Fatal("awaitPreviousAgent ignored the user's second interrupt")
	}
}
