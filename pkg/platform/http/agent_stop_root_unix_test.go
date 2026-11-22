//go:build linux || darwin

package http

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// startFakeRootAgent starts fakeRootAgent as a command named sudo, writing in
// dir: ready, its report (teardown.json), and the release it waits for.
func startFakeRootAgent(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	sudo := filepath.Join(dir, "sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\n"+fakeRootAgent+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sudo)
	cmd.Env = append(os.Environ(), "READY="+filepath.Join(dir, "ready"),
		"PROGRESS="+filepath.Join(dir, "teardown.json"), "RELEASE="+filepath.Join(dir, "release"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// fakeRootAgent is sudo running the agent as root, as the stop sees it: a
// process named sudo, which keploy may send only what sudo hands on. On
// SIGTERM it reports a teardown that keeps making progress, one beat every
// 10 ms. On the escalation signal it goes on -- as a root agent in the middle
// of a removal does, until that removal ends -- until the test creates
// $RELEASE, and then dies as the agent does on the escalation (SIGKILL to
// itself). It reports its own pid, as the agent does. It writes with shell
// builtins alone, as fakeNativeAgent does, and both its loops end once the
// test binary that started it is gone (kill -0 $PPID): killed mid-test, it
// runs no cleanup, and a stand-in that looped for ever outlived it.
const fakeRootAgent = `report() {
  printf '{"seq":%d,"beat":%d,"steps":true,"busy":true,"done":false,"pid":%d}' "$1" "$2" "$$" > "$PROGRESS"
}
term=0
usr2=0
trap 'term=1' TERM
trap 'usr2=1' USR2
: > "$READY"
while [ "$term" = 0 ]; do kill -0 "$PPID" 2>/dev/null || exit 0; sleep 0.01; done
beat=0
while :; do
  kill -0 "$PPID" 2>/dev/null || exit 0
  beat=$((beat + 1))
  report "$beat" "$beat"
  if [ "$usr2" = 1 ] && [ -e "$RELEASE" ]; then
    kill -KILL $$
  fi
  sleep 0.01
done`

// loggedCore tells seen of each entry whose message starts with prefix, as
// far as its buffer holds.
type loggedCore struct {
	zapcore.Core
	prefix string
	seen   chan struct{}
}

func newLoggedCore(prefix string) loggedCore {
	return loggedCore{Core: zapcore.NewNopCore(), prefix: prefix, seen: make(chan struct{}, 64)}
}

func (c loggedCore) Enabled(zapcore.Level) bool        { return true }
func (c loggedCore) With([]zapcore.Field) zapcore.Core { return c }
func (c loggedCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if strings.HasPrefix(e.Message, c.prefix) {
		select {
		case c.seen <- struct{}{}:
		default:
		}
	}
	return ce
}

// Ctrl+C pressed while keploy waits for an agent sudo runs as root, as told,
// and pressed again, never leaves that agent running: keploy cannot kill it
// (it is root), only send it the escalation signal through sudo, at every
// press, on which the agent ends as soon as the removal it is in the middle
// of ends or stops making progress -- and keploy waits for that. It offered a
// press to stop waiting, and exited at it with the root agent still running
// behind it.
func TestASecondCtrlCWaitsForARootAgentToExit(t *testing.T) {
	// A refused stop request from an agent that answered is one going away,
	// given the grace before its signal; the grace is not what this tests.
	defer func(g time.Duration) { agentStoppingGrace = g }(agentStoppingGrace)
	agentStoppingGrace = time.Millisecond
	utils.ResetInterruptedAgain()
	t.Cleanup(utils.ResetInterruptedAgain)
	theStopWasASignal(t)
	dir := t.TempDir()
	cmd := startFakeRootAgent(t, dir)
	readyFile, release := filepath.Join(dir, "ready"), filepath.Join(dir, "release")
	progress := filepath.Join(dir, "teardown.json")
	run := newAgentRun()
	run.ready.Store(true)
	waited := make(chan error, 1)
	go func() { err := cmd.Wait(); run.exited(); waited <- err }()
	starting := time.After(hangBound) // only turns a hang into a failure
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if returnsWithin(run.gone, 10*time.Millisecond) {
			t.Fatalf("the agent exited before it was ready: %v", <-waited)
		}
		select {
		case <-starting:
			t.Fatal("the agent never said it was ready")
		default:
		}
	}
	waiting := newLoggedCore("Keploy goes on waiting for the keploy agent to exit before it stops")
	a := New(zap.New(waiting), nil, &config.Config{Agent: config.Agent{AgentURI: closedAgentURL(t)}})
	returned := make(chan struct{})
	go func() {
		a.stopNativeAgent(run, cmd.Process.Pid, cmd, progress)
		close(returned)
	}()
	for {
		if _, err := os.Stat(progress); err == nil {
			break // it reports its teardown: keploy waits for it
		}
		if returnsWithin(run.gone, 5*time.Millisecond) {
			t.Fatalf("the agent exited before it reported its teardown: %v", <-waited)
		}
		select {
		case <-starting:
			t.Fatal("the agent never reported its teardown")
		default:
		}
	}
	// Ctrl+C, pressed over and over -- as told, and after, whatever keploy
	// offers: keploy never stops waiting for the root agent, and says so at
	// each press it hears.
	hung := time.After(hangBound) // only turns a hang into a failure
	for heard := 0; heard < 3; {
		utils.MarkInterruptedAgain()
		select {
		case <-returned:
			if !run.isGone() {
				t.Fatal("keploy stopped waiting at Ctrl+C while the agent sudo runs as root was still running: it is left behind")
			}
			t.Fatal("the root agent exited before it was released: this test proves nothing")
		case <-waiting.seen:
			heard++
		case <-hung:
			t.Fatal("the stop neither returned nor waited for the root agent")
		case <-time.After(5 * time.Millisecond): // the next press
		}
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	within(t, "stopNativeAgent", func() { <-returned })
	// It returned once the agent exited, or once it was killed and only not
	// reaped yet (halted): either way, killed by its own escalation.
	within(t, "the root agent's exit", func() { <-waited })
	if st := cmd.ProcessState.Sys().(syscall.WaitStatus); !st.Signaled() || st.Signal() != syscall.SIGKILL {
		t.Fatalf("the root agent ended with %v, want it to have killed itself on the escalation", cmd.ProcessState)
	}
}

const rootAgentOrphanEnv = "KEPLOY_TEST_ROOT_AGENT_ORPHAN"

// TestRootAgentOrphanParent is not a test of its own: with
// KEPLOY_TEST_ROOT_AGENT_ORPHAN set to a directory it is a test binary that
// starts the fake root agent there, writes its pid to the file "pid" once it
// is up, and waits to be killed. It exits once its stdin, a pipe the test
// binary that started it holds open, reads end of file: it ends with that
// binary.
func TestRootAgentOrphanParent(t *testing.T) {
	dir := os.Getenv(rootAgentOrphanEnv)
	if dir == "" {
		return
	}
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(5)
	}()
	cmd := startFakeRootAgent(t, dir)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond) // the parent of a leak test: waits for its stand-in
	}
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	select {} // killed here, mid-test: no cleanup runs
}

