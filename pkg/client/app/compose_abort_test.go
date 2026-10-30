package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// abortDaemon is a daemon holding one compose project's containers, by name
// (a name is its ID here), that keeps its events and replays those since a
// stream's start to it, as the daemon's events do. It lists containers newest
// first, as the daemon does. trail is what happened, in
// order: each die ("die NAME CODE"), each list of the project ("list"), each
// removal ("remove NAME" or "remove NAME force"). Its clock is ahead of the
// test's by ahead (behind, when negative): it stamps its events and says its
// time (Info) by it. pastReplays is how many of its past dies (died) each
// stream asked of it was given, in order.
type abortDaemon struct {
	docker.Client
	mu          sync.Mutex
	ahead       time.Duration
	project     string
	cs          map[string]*abortContainer
	made        int
	log         []events.Message
	subs        []abortSub
	trail       []string
	pastReplays []int
}

type abortContainer struct {
	name, service, status string
	exitCode              int
	// project is the container's compose project when it is not the
	// daemon's (another stack on the same daemon).
	project string
	made    int
}

type abortSub struct {
	ctx  context.Context
	f    events.ListOptions
	msgs chan events.Message
}

func newAbortDaemon(project string, cs ...*abortContainer) *abortDaemon {
	d := &abortDaemon{project: project, cs: map[string]*abortContainer{}}
	d.add(cs...)
	return d
}

// add makes the containers cs, in that order.
func (d *abortDaemon) add(cs ...*abortContainer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range cs {
		d.made++
		c.made = d.made
		d.cs[c.name] = c
	}
}

func (d *abortDaemon) labels(c *abortContainer) map[string]string {
	project := d.project
	if c.project != "" {
		project = c.project
	}
	return map[string]string{composeProjectLabelKey: project, composeServiceLabelKey: c.service}
}

func (d *abortDaemon) takes(s abortSub, m events.Message) bool {
	f := s.f.Filters
	return !(f.Contains("type") && !f.ExactMatch("type", string(m.Type)) ||
		f.Contains("event") && !f.ExactMatch("event", string(m.Action)) ||
		f.Contains("label") && !f.MatchKVList("label", m.Actor.Attributes))
}

// set makes the status of the container named name status, as a start of
// it does.
func (d *abortDaemon) set(name, status string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cs[name].status = status
}

// died is the die of the container named name, with code, ago before now:
// in the daemon's past events, which a stream from before it replays.
func (d *abortDaemon) died(name string, code int, ago time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	attrs := d.labels(d.cs[name])
	attrs["name"], attrs["exitCode"], attrs["test.past"] = name, fmt.Sprint(code), "1"
	d.log = append(d.log, events.Message{Type: events.ContainerEventType, Action: events.ActionDie,
		TimeNano: time.Now().Add(d.ahead - ago).UnixNano(), Actor: events.Actor{ID: name, Attributes: attrs}})
}

// Info says the daemon's time, by its clock.
func (d *abortDaemon) Info(context.Context) (system.Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return system.Info{SystemTime: time.Now().Add(d.ahead).Format(time.RFC3339Nano)}, nil
}

// exit ends the container named name with code, as its process's exit does.
func (d *abortDaemon) exit(name string, code int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.cs[name]
	c.status, c.exitCode = "exited", code
	attrs := d.labels(c)
	attrs["name"], attrs["exitCode"] = name, fmt.Sprint(code)
	m := events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: time.Now().Add(d.ahead).UnixNano(),
		Actor: events.Actor{ID: name, Attributes: attrs}}
	d.log = append(d.log, m)
	d.trail = append(d.trail, fmt.Sprintf("die %s %d", name, code))
	for _, s := range d.subs {
		if d.takes(s, m) {
			select {
			case s.msgs <- m:
			case <-s.ctx.Done():
			}
		}
	}
}

func (d *abortDaemon) Events(ctx context.Context, o events.ListOptions) (<-chan events.Message, <-chan error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var sec, nsec int64
	_, _ = fmt.Sscanf(o.Since, "%d.%d", &sec, &nsec)
	since := sec*int64(time.Second) + nsec
	s := abortSub{ctx: ctx, f: o, msgs: make(chan events.Message, 64)}
	past := 0
	for _, m := range d.log {
		if m.TimeNano >= since && d.takes(s, m) {
			s.msgs <- m
			if m.Actor.Attributes["test.past"] != "" {
				past++
			}
		}
	}
	d.pastReplays = append(d.pastReplays, past)
	d.subs = append(d.subs, s)
	return s.msgs, make(chan error)
}

