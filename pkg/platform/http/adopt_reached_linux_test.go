//go:build linux

package http

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// stopHelperEnv names what a stop helper (TestStopHelperProcess) is; the
// directory it writes in is stopHelperDirEnv.
const (
	stopHelperEnv    = "KEPLOY_TEST_STOP_HELPER"
	stopHelperDirEnv = "KEPLOY_TEST_STOP_HELPER_DIR"
)

// TestStopHelperProcess is not a test: it is a process of the application
// the tests below run, as stopHelperEnv names it. Each writes <name>.ready,
// with its pid, once its signals are set up, and a file for each signal that
// reaches it:
//
//   - acting acts on SIGINT, Go's way: notified of it, and of SIGCONT; its
//     SIGTERM is Go's default, which ends it, as a second signal ends an
//     application whose signal.NotifyContext's stop was called. Once it has
//     had SIGINT it writes acting.int, waits for the SIGCONT keploy's stop of
//     what the application left sends, and writes acting.done as it exits.
//   - ignoring ignores SIGINT, as an application started in the background
//     of a shell does (POSIX), and stops on SIGTERM: ignoring.term.
//   - daemon stops on SIGINT (daemon.int) or SIGTERM (daemon.term).
func TestStopHelperProcess(t *testing.T) {
	name := os.Getenv(stopHelperEnv)
	if name == "" {
		t.Skip("a process of the stop tests' application")
	}
	dir := os.Getenv(stopHelperDirEnv)
	note := func(file string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(3)
		}
	}
	switch name {
	case "acting":
		ints, conts := make(chan os.Signal, 1), make(chan os.Signal, 1)
		signal.Notify(ints, syscall.SIGINT)
		signal.Notify(conts, syscall.SIGCONT)
		note(name + ".ready")
		<-ints
		note(name + ".int")
		<-conts
		note(name + ".done")
	case "ignoring":
		signal.Ignore(syscall.SIGINT)
		terms := make(chan os.Signal, 1)
		signal.Notify(terms, syscall.SIGTERM)
		note(name + ".ready")
		<-terms
		note(name + ".term")
	case "hangs":
		// Acts on SIGINT, ignores SIGTERM, and never gets out.
		signal.Ignore(syscall.SIGTERM)
		ints := make(chan os.Signal, 1)
		signal.Notify(ints, syscall.SIGINT)
		note(name + ".ready")
		<-ints
		note(name + ".int")
		select {}
	case "daemon":
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		note(name + ".ready")
		if s := <-sigs; s == syscall.SIGINT {
			note(name + ".int")
		} else {
			note(name + ".term")
		}
	}
	os.Exit(0)
}

