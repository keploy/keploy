package app

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// stoppedAgentDocker is the daemon as keploy finds it once compose has stopped
// the agent: the agent's container is still there, stopped, holding the files
// it wrote. files maps container -> path -> content; a missing entry is a
// missing file.
type stoppedAgentDocker struct {
	docker.Client
	mu    sync.Mutex
	files map[string]map[string][]byte
	// states maps container -> how it ended; a missing entry is a container
	// docker no longer knows. removed, when set, says the teardown has run
	// and none of them exists any more.
	states  map[string]*container.State
	removed func() bool
	// onCopy runs as each CopyFromContainer is answered, so a test can see
	// what else had already happened by then.
	onCopy func()
	copied []string
}

func (d *stoppedAgentDocker) CopyFromContainer(_ context.Context, name, path string) (io.ReadCloser, container.PathStat, error) {
	d.mu.Lock()
	d.copied = append(d.copied, name+":"+path)
	d.mu.Unlock()
	if d.onCopy != nil {
		d.onCopy()
	}
	body, ok := d.files[name][path]
	if !ok {
		return nil, container.PathStat{}, errdefs.NotFound(errors.New("Could not find the file " + path + " in container " + name))
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: path[strings.LastIndex(path, "/")+1:], Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	_ = tw.Close()
	return io.NopCloser(&buf), container.PathStat{Name: path, Size: int64(len(body))}, nil
}

// A container with no state is gone -- which is also what ends the teardown's
// reap barrier at once.
// ContainerRemove answers the teardown's force-remove. The teardown reaches the
// daemon through the Engine API rather than a `docker rm -f` subprocess, so the
// fake has to implement it or ComposeDown nil-derefs the embedded interface.
// Removal is a no-op here: `removed` is what this fixture uses to say the
// containers are gone.
func (d *stoppedAgentDocker) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	return nil
}

// ContainerList answers the container-name availability probe, for the same
// reason. An empty list means "the name is free", which is what a torn-down
// project should look like.
func (d *stoppedAgentDocker) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	return nil, nil
}

func (d *stoppedAgentDocker) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	st, ok := d.states[name]
	if !ok || (d.removed != nil && d.removed()) {
		return container.InspectResponse{}, errors.New("no such container")
	}
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: st}}, nil
}

func (d *stoppedAgentDocker) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return nil, errors.New("no such container")
}

func composeReplayApp(d docker.Client) *App {
	return &App{
		logger:          zap.NewNop(),
		docker:          d,
		kind:            utils.DockerCompose,
		cmd:             "docker compose -f /tmp/keploy-compose.yaml up",
		composeFile:     "/tmp/keploy-compose.yaml",
		keployContainer: "keploy-v3-test",
		container:       "runner",
		composeServices: []string{"runner", "keploy-agent"},
		opts:            models.SetupOptions{MockMode: true, Mode: models.MODE_TEST},
	}
}

// TestRunReadsTheStoppedAgentBeforeTheTeardown pins the ORDER that makes a
// compose replay provable at all.
//
// Compose stops the agent service the moment the app exits, so what the agent
// served and missed exists only as the file it left in its stopped container
// -- and ComposeDown removes that container. Read after the teardown, the file
// is gone on every run, and every compose replay reports its outcome as
// unknown: consumed -1, missed -1, never isolated, and --strict failing a
// clean run.
func TestRunReadsTheStoppedAgentBeforeTheTeardown(t *testing.T) {
	argvLog := stubDockerCLI(t, "", 0)
	outcome := []byte(`{"consumed":[{"name":"mock-1"}],"missed":[]}`)
	d := &stoppedAgentDocker{files: map[string]map[string][]byte{
		"keploy-v3-test": {docker.AgentOutcomeFile: outcome},
	}, states: map[string]*container.State{
		"keploy-v3-test": ended("exited", 0, "2026-09-25T12:00:00Z", "2026-09-25T12:00:10Z"),
	}}
	d.removed = func() bool { return strings.Contains(recordedLog(t, argvLog), "ARG down") }
	tornDownFirst := false
	d.onCopy = func() {
		tornDownFirst = tornDownFirst || strings.Contains(recordedLog(t, argvLog), "ARG down")
	}
	a := composeReplayApp(d)

	if appErr := a.run(context.Background()); appErr.AppErrorType != models.ErrAppStopped {
		t.Fatalf("the stand-in compose project exited 0, run() reported %+v", appErr)
	}

	if tornDownFirst {
		t.Fatalf("the agent's container was read AFTER `docker compose down` removed it:\n%s", recordedLog(t, argvLog))
	}
	if !strings.Contains(recordedLog(t, argvLog), "ARG down") {
		t.Fatalf("run() never tore the project down:\n%s", recordedLog(t, argvLog))
	}
	got, ok := a.StoppedAgent()
	if !ok {
		t.Fatalf("run() never read the stopped agent (copied %v)", d.copied)
	}
	if got.OutcomeErr != nil || string(got.Outcome) != string(outcome) {
		t.Fatalf("read back %q, %v; want the file the agent left, %q", got.Outcome, got.OutcomeErr, outcome)
	}
}