func (d *abortDaemon) ContainerList(_ context.Context, o container.ListOptions) ([]container.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []container.Summary
	for _, c := range d.cs {
		if (!o.All && c.status != "running") || (o.Filters.Contains("label") && !o.Filters.MatchKVList("label", d.labels(c))) {
			continue
		}
		out = append(out, container.Summary{ID: c.name, Names: []string{"/" + c.name}, Labels: d.labels(c), Created: int64(c.made)})
	}
	slices.SortFunc(out, func(x, y container.Summary) int { return int(y.Created - x.Created) })
	if o.Filters.ExactMatch("label", composeProjectLabelKey+"="+d.project) {
		d.trail = append(d.trail, "list")
	}
	return out, nil
}

func (d *abortDaemon) ContainerInspect(_ context.Context, ref string) (container.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.cs[ref]
	if !ok {
		return container.InspectResponse{}, errdefs.NotFound(fmt.Errorf("No such container: %s", ref))
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: c.name, Name: "/" + c.name,
			State: &container.State{Status: c.status, Running: c.status == "running", ExitCode: c.exitCode}},
		Config: &container.Config{Labels: d.labels(c)},
	}, nil
}

func (d *abortDaemon) ContainerRemove(_ context.Context, ref string, o container.RemoveOptions) error {
	d.mu.Lock()
	c, ok := d.cs[ref]
	if !ok {
		d.mu.Unlock()
		return errdefs.NotFound(fmt.Errorf("No such container: %s", ref))
	}
	if c.status == "running" && !o.Force {
		d.mu.Unlock()
		return errdefs.Conflict(errors.New("cannot remove a running container"))
	}
	delete(d.cs, ref)
	entry := "remove " + ref
	if o.Force {
		entry += " force"
	}
	d.trail = append(d.trail, entry)
	d.mu.Unlock()
	return nil
}

func (d *abortDaemon) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return nil, errors.New("no logs here")
}

func (d *abortDaemon) trailNow() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.trail...)
}

// abortedUp is how playAbortedUp plays the abort. dc is the daemon keploy
// reads: the abortDaemon itself unless set (a wrapper of it). beforeDep runs
// once the project is up, before the dependency's exit; beforeAgent after
// keploy read the project on that exit, before the agent's; afterAgent after
// the agent's exit, given the file that ends the stand-in up.
type abortedUp struct {
	dc                     docker.Client
	beforeDep, beforeAgent func()
	afterAgent             func(ended string)
}

