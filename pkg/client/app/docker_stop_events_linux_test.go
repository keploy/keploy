package app

import (
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
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// eventDocker is a daemon holding one app container, whose main process is
// pid (a real process, so its progress can be read): it stops when stop is
// closed, or on a SIGKILL. seen false: the daemon has no such container.
type eventDocker struct {
	docker.Client
	seen bool
	gone bool // removed by the wait (--rm): the wait says no such container
	// exitOnWait: the container's main process exits as keploy first asks
	// the daemon to wait for it (keploy's interrupt reached it), and the
	// daemon, slow to take that exit, calls the container running on until
	// halt. waitFails: that many of the daemon's waits fail at once (-1:
	// every one), as a stream that broke.
	exitOnWait bool
	waitFails  int
	waits      int
	// killLags: a SIGKILL ends the container's main process, as the
	// kernel does, and the daemon, slow to take that exit, calls the
	// container running on (holding its name) until halt. killErr, set:
	// the daemon refuses every kill with it. killMissed, set: the container
	// stops as the kill comes, its main process exiting and the daemon
	// saying so (gone, set: removed), and the daemon refuses the kill with
	// it, as one of a container no longer running.
	killLags   bool
	killErr    error
	killMissed error
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
	d.mu.Lock()
	d.waits++
	exit := d.exitOnWait && d.waits == 1
	fail := d.waitFails != 0
	if d.waitFails > 0 {
		d.waitFails--
	}
	d.mu.Unlock()
	if exit {
		_ = syscall.Kill(d.pid, syscall.SIGKILL)
	}
	if fail {
		errs <- errors.New("the daemon's stream broke")
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
	switch {
	case d.killErr != nil:
		return d.killErr
	case d.killMissed != nil:
		_ = syscall.Kill(d.pid, syscall.SIGKILL)
		d.halt()
		return d.killMissed
	case signal == "SIGKILL" && d.killLags:
		_ = syscall.Kill(d.pid, syscall.SIGKILL)
	case signal == "SIGKILL":
		d.halt()
	}
	return nil
}

// holds says whether the daemon still calls the container running: the name
// the next start's docker run meets.
func (d *eventDocker) holds() bool {
	select {
	case <-d.stop:
		return false
	default:
		return true
	}
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

// started starts argv in a process group of its own, killed when the test
// ends.
func started(t *testing.T, argv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
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
		other := progressing(t)
		d := &eventDocker{seen: true, id: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", pid: other.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		scope := &utils.TeardownScope{}
		a.teardown.Store(scope)
		killed, _ := a.stopDockerRunByEvents(client)
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
	// A starved daemon is slow to say what it did. The container's main
	// process exiting, and every process of it gone, is the kernel's word,
	// which comes first, and is no kill, and no stop either: the daemon
	// calls the container running until it has taken the exit, its docker
	// client waits on it (exec kills that client once the command's
	// WaitDelay has run out after the stop), and the next start's docker run
	// meets the name of a container the daemon holds. Taken for the stop,
	// the main process's exit ended it with the daemon still calling the
	// container running. The stop is through on the daemon's word; a daemon
	// that never says so is an error once the bound on no news has passed,
	// or at Ctrl+C, and no kill, as nothing of the container's runs, and the
	// stop says so (the run's end does not carry its error on). A wait that
	// fails is asked again; only where that fails too does the main
	// process's exit stand for the stop. A daemon whose inspect ran out is
	// still asked to wait, and its word taken, not the docker client's
	// silence for a stall of the app's.
	t.Run("its main process exits, and the daemon never says so", func(t *testing.T) {
		shortStall(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, exitOnWait: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600") // its client, waiting on the daemon
		core, logs := observer.New(zapcore.WarnLevel)
		a := &App{logger: zap.New(core), docker: d, container: "app"}
		killed, err := a.stopDockerRunByEvents(client)
		if err == nil || !strings.Contains(err.Error(), `the application's processes had exited, but the docker daemon had not said its container "app" had stopped 300ms of keploy's runnable time later`) || killed != "" || len(d.kills) != 0 {
			t.Fatalf("said %q (err %v) after kills %v; want the daemon's silence an error, no kill", killed, err, d.kills)
		}
		if n := logs.FilterMessageSnippet("the docker daemon had not said its container").Len(); n != 1 {
			t.Fatalf("the stop said the daemon's silence %d times; want once, as the run's end does not carry its error on", n)
		}
	})
	t.Run("its main process exits before the daemon says so", func(t *testing.T) {
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, exitOnWait: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		gone := exitedOf(t, main.Process.Pid)
		over := make(chan struct{})
		defer close(over)
		go func() {
			select {
			case <-gone:
				d.halt()
			case <-over:
			}
		}()
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		killed, err := a.stopDockerRunByEvents(client)
		if err != nil || killed != "" || len(d.kills) != 0 {
			t.Fatalf("said %q (err %v) after kills %v; want the stop through on the daemon's word, no kill", killed, err, d.kills)
		}
	})
	// The stop offers to end at Ctrl+C at its first look (the run
	// interrupted, the offer at once), and the press comes in that look,
	// once the main process has exited: the wait for the daemon ends on
	// it. The stall bound is 10s, so a press the stop loses fails the row
	// (the daemon's silence an error after the bound), not holds it.
	t.Run("its main process exits, and Ctrl+C is pressed at the stop's offer before the daemon says so", func(t *testing.T) {
		shortStall(t, 10*time.Second)
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, exitOnWait: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{docker: d, container: "app"}
		taken := pressAtOffer(t, a, exitedOf(t, main.Process.Pid))
		killed, err := a.stopDockerRunByEvents(client)
		if err == nil || !strings.Contains(err.Error(), "at the press of Ctrl+C") || killed != "" || len(d.kills) != 0 || !taken.Load() {
			t.Fatalf("said %q (err %v) after kills %v, the press taken %v; want the press to end the wait for the daemon, no kill", killed, err, d.kills, taken.Load())
		}
	})
	t.Run("its main process exits, and every wait of the daemon's fails", func(t *testing.T) {
		shortStall(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, exitOnWait: true, waitFails: -1, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		killed, err := a.stopDockerRunByEvents(client)
		if err != nil || killed != "" || len(d.kills) != 0 || d.waits != 2 {
			t.Fatalf("said %q (err %v) after kills %v and %d waits; want the wait asked twice, then the main process's exit taken for the stop, no kill", killed, err, d.kills, d.waits)
		}
	})
	t.Run("its main process exits, the daemon's wait fails once, and the daemon never says so", func(t *testing.T) {
		shortStall(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, exitOnWait: true, waitFails: 1, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		killed, err := a.stopDockerRunByEvents(client)
		if err == nil || !strings.Contains(err.Error(), "the docker daemon had not said its container") || killed != "" || len(d.kills) != 0 {
			t.Fatalf("said %q (err %v) after kills %v; want the wait asked again, and the daemon's silence an error", killed, err, d.kills)
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
	// A kill ends the container in the kernel first, and the daemon takes
	// that exit later, as it does a stop's: until it has, it holds the
	// container's name, and the next start's docker run (its force-remove
	// of the old name first) meets it. The kill is through only on the
	// daemon's word, as the stop is; a kill the daemon does not confirm
	// fails the stop, said as the lagged stop is.
	t.Run("after its kill, the daemon never says it stopped", func(t *testing.T) {
		shortStall(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, killLags: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		core, logs := observer.New(zapcore.WarnLevel)
		a := &App{logger: zap.New(core), docker: d, container: "app"}
		killed, err := a.stopDockerRunByEvents(client)
		if !strings.Contains(killed, "made no progress on its way out for 300ms") || strings.Join(d.kills, ",") != "SIGKILL" {
			t.Fatalf("said %q after kills %v; want the stall kill said", killed, d.kills)
		}
		if err == nil || !strings.Contains(err.Error(), `the application's processes had exited, but the docker daemon had not said its container "app" had stopped 300ms of keploy's runnable time later`) {
			t.Fatalf("the stop ended with %v, the daemon holding the container %v; want the daemon's silence after the kill an error", err, d.holds())
		}
		if n := logs.FilterMessageSnippet("the docker daemon had not said its container").Len(); n != 1 {
			t.Fatalf("the stop said the daemon's silence after the kill %d times; want once, as the run's end does not carry its error on", n)
		}
	})
	t.Run("after its kill, the daemon says it stopped once it has taken the kill", func(t *testing.T) {
		shortStall(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, killLags: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		gone := exitedOf(t, main.Process.Pid)
		over := make(chan struct{})
		defer close(over)
		go func() {
			select {
			case <-gone:
				d.halt()
			case <-over:
			}
		}()
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		killed, err := a.stopDockerRunByEvents(client)
		if err != nil || !strings.Contains(killed, "made no progress on its way out for 300ms") || d.holds() {
			t.Fatalf("said %q (err %v), the daemon holding the container %v; want the kill through on the daemon's word", killed, err, d.holds())
		}
	})
	t.Run("after its kill, which the daemon refused, its container runs on", func(t *testing.T) {
		shortStall(t, 300*time.Millisecond)
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, killErr: errors.New("the daemon is busy"), id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		core, logs := observer.New(zapcore.WarnLevel)
		a := &App{logger: zap.New(core), docker: d, container: "app"}
		_, err := a.stopDockerRunByEvents(client)
		if err == nil || !strings.Contains(err.Error(), `the application's container "app" had not stopped after keploy killed it`) {
			t.Fatalf("the stop ended with %v; want a container still running after the kill an error", err)
		}
		if n := logs.FilterMessageSnippet("had not stopped after keploy killed it").Len(); n != 1 {
			t.Fatalf("the stop said the container running on after the kill %d times; want once", n)
		}
	})
	// A container that stopped as the kill came, the daemon refusing the
	// kill as one of a container not running (or gone): no kill reached it,
	// and the stop says it did not kill.
	for _, missed := range []struct {
		name string
		err  error
		gone bool
	}{
		{"not running", errdefs.Conflict(errors.New(`container "app" is not running`)), false},
		{"gone", errdefs.NotFound(errors.New("no such container: app")), true},
	} {
		t.Run("after its kill, which the daemon refused as one of a container "+missed.name+", it had stopped", func(t *testing.T) {
			shortStall(t, 300*time.Millisecond)
			main := started(t, "sleep", "600")
			d := &eventDocker{seen: true, gone: missed.gone, killMissed: missed.err, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
			client := started(t, "sleep", "600")
			a := &App{logger: zap.NewNop(), docker: d, container: "app"}
			killed, err := a.stopDockerRunByEvents(client)
			if err != nil || killed != "" || strings.Join(d.kills, ",") != "SIGKILL" {
				t.Fatalf("said %q (err %v) after kills %v; want a kill that reached nothing not said as one, and the stop through", killed, err, d.kills)
			}
		})
	}
	// A press of Ctrl+C in the wait after the kill, the daemon not having
	// said the container stopped (every process of it gone), is no stop:
	// the stop fails and says so, once, as the lagged stop does. Each wait
	// offers once: the press at the first offer is the kill's, the second
	// press is in the kill's wait, once the kill has ended the main
	// process. No stall bound is reached: the presses alone end the waits.
	t.Run("after its kill, Ctrl+C is pressed before the daemon says it stopped", func(t *testing.T) {
		shortStall(t, time.Hour)
		main := started(t, "sleep", "600")
		d := &eventDocker{seen: true, killLags: true, id: cgroupOf(t, main.Process.Pid), pid: main.Process.Pid, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{docker: d, container: "app"}
		taken, logs := pressAtNthOffer(t, a, 2, exitedOf(t, main.Process.Pid))
		killed, err := a.stopDockerRunByEvents(client)
		if !taken.Load() || killed != "keploy killed its container at the press of Ctrl+C, before it had stopped" || strings.Join(d.kills, ",") != "SIGKILL" {
			t.Fatalf("said %q after kills %v, the second press taken %v; want the press's kill said, and the second press taken in the wait after it", killed, d.kills, taken.Load())
		}
		const want = `keploy stopped waiting for the docker daemon to say its container "app" had stopped at the press of Ctrl+C`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("the stop ended with %v, the daemon holding the container %v; want the press in the wait after the kill an error, not a stop", err, d.holds())
		}
		if n := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet(want).Len(); n != 1 {
			t.Fatalf("the stop said the press in the wait after the kill %d times at Warn; want once, as the run's end does not carry its error on", n)
		}
	})
	// A container keploy never saw is waited for by its docker client: its
	// kill is through on the client's exit.
	t.Run("after its kill, the client of a container keploy never saw exits", func(t *testing.T) {
		shortStall(t, 300*time.Millisecond)
		d := &eventDocker{seen: false, stop: make(chan struct{})}
		client := started(t, "sleep", "600")
		a := &App{logger: zap.NewNop(), docker: d, container: "app"}
		killed, err := a.stopDockerRunByEvents(client)
		if err != nil || !strings.Contains(killed, "keploy could not find its container") || len(d.kills) != 0 {
			t.Fatalf("said %q (err %v) after kills %v; want the client's kill through on its exit", killed, err, d.kills)
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