// In a run keploy saw its agent ready in, the agent died and stopped the
// project when it crashed (not compose's own stop of it: 0, 143, or 137
// without running out of memory) and either compose never started the app
// (its container never started) or the app was killed by a signal (137,
// 143), the app's container saying so or, when docker could not say, the
// app's exit as keploy got it. An app that ended with a code of its own, or
// for running out of its own memory, keeps its end even when the agent then
// crashed; and an unknown app is never read as one that never started.
func TestAgentDiedAfterReady(t *testing.T) {
	agent := func(code int) *container.State { return &container.State{Status: "exited", ExitCode: code} }
	app := func(code int, oom bool) *container.State {
		return &container.State{Status: "exited", ExitCode: code, OOMKilled: oom, StartedAt: "2026-10-14T10:00:00Z"}
	}
	for _, tc := range []struct {
		name    string
		s       StoppedAgent
		appExit int
		want    bool
	}{
		{"the agent crashed and took the app (137)", StoppedAgent{Agent: agent(139), App: app(137, false)}, 137, true},
		{"the agent crashed and compose stopped the app (143)", StoppedAgent{Agent: agent(1), App: app(143, false)}, 143, true},
		{"the agent ran out of memory and took the app", StoppedAgent{Agent: &container.State{Status: "exited", ExitCode: 137, OOMKilled: true}, App: app(137, false)}, 137, true},
		{"docker could not say how the app ended: its 143 decides", StoppedAgent{Agent: agent(2)}, 143, true},
		{"docker could not say how the app ended: its own 0 stands", StoppedAgent{Agent: agent(2)}, 0, false},
		{"docker could not say how the app ended: its own 42 stands", StoppedAgent{Agent: agent(2)}, 42, false},
		{"the app's own 42, then a slow agent killed", StoppedAgent{Agent: agent(137), App: app(42, false)}, 42, false},
		// The agent crashed after the app ended with a code of its own: the
		// app's end, as docker reports it, stands, whatever keploy got.
		{"the app's own 42, then the agent crashed", StoppedAgent{Agent: agent(2), App: app(42, false)}, 42, false},
		{"the app's own 0, then the agent ran out of memory", StoppedAgent{Agent: &container.State{Status: "exited", ExitCode: 137, OOMKilled: true}, App: app(0, false)}, 0, false},
		{"docker says the app's own 42, keploy got 143", StoppedAgent{Agent: agent(2), App: app(42, false)}, 143, false},
		{"docker says the app was killed (143), keploy got 0", StoppedAgent{Agent: agent(2), App: app(143, false)}, 0, true},
		{"the app ran out of its own memory", StoppedAgent{Agent: agent(2), App: app(137, true)}, 137, false},
		{"the agent exited 0", StoppedAgent{Agent: agent(0), App: app(143, false)}, 143, false},
		{"nothing read of the agent", StoppedAgent{App: app(143, false)}, 143, false},
		// Compose's own stop of the agent (143, or 137 after its grace) is not
		// the agent's failure: the user stopped the project, or another
		// service of it failed.
		{"the user stopped the project: app 143, a slow agent killed (137)", StoppedAgent{Agent: agent(137), App: app(143, false)}, 143, false},
		{"the user stopped the project: app 143, agent 143", StoppedAgent{Agent: agent(143), App: app(143, false)}, 143, false},
		// The agent crashed after keploy saw it ready, before compose
		// started the app: the app's container has no exit of its own.
		{"compose never started the app (compose says 0)", StoppedAgent{Agent: agent(2), App: &container.State{Status: "created", StartedAt: "0001-01-01T00:00:00Z"}}, 0, true},
		{"compose never started the app (compose says 1)", StoppedAgent{Agent: agent(2), App: &container.State{Status: "created", StartedAt: "0001-01-01T00:00:00Z"}}, 1, true},
		{"compose never started the app, and the agent exited 0", StoppedAgent{Agent: agent(0), App: &container.State{Status: "created", StartedAt: "0001-01-01T00:00:00Z"}}, 0, false},
		// A dependency crashed, and compose stopped the agent over it (137,
		// or 143): the app never started, and it is not the agent's failure.
		{"a dependency crashed; compose killed the agent (137)", StoppedAgent{Agent: agent(137), App: &container.State{Status: "created", StartedAt: "0001-01-01T00:00:00Z"}}, 1, false},
		{"a dependency crashed; compose stopped the agent (143)", StoppedAgent{Agent: agent(143), App: &container.State{Status: "created", StartedAt: "0001-01-01T00:00:00Z"}}, 1, false},
	} {
		if got := tc.s.AgentDiedAfterReady(tc.appExit); got != tc.want {
			t.Errorf("%s: AgentDiedAfterReady(%d) = %v, want %v", tc.name, tc.appExit, got, tc.want)
		}
	}
}

