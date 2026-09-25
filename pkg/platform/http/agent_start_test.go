package http

// AN AGENT THAT CANNOT START MUST FAIL THE RUN, AT ONCE, AND SAY WHY.
//
// The native agent is a separate process. When it could not start -- no
// privileges to raise the memlock rlimit, no tracefs -- the readiness wait
// never looked at the process: it polled a dead port for the whole ready
// budget (330s), or, when the agent's exit failed the caller's errgroup
// first, returned a bare "context canceled" that `keploy mock record` then
// read as the user's Ctrl+C and exited 0 on, with the test command never run.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// agentExitEnv, set, makes this test binary stand in for an agent process
// that exits with the status it holds (see TestMain).
const agentExitEnv = "KEPLOY_TEST_AGENT_EXIT"

func TestMain(m *testing.M) {
	if status, ok := os.LookupEnv(agentExitEnv); ok {
		code, err := strconv.Atoi(status)
		if err != nil {
			os.Exit(125)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// agentExit is what cmd.Wait returns for an agent process that ended with
// status code: a real *exec.ExitError, from a real process -- this test
// binary, re-run as one -- and no shell, so it means the same on every
// platform.
func agentExit(t *testing.T, code int) error {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), agentExitEnv+"="+strconv.Itoa(code))
	err := cmd.Run()
	if code == 0 {
		if err != nil {
			t.Fatalf("an agent exiting 0 returned %v", err)
		}
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != code {
		t.Fatalf("an agent exiting %d returned %v", code, err)
	}
	return err
}

// deadAgentClient is a client whose agent never answers its health check, the
// state a readiness wait is in when the agent process has already gone.
func deadAgentClient(t *testing.T, healthy bool) *AgentClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthy {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	conf := &config.Config{}
	conf.Agent.AgentURI = srv.URL
	return &AgentClient{logger: zap.NewNop(), conf: conf}
}

// waitWithin runs waitForAgent with a ready budget far longer than the test
// allows, so a wait that does not notice the agent's exit fails here instead
// of passing slowly.
func waitWithin(t *testing.T, a *AgentClient, ctx context.Context, exited <-chan error) error {
	t.Helper()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	done := make(chan error, 1)
	go func() {
		done <- a.waitForAgent(ctx, exited, false, models.SetupOptions{AgentReadyTimeout: time.Minute})
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting for an agent that had already exited")
		return nil
	}
}

func TestAnAgentThatExitsBeforeItIsReadyFailsTheSetup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		wantErr  error // nil: a failure with no specific reason
		wantCode int   // the exit code armed for this process; 0 when none is
	}{
		{"it lacked privileges", utils.ExitPrivilegeRequired, utils.ErrPrivilegeRequired, utils.ExitPrivilegeRequired},
		{"the environment lacks something", utils.ExitEnvironmentUnsupported, utils.ErrEnvironmentUnsupported, utils.ExitEnvironmentUnsupported},
		{"it failed for another reason", utils.ExitKeployError, nil, 0},
		{"it exited cleanly", 0, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exited := make(chan error, 1)
			exited <- agentExit(t, tc.status)
			err := waitWithin(t, deadAgentClient(t, false), context.Background(), exited)
			if err == nil {
				t.Fatal("the setup succeeded without an agent")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %q, want it to carry %q", err, tc.wantErr)
			}
			// The generic 1 travels as the returned error, as every other
			// Setup failure does; only a specific reason is armed here.
			if utils.ErrCode != tc.wantCode {
				t.Fatalf("armed exit code %d, want %d", utils.ErrCode, tc.wantCode)
			}
		})
	}
}

// The agent's goroutine lives in the caller's errgroup, and its failing
// cancels the context the wait is selecting on at the same moment the exit is
// reported. Whichever of the two the select takes, the answer is the agent's
// exit -- never "context canceled", which the caller reads as the user's
// Ctrl+C. Repeated, because a select between two ready cases is random.
func TestAnAgentFailingTheGroupIsNotAnInterrupt(t *testing.T) {
	a := deadAgentClient(t, false)
	exit := agentExit(t, utils.ExitPrivilegeRequired)
	for i := 0; i < 100; i++ {
		grp, ctx := errgroup.WithContext(context.Background())
		exited := make(chan error, 1)
		// What startNativeAgent's wait goroutine does when the process ends.
		grp.Go(func() error {
			exited <- exit
			return exit
		})
		_ = grp.Wait()
		if err := waitWithin(t, a, ctx, exited); !errors.Is(err, utils.ErrPrivilegeRequired) {
			t.Fatalf("iteration %d: got %v, want the agent's own reason", i, err)
		}
	}
}