// The fake root agent never outlives a test binary killed mid-test (a go test
// timeout, a job slot's memory cap): its loops end once that binary is gone.
func TestNoFakeRootAgentOutlivesItsTest(t *testing.T) {
	dir := t.TempDir() // the parent's: removed here, as the parent is killed
	parent := exec.Command(os.Args[0], "-test.run=^TestRootAgentOrphanParent$")
	parent.Env = append(os.Environ(), rootAgentOrphanEnv+"="+dir)
	parent.Stdout, parent.Stderr = os.Stderr, os.Stderr
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := parent.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = parent.Wait(); close(reaped) }()
	t.Cleanup(func() {
		select {
		case <-reaped:
		default: // not reaped: its process group is still its own
			_ = syscall.Kill(-parent.Process.Pid, syscall.SIGKILL)
			<-reaped
		}
		_ = stdin.Close()
	})
	pid := 0
	hung := time.After(hangBound) // only turns a hang into a failure
	for pid == 0 {
		if b, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			continue
		}
		select {
		case <-reaped:
			t.Fatal("the parent ended before its stand-in was up")
		case <-hung:
			t.Fatal("the parent never said its stand-in's pid")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := parent.Process.Kill(); err != nil { // SIGKILL, mid-test
		t.Fatal(err)
	}
	<-reaped
	hung = time.After(hangBound) // only turns a leak into a failure
	for processAlive(pid) {
		select {
		case <-hung:
			_ = syscall.Kill(pid, syscall.SIGKILL) // this test's own leak, ended
			t.Fatalf("the fake root agent %d outlived the test binary that started it", pid)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// processAlive reports whether process pid still runs: it exists and, where
// /proc tells, is not a zombie -- which a PID 1 that reaps nothing would keep
// for ever.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		return true // no /proc here: kill alone tells
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	f := strings.Fields(string(b[i+1:]))
	return i >= 0 && len(f) > 0 && f[0][0] != 'Z' && f[0][0] != 'X'
}
