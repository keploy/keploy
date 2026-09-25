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

// The teardown and the log fetch after a run only need these to fail
// harmlessly: "no such container" ends the reap barrier at once.
func (d *stoppedAgentDocker) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{}, errors.New("no such container")
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
	}}
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

// Only a compose MOCK REPLAY has an outcome to read. Every other compose run
// -- keploy record, keploy test, mock record -- must not go asking the daemon
// for a file its agent never writes.
func TestReadStoppedAgentOnlyForAComposeMockReplay(t *testing.T) {
	for name, opts := range map[string]models.SetupOptions{
		"mock record":   {MockMode: true, Mode: models.MODE_RECORD},
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
	d := &stoppedAgentDocker{}
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
