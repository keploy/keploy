package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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
	// oneOff: a `docker compose run` container of service, not one of the
	// up's.
	oneOff bool
	made   int
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
	// compose labels every container of its own with oneoff, True or False.
	oneOff := "False"
	if c.oneOff {
		oneOff = "True"
	}
	return map[string]string{composeProjectLabelKey: project, composeServiceLabelKey: c.service, composeOneOffLabelKey: oneOff}
}

func (d *abortDaemon) takes(s abortSub, m events.Message) bool {
	f := s.f.Filters
	return (!f.Contains("type") || f.ExactMatch("type", string(m.Type))) &&
		(!f.Contains("event") || f.ExactMatch("event", string(m.Action))) &&
		(!f.Contains("label") || f.MatchKVList("label", m.Actor.Attributes))
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

// abortedUpRun is run() of the compose-dep-retry lane's app on a daemon,
// with the stand-in docker CLI (stubDockerCLI): the first up runs until the
// file ended exists (only a kill ends it before), then exits 3; the retried
// up exits 0 at once. ran gives run()'s result once it returns.
type abortedUpRun struct {
	argvLog, ended string
	ran            chan models.AppError
}

// startAbortedUpRun starts that run() on dc, and returns once its first up
// has started its child (see stubDockerCLI). The test's cleanup ends the
// stand-in up and the run.
func startAbortedUpRun(t *testing.T, dc docker.Client) abortedUpRun {
	t.Helper()
	ps := `{"Service":"dep","State":"exited","ExitCode":3,"ID":"tmpproj-dep-1"}
{"Service":"runner","State":"created","ExitCode":0,"ID":"runner"}
{"Service":"keploy-agent","State":"exited","ExitCode":0,"ID":"keploy-v3-test"}
`
	r := abortedUpRun{argvLog: stubDockerCLI(t, ps, 0), ended: filepath.Join(t.TempDir(), "up-ends"), ran: make(chan models.AppError, 1)}
	t.Setenv("KEPLOY_STUB_UP_WAIT_FILE", r.ended)
	t.Setenv("KEPLOY_STUB_UP_CODE", "3")
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
	go func() { r.ran <- a.run(ctx) }()
	t.Cleanup(func() {
		// The stand-in up waits for this file: one keploy did not end (its
		// kill broken) ends here, not as an orphan of the test.
		_ = os.WriteFile(r.ended, nil, 0o600)
		cancel()
		select {
		case appErr := <-r.ran:
			r.ran <- appErr
		case <-time.After(time.Minute):
			// A run() stuck past its stop (a read that outlives the watch)
			// fails the test here rather than hanging the package.
			t.Errorf("run() did not return within a minute of its context's end")
		}
	})
	// Until the first up has set its traps and started its child, an end of
	// it would not be the end of an up that has them.
	eventually(t, "the up starts", func() bool { return strings.Contains(recordedLog(t, r.argvLog), "UP-CHILD-STARTED") })
	return r
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
	dc := p.dc
	if dc == nil {
		dc = d
	}
	r := startAbortedUpRun(t, dc)
	argvLog, ended, ran := r.argvLog, r.ended, r.ran

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
	// The first up started a child, as the docker CLI starts its compose
	// plugin: keploy's kill is of the up's whole process group, so no part
	// of the aborted up is left waiting on the app's container beside the
	// retried up.
	if !strings.Contains(before, "UP-CHILD-STARTED") {
		t.Fatalf("the stand-in's first up started no child\n%s", log)
	}
	if strings.Contains(before, "UP-CHILD-OUTLIVED") {
		t.Fatalf("a child of the aborted up outlived keploy's end of it (keploy killed the up's process group leader, not the group)\n%s", log)
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
// others, at once or slow); every one fails with infoErr when that is set,
// says the time infoTime when that is set, and, with infoHangs, answers only
// when its caller's context ends, as a daemon whose /info hangs while its
// events still flow. infoReads counts those reads, and infoCapped those of
// them that hung until their cap passed.
// Once agentFails is set, every inspect of keploy's agent's container fails,
// as a daemon's that fails it for good (a socket proxy's rule); agentReads
// counts those inspects.
// listErrs, agentErrs and streamErrs are scripts: the project's next lists,
// the next inspects of keploy's agent's container and the next events
// streams asked for fail with them, one each, in turn (a nil one answers),
// and then answer. lists and agentAsks count every list of the project and
// every inspect of the agent's container; streams, every stream asked for.
type flakyDaemon struct {
	*abortDaemon
	agent      string
	slow       bool
	infoErr    error
	infoTime   *string
	infoHangs  bool
	breakFirst chan error
	asked      chan struct{}
	reopen     chan struct{}

	fmu                           sync.Mutex
	failList, failAgent, failInfo bool
	listFails, agentFails         bool
	listNeeds                     time.Duration
	streams, infoReads            int
	agentReads, infoCapped        int
	listErrs, agentErrs           []error
	streamErrs                    []error
	lists, agentAsks              int
}

// next takes the next error of *script, nil once it is done.
func (d *flakyDaemon) next(script *[]error) error {
	d.fmu.Lock()
	defer d.fmu.Unlock()
	if len(*script) == 0 {
		return nil
	}
	err := (*script)[0]
	*script = (*script)[1:]
	return err
}

func (d *flakyDaemon) Info(ctx context.Context) (system.Info, error) {
	d.fmu.Lock()
	d.infoReads++
	d.fmu.Unlock()
	if d.infoHangs {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			d.fmu.Lock()
			d.infoCapped++
			d.fmu.Unlock()
		}
		return system.Info{}, ctx.Err()
	}
	if d.infoErr != nil {
		return system.Info{}, d.infoErr
	}
	if err := d.fails(ctx, &d.failInfo); err != nil {
		return system.Info{}, err
	}
	if d.infoTime != nil {
		return system.Info{SystemTime: *d.infoTime}, nil
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
		d.fmu.Lock()
		d.lists++
		d.fmu.Unlock()
		if err := d.next(&d.listErrs); err != nil {
			return nil, err
		}
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
		d.fmu.Lock()
		d.agentAsks++
		d.fmu.Unlock()
		if err := d.next(&d.agentErrs); err != nil {
			return container.InspectResponse{}, err
		}
		d.fmu.Lock()
		fails := d.agentFails
		if fails {
			d.agentReads++
		}
		d.fmu.Unlock()
		if fails {
			return container.InspectResponse{}, errdefs.Forbidden(errors.New("Error response from daemon: forbidden by the socket proxy"))
		}
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
	if err := d.next(&d.streamErrs); err != nil {
		errs := make(chan error, 1)
		errs <- err
		return make(chan events.Message), errs
	}
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
		// longer than keploy's first cap (composeAbortFirstReadCap).
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
					fd.listNeeds = composeAbortFirstReadCap + composeAbortFirstReadCap/2
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

// TestAnAbortWatchOnADaemonThatWillNotSayItsTime: the up starts only once
// the watch has read the daemon's time, so a daemon that will not say it
// must leave the watch on keploy's clock, not hold the up: one that refuses
// the read, cannot be reached, or gives no time it reads, at the first read;
// one whose every read fails (a 5xx), after 3 reads (the ruled count, not
// composeAbortSinceReads: a change of the count must fail here). The up
// starts, and its abort is seen and retried. Each of the run's two up
// attempts (the aborted one and its retry) reads the time.
func TestAnAbortWatchOnADaemonThatWillNotSayItsTime(t *testing.T) {
	refused := errors.New("Error response from daemon: not served")
	empty, malformed := "", "Thu Oct 30 15:25:00 IST 2026"
	for _, tc := range []struct {
		name  string
		err   error
		time  *string
		perUp int
	}{
		{name: "forbidden (a socket proxy's rule)", err: errdefs.Forbidden(refused), perUp: 1},
		{name: "unauthorized", err: errdefs.Unauthorized(refused), perUp: 1},
		{name: "not found", err: errdefs.NotFound(refused), perUp: 1},
		{name: "not implemented", err: errdefs.NotImplemented(refused), perUp: 1},
		{name: "an invalid parameter", err: errdefs.InvalidParameter(refused), perUp: 1},
		{name: "an empty time", time: &empty, perUp: 1},
		{name: "a time keploy does not read", time: &malformed, perUp: 1},
		{name: "every read fails (a 500)", err: errdefs.System(errors.New("Error response from daemon: internal error")), perUp: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newAbortDaemon("tmpproj")
			fd := &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test", infoErr: tc.err, infoTime: tc.time}
			playAbortedUp(t, d, abortedUp{dc: fd})
			fd.fmu.Lock()
			reads := fd.infoReads
			fd.fmu.Unlock()
			if want := 2 * tc.perUp; reads != want {
				t.Fatalf("the two ups read the daemon's time %d times, want %d (%d each)", reads, want, tc.perUp)
			}
		})
	}
}

// TestAWatchOnKeploysClockSaysSoOncePerUp: a watch that takes keploy's clock
// says so in one Warn per up attempt, with how many reads of the daemon's
// time it made: that line is all a user is told of why the watch is on
// keploy's clock (the daemon refuses its time, as a socket proxy's rule does;
// it gives no time; every read fails). A daemon that says its time gets no
// Warn, and one keploy cannot reach gets another (see
// TestAnAbortWatchOnADaemonKeployCannotReachLetsTheUpStart). A daemon whose /info never answers while its events
// still flow, so that every read of its time runs past its cap, is read 3
// times (caps of 2, 4 and 8 s), not for ever with a cap that doubles: the up
// starts, on keploy's clock.
func TestAWatchOnKeploysClockSaysSoOncePerUp(t *testing.T) {
	const warning = "keploy could not read the docker daemon's time"
	empty := ""
	for _, tc := range []struct {
		name  string
		err   error
		time  *string
		hangs bool
		// perUp is how often each up attempt reads the daemon's time.
		perUp int
		// clock: the up attempt takes keploy's clock.
		clock bool
		// oneUp: one up attempt, not two (each of the hung daemon's takes
		// about 14 s, and a second shows nothing the 500's does not).
		oneUp bool
	}{
		{name: "the daemon says its time", perUp: 1},
		{name: "forbidden (a socket proxy's rule)", err: errdefs.Forbidden(errors.New("Error response from daemon: not served")), perUp: 1, clock: true},
		{name: "an empty time", time: &empty, perUp: 1, clock: true},
		{name: "every read fails (a 500)", err: errdefs.System(errors.New("Error response from daemon: internal error")), perUp: 3, clock: true},
		{name: "every read runs past its cap (its /info hangs)", hangs: true, perUp: 3, clock: true, oneUp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			fd := &flakyDaemon{abortDaemon: newAbortDaemon("tmpproj"), agent: "keploy-v3-test",
				infoErr: tc.err, infoTime: tc.time, infoHangs: tc.hangs}
			a := &App{logger: zap.New(core), docker: fd, kind: utils.DockerCompose,
				keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
			ups := 2
			if tc.oneUp {
				ups = 1
			}
			// Each up attempt (an up and its retry) watches on its own: it
			// reads the time, and says it took keploy's clock, again.
			for up := 1; up <= ups; up++ {
				watchAnUp(t, a)
				fd.fmu.Lock()
				reads, capped := fd.infoReads, fd.infoCapped
				fd.fmu.Unlock()
				if want := up * tc.perUp; reads != want {
					t.Fatalf("after up %d the daemon's time was read %d times, want %d (%d each up)", up, reads, want, tc.perUp)
				}
				if tc.hangs && capped != reads {
					t.Fatalf("%d of the %d reads of the hung daemon's time ended on their cap, want all: the row does not test reads past their cap", capped, reads)
				}
				warns := 0
				if tc.clock {
					warns = up
				}
				said := logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet(warning).All()
				if len(said) != warns || logs.Len() != warns {
					t.Fatalf("after up %d keploy said it took its clock in %d Warn lines (%d lines at Warn or above), want %d, one each up that takes keploy's clock: %v", up, len(said), logs.Len(), warns, logs.All())
				}
				if warns > 0 {
					if got := said[warns-1].ContextMap()["reads"]; got != int64(tc.perUp) {
						t.Fatalf("up %d's Warn says %v reads of the daemon's time, want %d", up, got, tc.perUp)
					}
				}
			}
		})
	}
}

// watchAnUp is one up attempt's watch, as run() makes it before the up
// starts and stops it once the up has returned. The up starts once the watch
// has returned: it fails the test when that is not within a minute, or when
// the watch ended the up it let start.
func watchAnUp(t *testing.T, a *App) {
	t.Helper()
	upCtx, stop := startWatch(t, a)
	defer stop()
	if err := upCtx.Err(); err != nil {
		t.Fatalf("the watch ended the up it let start: %v", context.Cause(upCtx))
	}
}

// startWatch makes one up attempt's watch, as run() does before the up
// starts, and fails the test when it does not return within a minute. The
// test calls stop once its up has returned (the test's cleanup does too).
func startWatch(t *testing.T, a *App) (upCtx context.Context, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	type watch struct {
		upCtx context.Context
		stop  func()
	}
	watched := make(chan watch, 1)
	go func() {
		upCtx, stop := a.watchComposeAbort(ctx)
		watched <- watch{upCtx, stop}
	}()
	select {
	case w := <-watched:
		t.Cleanup(w.stop)
		return w.upCtx, w.stop
	case <-time.After(time.Minute):
		t.Fatal("the watch did not return within a minute: the up waits for the daemon's time")
	}
	return nil, nil
}

// realConnectionFailure is the error a real Engine API client gives when
// nothing serves its DOCKER_HOST (here a socket that does not exist): the
// error the watch reads as keploy's client cannot connect.
func realConnectionFailure(t *testing.T) error {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.WithHost("unix://" + filepath.Join(t.TempDir(), "docker.sock")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = cli.Info(ctx)
	if !client.IsErrConnectionFailed(err) {
		t.Fatalf("a client at a socket that does not exist gave %v, not a connection failure", err)
	}
	return err
}

// realDroppedConnection is the error a real Engine API client gives when its
// DOCKER_HOST accepts the connection and drops it before any answer: a
// daemon, or a socket proxy in front of it, that keploy reaches. The client
// calls that a connection failure too.
func realDroppedConnection(t *testing.T) error {
	t.Helper()
	err := realClientInfo(t, client.WithHost("unix://"+droppingDaemon(t, nil)))
	if !client.IsErrConnectionFailed(err) {
		t.Fatalf("a client at a socket that drops each connection gave %v, not a connection failure", err)
	}
	return err
}

// realClientInfo reads the daemon's time with a real Engine API client made
// with opts, and gives the read's error.
func realClientInfo(t *testing.T, opts ...client.Opt) error {
	t.Helper()
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = cli.Info(ctx)
	if err == nil {
		t.Fatal("the read of the daemon's time succeeded")
	}
	return err
}

// droppingDaemon serves a unix socket that accepts each connection, reads
// the request's head, writes answer (if any) and closes the connection: with
// no answer, a daemon (or a socket proxy in front of it) that keploy's
// client reaches and that drops each ask before any answer. accepted, if
// not nil, counts the connections it accepted.
func droppingDaemon(t *testing.T, accepted *atomic.Int32, answer ...byte) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	serveAndDrop(t, l, func(c net.Conn) {
		if accepted != nil {
			accepted.Add(1)
		}
		if len(answer) > 0 {
			_, _ = c.Write(answer)
		}
	})
	return sock
}

// serveAndDrop accepts each connection of l until the test ends, reads its
// request's head, runs drop on it and closes it.
func serveAndDrop(t *testing.T, l net.Listener, drop func(net.Conn)) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			var head []byte
			buf := make([]byte, 4096)
			for !strings.Contains(string(head), "\r\n\r\n") {
				n, err := c.Read(buf)
				head = append(head, buf[:n]...)
				if err != nil {
					break
				}
			}
			drop(c)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-done
	})
}

