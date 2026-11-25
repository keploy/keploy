//go:build linux

package http

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// A DOCKER-RUN RUN'S STOP ENDS ITS `docker run` WHOLE, AND IS NOT HELD BY
// WHAT IT CANNOT END.
//
// keploy runs the agent's container through `sh -c 'docker container run
// ...'`, in a session of its own. Its stop signals the agent's container by
// name and waits for it, and then kills the command. It killed the command's
// first process alone: `sh`, or the `sudo` or `docker` sh ran in its place.
// Any other process of the command -- one the client started, or one sh ran
// beside it -- was left running after the stop. A process keploy may not
// signal (the `docker` that sudo runs as root) is left to end by itself, and
// does not hold the run's end: the command's output goes to keploy through
// files of its own (utils.ChildConsole), so the wait for the command is for
// its first process alone.
//
// The rows drive startAgent itself, through the `docker run` it executes, to
// the stop and the run's end.

// TestADockerRunsStopKillsWhatItsClientLeftHoldingItsOutput: a client that
// waits on a lagging daemon, ignores SIGTERM and holds the command's output
// in a process of its own (holder) ends with the stop, killed with every
// process of the command's group, and the run ends.
func TestADockerRunsStopKillsWhatItsClientLeftHoldingItsOutput(t *testing.T) {
	s := stopADockerRun(t, `trap '' TERM
"$dir/holder" "$dir" &
until [ -s "$dir/holder.pid" ]; do sleep 0.01; done
: > "$dir/running"
wait`)
	s.requireEnded(t)
	pid := s.holder(t)
	within(t, "the end of the process the client left holding the run's output", func() {
		for processRuns(pid) {
			time.Sleep(time.Millisecond)
		}
	})
}

// TestADockerRunsStopIsNotHeldByAClientKeployCannotSignal: a process of the
// command that no kill of keploy's reaches -- the root `docker` that sudo
// runs, here a holder in a session of its own -- does not hold the run's
// end, though it holds the command's output: the wait for the command is
// for its first process alone. The holder, out of keploy's reach, still
// runs.
func TestADockerRunsStopIsNotHeldByAClientKeployCannotSignal(t *testing.T) {
	s := stopADockerRun(t, `trap '' TERM
setsid "$dir/holder" "$dir" &
until [ -s "$dir/holder.pid" ]; do sleep 0.01; done
: > "$dir/running"
wait`)
	s.requireEnded(t)
	if pid := s.holder(t); !processRuns(pid) {
		t.Fatalf("the holder (pid %d), which keploy may not signal here, was ended: the row no longer holds the run's output past the stop", pid)
	}
}

// TestADockerRunsStopOfAClientThatEndsWithItsContainer: the stop of a client
// that ends once its container has stopped is as it was: the container is
// signalled and waited for by name, the client ends, and the run ends.
func TestADockerRunsStopOfAClientThatEndsWithItsContainer(t *testing.T) {
	s := stopADockerRun(t, `: > "$dir/running"
while held && ! grep -q '^kill --signal SIGTERM ' "$dir/calls" 2>/dev/null; do sleep 0.01; done
exit 0`)
	s.requireEnded(t)
}

// TestADockerRunThatEndsByItselfSaysItsStatus: a `docker run` that ends by
// itself -- the agent's container exited, with the agent's status -- ends
// the run with that status, as it did, and is not stopped.
func TestADockerRunThatEndsByItselfSaysItsStatus(t *testing.T) {
	dir := t.TempDir()
	writeDockerRunFakes(t, dir, `echo "the agent could not start" >&2
exit 3`)
	a, daemon, _ := dockerRunAgentClient()
	grp, gctx := errgroup.WithContext(context.Background())
	ctx, stop := context.WithCancel(context.WithValue(gctx, models.ErrGroupKey, grp))
	t.Cleanup(stop)
	exited, err := a.startAgent(ctx, true, dockerRunOpts())
	if err != nil {
		t.Fatalf("startAgent: %v", err)
	}
	within(t, "the docker run's end", func() {
		err = <-exited
	})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("the docker run ended with %v, want its exit status 3", err)
	}
	if calls := stopCalls(t, dir); len(calls) != 0 {
		t.Errorf("a run that ended by itself was stopped: %q", calls)
	}
	within(t, "the run's group", func() { _ = grp.Wait() })
	if daemon.askedRemoval() {
		t.Error("a run that ended by itself asked the engine for its container's removal")
	}
}