// An app whose container never started is one compose provisioned and
// stopped the project before starting; one that ran, or that docker could
// not report, is not.
func TestAppNeverStarted(t *testing.T) {
	if !(StoppedAgent{App: &container.State{Status: "created", StartedAt: "0001-01-01T00:00:00Z"}}).AppNeverStarted() {
		t.Error("a created, never-started app container is not read as never started")
	}
	if (StoppedAgent{App: &container.State{Status: "exited", ExitCode: 0, StartedAt: "2026-10-14T10:00:00Z"}}).AppNeverStarted() {
		t.Error("an app that ran is read as never started")
	}
	if (StoppedAgent{}).AppNeverStarted() {
		t.Error("an app docker could not report is read as never started")
	}
	// The state decides, not the start time: an app that ran, with a start
	// time keploy cannot read, did start.
	if (StoppedAgent{App: &container.State{Status: "exited", ExitCode: 0}}).AppNeverStarted() {
		t.Error("an exited app with no readable start time is read as never started")
	}
}

// A compose mock run and a compose recording read how the stopped agent and
// the app ended (a recording, to tell an agent that could not start from a
// project that failed on its own); only a mock replay has an outcome to read.
// keploy test must not go asking the daemon about a container it never looks
// at again, and nothing but a mock replay may go asking for a file its agent
// never writes.
func TestReadStoppedAgentOnlyForAComposeMockRunOrRecording(t *testing.T) {
	t.Run("mock record: how it ended, but no outcome", func(t *testing.T) {
		d := &stoppedAgentDocker{states: map[string]*container.State{"keploy-v3-test": {Status: "exited", ExitCode: 137}}}
		a := composeReplayApp(d)
		a.opts = models.SetupOptions{MockMode: true, Mode: models.MODE_RECORD}
		a.readStoppedAgent(context.Background())
		got, ok := a.StoppedAgent()
		if !ok || got.Agent == nil || got.Agent.ExitCode != 137 {
			t.Fatalf("a mock record did not read how its agent ended: %+v, %v", got, ok)
		}
		if len(d.copied) > 0 {
			t.Fatalf("a mock record asked for a replay outcome (copied %v)", d.copied)
		}
	})
	// keploy record reads how its agent ended, so that an agent that could
	// not start, and so stopped the project before the app ever ran, is told
	// from the project failing on its own (#4772); it has no outcome to read.
	t.Run("keploy record: how it ended, but no outcome", func(t *testing.T) {
		d := &stoppedAgentDocker{states: map[string]*container.State{
			"keploy-v3-test": {Status: "exited", ExitCode: 6},
			"runner":         {Status: "created", StartedAt: "0001-01-01T00:00:00Z"},
		}}
		a := composeReplayApp(d)
		a.opts = models.SetupOptions{Mode: models.MODE_RECORD}
		a.readStoppedAgent(context.Background())
		got, ok := a.StoppedAgent()
		if !ok || got.Agent == nil || got.Agent.ExitCode != 6 {
			t.Fatalf("a compose recording did not read how its agent ended: %+v, %v", got, ok)
		}
		// How the app ended is read too: without it an agent compose killed
		// non-zero after a project that failed on its own would be blamed.
		if got.App == nil || got.App.Status != "created" {
			t.Fatalf("a compose recording did not read how its app ended: %+v", got.App)
		}
		if !got.AgentFailed() || got.AppStarted() {
			t.Fatalf("an agent that exited 6 before the app ever started is not read as having stopped the project: %+v", got)
		}
		if len(d.copied) > 0 {
			t.Fatalf("a compose recording asked for a replay outcome (copied %v)", d.copied)
		}
	})
	t.Run("keploy test", func(t *testing.T) {
		d := &stoppedAgentDocker{}
		a := composeReplayApp(d)
		a.opts = models.SetupOptions{Mode: models.MODE_TEST}
		a.readStoppedAgent(context.Background())
		if _, ok := a.StoppedAgent(); ok || len(d.copied) > 0 {
			t.Fatalf("read the agent's container for a keploy test (copied %v)", d.copied)
		}
	})
	t.Run("docker run", func(t *testing.T) {
		d := &stoppedAgentDocker{}
		a := composeReplayApp(d)
		a.kind = utils.DockerRun
		a.readStoppedAgent(context.Background())
		if _, ok := a.StoppedAgent(); ok || len(d.copied) > 0 {
			t.Fatalf("read the agent's container for a docker run command (copied %v)", d.copied)
		}
	})
}