// TestOnlyAFailedDialCountsTowardsAnUnreachableDaemon: with a real Engine
// API client, the failures composeAbortCannotDial counts towards the watch's
// end (composeAbortUnreachableAsks of them in a row) are the client's failed
// dials: no socket, nobody listening, one keploy's user may not open, a
// host or an HTTP proxy that does not resolve, a Windows named pipe that
// does not exist (go-winio's dial error). The client calls a connection the
// daemon accepted and dropped or reset before any answer, or shut before it
// read the ask (on Linux the client's write of it fails), or answered with
// what is no HTTP, a connection failure too: none of those count, that
// daemon is there and the watch stays on. Nor does a dial the read's cap
// ended, which the client does not call a connection failure.
func TestOnlyAFailedDialCountsTowardsAnUnreachableDaemon(t *testing.T) {
	failedDial := func(err error) client.Opt {
		return client.WithDialContext(func(context.Context, string, string) (net.Conn, error) { return nil, err })
	}
	notFound := func(name string) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}}
	}
	for _, tc := range []struct {
		name string
		unix bool   // the case serves a unix socket
		op   string // the op of a *net.OpError the client's error carries, if it must carry one
		opts func(t *testing.T) []client.Opt
		want bool
	}{
		{name: "no socket", unix: true, want: true, opts: func(t *testing.T) []client.Opt {
			return []client.Opt{client.WithHost("unix://" + filepath.Join(t.TempDir(), "docker.sock"))}
		}},
		{name: "a socket nobody listens on", unix: true, want: true, opts: func(t *testing.T) []client.Opt {
			p := filepath.Join(t.TempDir(), "docker.sock")
			l, err := net.Listen("unix", p)
			if err != nil {
				t.Fatal(err)
			}
			l.(*net.UnixListener).SetUnlinkOnClose(false)
			_ = l.Close()
			return []client.Opt{client.WithHost("unix://" + p)}
		}},
		{name: "a socket keploy's user may not open", unix: true, want: true, opts: func(t *testing.T) []client.Opt {
			if os.Geteuid() == 0 {
				t.Skip("root opens any socket")
			}
			p := filepath.Join(t.TempDir(), "docker.sock")
			l, err := net.Listen("unix", p)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			return []client.Opt{client.WithHost("unix://" + p)}
		}},
		{name: "a host that does not resolve", want: true, opts: func(*testing.T) []client.Opt {
			return []client.Opt{client.WithHost("tcp://daemon.invalid:2375"), failedDial(notFound("daemon.invalid"))}
		}},
		{name: "an HTTP proxy that does not resolve", want: true, opts: func(*testing.T) []client.Opt {
			return []client.Opt{client.WithHost("tcp://daemon.invalid:2375"), client.WithHTTPClient(&http.Client{Transport: &http.Transport{
				Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "proxy.invalid:3128"}),
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, notFound("proxy.invalid")
				},
			}})}
		}},
		{name: "a Windows named pipe that does not exist", want: true, opts: func(*testing.T) []client.Opt {
			// go-winio's DialPipeContext, the client's dial of an npipe://
			// host, fails so (tryDialPipe).
			return []client.Opt{client.WithHost("tcp://127.0.0.1:2375"), failedDial(&os.PathError{Op: "open", Path: "//./pipe/docker_engine",
				Err: errors.New("The system cannot find the file specified.")})}
		}},
		{name: "a daemon that drops the connection before any answer", unix: true, opts: func(t *testing.T) []client.Opt {
			return []client.Opt{client.WithHost("unix://" + droppingDaemon(t, nil))}
		}},
		{name: "a daemon that will not read the ask", unix: true, op: "write", opts: func(t *testing.T) []client.Opt {
			if runtime.GOOS != "linux" {
				// Only Linux tells the peer of a SHUT_RD: on darwin and
				// BSD the write succeeds and the client waits for an
				// answer.
				t.Skip("only Linux fails a write to a peer that shut its read side")
			}
			// A daemon, or a socket proxy that turns a peer away, that
			// shuts its side of the connection before it reads the ask:
			// the client's write of the ask fails (broken pipe). The dial
			// waits for that shutdown, so the write always comes after
			// it, and the connection stays open until the test ends, so
			// the client's read of the answer ends no sooner.
			sock := filepath.Join(t.TempDir(), "docker.sock")
			l, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			shut := make(chan struct{}, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				var held []net.Conn
				defer func() {
					for _, c := range held {
						_ = c.Close()
					}
				}()
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					held = append(held, c)
					_ = c.(*net.UnixConn).CloseRead()
					select {
					case shut <- struct{}{}:
					default:
					}
				}
			}()
			t.Cleanup(func() {
				_ = l.Close()
				<-done
			})
			return []client.Opt{client.WithHost("unix://" + sock), client.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
				if err != nil {
					return nil, err
				}
				select {
				case <-shut:
					return c, nil
				case <-ctx.Done():
					_ = c.Close()
					return nil, ctx.Err()
				}
			})}
		}},
		{name: "a daemon that resets the connection before any answer", op: "read", opts: func(t *testing.T) []client.Opt {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serveAndDrop(t, l, func(c net.Conn) { _ = c.(*net.TCPConn).SetLinger(0) })
			return []client.Opt{client.WithHost("tcp://" + l.Addr().String())}
		}},
		{name: "a daemon that answers what is no HTTP (TLS to a plain client)", unix: true, opts: func(t *testing.T) []client.Opt {
			return []client.Opt{client.WithHost("unix://" + droppingDaemon(t, nil, []byte("\x15\x03\x01\x00\x02\x02\x28\r\n")...))}
		}},
		{name: "a dial the read's cap ended", opts: func(*testing.T) []client.Opt {
			return []client.Opt{client.WithHost("tcp://127.0.0.1:2375"), client.WithDialContext(func(context.Context, string, string) (net.Conn, error) {
				// The dial net gives once the read's context passed its
				// deadline.
				ended, cancel := context.WithDeadline(context.Background(), time.Now())
				defer cancel()
				return (&net.Dialer{}).DialContext(ended, "tcp", "127.0.0.1:9")
			})}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unix && runtime.GOOS == "windows" {
				t.Skip("a unix socket DOCKER_HOST")
			}
			err := realClientInfo(t, tc.opts(t)...)
			if tc.op != "" {
				var ops []string
				var op *net.OpError
				for e := err; errors.As(e, &op); e = op.Err {
					ops = append(ops, op.Op)
				}
				if !slices.Contains(ops, tc.op) {
					t.Fatalf("the client gave %v (ops %q), with no *net.OpError of op %q", err, ops, tc.op)
				}
			}
			if got := composeAbortCannotDial(err); got != tc.want {
				t.Fatalf("composeAbortCannotDial(%v) = %v, want %v (client.IsErrConnectionFailed: %v)", err, got, tc.want, client.IsErrConnectionFailed(err))
			}
		})
	}
}

