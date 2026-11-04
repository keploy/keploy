//go:build !windows

package http

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// fakeContainerAPI answers the boot watch's reads of one container from what
// the test last set, and counts them.
type fakeContainerAPI struct {
	mu        sync.Mutex
	state     *container.State // nil: an inspect with no state
	inspected int
	inspectEr error
	report    []byte // the agent's report of its start; nil: none written
	copied    []string
	copyErr   error
}

func (f *fakeContainerAPI) ContainerInspect(_ context.Context, _ string) (container.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspected++
	if f.inspectEr != nil {
		return container.InspectResponse{}, f.inspectEr
	}
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: f.state}}, nil
}

func (f *fakeContainerAPI) CopyFromContainer(_ context.Context, name, path string) (io.ReadCloser, container.PathStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copied = append(f.copied, name+":"+path)
	if f.copyErr != nil {
		return nil, container.PathStat{}, f.copyErr
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: filepath.Base(path), Mode: 0o644, Size: int64(len(f.report)), Typeflag: tar.TypeReg}); err != nil {
		return nil, container.PathStat{}, err
	}
	if _, err := tw.Write(f.report); err != nil {
		return nil, container.PathStat{}, err
	}
	if err := tw.Close(); err != nil {
		return nil, container.PathStat{}, err
	}
	return io.NopCloser(&buf), container.PathStat{}, nil
}

// engineCLIRecorder puts a `docker` and a `sudo` first on PATH that answer
// `inspect` with status and record every run, and returns what they ran.
func engineCLIRecorder(t *testing.T, status string) func() string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "ran")
	for _, bin := range []string{"docker", "sudo"} {
		script := "#!/bin/sh\necho \"" + bin + " $*\" >> \"" + log + "\"\n[ \"$1\" = inspect ] && echo " + status + " && exit 0\nexit 1\n"
		if err := os.WriteFile(filepath.Join(dir, bin), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() string {
		b, err := os.ReadFile(log)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
}

// The wait for an agent's start reads its container through the engine's
// API: its report, whether it runs, and its exit, with no `docker` (or sudo)
// run at all. A runner image with no `docker` binary reads it all the same.
func TestTheBootWatchReadsTheAgentsContainerThroughTheAPI(t *testing.T) {
	ran := engineCLIRecorder(t, "paused")
	api := &fakeContainerAPI{state: &container.State{Status: "running"}, report: []byte(`{"seq":3,"beat":7}`)}
	c := &namedContainer{name: "keploy-v3"}

	read, running := containerBootLook(zap.NewNop(), api, c)
	p, ok := read(context.Background())
	if !ok || p.Seq != 3 || p.Beat != 7 {
		t.Fatalf("the report read through the API: %+v, %v; want seq 3, beat 7", p, ok)
	}
	if want := "keploy-v3:" + kdocker.AgentBootProgressFile; strings.Join(api.copied, ",") != want {
		t.Fatalf("copied %v out of the container; want %s", api.copied, want)
	}
	if !c.isKnown() {
		t.Fatal("a report read out of the container did not note it there")
	}
	if !running(context.Background()) {
		t.Fatal("a container the API says runs is not taken to run")
	}

	ended := containerEnd(zap.NewNop(), api, c)
	if code, over := ended(context.Background()); over {
		t.Fatalf("a running container has ended: %d", code)
	}
	api.state = &container.State{Status: "exited", ExitCode: 3}
	if code, over := ended(context.Background()); !over || code != 3 {
		t.Fatalf("an exited container read through the API: %d, %v; want 3, ended", code, over)
	}
	if running(context.Background()) {
		t.Fatal("an exited container is taken to run")
	}

	// The API's not-found, whatever its words.
	api.inspectEr = errdefs.NotFound(errors.New("keploy-v3 is not there"))
	if code, over := ended(context.Background()); !over || code != containerGone {
		t.Fatalf("a container keploy knew, which the API says is not there: %d, %v; want containerGone, ended", code, over)
	}
	if got := ran(); got != "" {
		t.Fatalf("the boot watch ran the engine's CLI, with an API that answers:\n%s", got)
	}
}

// Where the API's socket cannot be reached the CLI reads, as before, and
// keeps reading: the API is not asked again.
func TestAnUnreachableAPIHasTheCLIRead(t *testing.T) {
	ran := engineCLIRecorder(t, "running")
	api := &fakeContainerAPI{inspectEr: client.ErrorConnectionFailed("unix:///nowhere/docker.sock")}
	_, running := containerBootLook(zap.NewNop(), api, &namedContainer{name: "keploy-v3"})

	for range 2 {
		if !running(context.Background()) {
			t.Fatal("the CLI's answer (running) was not read")
		}
	}
	if api.inspected != 1 {
		t.Fatalf("the API was asked %d times; want once, then the CLI", api.inspected)
	}
	if got := ran(); got != "docker inspect -f {{.State.Status}} keploy-v3\ndocker inspect -f {{.State.Status}} keploy-v3" {
		t.Fatalf("the CLI ran:\n%s\nwant two inspects of keploy-v3", got)
	}
}

// An answer of the engine through its API is no unreachable socket, and runs
// no CLI: an inspect with no state, or an error the daemon answers, is no
// answer, and keeps the last one; a not-found copy is no report.
func TestAnAPIAnswerIsNotHandedToTheCLI(t *testing.T) {
	ran := engineCLIRecorder(t, "running")
	api := &fakeContainerAPI{state: &container.State{Status: "paused"}}
	c := &namedContainer{name: "keploy-v3"}
	read, running := containerBootLook(zap.NewNop(), api, c)
	if running(context.Background()) {
		t.Fatal("a paused container is taken to run")
	}

	api.state = nil
	if running(context.Background()) {
		t.Fatal("an inspect with no state was read as an answer that the container runs")
	}
	if code, over := containerEnd(zap.NewNop(), api, c)(context.Background()); over {
		t.Fatalf("an inspect with no state ended the container: %d", code)
	}

	api.state = &container.State{Status: "running"}
	if !running(context.Background()) {
		t.Fatal("a container the API says runs is not taken to run")
	}
	api.inspectEr = errdefs.System(errors.New("the daemon is busy"))
	if !running(context.Background()) {
		t.Fatal("a daemon's error was read as an answer that the container does not run")
	}

	api.copyErr = errdefs.NotFound(errors.New("Could not find the file"))
	if _, ok := read(context.Background()); ok {
		t.Fatal("a report the agent has not written was read")
	}
	if got := ran(); got != "" {
		t.Fatalf("the boot watch ran the engine's CLI on the API's answers:\n%s", got)
	}
}

// A wait with no client of its own (WatchContainerAgentStart) reads through
// one made for the environment's engine endpoint, and through the CLI alone
// when the environment names none that can be made.
func TestAWaitWithNoClientMakesOneForTheEnvironment(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nowhere/docker.sock")
	if envContainerAPI(zap.NewNop()) == nil {
		t.Fatal("no API client was made for DOCKER_HOST unix:///nowhere/docker.sock")
	}
	t.Setenv("DOCKER_HOST", "not a host")
	if api := envContainerAPI(zap.NewNop()); api != nil {
		t.Fatalf("an API client %T was made for a DOCKER_HOST that names no endpoint", api)
	}
}
