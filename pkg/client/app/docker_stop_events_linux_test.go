package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// eventDocker is a daemon holding one app container, whose main process is
// pid (a real process, so its progress can be read): it stops when stop is
// closed, or on a SIGKILL. seen false: the daemon has no such container.
type eventDocker struct {
	docker.Client
	seen bool
	gone bool // removed by the wait (--rm): the wait says no such container
	// inspectErr, set: the daemon does not describe the container (a
	// starved daemon's inspect runs out), though it has it.
	inspectErr error
	id         string
	pid        int
	stop       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	kills      []string
}

func (d *eventDocker) halt() { d.once.Do(func() { close(d.stop) }) }

func (d *eventDocker) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	if d.inspectErr != nil {
		return container.InspectResponse{}, d.inspectErr
	}
	if !d.seen {
		return container.InspectResponse{}, errdefs.NotFound(errors.New("no such container"))
	}
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: d.id, State: &container.State{Running: true, Pid: d.pid}}}, nil
}

func (d *eventDocker) ContainerWait(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	out, errs := make(chan container.WaitResponse, 1), make(chan error, 1)
	if !d.seen {
		errs <- errdefs.NotFound(errors.New("no such container"))
		return out, errs
	}
	go func() {
		select {
		case <-d.stop:
			if d.gone {
				errs <- errdefs.NotFound(errors.New("no such container"))
				return
			}
			out <- container.WaitResponse{}
		case <-ctx.Done():
			errs <- ctx.Err()
		}
	}()
	return out, errs
}

func (d *eventDocker) ContainerKill(_ context.Context, _ string, signal string) error {
	d.mu.Lock()
	d.kills = append(d.kills, signal)
	d.mu.Unlock()
	if signal == "SIGKILL" {
		d.halt()
	}
	return nil
}

// cgroupOf is the cgroup pid is in, as its /proc shows it: what a container's
// process has its container's id in.
func cgroupOf(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return line[strings.LastIndex(line, ":")+1:]
}

// shortStall sets utils.StopStallBudget to d for the test.
func shortStall(t *testing.T, d time.Duration) {
	t.Helper()
	budget := utils.StopStallBudget
	utils.StopStallBudget = d
	t.Cleanup(func() { utils.StopStallBudget = budget })
}

// progressingProcess starts a process that, once it has written ready,
// closes one of the seven files it holds every 0.8 s, then sleeps: a shutdown
// that makes progress for some 5.6 s. (Single-digit descriptors: dash,
// Debian's sh, takes no others.)
func progressingProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	const closing = `exec 3</dev/null 4</dev/null 5</dev/null 6</dev/null 7</dev/null 8</dev/null 9</dev/null
touch ready
for fd in 3 4 5 6 7 8 9; do sleep 0.8; eval "exec $fd<&-"; done
exec sleep 600`
	dir := t.TempDir()
	cmd := started(t, "sh", "-c", "cd "+dir+" && "+closing)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatal("the container's process did not start")
		}
	}
}

// steppedProcess starts a process that holds seven files more than its
// standard three, and returns it with step, which has it close the next of
// them; once all seven are closed it sleeps. Its shutdown makes progress when
// the test says, not on a timer of its own. (Single-digit descriptors: dash,
// Debian's sh, takes no others.)
func steppedProcess(t *testing.T) (cmd *exec.Cmd, step func()) {
	t.Helper()
	// Its standard input is the steps (a read that fails, as the test ends,
	// closes nothing), and its standard output says it holds the seven.
	const closing = `exec 3</dev/null 4</dev/null 5</dev/null 6</dev/null 7</dev/null 8</dev/null 9</dev/null
echo ready
for fd in 3 4 5 6 7 8 9; do read _ || exec sleep 600; eval "exec $fd<&-"; done
exec sleep 600`
	steps, stepsW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ready, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stepsW.Close(); _ = ready.Close() })
	cmd = exec.Command("sh", "-c", closing)
	cmd.Stdin, cmd.Stdout = steps, readyW
	start(t, cmd)
	_, _ = steps.Close(), readyW.Close()
	if line, err := bufio.NewReader(ready).ReadString('\n'); line != "ready\n" {
		t.Fatalf("the process did not say it holds its files: %q (%v)", line, err)
	}
	return cmd, func() {
		if _, err := stepsW.Write([]byte("\n")); err != nil {
			t.Errorf("a step of the process: %v", err)
		}
	}
}

// paceSteps calls step n times, once every every of the process's runnable
// time (stall's process clock, the one a stop's stall bound is measured on)
// from now. The func it returns ends the pacing and waits for it to end.
func paceSteps(t *testing.T, step func(), n int, every time.Duration) (end func()) {
	t.Helper()
	clk, release := stall.Process()
	quit, done := make(chan struct{}), make(chan struct{})
	from := clk.Runnable()
	go func() {
		defer close(done)
		for i := 1; i <= n; i++ {
			for clk.Runnable()-from < time.Duration(i)*every {
				tick, stop := clk.After(stall.Tick)
				select {
				case <-quit:
					stop()
					return
				case <-tick:
				}
			}
			step()
		}
	}()
	var once sync.Once
	end = func() { once.Do(func() { close(quit); <-done; release() }) }
	t.Cleanup(end)
	return end
}