// TestAnAbortWatchOnADaemonKeployCannotReachLetsTheUpStart: keploy's Engine
// API client is client.FromEnv (DOCKER_HOST, else the default socket), while
// the compose CLI keploy runs reads its own context, or runs through sudo:
// keploy may not reach the daemon the up uses (Docker Desktop for Linux,
// rootless, colima; a user not in the docker group). With a real client at
// such a DOCKER_HOST, each up attempt's watch reads the daemon's time 3 times
// (composeAbortUnreachableAsks), says once that it does not watch the up,
// and returns, so the up starts, unwatched as before the watch: it never
// asks that daemon for its events. It read the daemon's time for ever once
// (the up never started), and then asked for its events every 200 ms for the
// whole up, a Debug line each.
func TestAnAbortWatchOnADaemonKeployCannotReachLetsTheUpStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		sock func(t *testing.T) string
	}{
		{name: "no socket at DOCKER_HOST", sock: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "docker.sock")
		}},
		{name: "a socket nobody listens on", sock: func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "docker.sock")
			l, err := net.Listen("unix", p)
			if err != nil {
				t.Fatal(err)
			}
			l.(*net.UnixListener).SetUnlinkOnClose(false)
			_ = l.Close()
			return p
		}},
		{name: "a socket keploy's user may not open", sock: func(t *testing.T) string {
			if os.Geteuid() == 0 {
				t.Skip("root opens any socket")
			}
			p := filepath.Join(t.TempDir(), "docker.sock")
			l, err := net.Listen("unix", p)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("a unix socket DOCKER_HOST")
			}
			t.Setenv("DOCKER_HOST", "unix://"+tc.sock(t))
			dc, err := docker.New(zap.NewNop(), &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			cd := &infoCountingDaemon{Client: dc}
			core, logs := observer.New(zap.WarnLevel)
			a := &App{logger: zap.New(core), docker: cd, kind: utils.DockerCompose,
				keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
			// Two up attempts (an up and its retry), each running for ten of
			// the watch's paces before it returns: a watch that asked for
			// the events of a daemon it cannot reach would ask in that time.
			for up := 1; up <= 2; up++ {
				upCtx, stop := startWatch(t, a)
				time.Sleep(10 * composeAbortRetryPace)
				if err := upCtx.Err(); err != nil {
					t.Fatalf("the watch ended up %d: %v", up, context.Cause(upCtx))
				}
				stop()
				if n, want := cd.reads.Load(), int32(up*3); n != want {
					t.Fatalf("after up %d the watch read the time of a daemon it cannot reach %d times, want %d (3 each up)", up, n, want)
				}
				if n := cd.streams.Load(); n != 0 {
					t.Fatalf("after up %d the watch asked a daemon it cannot reach for its events %d times, want none", up, n)
				}
				said := logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet(unreachableWarning).All()
				if len(said) != up || logs.Len() != up {
					t.Fatalf("after up %d keploy said it cannot reach the daemon in %d Warn lines (%d lines at Warn or above), want %d, one each up: %v", up, len(said), logs.Len(), up, logs.All())
				}
				if got := said[up-1].ContextMap()["asks"]; got != int64(3) {
					t.Fatalf("up %d's Warn says %v asks, want 3", up, got)
				}
			}
		})
	}
}