// playAbortedUp is the compose-dep-retry lane's all-attached case: the
// dependency exits before it is healthy, compose aborts and stops every
// container, keploy's agent with it, and the app's container, which it never
// started, stays "created". docker compose v5.6.1's up then waits for that
// container to go, forever, unless it happened to return first. The stand-in
// up waits until its file exists, which only afterAgent may write. keploy
// must see the stack's end in the daemon's events and end the up itself (not
// at the dependency's exit alone, while the agent still runs), leave the
// project as compose left it (no down: the retry's decision reads it), and
// retry the bring-up, which comes up this time.
func playAbortedUp(t *testing.T, d *abortDaemon, p abortedUp) {
	t.Helper()
	ps := `{"Service":"dep","State":"exited","ExitCode":3,"ID":"tmpproj-dep-1"}
{"Service":"runner","State":"created","ExitCode":0,"ID":"runner"}
{"Service":"keploy-agent","State":"exited","ExitCode":0,"ID":"keploy-v3-test"}
`
	argvLog := stubDockerCLI(t, ps, 0)
	ended := filepath.Join(t.TempDir(), "up-ends")
	t.Setenv("KEPLOY_STUB_UP_WAIT_FILE", ended)
	t.Setenv("KEPLOY_STUB_UP_CODE", "3")
	dc := p.dc
	if dc == nil {
		dc = d
	}
	a := &App{
		logger:          zap.NewNop(),
		docker:          dc,
		kind:            utils.DockerCompose,
		cmd:             "docker compose -f /tmp/keploy-compose.yaml up --abort-on-container-exit --exit-code-from runner",
		composeFile:     "/tmp/keploy-compose.yaml",
		keployContainer: "keploy-v3-test",
		container:       "runner",
		composeService:  "runner",
		composeServices: []string{"dep", "runner", keployAgentComposeService},
		opts:            models.SetupOptions{Mode: models.MODE_RECORD},
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan models.AppError, 1)
	go func() { ran <- a.run(ctx) }()
	t.Cleanup(func() {
		// The stand-in up waits for this file: one keploy did not end (its
		// kill broken) ends here, not as an orphan of the test.
		_ = os.WriteFile(ended, nil, 0o600)
		cancel()
		select {
		case <-ran:
		case <-time.After(time.Minute):
			// A run() stuck past its stop (a read that outlives the watch)
			// fails the test here rather than hanging the package.
			t.Errorf("run() did not return within a minute of its context's end")
		}
	})

	eventually(t, "the up starts", func() bool { return strings.Contains(recordedLog(t, argvLog), "ARG up") })
	// The up makes the project: the dependency and keploy's agent run, the
	// app waits on the dependency's health.
	d.add(&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "running"},
		&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "running"},
		&abortContainer{name: "runner", service: "runner", status: "created"})
	if p.beforeDep != nil {
		p.beforeDep()
	}
	d.exit("tmpproj-dep-1", 3)
	// keploy looks at the project on the dependency's exit; the agent runs,
	// so the up goes on.
	eventually(t, "keploy looks at the project after the dependency's exit", func() bool {
		_, after, _ := strings.Cut(strings.Join(d.trailNow(), "\n"), "die tmpproj-dep-1 3")
		return strings.Contains(after, "list")
	})
	if strings.Contains(recordedLog(t, argvLog), "UP-RETRIED") {
		t.Fatalf("keploy ended the up while keploy's agent still ran\n%s", recordedLog(t, argvLog))
	}
	if p.beforeAgent != nil {
		p.beforeAgent()
	}
	d.exit("keploy-v3-test", 0)
	if p.afterAgent != nil {
		p.afterAgent(ended)
	}

	var appErr models.AppError
	select {
	case appErr = <-ran:
		ran <- appErr
	case <-time.After(time.Minute):
		t.Fatalf("run() did not return within a minute: keploy waits on an up whose project compose stopped before it started the app\ndaemon: %v\n%s",
			d.trailNow(), recordedLog(t, argvLog))
	}
	log := recordedLog(t, argvLog)
	if !strings.Contains(log, "UP-RETRIED") {
		t.Fatalf("the bring-up was not retried (run() returned %+v)\n%s", appErr, log)
	}
	if appErr.AppErrorType != models.ErrAppStopped {
		t.Fatalf("the retried up exited 0, run() reported %+v", appErr)
	}
	before, _, _ := strings.Cut(log, "UP-RETRIED")
	if strings.Contains(before, "ARG down") {
		t.Fatalf("keploy took the project down before the retry, whose decision reads it as compose left it\n%s", log)
	}
	// The stand-in up ignores TERM and INT, as an up whose own handling of a
	// signal does not end it: keploy's end of it is a kill, which it cannot
	// catch.
	if strings.Contains(before, "UP-SIGNALLED") {
		t.Fatalf("keploy ended the up it saw abort with a signal compose can catch, not a kill\n%s", log)
	}
	// The retry removes the crashed dependency; the app's container goes
	// only in the run's teardown, after the retried up.
	trail := d.trailNow()
	retried := slices.Index(trail, "remove tmpproj-dep-1 force")
	for i, e := range trail {
		if e == "remove runner" || (e == "remove runner force" && i < retried) {
			t.Fatalf("keploy removed the app's container before the retry: %v", trail)
		}
	}
	if retried < 0 {
		t.Fatalf("the retry did not remove the crashed dependency: %v", trail)
	}
}

// TestAComposeUpThatAbortedBeforeTheAppStartedEndsAndIsRetried: the up
// either waits for the app's container (keploy must end it) or returns on
// its own (code 3) as the agent stops, as compose's up does when the failed
// start returned first; keploy retries the bring-up either way. Before,
// keploy waited on the first kind until the run's end.
func TestAComposeUpThatAbortedBeforeTheAppStartedEndsAndIsRetried(t *testing.T) {
	t.Run("the up waits for the app's container", func(t *testing.T) {
		playAbortedUp(t, newAbortDaemon("tmpproj"), abortedUp{})
	})
	t.Run("the up returns on its own as the agent stops", func(t *testing.T) {
		playAbortedUp(t, newAbortDaemon("tmpproj"), abortedUp{afterAgent: func(ended string) {
			if err := os.WriteFile(ended, nil, 0o600); err != nil {
				t.Fatalf("could not end the stand-in up: %v", err)
			}
		}})
	})
}

// flakyDaemon is an abortDaemon that fails reads the way a starved daemon
// does, once each time the test arms it: the next list of the project
// (failList), or the next inspect of keploy's agent's container
// (failAgent), fails at once, or, slow, answers only once the caller's cap
// has passed. Once listNeeds is set, every list of the project answers only
// to a caller whose cap leaves it at least that long, as a daemon slower
// than keploy's first cap on every read does; others' it answers only once
// their cap has passed. Once listFails is set, every list of the project
// fails, as a daemon's that fails for good. Its first events stream breaks
// when the test sends on breakFirst; the stream asked for after it opens only
// once the test closes reopen, and asked is closed as the watch asks for it.
// Its next read of the daemon's time fails once failInfo is set (as the
// others, at once or slow); it refuses every one when infoRefused is set.
type flakyDaemon struct {
	*abortDaemon
	agent       string
	slow        bool
	infoRefused bool
	breakFirst  chan error
	asked       chan struct{}
	reopen      chan struct{}

	fmu                           sync.Mutex
	failList, failAgent, failInfo bool
	listFails                     bool
	listNeeds                     time.Duration
	streams                       int
}