// Anything else in the caller's errgroup failing cancels the same context.
// The wait has to hand back what failed, not the bare "context canceled" a
// caller reads as the user's Ctrl+C.
func TestAGroupFailureIsReportedAsItself(t *testing.T) {
	grp, ctx := errgroup.WithContext(context.Background())
	failed := errors.New("the proxy's listener could not be opened")
	grp.Go(func() error { return failed })
	_ = grp.Wait()
	if err := waitWithin(t, deadAgentClient(t, false), ctx, make(chan error, 1)); !errors.Is(err, failed) {
		t.Fatalf("got %v, want the group's own failure", err)
	}
}

// The user stopping the run is still an interrupt: the caller gets the
// cancellation it recognises, and nothing is armed.
func TestAnInterruptWhileWaitingIsStillAnInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitWithin(t, deadAgentClient(t, false), ctx, make(chan error, 1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("an interrupt returned %v", err)
	}
	if utils.ErrCode != 0 {
		t.Fatalf("an interrupt armed exit code %d", utils.ErrCode)
	}
}

// And an agent that comes up is waited for, not failed.
func TestAnAgentThatAnswersIsReady(t *testing.T) {
	if err := waitWithin(t, deadAgentClient(t, true), context.Background(), make(chan error, 1)); err != nil {
		t.Fatalf("a healthy agent failed the setup: %v", err)
	}
}

// The remedy logged with the reason has to be one that can work. The agent
// that reports either reason already ran as root -- the native one elevated
// before it starts, the container as root with the capabilities keploy gives
// it -- so neither sudo nor setcap is ever the answer: a user in a CI
// container, told to run keploy as root, already was.
//
// Nor may it claim more than an exit status says. A container refused eBPF
// can be a rootless daemon's, or a rootful one's under an SELinux or AppArmor
// policy, and a user of the second, told to switch to a rootful daemon,
// already has one. A native agent refused it may not be in a container at
// all: on a bare host, root is refused eBPF by kernel lockdown or by an
// SELinux or AppArmor policy, which no container flag changes. And tracefs is
// only ever missing on Linux, while no native agent needs a network: it is
// reached over loopback, so a machine with its network off has nothing to
// connect. The agent container does need an address, on the network the
// application's command names.
func TestAnAgentThatCouldNotStartIsGivenARemedyThatCanWork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cause    error
		isDocker bool
		goos     string
		want     []string
		wrong    []string
	}{
		{"native, privileges", utils.ErrPrivilegeRequired, false, "linux", []string{"--privileged", "lockdown", "SELinux or AppArmor"}, nil},
		{"native, environment", utils.ErrEnvironmentUnsupported, false, "linux", []string{"mount -t tracefs"}, []string{"IPv4", "network"}},
		{"native on macOS, environment", utils.ErrEnvironmentUnsupported, false, "darwin", []string{"the agent's log"}, []string{"tracefs", "mount", "IPv4", "network"}},
		{"native on Windows, environment", utils.ErrEnvironmentUnsupported, false, "windows", []string{"the agent's log"}, []string{"tracefs", "mount", "IPv4", "network"}},
		{"docker, privileges", utils.ErrPrivilegeRequired, true, "linux", []string{"rootless", "SELinux or AppArmor"}, []string{"Run keploy against a rootful Docker daemon"}},
		{"docker, environment", utils.ErrEnvironmentUnsupported, true, "linux", []string{"mount -t debugfs", "--network none"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remedy := agentStartRemedy(tc.cause, tc.isDocker, tc.goos)
			for _, want := range tc.want {
				if !strings.Contains(remedy, want) {
					t.Fatalf("remedy %q does not say %q", remedy, want)
				}
			}
			for _, wrong := range tc.wrong {
				if strings.Contains(remedy, wrong) {
					t.Fatalf("remedy %q says %q, which does not apply", remedy, wrong)
				}
			}
			for _, wrong := range []string{"sudo", "setcap"} {
				if strings.Contains(remedy, wrong) {
					t.Fatalf("remedy %q recommends %s, which cannot help an agent that already ran as root", remedy, wrong)
				}
			}
		})
	}
}

