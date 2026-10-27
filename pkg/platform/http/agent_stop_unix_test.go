//go:build linux || darwin

package http

// A RUNNING AGENT IS STOPPED BY ASKING IT, AND WAITING FOR IT TO EXIT WHILE
// ITS TEARDOWN MAKES PROGRESS; IT IS KILLED ONLY ONCE THAT STOPS (stopWatch).
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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	agentUtils "go.keploy.io/server/v3/pkg/platform/http/utils"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

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

// fakeNativeAgent is a native agent for the stop: it waits for SIGTERM, then
// reports a teardown as mode says, in the agent's own format, at progress
// (the file its stop reads), one beat every 10 ms:
//
//	progressing  a step every beat, ten of them, then it exits 0
//	stalled      no step, beat after beat, forever
//	detached     the detach done, beat after beat, forever
//	endless      a step every beat, forever
//
// It writes with shell builtins alone: an external writer (mv, touch) it ran
// as it was killed would go on writing in the test's directory after it.
// The stop skips a report it reads half written. It ends once the test binary
// that ran it has (killed at a timeout, which runs no cleanup): in a process
// group of its own, it looped for ever, reparented to init. Run it with its
// test's directory as its argument (fakeAgentCommand), which names it.
const fakeNativeAgent = `report() {
  printf '{"seq":%d,"beat":%d,"steps":true,"done":%s}' "$1" "$2" "$3" > "$PROGRESS"
}
term=0
trap 'term=1' TERM
# busy: in the middle of teardown work, it does not die of the escalation.
[ "$MODE" = busy ] && trap '' USR2
: > "$READY"
while [ "$term" = 0 ]; do kill -0 "$PPID" 2>/dev/null || exit 0; sleep 0.01; done
: > "$GOT"
beat=0
while :; do
  beat=$((beat + 1))
  case "$MODE" in
    progressing) report "$beat" "$beat" false; [ "$beat" -ge 10 ] && exit 0 ;;
    stalled) report 1 "$beat" false ;;
    detached) report 1 "$beat" true ;;
    endless|busy) report "$beat" "$beat" false ;;
  esac
  kill -0 "$PPID" 2>/dev/null || exit 0
  sleep 0.01
done`