// An interrupted replay reports no outcome, and cmdCancel has already brought
// the project down: there is nothing to read, and nothing to spend the
// teardown's time asking for.
func TestReadStoppedAgentSkipsAnInterruptedRun(t *testing.T) {
	d := &stoppedAgentDocker{files: map[string]map[string][]byte{
		"keploy-v3-test": {docker.AgentOutcomeFile: []byte(`{}`)},
	}}
	a := composeReplayApp(d)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.readStoppedAgent(ctx)
	if _, ok := a.StoppedAgent(); ok || len(d.copied) > 0 {
		t.Fatalf("read the agent's container after an interrupt (copied %v)", d.copied)
	}
}

// An agent that left no file -- killed before it could write one, or an image
// older than this keploy -- is recorded as such, so the replay can say its
// outcome is unknown and why.
func TestReadStoppedAgentRecordsAMissingOutcome(t *testing.T) {
	d := &stoppedAgentDocker{states: map[string]*container.State{"keploy-v3-test": {Status: "exited", ExitCode: 137}}}
	a := composeReplayApp(d)
	a.readStoppedAgent(context.Background())
	got, ok := a.StoppedAgent()
	if !ok {
		t.Fatal("a stopped agent that left no file was not recorded as read")
	}
	if got.OutcomeErr == nil || !strings.Contains(got.OutcomeErr.Error(), "left no "+docker.AgentOutcomeFile) {
		t.Fatalf("OutcomeErr = %v, want one naming the missing file", got.OutcomeErr)
	}
}

// Docker answers "not found" for a container it does not know exactly as for
// a file a container lacks. A keploy-agent container missing from the daemon
// keploy asks -- the compose project ran on another one -- is not an agent
// that left no account, and saying so sent the user to the agent's image.
func TestReadStoppedAgentSaysWhenDockerDoesNotKnowTheAgent(t *testing.T) {
	d := &stoppedAgentDocker{}
	a := composeReplayApp(d)
	a.readStoppedAgent(context.Background())
	got, ok := a.StoppedAgent()
	if !ok {
		t.Fatal("an agent docker does not know was not recorded as read")
	}
	if got.OutcomeErr == nil || !strings.Contains(got.OutcomeErr.Error(), "could not find the keploy-agent container keploy-v3-test on the docker daemon it talks to") {
		t.Fatalf("OutcomeErr = %v, want one saying docker does not know the agent's container", got.OutcomeErr)
	}
	if len(d.copied) > 0 {
		t.Fatalf("asked for a file from a container docker does not know (copied %v)", d.copied)
	}
}