// And the remedy logged is the one for where the agent ran: the wait is told
// whether it started a container, and has to pass that on, along with the OS
// it runs on.
func TestTheRemedyLoggedIsTheOneForWhereTheAgentRan(t *testing.T) {
	for _, tc := range []struct {
		status int
		cause  error
	}{
		{utils.ExitPrivilegeRequired, utils.ErrPrivilegeRequired},
		{utils.ExitEnvironmentUnsupported, utils.ErrEnvironmentUnsupported},
	} {
		for _, isDocker := range []bool{false, true} {
			core, logs := observer.New(zap.ErrorLevel)
			a := deadAgentClient(t, false)
			a.logger = zap.New(core)
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			exited := make(chan error, 1)
			exited <- agentExit(t, tc.status)
			if err := a.waitForAgent(context.Background(), exited, isDocker, models.SetupOptions{AgentReadyTimeout: time.Minute}); !errors.Is(err, tc.cause) {
				t.Fatalf("status %d, docker=%v: got %v", tc.status, isDocker, err)
			}
			want := agentStartRemedy(tc.cause, isDocker, runtime.GOOS)
			entries := logs.FilterField(zap.String("next_step", want)).All()
			if len(entries) != 1 {
				t.Fatalf("status %d, docker=%v: logged %v, want one entry with next_step %q", tc.status, isDocker, logs.All(), want)
			}
		}
	}
}

// pinPerfEventParanoid points the knob docker mode relaxes at a file of the
// test's own, holding level, for the rest of the test. perm 0o444 makes it one
// this process may not write -- unless it is root, which may write anything.
func pinPerfEventParanoid(t *testing.T, level string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "perf_event_paranoid")
	if err := os.WriteFile(path, []byte(level+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	was := perfEventParanoidPath
	perfEventParanoidPath = path
	t.Cleanup(func() { perfEventParanoidPath = was })
	return path
}

func readPinned(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Docker mode lowers perf_event_paranoid to 2 for its agent's tracepoints, and
// only when it has to: a host already at 2 or below is left as it is. Writing 2
// over -1 would tighten it, and a container that is not --privileged can never
// write it at all -- /proc/sys is read-only to root in there -- so a host set
// up in advance is the only way keploy's docker mode runs from such a
// container.
func TestPerfEventParanoidIsLoweredOnlyWhenItHasToBe(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  string
	}{
		{"-1", "-1\n"},
		{"0", "0\n"},
		{"2", "2\n"},
		{"3", "2\n"},
		{"4", "2\n"},
		// Unreadable as a level: set it, rather than trust it.
		{"", "2\n"},
	} {
		t.Run("at "+tc.level, func(t *testing.T) {
			// Read-only whenever nothing should be written, so an attempt
			// fails the test even where the content would not show it.
			perm := os.FileMode(0o644)
			if tc.want == tc.level+"\n" {
				perm = 0o444
			}
			path := pinPerfEventParanoid(t, tc.level, perm)
			if err := relaxPerfEventParanoid(); err != nil {
				t.Fatalf("at %q: %v", tc.level, err)
			}
			if got := readPinned(t, path); got != tc.want {
				t.Fatalf("at %q: left %q, want %q", tc.level, got, tc.want)
			}
		})
	}
}

// A write this process is not allowed to make is a privilege it lacks, and
// says so; anything else stays a plain failure.
func TestPerfEventParanoidThatCannotBeWrittenIsAPrivilegeFailure(t *testing.T) {
	t.Run("not allowed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root may write a read-only file")
		}
		pinPerfEventParanoid(t, "4", 0o444)
		err := relaxPerfEventParanoid()
		if !errors.Is(err, utils.ErrPrivilegeRequired) || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("got %v, want the permission error tagged %q", err, utils.ErrPrivilegeRequired)
		}
	})
	t.Run("not there", func(t *testing.T) {
		was := perfEventParanoidPath
		perfEventParanoidPath = filepath.Join(t.TempDir(), "missing", "perf_event_paranoid")
		t.Cleanup(func() { perfEventParanoidPath = was })
		err := relaxPerfEventParanoid()
		if err == nil || errors.Is(err, utils.ErrPrivilegeRequired) {
			t.Fatalf("got %v, want a failure that is not a privilege one", err)
		}
	})
}

