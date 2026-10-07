package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// restartingDocker is a daemon whose containers change while the retry's `up`
// runs: each container is an id and a start time, or not running.
type restartingDocker struct {
	docker.Client
	mu         sync.Mutex
	containers map[string]*container.State
	ids        map[string]string
	// failing makes that many inspects fail, as a daemon too busy to answer.
	failing int
	removed []string
}

func (d *restartingDocker) set(name, id, startedAt string, running bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids[name] = id
	d.containers[name] = &container.State{Running: running, StartedAt: startedAt}
}

func (d *restartingDocker) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failing > 0 {
		d.failing--
		return container.InspectResponse{}, errors.New("context deadline exceeded")
	}
	st, ok := d.containers[name]
	if !ok {
		return container.InspectResponse{}, errdefs.NotFound(errors.New("no such container"))
	}
	cp := *st
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: d.ids[name], State: &cp}}, nil
}

func (d *restartingDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, cid := range d.ids {
		if cid == id {
			delete(d.containers, name)
			delete(d.ids, name)
			d.removed = append(d.removed, name)
			return nil
		}
	}
	return errdefs.NotFound(errors.New("no such container"))
}

func newRestartApp(t *testing.T) (*App, *restartingDocker, *atomic.Int32) {
	t.Helper()
	orig := agentRestartPoll
	agentRestartPoll = 10 * time.Millisecond
	t.Cleanup(func() { agentRestartPoll = orig })
	d := &restartingDocker{containers: map[string]*container.State{}, ids: map[string]string{}}
	a := &App{logger: zap.NewNop(), docker: d, keployContainer: "keploy-v3-test", container: "app", kind: utils.DockerCompose}
	var setUp atomic.Int32
	a.SetOnAgentRestart(func(context.Context) error { setUp.Add(1); return nil })
	return a, d, &setUp
}

// up is a retried `up` that runs steps one after another, a short while apart,
// and then the run.
func up(steps ...func()) func() utils.CmdError {
	return func() utils.CmdError {
		for _, step := range steps {
			time.Sleep(60 * time.Millisecond)
			step()
		}
		time.Sleep(100 * time.Millisecond)
		return utils.CmdError{}
	}
}

// compose kept the agent running: the app starts behind it, and nothing is
// set up again.
func TestRetryComposeUpLeavesAKeptAgentAlone(t *testing.T) {
	a, d, setUp := newRestartApp(t)
	d.set("keploy-v3-test", "agent-1", "t1", true)
	d.set("app", "app-1", "", false)
	a.retryComposeUp(context.Background(), up(func() { d.set("app", "app-1", "t2", true) }))
	if n := setUp.Load(); n != 0 {
		t.Fatalf("set a kept agent up again %d time(s)", n)
	}
}

// compose stopped the agent (every service attached) and its `up` starts it
// again: it is set up once.
func TestRetryComposeUpSetsUpAnAgentComposeStartedAgain(t *testing.T) {
	a, d, setUp := newRestartApp(t)
	d.set("keploy-v3-test", "agent-1", "t1", false)
	d.set("app", "app-1", "", false)
	a.retryComposeUp(context.Background(), up(func() { d.set("keploy-v3-test", "agent-1", "t3", true) }))
	if n := setUp.Load(); n != 1 {
		t.Fatalf("set the agent compose started again up %d time(s), want 1", n)
	}
}

// A recreate flag: the old agent still runs while `up` starts, and is replaced
// seconds later. The new one is set up, however long the old one lasted.
func TestRetryComposeUpSetsUpARecreatedAgent(t *testing.T) {
	a, d, setUp := newRestartApp(t)
	d.set("keploy-v3-test", "agent-1", "t1", true)
	d.set("app", "app-1", "", false)
	a.retryComposeUp(context.Background(), up(
		func() {}, func() {}, func() {}, // the old agent still answering
		func() { d.set("keploy-v3-test", "agent-1", "t1", false) },
		func() { d.set("keploy-v3-test", "agent-2", "t4", true) },
	))
	if n := setUp.Load(); n != 1 {
		t.Fatalf("set the recreated agent up %d time(s), want 1", n)
	}
}

// The watch ends with the retry's `up`, whatever it saw.
func TestRetryComposeUpStopsWatchingWhenUpReturns(t *testing.T) {
	a, d, setUp := newRestartApp(t)
	d.set("keploy-v3-test", "agent-1", "t1", true)
	done := make(chan struct{})
	go func() {
		a.retryComposeUp(context.Background(), func() utils.CmdError { return utils.CmdError{} })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch outlived the retry's up")
	}
	d.set("keploy-v3-test", "agent-2", "t5", true)
	time.Sleep(50 * time.Millisecond)
	if n := setUp.Load(); n != 0 {
		t.Fatalf("set an agent up %d time(s) after the retry's up returned", n)
	}
}