// TestAWatchOnADaemonThatDropsItsConnectionsStaysOn: keploy's real Engine
// API client reaches its DOCKER_HOST, and the daemon there (or a socket
// proxy in front of it) accepts each connection and drops it before any
// answer. The client calls that a connection failure, but the daemon is
// there: the watch reads its time 3 times (composeAbortSinceReads), takes
// keploy's clock, and stays on for the whole up, asking for its events
// again at a pace, with no word of a daemon it cannot reach. A watch that
// counted those asks as a daemon it cannot reach would stop after 3 and
// leave an aborted up waiting for ever again.
func TestAWatchOnADaemonThatDropsItsConnectionsStaysOn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a unix socket DOCKER_HOST")
	}
	var accepted atomic.Int32
	t.Setenv("DOCKER_HOST", "unix://"+droppingDaemon(t, &accepted))
	dc, err := docker.New(zap.NewNop(), &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cd := &infoCountingDaemon{Client: dc}
	core, logs := observer.New(zap.WarnLevel)
	a := &App{logger: zap.New(core), docker: cd, kind: utils.DockerCompose,
		keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
	upCtx, stop := startWatch(t, a)
	// The up runs for ten of the watch's paces: a watch that stopped would
	// stop within three asks.
	time.Sleep(10 * composeAbortRetryPace)
	if err := upCtx.Err(); err != nil {
		t.Fatalf("the watch ended the up: %v", context.Cause(upCtx))
	}
	stop()
	if n := cd.reads.Load(); n != 3 {
		t.Fatalf("the watch read the time of a daemon that drops its connections %d times, want 3", n)
	}
	if n := cd.streams.Load(); n <= 3 {
		t.Fatalf("the watch asked a daemon that drops its connections for its events %d times in ten paces, want more than 3: it stopped watching the up", n)
	}
	if accepted.Load() == 0 {
		t.Fatal("the daemon accepted no connection: the row does not test a daemon keploy reaches")
	}
	if said := logs.FilterMessageSnippet(unreachableWarning).Len(); said != 0 {
		t.Fatalf("keploy said %d times that it cannot reach a daemon that accepts its connections: %v", said, logs.All())
	}
	if said := logs.FilterMessageSnippet("keploy could not read the docker daemon's time").Len(); said != 1 || logs.Len() != 1 {
		t.Fatalf("keploy said it reads from its own clock in %d lines (%d lines at Warn or above), want one: %v", said, logs.Len(), logs.All())
	}
}

// unreachableWarning is the start of the line keploy says once an up's watch
// cannot reach the daemon (composeAbortLost).
const unreachableWarning = "keploy cannot reach the docker daemon"

// infoCountingDaemon counts its reads of the daemon's time, and the events
// streams asked of it.
type infoCountingDaemon struct {
	docker.Client
	reads, streams atomic.Int32
}

func (d *infoCountingDaemon) Info(ctx context.Context) (system.Info, error) {
	d.reads.Add(1)
	return d.Client.Info(ctx)
}

func (d *infoCountingDaemon) Events(ctx context.Context, o events.ListOptions) (<-chan events.Message, <-chan error) {
	d.streams.Add(1)
	return d.Client.Events(ctx, o)
}

// TestAWatchThatCannotConnectForItsEventsStops: the daemon said its time,
// then no ask for its events connects (the daemon went away: a restart,
// Docker Desktop's VM stopped). The watch asks composeAbortUnreachableAsks
// times in a row, says once that it does not watch the rest of the up, and
// stops, leaving the up to run. It asked again every 200 ms for the whole up
// before.
func TestAWatchThatCannotConnectForItsEventsStops(t *testing.T) {
	cannotConnect := realConnectionFailure(t)
	core, logs := observer.New(zap.WarnLevel)
	fd := &flakyDaemon{abortDaemon: newAbortDaemon("tmpproj"), agent: "keploy-v3-test",
		streamErrs: slices.Repeat([]error{cannotConnect}, 10)}
	a := &App{logger: zap.New(core), docker: fd, kind: utils.DockerCompose,
		keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
	upCtx, stop := startWatch(t, a)
	eventually(t, "the watch says it cannot reach the daemon", func() bool {
		return logs.FilterMessageSnippet(unreachableWarning).Len() > 0
	})
	// A watch that went on asking would ask again in this time.
	time.Sleep(10 * composeAbortRetryPace)
	if err := upCtx.Err(); err != nil {
		t.Fatalf("the watch ended the up: %v", context.Cause(upCtx))
	}
	stop()
	fd.fmu.Lock()
	streams := fd.streams
	fd.fmu.Unlock()
	if streams != composeAbortUnreachableAsks {
		t.Fatalf("the watch asked a daemon it cannot connect to for its events %d times, want %d", streams, composeAbortUnreachableAsks)
	}
	if said := logs.FilterMessageSnippet(unreachableWarning).Len(); said != 1 || logs.Len() != 1 {
		t.Fatalf("keploy said it cannot reach the daemon in %d lines (%d lines at Warn or above), want one: %v", said, logs.Len(), logs.All())
	}
}

// TestAWatchThatCannotConnectForADecisionsReadStops: the daemon's events
// came, then a read for the abort's decision cannot connect to the daemon:
// keploy's agent's inspect on the project's first die, or the project's list
// on the agent's die. The read asks composeAbortUnreachableAsks times in a
// row, and the watch says once that it does not watch the rest of the up and
// stops: a later die of the project (another service's) is not read, and the
// up is left to run. The read asked again every 200 ms for the whole up
// before.
func TestAWatchThatCannotConnectForADecisionsReadStops(t *testing.T) {
	cannotConnect := realConnectionFailure(t)
	for _, tc := range []struct {
		name string
		// list: the project's lists cannot connect from the agent's die on;
		// else the agent's inspects, from the dependency's die on.
		list bool
	}{
		{name: "keploy's agent's inspect on the project's first die"},
		{name: "the project's list on the agent's die", list: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			// The daemon lists newest first: the project's read on the
			// dependency's die stops at the agent, which runs.
			d := newAbortDaemon("tmpproj",
				&abortContainer{name: "tmpproj-cache-1", service: "cache", status: "running"},
				&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "running"},
				&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "running"},
				&abortContainer{name: "runner", service: "runner", status: "created"})
			fd := &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test"}
			a := &App{logger: zap.New(core), docker: fd, kind: utils.DockerCompose,
				keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
			upCtx, stop := startWatch(t, a)
			lost := slices.Repeat([]error{cannotConnect}, 10)
			counts := func() (agentAsks, lists, streams int) {
				fd.fmu.Lock()
				defer fd.fmu.Unlock()
				return fd.agentAsks, fd.lists, fd.streams
			}
			wantAgent, wantLists := composeAbortUnreachableAsks, 0
			if tc.list {
				d.exit("tmpproj-dep-1", 3)
				// That read's last ask: the agent's inspect, which finds it
				// running (the first was for its project).
				eventually(t, "the watch reads the project on the dependency's die", func() bool {
					agentAsks, lists, _ := counts()
					return agentAsks == 2 && lists == 1
				})
				fd.fmu.Lock()
				fd.listErrs = lost
				fd.fmu.Unlock()
				d.exit("keploy-v3-test", 0)
				wantAgent, wantLists = 2, 1+composeAbortUnreachableAsks
			} else {
				fd.fmu.Lock()
				fd.agentErrs = lost
				fd.fmu.Unlock()
				d.exit("tmpproj-dep-1", 3)
			}
			eventually(t, "the watch says it cannot reach the daemon", func() bool {
				return logs.FilterMessageSnippet(unreachableWarning).Len() > 0
			})
			// A later die of the project: a watch that went on would read
			// it.
			d.exit("tmpproj-cache-1", 0)
			// A watch that went on asking would ask again in this time.
			time.Sleep(10 * composeAbortRetryPace)
			if err := upCtx.Err(); err != nil {
				t.Fatalf("the watch ended the up: %v", context.Cause(upCtx))
			}
			stop()
			if agentAsks, lists, streams := counts(); agentAsks != wantAgent || lists != wantLists || streams != 1 {
				t.Fatalf("the watch inspected keploy's agent %d times, listed the project %d times and asked for %d events streams, want %d, %d and 1",
					agentAsks, lists, streams, wantAgent, wantLists)
			}
			if said := logs.FilterMessageSnippet(unreachableWarning).Len(); said != 1 || logs.Len() != 1 {
				t.Fatalf("keploy said it cannot reach the daemon in %d lines (%d lines at Warn or above), want one: %v", said, logs.Len(), logs.All())
			}
		})
	}
}