func (d *flakyDaemon) Info(ctx context.Context) (system.Info, error) {
	if d.infoRefused {
		return system.Info{}, errdefs.Forbidden(errors.New("Error response from daemon: forbidden by the socket proxy"))
	}
	if err := d.fails(ctx, &d.failInfo); err != nil {
		return system.Info{}, err
	}
	return d.abortDaemon.Info(ctx)
}

func (d *flakyDaemon) arm(list, agent bool) {
	d.fmu.Lock()
	defer d.fmu.Unlock()
	d.failList, d.failAgent = list, agent
}

// fails takes the armed failure of *armed, if any, and fails the read with
// it.
func (d *flakyDaemon) fails(ctx context.Context, armed *bool) error {
	d.fmu.Lock()
	fail := *armed
	*armed = false
	d.fmu.Unlock()
	if !fail {
		return nil
	}
	if d.slow {
		<-ctx.Done()
		return ctx.Err()
	}
	return errors.New("Error response from daemon: i/o timeout")
}

func (d *flakyDaemon) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	if o.Filters.ExactMatch("label", composeProjectLabelKey+"="+d.project) {
		if err := d.fails(ctx, &d.failList); err != nil {
			return nil, err
		}
		d.fmu.Lock()
		fails := d.listFails && slices.Equal(o.Filters.Get("label"), []string{composeProjectLabelKey + "=" + d.project})
		d.fmu.Unlock()
		if fails {
			return nil, errors.New("Error response from daemon: i/o timeout")
		}
		d.fmu.Lock()
		needs := d.listNeeds
		d.fmu.Unlock()
		if dl, ok := ctx.Deadline(); needs > 0 && (!ok || time.Until(dl) < needs) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	return d.abortDaemon.ContainerList(ctx, o)
}

func (d *flakyDaemon) ContainerInspect(ctx context.Context, ref string) (container.InspectResponse, error) {
	if ref == d.agent {
		if err := d.fails(ctx, &d.failAgent); err != nil {
			return container.InspectResponse{}, err
		}
	}
	return d.abortDaemon.ContainerInspect(ctx, ref)
}

func (d *flakyDaemon) Events(ctx context.Context, o events.ListOptions) (<-chan events.Message, <-chan error) {
	d.fmu.Lock()
	d.streams++
	n := d.streams
	d.fmu.Unlock()
	if d.breakFirst != nil {
		switch n {
		case 1:
			msgs, _ := d.abortDaemon.Events(ctx, o)
			return msgs, d.breakFirst
		case 2:
			close(d.asked)
			select {
			case <-d.reopen:
			case <-ctx.Done():
			}
		}
	}
	return d.abortDaemon.Events(ctx, o)
}

// TestTheAbortWatchReadsAFailedReadAgain: a read of the daemon for the
// abort's decision that fails, or answers only past its cap, as a starved
// daemon's does, is read again for the same die. On the agent's die, the
// abort's last, no other die comes: a decision given up there was the
// original hang back. On the project's first die, the project of keploy's
// agent read there is the one that die is told against.
func TestTheAbortWatchReadsAFailedReadAgain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		slow       bool
		agentFirst bool
		// slower: from the agent's die on, every list of the project takes
		// longer than keploy's first cap (dockerInspectBudget).
		slower bool
		// inRead: the agent's inspect fails in the read of the project on the
		// agent's die (its project is known by then).
		inRead bool
	}{
		{name: "the project's list on the agent's die fails"},
		{name: "the project's list on the agent's die answers past its cap", slow: true},
		{name: "the agent's inspect on the first die fails", agentFirst: true},
		{name: "the agent's inspect on the first die answers past its cap", slow: true, agentFirst: true},
		{name: "every list on the agent's die is slower than the first cap", slower: true},
		{name: "an inspect in the project's read on the agent's die fails", inRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newAbortDaemon("tmpproj")
			fd := &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test", slow: tc.slow}
			p := abortedUp{dc: fd}
			switch {
			case tc.slower:
				p.beforeAgent = func() {
					fd.fmu.Lock()
					fd.listNeeds = dockerInspectBudget + dockerInspectBudget/2
					fd.fmu.Unlock()
				}
			case tc.agentFirst:
				p.beforeDep = func() { fd.arm(false, true) }
			case tc.inRead:
				p.beforeAgent = func() { fd.arm(false, true) }
			default:
				p.beforeAgent = func() { fd.arm(true, false) }
			}
			playAbortedUp(t, d, p)
		})
	}
}