// Without a hook (no agent client) the retry just runs `up`.
func TestRetryComposeUpWithoutAHookJustRuns(t *testing.T) {
	a := &App{logger: zap.NewNop()}
	ran := false
	a.retryComposeUp(context.Background(), func() utils.CmdError { ran = true; return utils.CmdError{} })
	if !ran {
		t.Fatal("the retry's up did not run")
	}
}

// Docker too busy to say which agent ran before the retry: the agent it keeps
// is not taken for one compose started again once docker answers.
func TestRetryComposeUpKnowsWhichAgentRanBefore(t *testing.T) {
	a, d, setUp := newRestartApp(t)
	d.set("keploy-v3-test", "agent-1", "t1", true)
	d.set("app", "app-1", "", false)
	d.failing = 2
	a.retryComposeUp(context.Background(), up(func() {}, func() { d.set("app", "app-1", "t2", true) }))
	if n := setUp.Load(); n != 0 {
		t.Fatalf("set a kept agent up again %d time(s)", n)
	}
}

// When docker never says which agent ran before, the retry goes by when the
// agent running started: one kept from before it is left alone, one started
// after it began is set up.
func TestRetryComposeUpWithoutTheAgentBeforeGoesByItsStart(t *testing.T) {
	longAgo := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)

	a, d, setUp := newRestartApp(t)
	d.set("keploy-v3-test", "agent-1", longAgo, true)
	d.set("app", "app-1", "", false)
	d.failing = agentBaselineTries
	a.retryComposeUp(context.Background(), up(func() {}, func() { d.set("app", "app-1", "t2", true) }))
	if n := setUp.Load(); n != 0 {
		t.Fatalf("set a kept agent up again %d time(s)", n)
	}

	a, d, setUp = newRestartApp(t)
	d.set("keploy-v3-test", "agent-1", longAgo, true)
	d.set("app", "app-1", "", false)
	d.failing = agentBaselineTries
	a.retryComposeUp(context.Background(), up(func() {
		d.set("keploy-v3-test", "agent-2", time.Now().Format(time.RFC3339Nano), true)
	}))
	if n := setUp.Load(); n != 1 {
		t.Fatalf("set the agent compose recreated up %d time(s), want 1", n)
	}
}

// No agent container before the retry (compose removed it): the one `up`
// creates is set up.
func TestRetryComposeUpSetsUpAnAgentComposeCreated(t *testing.T) {
	a, d, setUp := newRestartApp(t)
	d.set("app", "app-1", "", false)
	a.retryComposeUp(context.Background(), up(func() { d.set("keploy-v3-test", "agent-2", "t2", true) }))
	if n := setUp.Load(); n != 1 {
		t.Fatalf("set the agent compose created up %d time(s), want 1", n)
	}
}

// Once the app's container runs, its agent is the one the run set up, and the
// watch is over: an agent that changes later is not the retry's to set up.
func TestRetryComposeUpStopsWatchingOnceTheAppStarts(t *testing.T) {
	a, d, setUp := newRestartApp(t)
	d.set("keploy-v3-test", "agent-1", "t1", true)
	d.set("app", "app-1", "", false)
	a.retryComposeUp(context.Background(), up(
		func() { d.set("app", "app-1", "t2", true) },
		func() {}, func() {},
		func() { d.set("keploy-v3-test", "agent-2", "t3", true) },
	))
	if n := setUp.Load(); n != 0 {
		t.Fatalf("set an agent up %d time(s) after the app started", n)
	}
}

// The retry's `up` gets a fresh container for each dependency that crashed,
// as the teardown it replaced gave it; nothing else is removed.
func TestRemoveCrashedDependencies(t *testing.T) {
	a, d, _ := newRestartApp(t)
	a.composeService = "app"
	for _, name := range []string{"app", "keploy-v3-test", "dep", "healthy", "migrate"} {
		d.set(name, name+"-id", "t1", false)
	}
	states := []composeServiceState{
		{Service: "app", State: "created", ID: "app-id"},
		{Service: keployAgentComposeService, State: "exited", ExitCode: 1, ID: "keploy-v3-test-id"},
		{Service: "dep", State: "exited", ExitCode: 3, ID: "dep-id"},
		{Service: "healthy", State: "exited", ExitCode: 0, ID: "healthy-id"},
		{Service: "migrate", State: "exited", ExitCode: 1, ID: "migrate-id", Labels: "com.docker.compose.oneoff=True"},
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	a.removeCrashedDependencies(cancelled, states)
	if len(d.removed) != 0 {
		t.Fatalf("removed %v in a cancelled run", d.removed)
	}

	a.removeCrashedDependencies(context.Background(), states)
	if strings.Join(d.removed, ",") != "dep" {
		t.Fatalf("removed %v, want only the crashed dependency", d.removed)
	}
}