// TestAWatchEndedAsItsThirdConnectFailsSaysNothing: the run ends (its
// context) just as the third read of the daemon's time in a row fails to
// connect. That is the watch's end, not a daemon keploy lost: keploy says
// nothing of a daemon it cannot reach, for an up that is over, not
// unwatched.
func TestAWatchEndedAsItsThirdConnectFailsSaysNothing(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &endingUnreachableDaemon{abortDaemon: newAbortDaemon("tmpproj"), err: realConnectionFailure(t), end: cancel}
	a := &App{logger: zap.New(core), docker: d, kind: utils.DockerCompose,
		keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
	_, stop := a.watchComposeAbort(ctx)
	stop()
	if n := d.reads.Load(); n != composeAbortUnreachableAsks {
		t.Fatalf("the watch read the daemon's time %d times, want %d: the row does not end the run on the last", n, composeAbortUnreachableAsks)
	}
	if logs.Len() != 0 {
		t.Fatalf("keploy said %d lines at Warn or above for a watch that ended with its run, want none: %v", logs.Len(), logs.All())
	}
}

// endingUnreachableDaemon is a daemon keploy cannot connect to: each read
// of its time fails so, and the composeAbortUnreachableAsks-th ends the run
// (end) first.
type endingUnreachableDaemon struct {
	*abortDaemon
	err   error
	end   context.CancelFunc
	reads atomic.Int32
}

func (d *endingUnreachableDaemon) Info(context.Context) (system.Info, error) {
	if d.reads.Add(1) == composeAbortUnreachableAsks {
		d.end()
	}
	return system.Info{}, d.err
}

// TestAWatchEndedAsItsThirdEventsDialFailsSaysNothing: the run ends just as
// the third ask in a row for the daemon's events fails to open a
// connection. The watch takes either (its select picks at random), and
// either is the watch's end, not a daemon keploy lost: keploy says nothing
// of a daemon it cannot reach, in any of many ups, each of which takes the
// failure with even odds.
func TestAWatchEndedAsItsThirdEventsDialFailsSaysNothing(t *testing.T) {
	cannotConnect := realConnectionFailure(t)
	core, logs := observer.New(zap.WarnLevel)
	for up := 1; up <= 16; up++ {
		ctx, cancel := context.WithCancel(context.Background())
		d := &endingEventsDaemon{abortDaemon: newAbortDaemon("tmpproj"), err: cannotConnect, end: cancel}
		a := &App{logger: zap.New(core), docker: d, kind: utils.DockerCompose,
			keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
		_, stop := a.watchComposeAbort(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(time.Minute):
			t.Fatalf("up %d: the watch did not ask for the daemon's events %d times within a minute", up, composeAbortUnreachableAsks)
		}
		stop()
		cancel()
		if n := d.streams.Load(); n != composeAbortUnreachableAsks {
			t.Fatalf("up %d: the watch asked for the daemon's events %d times, want %d: the row does not end the run on the last", up, n, composeAbortUnreachableAsks)
		}
		if logs.Len() != 0 {
			t.Fatalf("up %d: keploy said %d lines at Warn or above for a watch that ended with its run, want none: %v", up, logs.Len(), logs.All())
		}
	}
}

// endingEventsDaemon says its time, and each ask for its events fails to
// open a connection; the composeAbortUnreachableAsks-th ends the run (end)
// before it fails.
type endingEventsDaemon struct {
	*abortDaemon
	err     error
	end     context.CancelFunc
	streams atomic.Int32
}

func (d *endingEventsDaemon) Events(context.Context, events.ListOptions) (<-chan events.Message, <-chan error) {
	if d.streams.Add(1) == composeAbortUnreachableAsks {
		d.end()
	}
	errs := make(chan error, 1)
	errs <- d.err
	return make(chan events.Message), errs
}

// TestAWatchStopsOnlyOnConnectionFailuresInARow: an ask whose connection
// the daemon accepted, even one it fails or drops before any answer (the
// real client's errors), starts the count of asks that cannot open a
// connection (composeAbortCannotDial) again: two failed dials, one the
// daemon fails or drops, and two more, or five failures in a row of a
// daemon keploy reaches, leave the watch on, and the abort is still seen.
// Each such ask comes where the watch must ask again: an events stream,
// keploy's agent's inspect on the project's first die, the project's list
// on the agent's die (the abort's last die).
func TestAWatchStopsOnlyOnConnectionFailuresInARow(t *testing.T) {
	cannotConnect := realConnectionFailure(t)
	dropped := realDroppedConnection(t)
	internal := errdefs.System(errors.New("Error response from daemon: internal error"))
	timeout := errors.New("Error response from daemon: i/o timeout")
	notInARow := []error{cannotConnect, cannotConnect, internal, cannotConnect, cannotConnect}
	droppedBetween := []error{cannotConnect, cannotConnect, dropped, cannotConnect, cannotConnect}
	reached := slices.Repeat([]error{timeout}, 5)
	for _, tc := range []struct {
		name                    string
		streams, agent, onAgent []error
	}{
		{name: "events streams: not in a row", streams: notInARow},
		{name: "keploy's agent's inspect: not in a row", agent: notInARow},
		{name: "the project's list on the agent's die: not in a row", onAgent: notInARow},
		{name: "events streams: a dropped connection between", streams: droppedBetween},
		{name: "keploy's agent's inspect: a dropped connection between", agent: droppedBetween},
		{name: "the project's list on the agent's die: a dropped connection between", onAgent: droppedBetween},
		{name: "events streams: five the daemon fails", streams: []error{internal, internal, internal, internal, internal}},
		{name: "events streams: five the daemon drops", streams: slices.Repeat([]error{dropped}, 5)},
		{name: "the project's list on the agent's die: five the daemon fails", onAgent: reached},
		{name: "keploy's agent's inspect: five the daemon drops", agent: slices.Repeat([]error{dropped}, 5)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newAbortDaemon("tmpproj")
			fd := &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test", streamErrs: tc.streams}
			playAbortedUp(t, d, abortedUp{
				dc: fd,
				beforeDep: func() {
					fd.fmu.Lock()
					fd.agentErrs = tc.agent
					fd.fmu.Unlock()
				},
				beforeAgent: func() {
					fd.fmu.Lock()
					fd.listErrs = tc.onAgent
					fd.fmu.Unlock()
				},
			})
			fd.fmu.Lock()
			left := len(fd.streamErrs) + len(fd.agentErrs) + len(fd.listErrs)
			fd.fmu.Unlock()
			if left != 0 {
				t.Fatalf("%d of the row's failures were never asked for: the row does not test them", left)
			}
		})
	}
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

// TestAnAgentReadThatKeepsFailingEndsWithTheUp: from the project's first
// die on, every inspect of keploy's agent's container fails (a socket proxy
// that forbids it), so the watch never learns its project, and compose's up
// returns on its own. That read ends with the watch, which ends with the up:
// run() returns, and keploy retries the bring-up.
func TestAnAgentReadThatKeepsFailingEndsWithTheUp(t *testing.T) {
	d := newAbortDaemon("tmpproj")
	fd := &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test"}
	r := startAbortedUpRun(t, fd)
	d.add(&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "running"},
		&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "running"},
		&abortContainer{name: "runner", service: "runner", status: "created"})
	fd.fmu.Lock()
	fd.agentFails = true
	fd.fmu.Unlock()
	d.exit("tmpproj-dep-1", 3)
	eventually(t, "the watch reads keploy's agent's container again", func() bool {
		fd.fmu.Lock()
		defer fd.fmu.Unlock()
		return fd.agentReads >= 2
	})
	d.exit("keploy-v3-test", 0)
	if err := os.WriteFile(r.ended, nil, 0o600); err != nil {
		t.Fatalf("could not end the stand-in up: %v", err)
	}
	select {
	case appErr := <-r.ran:
		r.ran <- appErr
	case <-time.After(time.Minute):
		t.Fatalf("run() did not return within a minute of the up's own return: the watch's read of keploy's agent outlives the up\n%s", recordedLog(t, r.argvLog))
	}
	if log := recordedLog(t, r.argvLog); !strings.Contains(log, "UP-RETRIED") {
		t.Fatalf("the bring-up was not retried\n%s", log)
	}
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

// TestComposeAbortedBeforeTheAppReadsTheWholeProject: an up is over before
// the app started only when nothing of the project runs, keploy's agent ran
// and stopped, and the app's container never started; not while a container
// still runs or restarts, nor while the agent was never started (compose may
// still start it, and the app after it), nor once the app's container ran.
// Nor for a scaled app service (scaled: a second container of it): keploy
// decides on the app's one container only, as its stop of the app does
// (compose_appstop.go), so a project with two is never read as that end.
// A one-off container (`docker compose run`) is in the project's list but
// not of the up: an exited one of the app's service (appRun: `docker compose
// run app` without --rm) is not a second app container, nor is a running
// one of another service (depRun) a container of the up that still runs.
// Read as the up's, either hid the abort, and keploy waited on the up.
func TestComposeAbortedBeforeTheAppReadsTheWholeProject(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		app, scaled, agent, dep string
		appRun, depRun          string
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
		{name: "all stopped, app never started, an exited one-off of the app", app: "created", appRun: "exited", agent: "exited", dep: "exited", want: true},
		{name: "all stopped, app never started, a one-off of the dependency runs", app: "created", depRun: "running", agent: "exited", dep: "exited", want: true},
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
			if tc.appRun != "" {
				cs = append(cs, &abortContainer{name: "p-app-run-0123456789ab", service: "app", status: tc.appRun, oneOff: true})
			}
			if tc.depRun != "" {
				cs = append(cs, &abortContainer{name: "p-dep-run-0123456789ab", service: "dep", status: tc.depRun, oneOff: true})
			}
			d := newAbortDaemon("p", cs...)
			a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose, keployContainer: "agent", container: "app", composeService: "app"}
			if got, err := a.composeAbortedBeforeTheApp(context.Background(), "p"); got != tc.want || err != nil {
				t.Fatalf("composeAbortedBeforeTheApp = %v, %v, want %v", got, err, tc.want)
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
	if aborted, err := a.composeAbortedBeforeTheApp(context.Background(), "p"); aborted || err != nil {
		t.Fatalf("compose's recreate of a running agent (the new agent created, the old one stopped) is read as the abort (%v, %v): keploy kills the retried up", aborted, err)
	}
}

// TestAContainerRemovedSinceTheListIsNotInTheAbortsDecision: the project's
// read lists its containers, then inspects each. One removed in between (the
// app's old container, which compose removes when it recreates the app)
// answers NotFound: it is gone, so the read decides on the others, and the
// abort is still seen, on that list. Read as "not the abort", the watch let
// that die go, and on the abort's last die no other die comes; read as a
// failed read, the decision waited for another list.
func TestAContainerRemovedSinceTheListIsNotInTheAbortsDecision(t *testing.T) {
	d := &removedSinceListDaemon{abortDaemon: newAbortDaemon("p",
		&abortContainer{name: "p-dep-1", service: "dep", status: "exited", exitCode: 3},
		&abortContainer{name: "agent", service: keployAgentComposeService, status: "exited"},
		&abortContainer{name: "app", service: "app", status: "created"},
	), removed: &abortContainer{name: "0123456789ab_p-app-1", service: "app", status: "exited"}}
	a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose, keployContainer: "agent", container: "app", composeService: "app"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if aborted, err := a.composeAbortedBeforeTheApp(ctx, "p"); !aborted || err != nil {
		t.Fatalf("a container removed between the project's list and its inspect is read as the project not aborted (%v, %v): keploy waits on the up", aborted, err)
	}
	if !d.listedRemoved.Load() {
		t.Fatal("the read never listed the removed container: the row does not test it")
	}
	lists := 0
	for _, e := range d.trailNow() {
		if e == "list" {
			lists++
		}
	}
	if lists != 1 {
		t.Fatalf("keploy listed the project %d times, want 1 (%v): a container removed since the list is read as a failed read, and the decision waits for another list", lists, d.trailNow())
	}
}

// TestTheAbortWatchPassesByADieBeforeKeploysAgentExists: compose builds or
// pulls the project's images before it makes any container, which can take
// minutes, and another project's container dies meanwhile. keploy's agent's
// container does not exist yet, so that die is not of keploy's project: the
// watch reads the agent's container once on it, passes it by, and reads the
// project again on the next die; the project's abort is then seen. Read as a
// failed read, the missing agent kept the watch asking the daemon for it
// every 200 ms until compose made it.
func TestTheAbortWatchPassesByADieBeforeKeploysAgentExists(t *testing.T) {
	d := &agentInspectCountingDaemon{abortDaemon: newAbortDaemon("tmpproj"), agent: "keploy-v3-test"}
	a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose,
		keployContainer: "keploy-v3-test", container: "runner", composeService: "runner"}
	upCtx, stop := a.watchComposeAbort(context.Background())
	defer stop()
	d.add(&abortContainer{name: "other-db-1", service: "db", status: "running", project: "other"})
	d.exit("other-db-1", 1)
	eventually(t, "the watch reads keploy's agent's container on the other project's die", func() bool {
		return d.agentInspects.Load() > 0
	})
	// No die comes for a second: nothing asks for the agent again.
	time.Sleep(time.Second)
	if n := d.agentInspects.Load(); n != 1 {
		t.Fatalf("keploy read its agent's container %d times on one die before compose made it, want 1: the missing agent is read as a failed read and asked for again", n)
	}
	d.add(
		&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "running"},
		&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "running"},
		&abortContainer{name: "runner", service: "runner", status: "created"})
	d.exit("tmpproj-dep-1", 3)
	d.exit("keploy-v3-test", 0)
	select {
	case <-upCtx.Done():
	case <-time.After(time.Minute):
		t.Fatalf("the watch did not end the up within a minute of its project's abort after another project's die before compose made keploy's agent (daemon %v)", d.trailNow())
	}
	if cause := context.Cause(upCtx); !errors.Is(cause, errComposeUpAborted) {
		t.Fatalf("the up ended with %v, not keploy's end of the abort", cause)
	}
}

