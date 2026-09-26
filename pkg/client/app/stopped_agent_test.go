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

// Only a compose MOCK run reads the stopped agent, and only a replay has an
// outcome to read. keploy record and keploy test must not go asking the daemon
// about a container they never look at again, and a mock record must not go
// asking for a file its agent never writes.
func TestReadStoppedAgentOnlyForAComposeMockRun(t *testing.T) {
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
	for name, opts := range map[string]models.SetupOptions{
		"keploy test":   {Mode: models.MODE_TEST},
		"keploy record": {Mode: models.MODE_RECORD},
	} {
		t.Run(name, func(t *testing.T) {
			d := &stoppedAgentDocker{}
			a := composeReplayApp(d)
			a.opts = opts
			a.readStoppedAgent(context.Background())
			if _, ok := a.StoppedAgent(); ok || len(d.copied) > 0 {
				t.Fatalf("read the agent's container for a %s (copied %v)", name, d.copied)
			}
		})
	}
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