// started starts argv in a process group of its own, killed when the test
// ends.
func started(t *testing.T, argv ...string) *exec.Cmd {
	t.Helper()
	return start(t, exec.Command(argv[0], argv[1:]...))
}

// start starts cmd in a process group of its own, killed when the test ends.
func start(t *testing.T, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	return cmd
}

// Under the switch a docker run app's stop waits for its container to stop,
// the daemon's wait, however long docker's own stop and removal take on a
// starved host: here longer than the old 5 s grace. It kills only when the
// container's processes made no progress on its way out for the stall bound
// of the time both they and keploy could run, and says so. A container
// keploy never saw is waited for by its docker client.
func TestADockerRunStopWaitsForTheContainersStopEvent(t *testing.T) {
	t.Run("its container stops after the old grace", func(t *testing.T) {
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		go func() { time.Sleep(graceBudget + time.Second); d.halt() }()
		killed, _ := a.stopDockerRunByEvents(client)
		if killed != "" || len(d.kills) != 0 {
			t.Fatalf("said %q after kills %v; want the container's stop waited for, no kill", killed, d.kills)
		}
	})
	t.Run("its container is removed (--rm) as the wait runs", func(t *testing.T) {
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, gone: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		go func() { time.Sleep(time.Second); d.halt() }()
		killed, _ := a.stopDockerRunByEvents(client)
		if killed != "" || len(d.kills) != 0 {
			t.Fatalf("said %q after kills %v; want a container removed as it stopped taken for stopped", killed, d.kills)
		}
	})
	t.Run("its container makes no progress", func(t *testing.T) {
		shortStall(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		killed, _ := a.stopDockerRunByEvents(client)
		if !strings.Contains(killed, "made no progress on its way out for 300ms of its and keploy's runnable time") || strings.Join(d.kills, ",") != "SIGKILL" {
			t.Fatalf("said %q after kills %v; want the stall kill said", killed, d.kills)
		}
	})
	// A container whose processes keep closing what they hold is making
	// progress: no kill while it does, though that runs past the stall
	// bound. The daemon's pid of a container is read for it only when its
	// cgroup names the container: one the daemon reports from another PID
	// namespace (a remote or rootless daemon) names another process here,
	// whose progress is no news of the container's, so the stall bound runs
	// on the container's stop alone, and the kill comes while that other
	// process is still closing its files.
	progressing := progressingProcess
	held := func(pid int) int {
		fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		return len(fds)
	}
	t.Run("its container keeps making progress past the stall bound", func(t *testing.T) {
		shortStall(t, 2*time.Second)
		main := progressing(t)
		d := &eventDocker{seen: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		// The run's teardown: the container's steps out move it.
		scope := &utils.TeardownScope{}
		a.teardown.Store(scope)
		go func() { time.Sleep(5 * time.Second); d.halt() }()
		killed, _ := a.stopDockerRunByEvents(client)
		if killed != "" || len(d.kills) != 0 {
			t.Fatalf("said %q after kills %v; want a container making progress waited for", killed, d.kills)
		}
		if scope.Moves() == 0 {
			t.Fatal("the container's steps out moved the run's teardown not once")
		}
	})
	t.Run("a pid the daemon names from another namespace", func(t *testing.T) {
		shortStall(t, 2*time.Second)
		other, step := steppedProcess(t)
		d := &eventDocker{seen: true, id: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", pid: other.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		scope := &utils.TeardownScope{}
		a.teardown.Store(scope)
		// The other process closes a file every 0.8 s, for 5.6 s, of
		// keploy's runnable time: the time the stall bound runs on, so the
		// kill, 2 s of it on, comes before its last step however starved
		// the host is. (Its steps on wall time raced the bound: on a
		// starved host keploy's 2 s of runnable time outlast 5.6 s of
		// wall time.)
		paced := paceSteps(t, step, 7, 800*time.Millisecond)
		killed, _ := a.stopDockerRunByEvents(client)
		paced()
		if killed == "" || strings.Join(d.kills, ",") != "SIGKILL" {
			t.Fatalf("said %q after kills %v; want the stall kill", killed, d.kills)
		}
		// Nor are its steps the container's teardown.
		if n := scope.Moves(); n != 0 {
			t.Fatalf("another process's progress moved the run's teardown %d times", n)
		}
		if n := held(other.Process.Pid); n <= 3 {
			t.Fatalf("the kill came once the other process had closed its files (%d left): its progress was read for the container's", n)
		}
	})
	// A starved daemon is slow to say what it did: its word is not the only
	// event. The container's main process exiting is its stop, though the
	// daemon has not said so yet; and a daemon whose inspect ran out is
	// still asked to wait, and its word taken, not the docker client's
	// silence for a stall of the app's.
	t.Run("its main process exits before the daemon says so", func(t *testing.T) {
		shortStall(t, 3*time.Second)
		// A process of it outlives the main one: the main one's exit is
		// the container's stop all the same (docker ends the rest with it).
		main := started(t, "sh", "-c", "sleep 600 & sleep 1")
		d := &eventDocker{seen: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600") // its client, waiting on the daemon
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		killed, _ := a.stopDockerRunByEvents(client)
		if killed != "" || len(d.kills) != 0 {
			t.Fatalf("said %q after kills %v; want the main process's exit taken for the container's stop", killed, d.kills)
		}
	})
	// A client that exits with its container still running (a shell
	// around it that died on the interrupt, a client that proxies no
	// signal) is not the container's stop: the container's own events, and
	// the stall rule, decide.
	t.Run("its client exits, the container running on", func(t *testing.T) {
		shortStall(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "true")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		killed, _ := a.stopDockerRunByEvents(client)
		if !strings.Contains(killed, "its container made no progress on its way out for 300ms") || strings.Join(d.kills, ",") != "SIGKILL" {
			t.Fatalf("said %q after kills %v; want the running container waited for, and killed on its stall", killed, d.kills)
		}
	})
	t.Run("the daemon did not describe it, then says it stopped", func(t *testing.T) {
		shortStall(t, 3*time.Second)
		d := &eventDocker{seen: true, inspectErr: context.DeadlineExceeded, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		go func() { time.Sleep(time.Second); d.halt() }()
		killed, _ := a.stopDockerRunByEvents(client)
		if killed != "" || len(d.kills) != 0 {
			t.Fatalf("said %q after kills %v; want the daemon's word waited for", killed, d.kills)
		}
	})
	t.Run("the daemon did not describe it, nor says it stopped", func(t *testing.T) {
		shortStall(t, 300*time.Millisecond)
		d := &eventDocker{seen: true, inspectErr: context.DeadlineExceeded, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		killed, _ := a.stopDockerRunByEvents(client)
		if !strings.Contains(killed, "keploy could not read its container's processes, and neither the docker daemon nor its docker client said it had stopped, for 300ms") || strings.Join(d.kills, ",") != "SIGKILL" {
			t.Fatalf("said %q after kills %v; want a kill said as one on no news", killed, d.kills)
		}
	})
	// At the user's Ctrl+C, which the stop offers once the run is
	// interrupted, the stop kills at once, and says so; while it waits it
	// holds the drains.
	t.Run("a press of Ctrl+C", func(t *testing.T) {
		shortStall(t, time.Hour)
		offer := utils.StopOffer
		utils.StopOffer = 0
		t.Cleanup(func() { utils.StopOffer = offer; utils.ClearInterrupted(); utils.ResetInterruptedAgain() })
		utils.MarkInterrupted()
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		held := make(chan bool, 1)
		go func() {
			for !utils.StopHoldsDrains() {
				time.Sleep(5 * time.Millisecond)
			}
			held <- true
			for !utils.MarkInterruptedAgain() {
				time.Sleep(5 * time.Millisecond)
			}
		}()
		killed, _ := a.stopDockerRunByEvents(client)
		if !<-held {
			t.Fatal("the stop did not hold the drains")
		}
		if killed != "keploy killed its container at the press of Ctrl+C, before it had stopped" || strings.Join(d.kills, ",") != "SIGKILL" {
			t.Fatalf("said %q after kills %v; want the press's kill said", killed, d.kills)
		}
		if utils.StopHoldsDrains() {
			t.Fatal("the stop still holds the drains once it is over")
		}
	})
	t.Run("a container keploy never saw", func(t *testing.T) {
		d := &eventDocker{seen: false, stop: make(chan struct{})}
		client := started(t, "sh", "-c", "sleep 1")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		killed, _ := a.stopDockerRunByEvents(client)
		if killed != "" || len(d.kills) != 0 {
			t.Fatalf("said %q after kills %v; want the docker client's exit waited for", killed, d.kills)
		}
	})
}

// Only a stop that says its kills (a set's end, a recording's end) waits on
// the app's exit event, and only under the switch: with it off, the stop is
// as it was.
func TestOnlyASwitchedStopThatSaysItsKillsWaitsOnTheExitEvent(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	if stopsByEvents(true) {
		t.Fatal("with the switch off, a set's end stop waits on events")
	}
	t.Setenv(provenswitch.Env, "1")
	if !stopsByEvents(true) || stopsByEvents(false) {
		t.Fatalf("under the switch: a set's end stop by events %v, another %v; want true, false", stopsByEvents(true), stopsByEvents(false))
	}
}