// agentInspectCountingDaemon counts the inspects of keploy's agent's
// container (agent), made or not.
type agentInspectCountingDaemon struct {
	*abortDaemon
	agent         string
	agentInspects atomic.Int32
}

func (d *agentInspectCountingDaemon) ContainerInspect(ctx context.Context, ref string) (container.InspectResponse, error) {
	if ref == d.agent {
		d.agentInspects.Add(1)
	}
	return d.abortDaemon.ContainerInspect(ctx, ref)
}

// removedSinceListDaemon lists removed, a container it no longer has, in its
// first list of the project only, as the daemon does for a container removed
// between that list and its inspect.
type removedSinceListDaemon struct {
	*abortDaemon
	removed       *abortContainer
	listedRemoved atomic.Bool
}

func (d *removedSinceListDaemon) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	out, err := d.abortDaemon.ContainerList(ctx, o)
	if err != nil || !o.Filters.ExactMatch("label", composeProjectLabelKey+"="+d.project) || !d.listedRemoved.CompareAndSwap(false, true) {
		return out, err
	}
	return append(out, container.Summary{ID: d.removed.name, Names: []string{"/" + d.removed.name}, Labels: d.labels(d.removed)}), nil
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
			if aborted, err := a.composeAbortEnds(context.Background(), "p", tc.m); aborted != tc.aborted || err != nil {
				t.Fatalf("composeAbortEnds = %v, %v, want %v", aborted, err, tc.aborted)
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
