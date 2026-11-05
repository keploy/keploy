//go:build !windows

package http

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// apiDaemon is the engine API client an AgentClient runs its agent's
// container with, whose reads of that container are api's.
type apiDaemon struct {
	kdocker.Client
	api *fakeContainerAPI
}

func (d apiDaemon) ContainerInspect(ctx context.Context, name string) (container.InspectResponse, error) {
	return d.api.ContainerInspect(ctx, name)
}

func (d apiDaemon) CopyFromContainer(ctx context.Context, name, path string) (io.ReadCloser, container.PathStat, error) {
	return d.api.CopyFromContainer(ctx, name, path)
}

// reads is how many inspects and copies api has answered.
func (f *fakeContainerAPI) reads() (inspected, copied int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspected, len(f.copied)
}

// lookEvery has the watches look at the agent once a millisecond.
func lookEvery(t *testing.T) {
	t.Helper()
	look := bootLook
	bootLook = time.Millisecond
	t.Cleanup(func() { bootLook = look })
}

// awaitWait waits for the error of a wait on the agent, and fails the row at
// once if the wait ran the engine's CLI (ran) before it ended: a wait whose
// looks lost the engine API client they were to read through reads through
// the CLI, which a runner image with no `docker` binary does not have.
func awaitWait(t *testing.T, ran func() string, errs <-chan error) error {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case err := <-errs:
			if got := ran(); got != "" {
				t.Fatalf("the wait ran the engine's CLI, with an engine API client that answers:\n%s", got)
			}
			return err
		case <-deadline:
			t.Fatalf("the wait did not end; the engine's CLI ran:\n%s", ran())
		case <-time.After(5 * time.Millisecond):
			if got := ran(); got != "" {
				t.Fatalf("the wait ran the engine's CLI, with an engine API client that answers:\n%s", got)
			}
		}
	}
}

// The watch of an agent's start in a container keploy set up reads it
// through the engine API client keploy runs that container with: whether it
// runs and its report (WatchAgentStart, and the release of a compose app,
// MakeAgentReadyForDockerCompose), and, for a watch that ends on the
// container's exit, whether it has exited. No `docker` (or sudo) runs.
func TestAClientsWatchReadsTheAgentsContainerThroughItsOwnEngineAPIClient(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endOnExit bool
		state     container.State
		report    string
		want      string
	}{
		{"its report says its start failed", false, container.State{Status: "running"},
			`{"seq":4,"beat":9,"failed":true,"failure":"failed to load eBPF"}`, "the keploy agent could not start: failed to load eBPF"},
		{"its container exited", true, container.State{Status: "exited", ExitCode: 7},
			`{"seq":4,"beat":9}`, "the keploy agent's container exited with code 7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookEvery(t)
			ran := engineCLIRecorder(t, "running")
			state := tc.state
			api := &fakeContainerAPI{state: &state, report: []byte(tc.report)}
			a := New(zap.NewNop(), apiDaemon{api: api}, &config.Config{Agent: config.Agent{AgentURI: "http://127.0.0.1:1"}})
			a.agentContainer = &namedContainer{name: "keploy-v3-api"}
			ctx, cancel := context.WithCancel(context.Background())
			errs, over := a.watchAgent(ctx, tc.endOnExit)
			// A row that fails leaves no watch looking behind it.
			t.Cleanup(func() { cancel(); <-over })
			err := awaitWait(t, ran, errs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the watch said %v; want %q", err, tc.want)
			}
			<-over
			if inspected, copied := api.reads(); inspected == 0 || copied == 0 {
				t.Fatalf("the client's engine API answered %d inspects and %d copies; want the watch's reads", inspected, copied)
			}
		})
	}
}

// The wait for an agent keploy started with `docker run` (waitForAgent)
// reads its start through the engine API client keploy started it with: the
// agent, which answers only once its report has been read out of its
// container, ends the wait, with no `docker` (or sudo) run.
func TestTheDockerRunWaitReadsTheAgentsContainerThroughItsEngineAPIClient(t *testing.T) {
	lookEvery(t)
	t.Setenv("KEPLOY_PROVEN", "")
	ran := engineCLIRecorder(t, "running")
	api := &fakeContainerAPI{state: &container.State{Status: "running"}, report: []byte(`{"seq":2,"beat":3}`)}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, copied := api.reads(); copied == 0 {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(agent.Close)
	a := New(zap.NewNop(), apiDaemon{api: api}, &config.Config{Agent: config.Agent{AgentURI: agent.URL}})
	const name = "keploy-v3-run"
	a.nameAgentContainer(name)
	t.Cleanup(a.forgetAgentContainer)
	ctx, cancel := context.WithCancel(context.Background())
	done, ended := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(ended)
		done <- a.waitForAgent(ctx, nil, true, models.SetupOptions{KeployContainer: name})
	}()
	// A row that fails leaves no wait looking behind it.
	t.Cleanup(func() { cancel(); <-ended })
	if err := awaitWait(t, ran, done); err != nil {
		t.Fatalf("the wait ended with %v; want the agent's answer, once its report was read through the API", err)
	}
}