// stopHelperBinary is a copy of this test binary in dir, to run the stop
// helpers from: what runs this process's own executable is keploy's, which
// the stop of what the application left never stops (adoptedRunning).
func stopHelperBinary(t *testing.T, dir string) string {
	t.Helper()
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "stop-helper")
	if err := os.WriteFile(bin, self, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// helperCommand is the shell words that run stop helper name from bin,
// writing in dir.
func helperCommand(bin, name, dir string) string {
	return fmt.Sprintf("env %s=%s %s=%s %s -test.run='^TestStopHelperProcess$'", stopHelperEnv, name, stopHelperDirEnv, dir, bin)
}

// helperPid is the pid stop helper name wrote in dir as it got ready.
func helperPid(t *testing.T, dir, name string) int {
	t.Helper()
	var pid int
	await(t, name+" ready", func() bool {
		b, err := os.ReadFile(filepath.Join(dir, name+".ready"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(b))
		return err == nil
	})
	return pid
}

// newRunClient is an agent client whose application is command, run as keploy
// runs a native one.
func newRunClient(command string) *AgentClient {
	cfg := &config.Config{}
	cfg.Agent.AgentURI = "http://127.0.0.1:1/agent"
	client := New(zap.NewNop(), nil, cfg)
	client.apps.Store(uint64(0), app.NewApp(zap.NewNop(), command, nil,
		models.SetupOptions{CommandType: string(utils.Native)}))
	return client
}

// keploy's stop of the application interrupts it (SIGINT to each of its
// process groups) and waits for each group's leader. A shell that waits on
// the app it started in the background (`app & wait`) dies on the interrupt
// at once, and the app, orphaned, is one of the processes the run adopted,
// which the stop of those ends (stopAdopted). One the interrupt reached that
// acts on it is stopping already, and is not asked again: a second signal
// ends many an application mid-shutdown (Go's signal.NotifyContext, once its
// stop is called), and the set's end would be judged on a shutdown keploy
// cut. One the interrupt reached that ignores it (as a shell starts a
// background command) is asked with SIGTERM, as is a daemon the interrupt
// did not reach (a session of its own); each ends, and the stop says no kill.
func TestTheStopDoesNotAskAgainWhatActsOnTheInterrupt(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	dir := t.TempDir()
	bin := stopHelperBinary(t, t.TempDir())
	client := newRunClient(fmt.Sprintf("(setsid %s &); %s & %s & wait",
		helperCommand(bin, "daemon", dir), helperCommand(bin, "acting", dir), helperCommand(bin, "ignoring", dir)))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	pids := map[string]int{}
	for _, name := range []string{"acting", "ignoring", "daemon"} {
		pid := helperPid(t, dir, name)
		pids[name] = pid
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	}
	// The daemon is adopted before the stop: not the tree's, nor its groups'.
	await(t, "the daemon adopted", func() bool { return parentIs(pids["daemon"], os.Getpid()) })
	cancel(models.ErrAppStoppedForSetEnd)
	appErr := <-done
	if appErr.StopKilled != "" {
		t.Errorf("the stop killed: %q", appErr.StopKilled)
	}
	exists := func(file string) bool { _, err := os.Stat(filepath.Join(dir, file)); return err == nil }
	for file, want := range map[string]bool{
		"acting.int": true, "acting.done": true,
		"ignoring.term": true,
		"daemon.term":   true, "daemon.int": false,
	} {
		if got := exists(file); got != want {
			t.Errorf("%s written %v, want %v (pids %v)", file, got, want, pids)
		}
	}
	for name, pid := range pids {
		if parentIs(pid, os.Getpid()) {
			t.Errorf("%s (%d) outlived the stop", name, pid)
		}
	}
}

// When keploy's stop had to kill the application itself, the run says that
// kill, not a later one of a daemon the application left: the first kill is
// what cut the set's end.
func TestTheAppsKillIsSaidBeforeADaemonsStallKill(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	budget := utils.StopStallBudget
	utils.StopStallBudget = 300 * time.Millisecond
	t.Cleanup(func() { utils.StopStallBudget = budget })
	dir := t.TempDir()
	daemonPid, ready := filepath.Join(dir, "daemon"), filepath.Join(dir, "ready")
	client := newRunClient(fmt.Sprintf(`(setsid sh -c 'trap "" INT TERM; echo $$ > %s; exec sleep 600' &); trap "" INT; touch %s; exec sleep 600`, daemonPid, ready))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	await(t, "the app ready", func() bool { _, err := os.Stat(ready); return err == nil })
	var pid int
	await(t, "the daemon adopted", func() bool {
		b, err := os.ReadFile(daemonPid)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && parentIs(pid, os.Getpid())
	})
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel(models.ErrAppStoppedForSetEnd)
	appErr := <-done
	if !strings.Contains(appErr.StopKilled, "after keploy interrupted it, so keploy killed it") || strings.Contains(appErr.StopKilled, "which the application left running") {
		t.Fatalf("the run said %q; want the app's kill, and only it", appErr.StopKilled)
	}
	if parentIs(pid, os.Getpid()) {
		t.Errorf("the daemon (%d) outlived the stop", pid)
	}
}

// A process acting on keploy's interrupt that then never gets out is killed
// for want of progress, and the kill is said, though it ignores SIGTERM: it
// was on its way out, and the kill cut it.
func TestAKillOfOneActingOnTheInterruptIsSaid(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	dir := t.TempDir()
	bin := stopHelperBinary(t, t.TempDir())
	client := newRunClient(fmt.Sprintf("%s & wait", helperCommand(bin, "hangs", dir)))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	pid := helperPid(t, dir, "hangs")
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel(models.ErrAppStoppedForSetEnd)
	appErr := <-done
	if !strings.Contains(appErr.StopKilled, "made no progress on its way out") {
		t.Fatalf("the run said %q; want the kill of the process acting on the interrupt said", appErr.StopKilled)
	}
}

// Whether a daemon ignores SIGTERM is read as keploy asks it to stop with it:
// one whose handler then ignores further TERMs while it shuts down, and that
// stalls on its way out, had a way out, and its kill is said.
func TestAKillOfOneThatIgnoredTermOnlyOnItsWayOutIsSaid(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	dir := t.TempDir()
	script := filepath.Join(dir, "daemon.sh")
	if err := os.WriteFile(script, []byte(`trap 'trap "" TERM; while :; do sleep 1; done' TERM
touch "$1"
while :; do sleep 1; done
`), 0o755); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(dir, "ready")
	client := newRunClient(fmt.Sprintf("sh %s %s & wait", script, ready))
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	await(t, "the daemon ready", func() bool { _, err := os.Stat(ready); return err == nil })
	cancel(models.ErrAppStoppedForSetEnd)
	appErr := <-done
	if !strings.Contains(appErr.StopKilled, "made no progress on its way out") {
		t.Fatalf("the run said %q; want the kill of a daemon that was on its way out said", appErr.StopKilled)
	}
}