// MockOutcome is the account the agent left, decoded -- and an account keploy
// does not have is an error, never an empty one: "served nothing, missed
// nothing" passes --strict and meters a replay as zero.
func TestStoppedAgentMockOutcome(t *testing.T) {
	t.Run("the account the agent left", func(t *testing.T) {
		got, err := StoppedAgent{Outcome: []byte(`{"consumed":[{"name":"mock-1"}],"missed":[{"actual_summary":"GET /price/tsla"}]}`)}.MockOutcome()
		if err != nil || len(got.Consumed) != 1 || got.Consumed[0].Name != "mock-1" || len(got.Missed) != 1 || got.Missed[0].ActualSummary != "GET /price/tsla" {
			t.Fatalf("MockOutcome() = %+v, %v; want mock-1 served and GET /price/tsla missed", got, err)
		}
	})
	left := errors.New("the keploy-agent container keploy-v3-test left no " + docker.AgentOutcomeFile)
	for _, tc := range []struct {
		name    string
		stopped StoppedAgent
		wantErr error // nil: any error
	}{
		{name: "an agent that left nothing", stopped: StoppedAgent{OutcomeErr: left}, wantErr: left},
		{name: "a read that never happened", stopped: StoppedAgent{}},
		{name: "an account that is not one", stopped: StoppedAgent{Outcome: []byte(`{"consumed":`)}},
		{name: "an empty file", stopped: StoppedAgent{Outcome: []byte{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.stopped.MockOutcome()
			if err == nil {
				t.Fatalf("MockOutcome() = %+v, nil; want an error", got)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("MockOutcome() error %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestReadContainerFileRefusesWhatItCannotTrust(t *testing.T) {
	t.Run("over the limit", func(t *testing.T) {
		d := &stoppedAgentDocker{files: map[string]map[string][]byte{"c": {"/f": []byte("0123456789")}}}
		if _, err := readContainerFile(context.Background(), d, "c", "/f", 9); err == nil {
			t.Fatal("read a 10-byte file under a 9-byte limit")
		}
		body, err := readContainerFile(context.Background(), d, "c", "/f", 10)
		if err != nil || string(body) != "0123456789" {
			t.Fatalf("a file exactly at the limit: %q, %v", body, err)
		}
	})
	t.Run("not a regular file", func(t *testing.T) {
		d := &linkDocker{}
		if _, err := readContainerFile(context.Background(), d, "c", "/f", 1<<20); err == nil {
			t.Fatal("read a symlink as though it were the file")
		}
	})
}

// linkDocker answers every copy with a symlink.
type linkDocker struct{ docker.Client }

func (linkDocker) CopyFromContainer(context.Context, string, string) (io.ReadCloser, container.PathStat, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "f", Linkname: "/etc/shadow", Typeflag: tar.TypeSymlink})
	_ = tw.Close()
	return io.NopCloser(&buf), container.PathStat{Name: "f"}, nil
}

// The two containers as a compose run leaves them. Times are docker's.
func ended(status string, code int, started, finished string) *container.State {
	return &container.State{Status: status, Running: status == "running", ExitCode: code, StartedAt: started, FinishedAt: finished}
}

const neverStarted = "0001-01-01T00:00:00Z"

func oomKilled(s *container.State) *container.State {
	s.OOMKilled = true
	return s
}

// AgentFailed tells the agent failing apart from the stop compose gives it
// once the app is done. Blaming the agent for that stop fails every compose
// run; missing it blames the test command for the agent's death.
func TestAgentFailed(t *testing.T) {
	const at, later = "2026-09-25T12:00:00Z", "2026-09-25T12:00:10Z"
	for _, tc := range []struct {
		name         string
		agent, app   *container.State
		want         bool
		wantAppStart bool
	}{
		{
			name:         "the normal end: the app exits, compose stops the agent gracefully",
			agent:        ended("exited", 0, at, later),
			app:          ended("exited", 7, at, later),
			wantAppStart: true,
		},
		{
			name:         "the app returned its own code, then compose killed an agent slow to stop",
			agent:        ended("exited", 137, at, later),
			app:          ended("exited", 0, at, at),
			wantAppStart: true,
		},
		{
			name:         "the app was OOM-killed on its own, and compose stopped the agent after it",
			agent:        ended("exited", 0, at, later),
			app:          ended("exited", 137, at, at),
			wantAppStart: true,
		},
		{
			name:         "the app ran out of its own memory, and compose then killed an agent slow to stop",
			agent:        ended("exited", 137, at, later),
			app:          oomKilled(ended("exited", 137, at, at)),
			wantAppStart: true,
		},
		{
			name:  "the agent could not start: compose never started the app",
			agent: ended("exited", 6, at, at),
			app:   ended("created", 0, neverStarted, neverStarted),
			want:  true,
		},
		{
			// The case timestamps get wrong: docker recorded the app first.
			name:         "the agent was killed mid-run, and the namespace took the app with it",
			agent:        ended("exited", 137, at, "2026-09-25T12:00:05.003Z"),
			app:          ended("exited", 137, at, "2026-09-25T12:00:05Z"),
			want:         true,
			wantAppStart: true,
		},
		{
			name:         "the agent crashed mid-run, and compose stopped the app over it",
			agent:        ended("exited", 2, at, at),
			app:          ended("exited", 143, at, later),
			want:         true,
			wantAppStart: true,
		},
		{
			name:         "the agent failed under an app still running",
			agent:        ended("exited", 1, at, at),
			app:          ended("running", 0, at, neverStarted),
			want:         true,
			wantAppStart: true,
		},
		{
			name:         "the agent is still running",
			agent:        ended("running", 0, at, neverStarted),
			app:          ended("exited", 0, at, later),
			wantAppStart: true,
		},
		{name: "docker cannot say how the agent ended", app: ended("exited", 0, at, later), wantAppStart: true},
		{name: "docker cannot say how the app ended, and the agent exited cleanly", agent: ended("exited", 0, at, later)},
		{name: "docker cannot say how the app ended, and the agent failed", agent: ended("exited", 6, at, at), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := StoppedAgent{Agent: tc.agent, App: tc.app}
			if got := s.AgentFailed(); got != tc.want {
				t.Errorf("AgentFailed = %v, want %v", got, tc.want)
			}
			if got := s.AppStarted(); got != tc.wantAppStart {
				t.Errorf("AppStarted = %v, want %v", got, tc.wantAppStart)
			}
		})
	}
}

// The states are read in the same window as the outcome -- after compose
// stopped the project, before keploy's teardown removed it -- and for the two
// containers that decide who failed: the agent, and the app it serves.
func TestRunReadsHowTheAgentAndTheAppEnded(t *testing.T) {
	argvLog := stubDockerCLI(t, "", 0)
	d := &stoppedAgentDocker{states: map[string]*container.State{
		"keploy-v3-test": ended("exited", 6, "2026-09-25T12:00:00Z", "2026-09-25T12:00:01Z"),
		"runner":         ended("created", 0, neverStarted, neverStarted),
	}}
	d.removed = func() bool { return strings.Contains(recordedLog(t, argvLog), "ARG down") }
	a := composeReplayApp(d)
	a.run(context.Background())
	got, ok := a.StoppedAgent()
	if !ok || got.Agent == nil || got.App == nil {
		t.Fatalf("run() did not read how the agent and the app ended: %+v", got)
	}
	if got.Container != "keploy-v3-test" || got.Agent.ExitCode != 6 || got.App.Status != "created" {
		t.Fatalf("read %+v / agent %+v / app %+v", got, got.Agent, got.App)
	}
	if !got.AgentFailed() {
		t.Fatal("an agent that exited 6 before the app ever started was not reported as having failed")
	}
}