// TestTheAbortWatchAsksForABrokenStreamAgain: the events stream breaks after
// the dependency's die, and the agent's die comes while the watch is between
// streams: the stream asked for again from its last event brings it.
func TestTheAbortWatchAsksForABrokenStreamAgain(t *testing.T) {
	d := newAbortDaemon("tmpproj")
	fd := &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test", breakFirst: make(chan error, 1),
		asked: make(chan struct{}), reopen: make(chan struct{})}
	playAbortedUp(t, d, abortedUp{
		dc: fd,
		beforeAgent: func() {
			fd.breakFirst <- errors.New("unexpected EOF")
			select {
			case <-fd.asked:
			case <-time.After(time.Minute):
				t.Fatal("the watch did not ask for its events stream again within a minute after it broke")
			}
		},
		afterAgent: func(string) { close(fd.reopen) },
	})
}

// TestTheAbortWatchReadsFromItsStart: the watch reads the daemon's events
// from the moment it is made, not from when its goroutine first runs: on a
// loaded host the whole abort can be over before that. With one P, the
// watch's goroutine runs only once this one waits, after both dies. That
// moment is the daemon's: a daemon whose clock is behind keploy's stamps
// those dies before keploy's now, and a stream from keploy's now misses them.
func TestTheAbortWatchReadsFromItsStart(t *testing.T) {
	for _, ahead := range []time.Duration{0, -5 * time.Second} {
		t.Run(fmt.Sprintf("the daemon's clock %v ahead", ahead), func(t *testing.T) {
			defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
			d := newAbortDaemon("tmpproj",
				&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "running"},
				&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "running"},
				&abortContainer{name: "runner", service: "runner", status: "created"})
			d.ahead = ahead
			a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose,
				keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
			upCtx, stop := a.watchComposeAbort(context.Background())
			defer stop()
			d.exit("tmpproj-dep-1", 3)
			d.exit("keploy-v3-test", 0)
			select {
			case <-upCtx.Done():
			case <-time.After(time.Minute):
				t.Fatalf("the watch did not end the up within a minute of an abort that was over before its goroutine ran (daemon %v)", d.trailNow())
			}
			if cause := context.Cause(upCtx); !errors.Is(cause, errComposeUpAborted) {
				t.Fatalf("the up ended with %v, not keploy's end of the abort", cause)
			}
		})
	}
}

// TestTheAbortWatchReadsEventsOnTheDaemonsClock: the retried up's watch
// starts 2.5 s after the last attempt's abort ended (the retry's backoff and
// its reads), by keploy's clock, on a daemon whose clock is 5 s ahead. The
// last attempt's dies, the agent's among them, are then later than keploy's
// now by the daemon's stamps; a stream from keploy's now replays them, and
// the project as that abort left it (the agent exited, the app created, the
// crashed dependency removed by the retry) reads as the abort: keploy killed
// the retried up before compose started anything. From the daemon's time,
// read before the up starts (again when that read fails), the watch is given
// none of the last attempt's dies, and sees this up's abort.
func TestTheAbortWatchReadsEventsOnTheDaemonsClock(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fails, slow bool
	}{
		{name: "the daemon's time is read"},
		{name: "a failed read of the daemon's time is read again", fails: true},
		{name: "a read of the daemon's time past its cap is read again", fails: true, slow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newAbortDaemon("tmpproj",
				&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "exited"},
				&abortContainer{name: "runner", service: "runner", status: "created"})
			d.ahead = 5 * time.Second
			d.died("keploy-v3-test", 0, 2500*time.Millisecond)
			fd := &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test", slow: tc.slow, failInfo: tc.fails}
			a := &App{logger: zap.NewNop(), docker: fd, kind: utils.DockerCompose,
				keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
			upCtx, stop := a.watchComposeAbort(context.Background())
			defer stop()
			// The retried up starts keploy's agent again and makes the
			// dependency anew, which exits before it is healthy again.
			d.set("keploy-v3-test", "running")
			d.add(&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "running"})
			d.exit("tmpproj-dep-1", 3)
			d.exit("keploy-v3-test", 0)
			select {
			case <-upCtx.Done():
			case <-time.After(time.Minute):
				t.Fatalf("the watch did not end the retried up within a minute of its abort (daemon %v)", d.trailNow())
			}
			if cause := context.Cause(upCtx); !errors.Is(cause, errComposeUpAborted) {
				t.Fatalf("the up ended with %v, not keploy's end of the abort", cause)
			}
			d.mu.Lock()
			replays := slices.Clone(d.pastReplays)
			d.mu.Unlock()
			if len(replays) == 0 || replays[0] != 0 {
				t.Fatalf("the retried up's watch was given %v past dies (the last attempt's: the daemon's clock is %v ahead of keploy's), want none", replays, d.ahead)
			}
		})
	}
}