// TestDockerAgentCancelOfAClientAlreadyWaitedFor: a `docker run` client that
// ended and was waited for before the stop's kill is not killed -- its pid,
// and its group's, may be another's by then -- and the stop reports no
// failure: the command had ended.
func TestDockerAgentCancelOfAClientAlreadyWaitedFor(t *testing.T) {
	log, release := fakeEngine(t)
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	client := exec.Command("true")
	if err := client.Run(); err != nil {
		t.Fatal(err)
	}
	run := newAgentRun()
	t.Cleanup(run.engine.end) // the run's end, which asks for its volumes, is not run here
	run.ready.Store(true)
	if err := dockerAgentCancel(zap.NewNop(), client, run, stopTestContainer, kdocker.NewRemovalWatch(&removesAtOnce{}, stopTestContainer))(); err != nil {
		t.Fatalf("the stop of a client already waited for: %v", err)
	}
	requireSignalledAndWaited(t, engineLines(t, log))
}

// dockerRunStop is a docker-run run stopped once its `docker run` ran
// (stopADockerRun).
type dockerRunStop struct {
	dir    string
	name   string
	exited <-chan error
	grp    *errgroup.Group
	daemon *removesAtOnce
	logged *observer.ObservedLogs
}

// stopADockerRun starts a docker-run run whose `docker container run` runs
// client, a shell fragment run with $dir the fakes' directory and held true
// while the test holds the engine, and stops it once client has marked
// itself ready ($dir/running): a client that starts a holder marks itself
// only once the holder runs, so the stop never ends the client first. The
// holder in $dir ignores SIGTERM and holds the output it was given until
// the test is done with it; it writes its pid to $dir/holder.pid.
func stopADockerRun(t *testing.T, client string) *dockerRunStop {
	t.Helper()
	dir := t.TempDir()
	writeDockerRunFakes(t, dir, client)
	a, daemon, logged := dockerRunAgentClient()
	grp, gctx := errgroup.WithContext(context.Background())
	ctx, stop := context.WithCancel(context.WithValue(gctx, models.ErrGroupKey, grp))
	t.Cleanup(stop)
	opts := dockerRunOpts()
	exited, err := a.startAgent(ctx, true, opts)
	if err != nil {
		t.Fatalf("startAgent: %v", err)
	}
	running := filepath.Join(dir, "running")
	hung := time.After(hangBound)
	for {
		if _, err := os.Stat(running); err == nil {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("the run ended before its `docker run` ran, with %v; the run logged:%s", err, linesOf(logged))
		case <-hung:
			t.Fatalf("the run's `docker run` never ran within %v; the run logged:%s", hangBound, linesOf(logged))
		case <-time.After(time.Millisecond):
		}
	}
	stop()
	return &dockerRunStop{dir: dir, name: opts.KeployContainer, exited: exited, grp: grp, daemon: daemon, logged: logged}
}

// requireEnded fails the test unless the stopped run has ended, without an
// error, and its group with it; the stop signalled the agent's container by
// name with SIGTERM and waited for it, and sent it nothing else; and the
// run's end asked the engine for that container's removal and then removed
// the run's CA volume.
func (s *dockerRunStop) requireEnded(t *testing.T) {
	t.Helper()
	ended := make(chan error, 1)
	go func() { ended <- <-s.exited }()
	select {
	case err := <-ended:
		if err != nil {
			t.Errorf("the docker run ended with %v", err)
		}
	case <-time.After(hangBound):
		t.Fatalf("the docker run did not end within %v of its stop; the run logged:%s", hangBound, linesOf(s.logged))
	}
	within(t, "the run's group", func() {
		if err := s.grp.Wait(); err != nil {
			t.Errorf("the run's group: %v", err)
		}
	})
	calls := stopCalls(t, s.dir)
	kill, wait := "kill --signal SIGTERM "+s.name, "wait "+s.name
	if len(calls) == 0 || calls[0] != kill || count(calls, wait) == 0 {
		t.Fatalf("the stop's calls to the engine are %q; want %q first, and %q", calls, kill, wait)
	}
	for _, c := range calls {
		if c != kill && c != wait {
			t.Fatalf("the stop's calls to the engine %q include %q", calls, c)
		}
	}
	waited, looked, removed := s.daemon.asked()
	if waited != s.name+" "+string(container.WaitConditionRemoved) || looked != s.name {
		t.Errorf("the run asked the engine for the removal of %q and looked at %q; want both of %s", waited, looked, s.name)
	}
	if want := kdocker.TLSVolumeFor(s.name); removed != want {
		t.Errorf("the run's end removed the volume %q; want its CA volume %q", removed, want)
	}
}