// engineAPISocket is an engine, at the unix socket DOCKER_HOST names, of API
// version 1.41 (older than this client's own), that runs the agent's
// container name with report in it: it answers /_ping, and the container's
// inspect and the copy of the report out of it under /v1.41. What else it is
// asked it refuses, and notes.
type engineAPISocket struct {
	mu         sync.Mutex
	unexpected []string
	reads      int
	open       int
}

func startEngineAPISocket(t *testing.T, name string, report []byte) *engineAPISocket {
	t.Helper()
	// A socket's path is bounded (108 bytes): t.TempDir's names the test.
	dir, err := os.MkdirTemp("", "kapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "docker.sock")
	e := &engineAPISocket{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_ping":
			w.Header().Set("Api-Version", "1.41")
			_, _ = io.WriteString(w, "OK")
		case r.URL.Path == "/v1.41/containers/"+name+"/json":
			e.read()
			_ = json.NewEncoder(w).Encode(container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{
				ID: "c0ffee", Name: "/" + name, State: &container.State{Status: "running"},
			}})
		case r.URL.Path == "/v1.41/containers/"+name+"/archive" && r.URL.Query().Get("path") == kdocker.AgentBootProgressFile:
			e.read()
			stat, err := json.Marshal(container.PathStat{Name: filepath.Base(kdocker.AgentBootProgressFile), Size: int64(len(report)), Mode: 0o644})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
			w.Header().Set("Content-Type", "application/x-tar")
			tw := tar.NewWriter(w)
			_ = tw.WriteHeader(&tar.Header{Name: filepath.Base(kdocker.AgentBootProgressFile), Mode: 0o644, Size: int64(len(report)), Typeflag: tar.TypeReg})
			_, _ = tw.Write(report)
			_ = tw.Close()
		default:
			e.mu.Lock()
			e.unexpected = append(e.unexpected, r.Method+" "+r.URL.String())
			e.mu.Unlock()
			http.Error(w, "an engine of API 1.41 does not answer this", http.StatusBadRequest)
		}
	}))
	_ = srv.Listener.Close()
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = l
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		e.mu.Lock()
		defer e.mu.Unlock()
		switch s {
		case http.StateNew:
			e.open++
		case http.StateClosed, http.StateHijacked:
			e.open--
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	t.Setenv("DOCKER_API_VERSION", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	return e
}

func (e *engineAPISocket) read() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reads++
}

// state is what it has been asked that it refused, how many reads it
// answered, and how many connections to it are open.
func (e *engineAPISocket) state() (unexpected []string, reads, open int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.unexpected...), e.reads, e.open
}

// The watch of an agent's start that keploy did not set up
// (WatchContainerAgentStart: enterprise's headless record waits on the agent
// docker compose starts with it) reads the agent's container through an
// engine API client at the endpoint DOCKER_HOST names, with no `docker` (or
// sudo) run: at the API version the engine speaks, which it asks first, and
// an engine older than the client's own API answers it. Once the watch is
// over, its client is closed: no connection to the engine's socket is left
// open behind it.
func TestAWaitWithNoClientReadsTheAgentsContainerThroughTheEnvironmentsEngineAPI(t *testing.T) {
	lookEvery(t)
	ran := engineCLIRecorder(t, "running")
	const name = "keploy-v3-env"
	e := startEngineAPISocket(t, name, []byte(`{"seq":4,"beat":9,"failed":true,"failure":"failed to load eBPF"}`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := awaitWait(t, ran, WatchContainerAgentStart(ctx, zap.NewNop(), name))
	if err == nil || !strings.Contains(err.Error(), "the keploy agent could not start: failed to load eBPF") {
		t.Fatalf("the watch said %v; want the agent's failure, read through the engine's API", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		unexpected, reads, open := e.state()
		if len(unexpected) != 0 {
			t.Fatalf("the watch asked the engine what an engine of API 1.41 does not answer:\n%s", strings.Join(unexpected, "\n"))
		}
		if reads == 0 {
			t.Fatal("the watch read nothing through the engine's API")
		}
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections to the engine's socket are still open after the watch ended", open)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A read of the agent's report out of its container takes no more than
// 1 MiB from the engine, however much the engine sends. The row names the
// size itself, not bootReportMax, so a change of the bound fails it.
func TestAReportReadThroughTheAPIIsBounded(t *testing.T) {
	const oneMiB = 1 << 20
	api := &fakeContainerAPI{report: make([]byte, 2*oneMiB)}
	out, err := newContainerLook(zap.NewNop(), api).copyOut(context.Background(), "keploy-v3", kdocker.AgentBootProgressFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != oneMiB {
		t.Fatalf("the read took %d bytes from the engine; want its 1 MiB bound, %d", len(out), oneMiB)
	}
}