// TestAnAbortWatchOnADaemonThatWillNotSayItsTime: a daemon that refuses
// to say its time (a socket proxy that forbids the request) leaves the watch
// on keploy's clock: the up starts, and its abort is seen and retried.
func TestAnAbortWatchOnADaemonThatWillNotSayItsTime(t *testing.T) {
	d := newAbortDaemon("tmpproj")
	playAbortedUp(t, d, abortedUp{dc: &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test", infoRefused: true}})
}

// TestAReadThatKeepsFailingEndsWithTheUp: from the agent's die on, every list
// of the project fails, and compose's up returns on its own. The decision's
// reads end with the watch, which ends with the up: keploy retries the
// bring-up rather than wait on reads that would never end.
func TestAReadThatKeepsFailingEndsWithTheUp(t *testing.T) {
	d := newAbortDaemon("tmpproj")
	fd := &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test"}
	playAbortedUp(t, d, abortedUp{
		dc: fd,
		beforeAgent: func() {
			fd.fmu.Lock()
			fd.listFails = true
			fd.fmu.Unlock()
		},
		afterAgent: func(ended string) {
			if err := os.WriteFile(ended, nil, 0o600); err != nil {
				t.Fatalf("could not end the stand-in up: %v", err)
			}
		},
	})
}

// TestTheAbortWatchTellsItsProjectByKeployAgent: another compose project on
// the daemon has a container die before keploy's project does. The watch's
// project is the one keploy's agent's container is of, not the first die's,
// or keploy's own abort is never seen.
func TestTheAbortWatchTellsItsProjectByKeployAgent(t *testing.T) {
	d := newAbortDaemon("tmpproj")
	playAbortedUp(t, d, abortedUp{beforeDep: func() {
		d.add(&abortContainer{name: "other-db-1", service: "db", status: "running", project: "other"})
		d.exit("other-db-1", 1)
	}})
}

// TestTheProvenSwitchLeavesTheAbortWatchOff: under the proven switch keploy
// leaves compose's abort flags out and ends the run on the app's exit itself
// (compose_appstop.go), so no up is watched for the abort; outside it, a
// compose app's up is.
func TestTheProvenSwitchLeavesTheAbortWatchOff(t *testing.T) {
	a := &App{logger: zap.NewNop(), docker: newAbortDaemon("p"), kind: utils.DockerCompose,
		keployContainer: "agent", container: "app", composeService: "app"}
	if !a.watchesComposeAbort() {
		t.Fatal("a compose app's up is not watched for the abort with the proven switch off")
	}
	t.Setenv(provenswitch.Env, "1")
	if a.watchesComposeAbort() {
		t.Fatal("a compose app's up is watched for the abort under the proven switch")
	}
}

// TestTheVendoredComposeHasNoAbortWait: the compose library keploy vendors
// for its in-process path must be older than v5.6.0, whose up waits forever
// for an app's container it never started after it aborted
// (docker/compose#14426). keploy does not watch that path for the abort
// (compose_abort.go); a bump to v5.6.0 or later must bring the watch to it
// first, and fails here until then.
func TestTheVendoredComposeHasNoAbortWait(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var mod []byte
	for {
		if mod, err = os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod above the package")
		}
		dir = up
	}
	const path = "github.com/docker/compose/v5"
	found := false
	for _, line := range strings.Split(string(mod), "\n") {
		f := strings.Fields(line)
		if slices.Contains(f, "=>") && slices.Contains(f, path) {
			t.Fatalf("go.mod replaces %s (%s): check the replacement has no wait for a never-started app's container (docker/compose#14426), then teach this row its version", path, strings.TrimSpace(line))
		}
		if len(f) < 2 || f[0] != path && !(f[0] == "require" && len(f) > 2 && f[1] == path) {
			continue
		}
		v := f[1]
		if f[0] == "require" {
			v = f[2]
		}
		var major, minor int
		if _, err := fmt.Sscanf(v, "v%d.%d", &major, &minor); err != nil {
			t.Fatalf("go.mod's %s version %q does not read as vX.Y: %v", path, v, err)
		}
		if major != 5 || minor >= 6 {
			t.Fatalf("go.mod has %s %s: compose v5.6.0 and later wait forever after an abort before the app started (docker/compose#14426), and keploy does not watch its in-process up for that (compose_abort.go); watch it before the bump", path, v)
		}
		found = true
	}
	if !found {
		t.Fatalf("go.mod does not require %s: this row no longer reads the vendored compose; move it with the library", path)
	}
}

// TestComposeAbortedBeforeTheAppReadsTheWholeProject: an up is over before
// the app started only when nothing of the project runs, keploy's agent ran
// and stopped, and the app's container never started; not while a container
// still runs or restarts, nor while the agent was never started (compose may
// still start it, and the app after it), nor once the app's container ran.
// Nor for a scaled app service (scaled: a second container of it): keploy
// decides on the app's one container only, as its stop of the app does
// (compose_appstop.go), so a project with two is never read as that end.
func TestComposeAbortedBeforeTheAppReadsTheWholeProject(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		app, scaled, agent, dep string
		want                    bool
	}{
		{name: "all stopped, app never started", app: "created", agent: "exited", dep: "exited", want: true},
		{name: "agent dead, app never started", app: "created", agent: "dead", dep: "exited", want: true},
		{name: "agent still runs", app: "created", agent: "running", dep: "exited"},
		{name: "agent never started", app: "created", agent: "created", dep: "exited"},
		{name: "dependency restarting", app: "created", agent: "exited", dep: "restarting"},
		{name: "app ran and exited", app: "exited", agent: "exited", dep: "exited"},
		{name: "app runs", app: "running", agent: "exited", dep: "exited"},
		{name: "no app container", agent: "exited", dep: "exited"},
		{name: "no agent container", app: "created", dep: "exited"},
		{name: "scaled app, neither started", app: "created", scaled: "created", agent: "exited", dep: "exited"},
		{name: "scaled app, one ran", app: "created", scaled: "exited", agent: "exited", dep: "exited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := []*abortContainer{{name: "p-dep-1", service: "dep", status: tc.dep, exitCode: 3}}
			if tc.agent != "" {
				cs = append(cs, &abortContainer{name: "agent", service: keployAgentComposeService, status: tc.agent})
			}
			if tc.app != "" {
				cs = append(cs, &abortContainer{name: "app", service: "app", status: tc.app})
			}
			if tc.scaled != "" {
				cs = append(cs, &abortContainer{name: "p-app-2", service: "app", status: tc.scaled})
			}
			d := newAbortDaemon("p", cs...)
			a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose, keployContainer: "agent", container: "app", composeService: "app"}
			if got := a.composeAbortedBeforeTheApp(context.Background(), "p"); got != tc.want {
				t.Fatalf("composeAbortedBeforeTheApp = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestComposeRecreatingARunningAgentIsNotTheAbort: the retried
// `up --always-recreate-deps app` after an app-attached abort, which left
// keploy's agent running, or an up with --force-recreate. compose has made
// the new agent (under a temporary name, still "created") and the new
// dependency (the crashed one, which keploy's retry removed), and stops the
// old agent: that die is not the abort, compose goes on to start them. The
// daemon lists the old agent last; read as the abort, keploy killed the
// retried up partway through the recreate.
func TestComposeRecreatingARunningAgentIsNotTheAbort(t *testing.T) {
	d := newAbortDaemon("p",
		&abortContainer{name: "agent", service: keployAgentComposeService, status: "exited"},
		&abortContainer{name: "app", service: "app", status: "created"},
		&abortContainer{name: "p-dep-1", service: "dep", status: "created"},
		&abortContainer{name: "0123456789ab_agent", service: keployAgentComposeService, status: "created"},
	)
	a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose, keployContainer: "agent", container: "app", composeService: "app"}
	if a.composeAbortedBeforeTheApp(context.Background(), "p") {
		t.Fatal("compose's recreate of a running agent (the new agent created, the old one stopped) is read as the abort: keploy kills the retried up")
	}
}

// TestComposeAbortCmdErrorSaysKeployEndedTheUp: an up keploy ended on the
// abort says so, and stays a Runtime error with the up's own exit in it, the
// error the retry's decision reads; an up the watch did not end is as it
// returned.
func TestComposeAbortCmdErrorSaysKeployEndedTheUp(t *testing.T) {
	exit := &exec.ExitError{}
	in := utils.CmdError{Type: utils.Runtime, Err: exit}

	upCtx, endUp := context.WithCancelCause(context.Background())
	if got := composeAbortCmdError(upCtx, in); got != in {
		t.Fatalf("an up the watch did not end became %+v", got)
	}
	endUp(errComposeUpAborted)
	got := composeAbortCmdError(upCtx, in)
	if got.Type != utils.Runtime || !errors.Is(got.Err, errComposeUpAborted) || !errors.As(got.Err, &exit) ||
		!strings.Contains(got.Err.Error(), "keploy ended docker compose's up") {
		t.Fatalf("an up keploy ended reads %+v", got)
	}
	if got := composeAbortCmdError(context.Background(), utils.CmdError{}); got.Err != nil {
		t.Fatalf("an up that exited 0, not ended by keploy, became %+v", got)
	}
	// An up that exited 0 as keploy ended it: the app never started, so it
	// is the same Runtime error, which the retry's decision reads.
	got = composeAbortCmdError(upCtx, utils.CmdError{})
	if got.Type != utils.Runtime || !errors.Is(got.Err, errComposeUpAborted) ||
		!strings.Contains(got.Err.Error(), "keploy ended docker compose's up") {
		t.Fatalf("an up keploy ended that returned no error reads %+v", got)
	}
}

// TestAUsersStopIsNotKeployEndingTheUp: an up the run's own end stopped (a
// user's Ctrl+C, the record timer) is stopped by the run's cancel, and its
// error is as it returned: only the watch's end of it is keploy's kill and
// reads "keploy ended docker compose's up".
func TestAUsersStopIsNotKeployEndingTheUp(t *testing.T) {
	run, stopRun := context.WithCancel(context.Background())
	upCtx, endUp := context.WithCancelCause(run)
	defer endUp(nil)
	stopRun()
	in := utils.CmdError{Type: utils.Runtime, Err: &exec.ExitError{}}
	if got := composeAbortCmdError(upCtx, in); got != in {
		t.Fatalf("an up the run's end stopped reads %+v", got)
	}
	stopped := false
	cancel := func(*exec.Cmd) func() error { return func() error { stopped = true; return nil } }
	// No process: a kill of the up's group here is the bug, and fails.
	if err := endsAbortedComposeUp(upCtx, zap.NewNop(), cancel)(&exec.Cmd{})(); err != nil || !stopped {
		t.Fatalf("an up the run's end stopped was not stopped by the run's cancel (stopped %v, err %v)", stopped, err)
	}
}

// TestComposeAbortEndsReadsOnlyItsProject: the watch reads the project on
// each die of one of its own containers, the app's too (compose recreating a
// running app stops the old one and goes on, and the up can still abort), and
// that die is the abort's end once every container has stopped with the
// app's never started. Other projects' dies on the same daemon, and one-off
// containers', are not its project's end.
func TestComposeAbortEndsReadsOnlyItsProject(t *testing.T) {
	die := func(project, service string, oneOff bool) events.Message {
		attrs := map[string]string{composeProjectLabelKey: project, composeServiceLabelKey: service}
		if oneOff {
			attrs[composeOneOffLabelKey] = "True"
		}
		return events.Message{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: service, Attributes: attrs}}
	}
	for _, tc := range []struct {
		name           string
		m              events.Message
		agent, app     string
		aborted, reads bool
	}{
		{name: "the app's die", m: die("p", "app", false), agent: "running", app: "exited", reads: true},
		{name: "the app's die, everything stopped", m: die("p", "app", false), agent: "exited", app: "exited", reads: true},
		{name: "another project's die", m: die("q", "dep", false), agent: "exited", app: "created"},
		{name: "a one-off's die", m: die("p", "dep", true), agent: "exited", app: "created"},
		{name: "a dependency's die, the agent still runs", m: die("p", "dep", false), agent: "running", app: "created", reads: true},
		{name: "the agent's die, the app never started", m: die("p", keployAgentComposeService, false), agent: "exited", app: "created", aborted: true, reads: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newAbortDaemon("p",
				&abortContainer{name: "p-dep-1", service: "dep", status: "exited", exitCode: 3},
				&abortContainer{name: "agent", service: keployAgentComposeService, status: tc.agent},
				&abortContainer{name: "app", service: "app", status: tc.app},
			)
			a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose, keployContainer: "agent", container: "app", composeService: "app"}
			if aborted := a.composeAbortEnds(context.Background(), "p", tc.m); aborted != tc.aborted {
				t.Fatalf("composeAbortEnds = %v, want %v", aborted, tc.aborted)
			}
			if reads := slices.Contains(d.trailNow(), "list"); reads != tc.reads {
				t.Fatalf("the watch read the project: %v, want %v (%v)", reads, tc.reads, d.trailNow())
			}
		})
	}
}