// setupRefused runs Setup for a docker-mode command and returns what it
// failed with, the exit code it armed, and the next_step it logged.
func setupRefused(t *testing.T, a *AgentClient, cmd string, opts models.SetupOptions) (error, int, []string) {
	t.Helper()
	core, logs := observer.New(zap.ErrorLevel)
	a.logger = zap.New(core)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	err := a.Setup(context.Background(), cmd, opts)
	var steps []string
	for _, e := range logs.All() {
		if step, ok := e.ContextMap()["next_step"].(string); ok {
			steps = append(steps, step)
		}
	}
	return err, utils.ErrCode, steps
}

// In docker mode the CLI checks its own capabilities before it changes
// anything. It used to come after: the user's --from-container container
// stopped, the host's perf_event_paranoid written, and keploy run as root in an
// unprivileged CI container failed on that write with a bare 1 before it
// reached the check that says why. Refused, the run fails with the privilege
// code, an error that carries the reason -- what a caller classifies the
// failure by (errors.Is), not only what this process exits with -- and the
// remedy, and nothing has been touched.
func TestDockerModeWithoutItsCapabilitiesChangesNothingAndSaysWhy(t *testing.T) {
	if utils.CheckRequiredPermissions() == nil {
		t.Skip("this process has the capabilities the check asks for, so it would go on to start the agent container")
	}
	for _, tc := range []struct {
		name string
		cmd  string
		opts models.SetupOptions
	}{
		{"docker run", "docker run --rm --name app alpine true", models.SetupOptions{CommandType: string(utils.DockerRun)}},
		{"--from-container", "", models.SetupOptions{CommandType: string(utils.FromContainer), FromContainer: "demo-app"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Writable, and at a level docker mode lowers: a check that came
			// after the write would find it done.
			pinned := pinPerfEventParanoid(t, "4", 0o644)
			daemon := newRestoreFake(fakeContainer{running: true, hasEndpoint: true, publishedPorts: true})
			a := deadAgentClient(t, false)
			a.dockerClient = daemon
			err, code, steps := setupRefused(t, a, tc.cmd, tc.opts)
			if !errors.Is(err, utils.ErrPrivilegeRequired) {
				t.Fatalf("got %v, want it to carry %q", err, utils.ErrPrivilegeRequired)
			}
			if code != utils.ExitPrivilegeRequired {
				t.Fatalf("armed exit code %d, want %d", code, utils.ExitPrivilegeRequired)
			}
			if len(steps) != 1 || steps[0] != dockerModeCapabilitiesRemedy {
				t.Fatalf("logged next_step %q, want only %q", steps, dockerModeCapabilitiesRemedy)
			}
			if daemon.stops != 0 {
				t.Fatalf("the user's container was stopped %d time(s) before keploy knew it could not run", daemon.stops)
			}
			if got := readPinned(t, pinned); got != "4\n" {
				t.Fatalf("perf_event_paranoid was written (%q) before keploy knew it could not run", got)
			}
		})
	}
}

// docker compose starts the agent as a service of the user's own project and
// has never been held to the capability check, so the first thing it can be
// refused is the perf_event_paranoid write -- and that is a privilege failure,
// with its own remedy, not the bare 1 it was.
func TestDockerComposeThatMayNotLowerPerfEventParanoidSaysWhy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("perf_event_paranoid is Linux's")
	}
	if os.Geteuid() == 0 {
		t.Skip("root may write a read-only file")
	}
	pinPerfEventParanoid(t, "4", 0o444)
	err, code, steps := setupRefused(t, deadAgentClient(t, false), "docker compose up", models.SetupOptions{CommandType: string(utils.DockerCompose)})
	if !errors.Is(err, utils.ErrPrivilegeRequired) {
		t.Fatalf("got %v, want it to carry %q", err, utils.ErrPrivilegeRequired)
	}
	if code != utils.ExitPrivilegeRequired {
		t.Fatalf("armed exit code %d, want %d", code, utils.ExitPrivilegeRequired)
	}
	if len(steps) != 1 || steps[0] != perfEventParanoidRemedy {
		t.Fatalf("logged next_step %q, want only %q", steps, perfEventParanoidRemedy)
	}
}