// fakeAgentCommand runs fakeNativeAgent for the test whose directory is dir,
// in a process group of its own, as keploy runs an agent.
func fakeAgentCommand(dir string, env ...string) *exec.Cmd {
	cmd := exec.Command("sh", "-c", fakeNativeAgent, "fake-agent", dir)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// stopNativeAgent, the native stop goroutine's call, against a real process:
// the agent, which cannot be asked, gets SIGTERM, answered or not, and is
// waited for while its teardown makes progress. It is killed only once it has
// stopped making any, has detached everything and is still there, or Ctrl+C
// is pressed again.
func TestStopNativeAgentByItsTeardownsProgress(t *testing.T) {
	// A refused stop request from an agent that answered is one going away,
	// given the grace before its signal; the grace is not what this tests.
	defer func(g time.Duration) { agentStoppingGrace = g }(agentStoppingGrace)
	agentStoppingGrace = time.Millisecond
	defer func(n uint64) { stallChecks = n }(stallChecks)
	stallChecks = 3
	for _, tc := range []struct {
		mode string
		// presses: the user's Ctrl+C presses while keploy waits, each once
		// the stop has said a press stops the agent.
		presses  int
		wantKill bool
		// atOnce: killed by SIGKILL (a press after the first), not the
		// escalation.
		atOnce bool
	}{
		{mode: "progressing"},
		{mode: "stalled", wantKill: true},
		{mode: "detached", wantKill: true},
		{mode: "endless", presses: 1, wantKill: true},
		{mode: "busy", presses: 2, wantKill: true, atOnce: true},
	} {
		for _, ready := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/ready=%v", tc.mode, ready), func(t *testing.T) {
				utils.ResetInterruptedAgain()
				t.Cleanup(utils.ResetInterruptedAgain)
				dir := t.TempDir()
				readyFile, got := filepath.Join(dir, "ready"), filepath.Join(dir, "got-term")
				progress := filepath.Join(dir, "teardown.json")
				cmd := fakeAgentCommand(dir, "READY="+readyFile, "GOT="+got, "PROGRESS="+progress, "MODE="+tc.mode)
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
						t.Fatalf("the agent exited before it was ready: %v", <-waited)
					}
				}
				if tc.presses > 0 {
					theStopWasASignal(t)
					go func() {
						// Each pressed until heard: once the stop has said a
						// press stops the agent (stopWatch), its teardown has
						// begun, and, for the next, the offer before was
						// answered (a press finds none standing).
						for pressed, withdrawn := 0, true; pressed < tc.presses; {
							if returnsWithin(run.gone, 5*time.Millisecond) {
								return
							}
							if _, err := os.Stat(progress); err != nil {
								continue
							}
							heard := utils.MarkInterruptedAgain()
							switch {
							case heard && withdrawn:
								pressed++
								withdrawn = false
							case !heard:
								withdrawn = true
							}
						}
					}()
				}
				a := clientFor(closedAgentURL(t)) // the API cannot be asked
				within(t, "stopNativeAgent", func() { a.stopNativeAgent(run, cmd.Process.Pid, cmd, progress) })
				var err error
				within(t, "the agent's exit", func() { err = <-waited })
				if _, statErr := os.Stat(got); statErr != nil {
					t.Fatal("the agent never got SIGTERM")
				}
				st := cmd.ProcessState.Sys().(syscall.WaitStatus)
				// A verdict, and the first press, kill with the escalation
				// signal (SIGUSR2, which kills this stand-in as it kills the
				// agent once no teardown work in progress is making
				// progress); a later press with SIGKILL, at once.
				want := syscall.Signal(syscall.SIGUSR2)
				if tc.atOnce {
					want = syscall.SIGKILL
				}
				killed := st.Signaled() && st.Signal() == want
				if killed != tc.wantKill {
					t.Fatalf("killed=%v (%v, %v), want killed=%v by %v", killed, cmd.ProcessState, err, tc.wantKill, want)
				}
				if !tc.wantKill && err != nil {
					t.Fatalf("the agent did not exit on its own: %v", err)
				}
			})
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
	within(t, "stopPTYAgent", func() { a.stopPTYAgent(run, cmd.Process.Pid, h, "") })
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
// arguments and, for `wait`, blocks until the test releases it -- or until a
// `kill` without --signal (SIGKILL), or with SIGUSR2 (the agent's escalation
// signal, on which it kills itself), ends the container. Its `cp` serves the
// agent's teardown progress as the engine does, a tar stream, from
// engineProgressFile(log), and logs each read's content to engineReads(log);
// `cp` is not logged as a call.
//
// No invocation outlives its test (reapEngine): a `wait`, or a `cp` made to
// hang (slowcp), also ends once the test is done with the engine, once its
// directory is gone, and once the test binary that ran it is (killed at a
// `go test` timeout, it runs no cleanup). Each looped for ever before,
// reparented to init, holding a job slot.
func fakeEngine(t *testing.T) (log, release string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	release = filepath.Join(dir, "release")
	progress := filepath.Dir(engineProgressFile(log))
	if err := os.MkdirAll(progress, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
dir="` + dir + `"
echo $$ >> "$dir/pids"
# held: the test still holds this call -- it is not done with the engine, the
# engine's directory is there, and the test binary that ran it lives.
held() { [ ! -e "$dir/done" ] && [ -d "$dir" ] && kill -0 "$PPID" 2>/dev/null; }
if [ "$1" = cp ]; then
  echo cp >> "` + engineCps(log) + `"
fi
if [ "$1" = cp ] && [ -e "$dir/slowcp" ]; then
  while held; do sleep 0.05; done
  exit 1
fi
if [ "$1" = cp ]; then
  f="` + engineProgressFile(log) + `"
  [ -f "$f" ] || exit 1
  { cat "$f"; echo; } >> "` + engineReads(log) + `"
  exec tar -C "` + progress + `" -cf - "$(basename "$f")"
fi
echo "$*" >> "` + log + `"
if [ "$1" = wait ]; then
  while [ ! -e "` + release + `" ] && held; do sleep 0.05; done
fi
if [ "$1" = kill ] && { [ "$2" != --signal ] || [ "$3" = SIGUSR2 ]; }; then
  : > "` + release + `"
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Registered after t.TempDir, so it runs before the directory is removed.
	t.Cleanup(func() { reapEngine(t, dir) })
	return log, release
}

// reapEngine ends every invocation of the fake engine in dir that is still
// running as its test ends -- a `wait` on a container a test never released,
// or that does not exist, or a hanging `cp` -- and waits for each to exit.
// The test is done with the engine (done), which lets each go. One still
// running after hangBound is killed, and fails the test.
func reapEngine(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "done"), nil, 0o644); err != nil {
		t.Errorf("could not let the fake engine's calls go: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "pids"))
	if err != nil && !os.IsNotExist(err) {
		t.Errorf("could not read the fake engine's pids: %v", err)
	}
	deadline := time.Now().Add(hangBound) // only turns a hang into a failure
	for _, f := range strings.Fields(string(b)) {
		var pid int
		if _, err := fmt.Sscan(f, &pid); err != nil || pid <= 0 {
			continue
		}
		for processRunning(t, pid, filepath.Join(dir, "docker")) {
			if time.Now().After(deadline) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Errorf("the fake engine's process %d did not end with its test; killed it", pid)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// processRunning reports whether pid is still running, as the process whose
// arguments contain match (a fake engine's script, a fake agent's directory):
// not gone, not exited awaiting its reaping, and not another process that has
// since taken its pid. A process table it cannot read fails the test: taken
// for "gone", it passed the tests that wait on it without checking anything.
func processRunning(t *testing.T, pid int, match string) bool {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	var args string
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/proc/self/cmdline"); err != nil {
			t.Fatalf("no process table to read (/proc): %v", err)
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		switch {
		case os.IsNotExist(err) || errors.Is(err, syscall.ESRCH):
			return false
		case err != nil:
			t.Fatalf("read process %d's arguments: %v", pid, err)
		}
		args = strings.ReplaceAll(string(b), "\x00", " ") // a zombie's are empty
	} else {
		out, err := exec.Command("ps", "-o", "stat=,args=", "-p", fmt.Sprint(pid)).Output()
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			return false // no such process
		case err != nil:
			t.Fatalf("read process %d's arguments: %v", pid, err)
		}
		line := strings.TrimSpace(string(out))
		if strings.HasPrefix(line, "Z") {
			return false
		}
		args = line
	}
	return strings.Contains(args, match)
}

// A call to the fake engine that its test never let go -- a `wait` on a
// container it never released, or that does not exist, or a `cp` made to
// hang -- ends with the test, before the engine's directory is removed: one
// that did not looped for ever, reparented to init.
func TestAFakeEngineCallEndsWithItsTest(t *testing.T) {
	for _, call := range []string{"wait", "cp"} {
		var cmd *exec.Cmd
		exited := make(chan error, 1)
		t.Run(call, func(t *testing.T) {
			log, _ := fakeEngine(t)
			dir := filepath.Dir(log)
			if call == "cp" {
				if err := os.WriteFile(filepath.Join(dir, "slowcp"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd = exec.Command(filepath.Join(dir, "docker"), call, stopTestContainer)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go func() { exited <- cmd.Wait() }()
			within(t, "the call under way", func() {
				for {
					pids, _ := os.ReadFile(filepath.Join(dir, "pids"))
					if strings.Contains(string(pids), fmt.Sprint(cmd.Process.Pid)) {
						return
					}
					time.Sleep(time.Millisecond)
				}
			})
			// The test ends here, the call still held.
		})
		if cmd == nil || cmd.Process == nil {
			continue // the subtest failed before the call began
		}
		select {
		case <-exited:
		case <-time.After(hangBound): // only turns a hang into a failure
			_ = cmd.Process.Kill()
			<-exited
			t.Fatalf("a %s the test never let go was still running after its test ended", call)
		}
	}
}

// orphanEnv, set to "engine", "agent" or "agent-teardown", makes
// TestOrphanHelper stand in for a test binary killed with a fake engine's
// `wait`, or a fake native agent waiting for its stop or in its teardown,
// under way.
const orphanEnv = "KEPLOY_TEST_ORPHAN"

// TestOrphanHelper starts a fake engine's `wait`, or a fake native agent, and
// exits as a test binary killed at a `go test` timeout does: at once, its
// cleanups not run. It says what it left running: its pid, and what its
// arguments contain.
func TestOrphanHelper(t *testing.T) {
	what := os.Getenv(orphanEnv)
	if what == "" {
		t.Skip("a helper for TestATestsChildEndsWithTheTestBinary")
	}
	dir := t.TempDir()
	var cmd *exec.Cmd
	var match string
	var started func() bool
	switch what {
	case "engine":
		log, _ := fakeEngine(t)
		match = filepath.Join(filepath.Dir(log), "docker")
		cmd = exec.Command(match, "wait", stopTestContainer)
		started = func() bool { return count(engineLines(t, log), "wait "+stopTestContainer) > 0 }
	case "agent", "agent-teardown":
		ready, got := filepath.Join(dir, "ready"), filepath.Join(dir, "got")
		match = dir
		cmd = fakeAgentCommand(dir, "READY="+ready, "GOT="+got, "PROGRESS="+filepath.Join(dir, "p"), "MODE=endless")
		started = func() bool {
			if _, err := os.Stat(ready); err != nil {
				return false
			}
			if what == "agent" {
				return true
			}
			_ = cmd.Process.Signal(syscall.SIGTERM) // its stop: it reports its teardown, endlessly
			_, err := os.Stat(got)
			return err == nil
		}
	default:
		t.Fatalf("%s=%q", orphanEnv, what)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	hung := time.After(hangBound) // only turns a hang into a failure
	for !started() {
		select {
		case err := <-exited:
			t.Fatalf("the %s exited before it was under way: %v", what, err)
		case <-hung:
			_ = cmd.Process.Kill()
			t.Fatalf("the %s never got under way", what)
		case <-time.After(time.Millisecond):
		}
	}
	fmt.Printf("orphan %d %s\n", cmd.Process.Pid, match)
	os.Exit(3)
}

// A test's child under way when its test binary dies -- killed at a timeout,
// which runs no cleanup -- ends on its own: a fake engine's `wait` and a fake
// native agent, each in a process group of its own, looped for ever,
// reparented to init, holding a job slot.
func TestATestsChildEndsWithTheTestBinary(t *testing.T) {
	for _, what := range []string{"engine", "agent", "agent-teardown"} {
		t.Run(what, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), hangBound) // only turns a hang into a failure
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOrphanHelper$", "-test.timeout="+hangBound.String())
			cmd.Env = append(os.Environ(), orphanEnv+"="+what, "TMPDIR="+t.TempDir())
			cmd.WaitDelay = time.Second
			out, err := cmd.Output()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 3 {
				t.Fatalf("the helper did not exit as a killed binary would (%v): %s", err, out)
			}
			var pid int
			var match string
			if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "orphan %d %s", &pid, &match); err != nil {
				t.Fatalf("the helper did not say what it left running (%v): %q", err, out)
			}
			t.Cleanup(func() {
				if processRunning(t, pid, match) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			// On the test's goroutine: processRunning may fail the test.
			deadline := time.Now().Add(hangBound) // only turns a hang into a failure
			for processRunning(t, pid, match) {
				if time.Now().After(deadline) {
					t.Fatalf("the orphan %d did not end within %v of its test binary", pid, hangBound)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// engineCps is where the fake engine notes each `cp` it is asked for: each
// of a stop's reads of the agent's progress, one a poll.
func engineCps(log string) string { return filepath.Join(filepath.Dir(log), "cps") }

// engineProgressFile is where the fake engine's container holds the agent's
// teardown progress (kdocker.AgentTeardownProgressFile).
func engineProgressFile(log string) string {
	return filepath.Join(filepath.Dir(log), "container", filepath.Base(kdocker.AgentTeardownProgressFile))
}

// engineReads is where the fake engine logs what each `cp` read.
func engineReads(log string) string { return filepath.Join(filepath.Dir(log), "reads") }

// reportInContainer has the fake engine's agent report r, whole.
func reportInContainer(t *testing.T, log string, r utils.TeardownProgress) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	f := engineProgressFile(log)
	if err := os.WriteFile(f+".tmp", b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f+".tmp", f); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// awaitEngineRead returns once the stop under test has read report (a JSON
// line reportInContainer returned) out of the container, and read it once
// more since, or once returned is closed.
func awaitEngineRead(t *testing.T, log, report string, returned <-chan struct{}) {
	t.Helper()
	hung := time.After(hangBound)
	for {
		b, _ := os.ReadFile(engineReads(log))
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if l == report && i+1 < len(lines)-1 {
				return
			}
		}
		select {
		case <-returned:
			return
		case <-hung:
			t.Fatalf("the stop never read %s out of the container", report)
		case <-time.After(2 * time.Millisecond):
		}
	}
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
	go func() { stopAgentContainer(zap.NewNop(), stopTestContainer, false, nil); close(returned) }()

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
	go func() { stopAgentContainer(zap.NewNop(), stopTestContainer, true, nil); close(returned) }()
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
	go func() { stopAgentContainer(zap.NewNop(), stopTestContainer, false, nil); close(returned) }()
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

// The agent's container is waited for while its teardown makes progress, and
// killed only once it has stopped making any, or has detached everything and
// is still there a check later -- with the escalation signal, on which the
// agent dies once no teardown work in progress is making progress (SIGUSR2),
// not SIGKILL.
func TestTheAgentContainerIsKilledOnlyWhenItStopsProgressing(t *testing.T) {
	defer func(d time.Duration) { dockerStopPoll = d }(dockerStopPoll)
	dockerStopPoll = 5 * time.Millisecond
	defer func(n uint64) { stallChecks = n }(stallChecks)
	stallChecks = 3
	for _, tc := range []struct {
		name     string
		report   func(beat uint64) utils.TeardownProgress
		wantKill bool
	}{
		{"progressing", func(b uint64) utils.TeardownProgress { return p(b+1, b, false) }, false},
		{"stalled", func(b uint64) utils.TeardownProgress { return p(1, b, false) }, true},
		{"detached", func(b uint64) utils.TeardownProgress { return p(1, b, true) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			utils.ResetInterruptedAgain()
			t.Cleanup(utils.ResetInterruptedAgain)
			log, release := fakeEngine(t)
			returned := make(chan struct{})
			go func() { stopAgentContainer(zap.NewNop(), stopTestContainer, false, nil); close(returned) }()
			awaitEngineWait(t, log, returned)
			for beat := uint64(0); ; beat++ {
				if !returnsWithin(returned, 0) && !tc.wantKill && beat == 4*stallChecks {
					// It has made progress across many times stallChecks
					// checks; now it exits.
					if err := os.WriteFile(release, nil, 0o644); err != nil {
						t.Fatal(err)
					}
					break
				}
				awaitEngineRead(t, log, reportInContainer(t, log, tc.report(beat)), returned)
				if returnsWithin(returned, 0) {
					break
				}
			}
			within(t, "stopAgentContainer", func() { <-returned })
			calls := engineLines(t, log)
			if killed := count(calls, "kill --signal SIGUSR2 "+stopTestContainer) == 1; killed != tc.wantKill {
				t.Fatalf("killed=%v, want %v; calls %q", killed, tc.wantKill, calls)
			}
			if count(calls, "kill "+stopTestContainer) != 0 {
				t.Fatalf("calls %q: SIGKILL, though only Ctrl+C pressed again sends it", calls)
			}
			if count(calls, "kill --signal SIGTERM "+stopTestContainer) != 1 {
				t.Fatalf("calls %q: want one SIGTERM", calls)
			}
		})
	}
}

// An agent that never answered is sent SIGTERM again only until it reports its
// teardown: it got one, and more only make it say so.
func TestResendsStopOnceTheAgentReportsItsTeardown(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	defer func(f, m time.Duration) { sigtermResendFirst, sigtermResendMax = f, m }(sigtermResendFirst, sigtermResendMax)
	sigtermResendFirst, sigtermResendMax = time.Millisecond, 2*time.Millisecond
	defer func(d time.Duration) { dockerStopPoll = d }(dockerStopPoll)
	dockerStopPoll = 5 * time.Millisecond
	sigterm := "kill --signal SIGTERM " + stopTestContainer

	log, release := fakeEngine(t)
	core, logs := observer.New(zap.DebugLevel)
	returned := make(chan struct{})
	go func() { stopAgentContainer(zap.New(core), stopTestContainer, true, nil); close(returned) }()
	hung := time.After(hangBound)
	for count(engineLines(t, log), sigterm) < 2 {
		select {
		case <-hung:
			t.Fatalf("an unanswered agent was not signalled again; calls %q", engineLines(t, log))
		case <-returned:
			t.Fatal("the stop returned while the container was running")
		case <-time.After(2 * time.Millisecond):
		}
	}
	beat := uint64(0)
	reportBeats := func(n uint64) {
		for end := beat + n; beat < end; beat++ {
			awaitEngineRead(t, log, reportInContainer(t, log, p(beat+1, beat, false)), returned)
		}
	}
	// Once the resends have stopped, as they say, no SIGTERM follows.
	stopped := time.After(hangBound)
	for logs.FilterMessageSnippet("is not sent another").Len() == 0 {
		select {
		case <-stopped:
			t.Fatalf("the resends never stopped after the agent reported its teardown; calls %q", engineLines(t, log))
		default:
		}
		reportBeats(1)
	}
	settled := count(engineLines(t, log), sigterm)
	reportBeats(10)
	if n := count(engineLines(t, log), sigterm); n != settled {
		t.Fatalf("SIGTERM was sent %d more times after the agent reported its teardown", n-settled)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	within(t, "stopAgentContainer", func() { <-returned })
}

// A `cp` out of the container that hangs never holds up the stop once the
// container has exited: it runs under a context the stop cancels.
func TestAHangingCpDoesNotHoldUpTheDockerStop(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	defer func(d time.Duration) { dockerStopPoll = d }(dockerStopPoll)
	dockerStopPoll = 5 * time.Millisecond
	log, release := fakeEngine(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(log), "slowcp"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	returned := make(chan struct{})
	go func() { stopAgentContainer(zap.NewNop(), stopTestContainer, false, nil); close(returned) }()
	awaitEngineWait(t, log, returned)
	_ = returnsWithin(returned, 50*time.Millisecond) // a `cp` is under way by now, as a rule
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	within(t, "stopAgentContainer, with a `cp` that hangs", func() { <-returned })
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

// Ctrl+C pressed while the agent's container is waited for, once the stop
// has said a press stops it, sends the escalation signal: the agent dies once
// no teardown work in progress is making progress, here at once.
func TestASecondCtrlCKillsTheAgentContainer(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	theStopWasASignal(t)
	log, _ := fakeEngine(t)
	ready := newAgentRun()
	ready.ready.Store(true)
	returned := make(chan struct{})
	go func() { stopAgentContainerFor(zap.NewNop(), ready, stopTestContainer); close(returned) }()
	awaitEngineWait(t, log, returned)
	ctrlCWhileWaited(t)
	within(t, "stopAgentContainerFor", func() { <-returned })
	if got, want := strings.Join(engineCalls(t, log), " "), "kill --signal SIGTERM "+stopTestContainer+"|wait "+stopTestContainer+"|kill --signal SIGUSR2 "+stopTestContainer; got != want {
		t.Fatalf("engine calls %q, want %q: Ctrl+C pressed escalates the container's stop", got, want)
	}
}

// awaitAgentExit: the group waits for the agent's exit, and once the stop has
// killed the agent at once at the user's Ctrl+C (run.letGo) it lets the group
// go, not waiting for the agent to finish dying. An interrupt alone does not:
// only the stop, which hears no copy of its own signal, says what one was.
func TestAwaitAgentExitLetsGoOnASecondInterruptOnceStopping(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	a := &AgentClient{logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	reaped := make(chan error, 1)
	run := newAgentRun()
	returned := make(chan struct{})
	var ok atomic.Bool
	go func() { _, o := a.awaitAgentExit(ctx, reaped, run, 1); ok.Store(o); close(returned) }()
	cancel() // the agent stop begins
	utils.MarkInterruptedAgain()
	if returnsWithin(returned, 200*time.Millisecond) {
		t.Fatal("the group stopped waiting on the stop, or on an interrupt its stop did not act on")
	}
	run.stoppedAtOnce() // its stop killed it at once at the user's Ctrl+C
	within(t, "awaitAgentExit", func() { <-returned })
	if ok.Load() {
		t.Fatal("awaitAgentExit reported an exit it did not see")
	}

	reaped2 := make(chan error, 1)
	reaped2 <- nil
	if _, o := a.awaitAgentExit(context.Background(), reaped2, newAgentRun(), 1); !o {
		t.Fatal("awaitAgentExit missed the exit")
	}
}

// The user's Ctrl+C while keploy waits for a stopping agent, once the
// recording's drains are done and their offers withdrawn: the stop says a
// press stops the agent. One the CLI started directly is killed at once at
// the press after the one that escalated, and the group lets go at that press
// (awaitAgentExit, run.letGo): it does not wait out the teardown's bound to
// then say a goroutine ignored its cancellation. One that runs under sudo (a
// non-root user's), as root, cannot be killed at once: each press only sends
// it the escalation signal again, and the group waits for its exit while its
// stop holds the drains -- keploy never exits leaving a root agent running.
// The press after the first had the group let go of it, still running.
func TestAUsersCtrlCDuringTheAgentStopLetsTheGroupGo(t *testing.T) {
	for _, tc := range []struct {
		name string
		root bool
	}{{"direct", false}, {"sudo", true}} {
		t.Run(tc.name, func(t *testing.T) {
			utils.ResetInterruptedAgain()
			t.Cleanup(utils.ResetInterruptedAgain)
			utils.MarkInterrupted() // the user's Ctrl+C stopped the recording
			t.Cleanup(utils.ClearInterrupted)
			core, logs := observer.New(zapcore.InfoLevel)
			a := clientFor(closedAgentURL(t))
			a.logger = zap.New(core)
			run := newAgentRun()
			run.ready.Store(true)
			reaped := make(chan error, 1)
			exit := sync.OnceFunc(func() { run.exited(); reaped <- nil })
			t.Cleanup(exit)
			ctx, cancel := context.WithCancel(context.Background())
			var grp errgroup.Group
			var beat atomic.Uint64
			kills := make(chan bool, 64)
			// nextKill returns whether the stop's next kill was to be at once.
			nextKill := func(what string) bool {
				t.Helper()
				select {
				case now := <-kills:
					return now
				case <-time.After(time.Minute): // only turns a hang into a failure
					t.Fatalf("the stop made no kill at %s", what)
					return false
				}
			}
			grp.Go(func() error { a.awaitAgentExit(ctx, reaped, run, 1); return nil })
			grp.Go(func() error {
				<-ctx.Done()
				a.endAgent(run, 1,
					func() error { return nil }, // asked over its API
					func() error { return nil },
					func(now bool) error { kills <- now; return nil }, // busy removing its uprobes: no kill ends it here
					killReach{noAtOnce: tc.root},
					func(context.Context) (utils.TeardownProgress, bool) {
						// The agent's own pid, which a running process has.
						return utils.TeardownProgress{Seq: 1, Beat: beat.Add(1), Busy: true, Steps: true, Pid: os.Getpid()}, true
					})
				return nil
			})
			cancel() // setupCtxCancel: the agent stop begins
			// Once the stop has read the agent's first report: a press before it
			// has the escalation sent again at that report (stopWatch), a kill
			// more than the presses make.
			within(t, "the stop saying a press stops the agent", func() {
				for logs.FilterMessageSnippet("Press Ctrl+C again to stop the keploy agent").Len() == 0 ||
					logs.FilterMessageSnippet("Waiting for the keploy agent to finish shutting down").Len() == 0 {
					time.Sleep(10 * time.Millisecond)
				}
			})
			// The user presses Ctrl+C, as told: the agent, busy removing its
			// uprobes, does not die of the escalation.
			ctrlCWhileWaited(t)
			if nextKill("the first press") {
				t.Fatal("the first press killed the agent at once")
			}
			if !tc.root {
				// The user presses again once asked: a kill at once, and
				// the group lets go.
				ctrlCAgainWhileWaited(t)
				if !nextKill("the press after the second offer") {
					t.Fatal("the press after the second offer was no kill at once")
				}
				// The bound only catches a group that never lets go; it
				// lets go at once.
				if _, timedOut := utils.DrainErrGroupStatus(a.logger, "record-setup", &grp, 30*time.Second); timedOut {
					t.Fatalf("the user's Ctrl+C during the agent stop did not let the group go: %v", logs.FilterLevelExact(zapcore.ErrorLevel).All())
				}
				return
			}
			// Under sudo: no press stops keploy waiting. The user presses
			// again anyway: the escalation again, and keploy goes on
			// waiting, holding the drains.
			utils.MarkInterruptedAgain()
			nextKill("a press after the first")
			if logs.FilterMessageSnippet("stop the keploy agent at once").Len() != 0 ||
				logs.FilterMessageSnippet("stop waiting for the keploy agent").Len() != 0 {
				t.Fatalf("the stop offered a press that would leave the agent under sudo running: %v", logs.All())
			}
			groupDone := make(chan struct{})
			go func() { _ = grp.Wait(); close(groupDone) }()
			select {
			case <-run.letGo:
				t.Fatal("the group let go of an agent under sudo, as root, still running")
			case <-groupDone:
				t.Fatal("the group ended with an agent under sudo, as root, still running")
			default:
			}
			if !utils.AgentStopInProgress() {
				t.Fatal("the stop of an agent under sudo let the drains go while it runs")
			}
			exit() // the agent ends its teardown and exits
			// The bound only catches a group that never lets go.
			if _, timedOut := utils.DrainErrGroupStatus(a.logger, "record-setup", &grp, 30*time.Second); timedOut {
				t.Fatalf("the group did not end at the agent's exit: %v", logs.FilterLevelExact(zapcore.ErrorLevel).All())
			}
		})
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

// An interrupt that came before the docker stop began does not force it:
// `sudo pkill -INT -f keploy` signals sudo and keploy both, and sudo hands its
// signal on, so one stop reaches keploy twice. The container gets its SIGTERM
// and is waited for, and the copy, whether it came before the stop or during
// it, kills nothing: the stop offers a press only a whole period after its
// signal came -- on the test's clock, which stands still, never.
func TestAnInterruptBeforeTheDockerStopDoesNotForceIt(t *testing.T) {
	defer func(d time.Duration) { dockerStopPoll = d }(dockerStopPoll)
	dockerStopPoll = 10 * time.Millisecond
	at := time.Now()
	defer utils.SetNoticeClockForTest(func() time.Time { return at })()
	for _, copyAt := range []string{"before", "during"} {
		for _, ready := range []bool{true, false} {
			utils.ResetInterruptedAgain()
			utils.ClearInterrupted()
			t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
			utils.MarkInterrupted() // the stop
			if copyAt == "before" {
				utils.MarkInterruptedAgain()
			}
			anInterruptedDockerStopIsNotForced(t, ready, copyAt == "during")
		}
	}
}

func anInterruptedDockerStopIsNotForced(t *testing.T, ready, copyDuring bool) {
	t.Helper()
	log, release := fakeEngine(t)
	run := newAgentRun()
	run.ready.Store(ready)
	returned := make(chan struct{})
	go func() { stopAgentContainerFor(zap.NewNop(), run, stopTestContainer); close(returned) }()
	awaitEngineWait(t, log, returned)
	if copyDuring {
		// Once the stop has polled the container a few times.
		within(t, "the stop's polls", func() {
			for {
				if b, _ := os.ReadFile(engineCps(log)); bytes.Count(b, []byte("\n")) >= 3 {
					return
				}
				time.Sleep(time.Millisecond)
			}
		})
		utils.MarkInterruptedAgain()
	}
	// Only that it has NOT been killed: the container is still running, so
	// a stop that took the copy for a press kills it.
	_ = returnsWithin(returned, 100*time.Millisecond)
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	within(t, "stopAgentContainerFor", func() { <-returned })
	calls := engineLines(t, log)
	requireSignalledAndWaited(t, calls)
	if count(calls, "kill "+stopTestContainer) != 0 || count(calls, "kill --signal SIGUSR2 "+stopTestContainer) != 0 {
		t.Fatalf("ready=%v, copy during the stop=%v: a copy of the stop forced it; calls %q", ready, copyDuring, calls)
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

	// An interrupt given while it waits (one from before is no interrupt of
	// the wait: it may be the first stop delivered twice).
	returned := make(chan error, 1)
	go func() { returned <- a.awaitPreviousAgent(context.Background()) }()
	select { // only that it has NOT returned yet
	case err := <-returned:
		t.Fatalf("returned (%v) while the previous agent was still running", err)
	case <-time.After(100 * time.Millisecond):
	}
	utils.MarkInterruptedAgain()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("awaitPreviousAgent ignored the user's second interrupt")
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("awaitPreviousAgent ignored the user's second interrupt")
	}

	// And it gives up after its bound, as a drain does, when no agent stop
	// holds the drains: a new start must not wait forever for an agent that
	// will not exit.
	defer func(d time.Duration) { agentExitBound = d }(agentExitBound)
	agentExitBound = time.Millisecond
	utils.ResetInterruptedAgain()
	if err := a.awaitPreviousAgent(context.Background()); err == nil {
		t.Fatal("awaitPreviousAgent waited past its bound for an agent that never exits")
	}
}

// An agent docker compose stops, whose recording gave a drain up on a stall
// (its stream may stay open, and the agent waits for it), is sent the
// escalation signal once, which ends that wait, and is then waited for as any
// other: its teardown judged, never SIGTERM nor SIGKILL from keploy.
func TestEndComposeAgentReleasesAnAgentAStalledDrainHolds(t *testing.T) {
	defer func(d time.Duration) { dockerStopPoll = d }(dockerStopPoll)
	dockerStopPoll = 5 * time.Millisecond
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	// The fake engine's container ends at the escalation signal, as the
	// agent, released, then goes.
	log, _ := fakeEngine(t)
	a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{KeployContainer: stopTestContainer}}
	returned := make(chan struct{})
	go func() { a.EndComposeAgent(true); close(returned) }()
	within(t, "EndComposeAgent", func() { <-returned })
	calls := engineLines(t, log)
	sig, wait := slices.Index(calls, "kill --signal SIGUSR2 "+stopTestContainer), slices.Index(calls, "wait "+stopTestContainer)
	if sig < 0 || wait < sig || count(calls, "kill --signal SIGUSR2 "+stopTestContainer) != 1 {
		t.Fatalf("calls %q: want the escalation signal once, then the wait", calls)
	}
	if n := count(calls, "kill --signal SIGTERM "+stopTestContainer) + count(calls, "kill "+stopTestContainer); n != 0 {
		t.Fatalf("calls %q: keploy sent SIGTERM or SIGKILL", calls)
	}
}

// The agent docker compose stops with the app's project, once a recording has
// drained it, is waited for while its teardown makes progress, not for
// compose's grace (24h under keploy record): one that ignores its stop -- its
// teardown takes no step -- is wedged, and is killed with the escalation
// signal, keploy saying so at WARN. One that exits is only waited for. keploy
// sends neither a stop signal of its own: compose's is the agent's stop.
func TestEndComposeAgentKillsOnlyAWedgedAgent(t *testing.T) {
	defer func(d time.Duration) { dockerStopPoll = d }(dockerStopPoll)
	dockerStopPoll = 5 * time.Millisecond
	defer func(n uint64) { stallChecks = n }(stallChecks)
	stallChecks = 3
	for _, tc := range []struct {
		name   string
		report func(beat uint64) utils.TeardownProgress
		wedged bool
	}{
		{"an agent that exits", func(b uint64) utils.TeardownProgress { return p(b+1, b, false) }, false},
		{"an agent that ignores its stop", func(b uint64) utils.TeardownProgress { return p(1, b, false) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			utils.ResetInterruptedAgain()
			t.Cleanup(utils.ResetInterruptedAgain)
			log, release := fakeEngine(t)
			core, logs := observer.New(zap.WarnLevel)
			a := &AgentClient{logger: zap.New(core), conf: &config.Config{KeployContainer: stopTestContainer}}
			returned := make(chan struct{})
			go func() { a.EndComposeAgent(false); close(returned) }()
			awaitEngineWait(t, log, returned)
			for beat := uint64(0); ; beat++ {
				if !returnsWithin(returned, 0) && !tc.wedged && beat == 4*stallChecks {
					// Its teardown went on across many times stallChecks
					// checks; now it exits.
					if err := os.WriteFile(release, nil, 0o644); err != nil {
						t.Fatal(err)
					}
					break
				}
				awaitEngineRead(t, log, reportInContainer(t, log, tc.report(beat)), returned)
				if returnsWithin(returned, 0) {
					break
				}
			}
			within(t, "EndComposeAgent", func() { <-returned })
			calls := engineLines(t, log)
			if killed := count(calls, "kill --signal SIGUSR2 "+stopTestContainer) == 1; killed != tc.wedged {
				t.Fatalf("killed=%v, want %v; calls %q", killed, tc.wedged, calls)
			}
			if n := count(calls, "kill --signal SIGTERM "+stopTestContainer) + count(calls, "kill "+stopTestContainer); n != 0 {
				t.Fatalf("calls %q: keploy signalled an agent compose had stopped", calls)
			}
			said := logs.FilterMessageSnippet("Stopping the keploy agent by force").Len() == 1
			if said != tc.wedged {
				t.Fatalf("said it killed a wedged agent: %v, want %v (%v)", said, tc.wedged, logs.All())
			}
		})
	}
}

// The stop of an agent's container moves the teardown of the run that
// started the agent (stopAgentContainerFor gives its watch the agent's run's
// scope) at each report the container holds that shows its teardown going
// on: that run's drains wait on it.
func TestAContainerisedAgentsStopMovesTheTeardownOfTheRunThatStartedIt(t *testing.T) {
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	defer func(d time.Duration) { dockerStopPoll = d }(dockerStopPoll)
	dockerStopPoll = 5 * time.Millisecond
	log, release := fakeEngine(t)
	a := &AgentClient{logger: zap.NewNop()}
	ctx, scope := utils.NewTeardownScope(context.Background())
	_, stop, run, err := a.beginRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	run.ready.Store(true)
	returned := make(chan struct{})
	go func() { stopAgentContainerFor(zap.NewNop(), run, stopTestContainer); close(returned) }()
	awaitEngineWait(t, log, returned)
	for n := uint64(1); n <= 3; n++ {
		awaitEngineRead(t, log, reportInContainer(t, log, utils.TeardownProgress{Seq: n, Beat: n}), returned)
		for deadline := time.Now().Add(hangBound); scope.Moves() < n; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) { // only turns a hang into a failure
				t.Fatalf("the container's report %d did not move the teardown of the run that started the agent (%d movements)", n, scope.Moves())
			}
		}
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	within(t, "stopAgentContainerFor", func() { <-returned })
}