// TestComposeRecreatingARunningAppLeavesTheWatchOn: compose recreates the
// running app (its config changed): it makes the new one, stops the old one
// (a die of the app's service) and removes it. The dependency then exits
// before it is healthy and compose aborts, with the new app's container
// never started. The old app's die is not the watch's end: the abort that
// follows in the same up is seen.
func TestComposeRecreatingARunningAppLeavesTheWatchOn(t *testing.T) {
	d := newAbortDaemon("tmpproj",
		&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "running"},
		&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "running"},
		&abortContainer{name: "runner", service: "runner", status: "running"})
	a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose,
		keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
	upCtx, stop := a.watchComposeAbort(context.Background())
	defer stop()
	d.add(&abortContainer{name: "0123456789ab_runner", service: "runner", status: "created"})
	d.exit("runner", 137)
	eventually(t, "the watch reads the project on the old app's die", func() bool {
		_, after, _ := strings.Cut(strings.Join(d.trailNow(), "\n"), "die runner 137")
		return strings.Contains(after, "list")
	})
	_ = d.ContainerRemove(context.Background(), "runner", container.RemoveOptions{Force: true})
	d.exit("tmpproj-dep-1", 3)
	d.exit("keploy-v3-test", 0)
	select {
	case <-upCtx.Done():
	case <-time.After(time.Minute):
		t.Fatalf("the watch did not end the up within a minute of an abort after compose recreated the running app (daemon %v)", d.trailNow())
	}
	if cause := context.Cause(upCtx); !errors.Is(cause, errComposeUpAborted) {
		t.Fatalf("the up ended with %v, not keploy's end of the abort", cause)
	}
}