// holder is the pid of the holder its client started.
func (s *dockerRunStop) holder(t *testing.T) int {
	t.Helper()
	var b []byte
	within(t, "the holder's pid", func() {
		for {
			var err error
			if b, err = os.ReadFile(filepath.Join(s.dir, "holder.pid")); err == nil && strings.HasSuffix(string(b), "\n") {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("the holder's pid %q: %v", b, err)
	}
	return pid
}

// writeDockerRunFakes puts the engine's CLI in dir first on PATH
// (fakeEngineForStartInDocker): its `container run` runs run, a shell
// fragment (stopADockerRun); every `kill`, `wait` and `stop` is logged to
// dir/calls and answered at once, as an engine whose container exits at its
// first SIGTERM answers them; any other call is answered at once. Nothing
// it starts outlives its test: each loop ends once the test is done with the
// engine, once its directory is gone, and once the test binary is.
func writeDockerRunFakes(t *testing.T, dir, run string) {
	t.Helper()
	held := `held() { [ ! -e "$dir/done" ] && [ -d "$dir" ] && kill -0 ` + strconv.Itoa(os.Getpid()) + ` 2>/dev/null; }
`
	holder := `#!/bin/sh
dir="$1"
` + held + `trap '' TERM
echo $$ > "$dir/holder.pid"
while held; do sleep 0.01; done
`
	if err := os.WriteFile(filepath.Join(dir, "holder"), []byte(holder), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(dir, "done"), nil, 0o644) })
	fakeEngineForStartInDocker(t, dir, `#!/bin/sh
dir="`+dir+`"
`+held+`if [ "$1 $2" = "container run" ]; then
`+run+`
fi
case "$1" in kill|wait|stop) echo "$*" >> "$dir/calls" ;; esac
exit 0
`)
}

// stopCalls are the stop's calls to the engine (writeDockerRunFakes).
func stopCalls(t *testing.T, dir string) []string {
	t.Helper()
	return engineLines(t, filepath.Join(dir, "calls"))
}

// dockerRunAgentClient is an AgentClient for a docker-run run, its engine's
// API removesAtOnce, logging what it says at DEBUG and above to logged.
func dockerRunAgentClient() (*AgentClient, *removesAtOnce, *observer.ObservedLogs) {
	core, logged := observer.New(zapcore.DebugLevel)
	daemon := &removesAtOnce{}
	return &AgentClient{logger: zap.New(core), conf: &config.Config{InstallationID: "install-id"}, dockerClient: daemon}, daemon, logged
}

// dockerRunOpts are a docker-run run's options, its agent's container named
// as a run's is.
func dockerRunOpts() models.SetupOptions {
	return models.SetupOptions{
		KeployContainer: kdocker.AgentContainerPrefix + "0a1b2c3d4e5f6071",
		Mode:            models.MODE_RECORD, AgentPort: 43904, ProxyPort: 16793, DnsPort: 26793,
	}
}

// removesAtOnce is the engine's API for a run's end: it says at once that
// the agent's container is removed, finds it not running, and removes a
// volume at once, and records what it was asked of each.
type removesAtOnce struct {
	kdocker.Client
	mu                      sync.Mutex
	waited, looked, removed string
}

func (d *removesAtOnce) ContainerWait(_ context.Context, name string, cond container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	d.mu.Lock()
	d.waited = name + " " + string(cond)
	d.mu.Unlock()
	said := make(chan container.WaitResponse, 1)
	said <- container.WaitResponse{}
	return said, make(chan error)
}

func (d *removesAtOnce) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	d.mu.Lock()
	d.looked = name
	d.mu.Unlock()
	return container.InspectResponse{}, nil
}

func (d *removesAtOnce) VolumeRemove(_ context.Context, name string, _ bool) error {
	d.mu.Lock()
	if strings.HasPrefix(name, "keploy-tls-certs-") {
		d.removed = name
	}
	d.mu.Unlock()
	return nil
}

func (d *removesAtOnce) asked() (waited, looked, removed string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.waited, d.looked, d.removed
}

func (d *removesAtOnce) askedRemoval() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.waited != ""
}

// processRuns reports whether the process pid runs: it exists, and is not a
// zombie left for its parent to reap.
func processRuns(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The state follows the command's name, which closes with ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}
