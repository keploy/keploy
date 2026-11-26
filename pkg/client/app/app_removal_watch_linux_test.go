//go:build linux

package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type removalWatchDocker struct {
	docker.Client
	mu             sync.Mutex
	exited         atomic.Bool
	silentWatch    bool
	watchCalled    chan container.WaitCondition
	exitedInspects chan struct{}
	watchName      string
	removed        chan container.WaitResponse
}

func (d *removalWatchDocker) ContainerInspect(_ context.Context, _ string) (container.InspectResponse, error) {
	if !d.exited.Load() {
		return container.InspectResponse{}, cerrdefs.ErrNotFound
	}
	if d.exitedInspects != nil {
		select {
		case d.exitedInspects <- struct{}{}:
		default:
		}
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			State: &container.State{Status: "exited", Running: false},
		},
	}, nil
}

func (d *removalWatchDocker) ContainerRemove(_ context.Context, _ string, _ container.RemoveOptions) error {
	return cerrdefs.ErrNotFound
}

func (d *removalWatchDocker) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	return nil, nil
}

func (d *removalWatchDocker) ContainerKill(_ context.Context, _ string, _ string) error {
	return nil
}

func (d *removalWatchDocker) ContainerWait(ctx context.Context, name string, cond container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	d.mu.Lock()
	d.watchName = name
	d.mu.Unlock()
	select {
	case d.watchCalled <- cond:
	default:
	}
	if d.silentWatch {
		<-ctx.Done()
		errCh := make(chan error, 1)
		errCh <- ctx.Err()
		// Return a non-nil result channel just like the real Docker SDK so
		// RemovalWatch.Wait must observe w.ctx.Err() != nil rather than
		// relying on a nil said channel.
		return make(chan container.WaitResponse), errCh
	}
	errCh := make(chan error, 1)
	go func() {
		<-ctx.Done()
		errCh <- ctx.Err()
	}()
	return d.removed, errCh
}

func writeFakeDockerCLI(t *testing.T, dir, started, exited string) {
	t.Helper()
	bin := filepath.Join(dir, "docker")
	script := `#!/bin/sh
: > "` + started + `"
trap ': > "` + exited + `"; exit 0' INT TERM
while [ -d "` + dir + `" ]; do sleep 0.02; done
: > "` + exited + `"
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestDockerRunWaitsForRmContainerRemoval verifies that when teardown cancels a
// `docker run --rm --name ...` app, App.run subscribes to WaitConditionRemoved
// before stopping it and does not return until the engine reports removal
// (releasing per-run volumes like keploy-sockets-vol-<id>). Commands with
// --rm=false do not subscribe, and a silent daemon times out cleanly on
// appRemovalAskBudget.
func TestDockerRunWaitsForRmContainerRemoval(t *testing.T) {
	t.Run("rm container waits for engine removal before returning", func(t *testing.T) {
		dir := t.TempDir()
		started := filepath.Join(dir, "started")
		exited := filepath.Join(dir, "exited")
		writeFakeDockerCLI(t, dir, started, exited)

		dc := &removalWatchDocker{
			watchCalled:    make(chan container.WaitCondition, 4),
			exitedInspects: make(chan struct{}, 8),
			removed:        make(chan container.WaitResponse, 1),
		}
		a := NewApp(zap.NewNop(), "docker run --rm --name app-rm-test busybox:1.37", dc, models.SetupOptions{
			Container:   "app-rm-test",
			CommandType: string(utils.DockerRun),
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan models.AppError, 1)
		go func() { done <- a.Run(ctx) }()

		deadline := time.After(5 * time.Second)
		for {
			if _, err := os.Stat(started); err == nil {
				break
			}
			select {
			case appErr := <-done:
				t.Fatalf("App.Run exited before fake docker started: %+v", appErr)
			case <-deadline:
				t.Fatal("timed out waiting for fake docker to start")
			case <-time.After(10 * time.Millisecond):
			}
		}

		dc.exited.Store(true)
		cancel()

		select {
		case cond := <-dc.watchCalled:
			if cond != container.WaitConditionRemoved {
				t.Fatalf("ContainerWait condition = %q, want %q", cond, container.WaitConditionRemoved)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("App.Run did not subscribe to ContainerWait(removed) on cancel")
		}
		dc.mu.Lock()
		gotName := dc.watchName
		dc.mu.Unlock()
		if gotName != "app-rm-test" {
			t.Fatalf("ContainerWait container = %q, want %q", gotName, "app-rm-test")
		}

		// Wait until fake `docker run` has exited AND at least two post-exit
		// ContainerInspect calls have occurred (waitTillExit and
		// RemovalWatch.Wait), so App.run is now guaranteed to be sitting in
		// appRemoval.Wait (and would return immediately if appRemoval.Wait
		// were skipped).
		exitDeadline := time.After(5 * time.Second)
		for {
			if _, err := os.Stat(exited); err == nil {
				break
			}
			select {
			case appErr := <-done:
				t.Fatalf("App.Run returned before fake docker exited and before engine reported removal: %+v", appErr)
			case <-exitDeadline:
				t.Fatal("timed out waiting for fake docker to exit")
			case <-time.After(10 * time.Millisecond):
			}
		}
		for i := 0; i < 2; i++ {
			select {
			case <-dc.exitedInspects:
			case appErr := <-done:
				t.Fatalf("App.Run returned before engine reported container removal (after %d exited inspects): %+v", i, appErr)
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for exited inspect #%d", i+1)
			}
		}

		select {
		case appErr := <-done:
			t.Fatalf("App.Run returned before engine reported container removal: %+v", appErr)
		case <-time.After(200 * time.Millisecond):
		}

		// Engine finishes --rm removal and releases the container's volumes.
		dc.removed <- container.WaitResponse{}
		select {
		case appErr := <-done:
			if appErr.AppErrorType != models.ErrCtxCanceled {
				t.Fatalf("App.Run error type = %v (err=%v), want ErrCtxCanceled", appErr.AppErrorType, appErr.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("App.Run did not return after engine reported container removal")
		}
	})

	t.Run("non-rm container does not wait for removal", func(t *testing.T) {
		dir := t.TempDir()
		started := filepath.Join(dir, "started")
		exited := filepath.Join(dir, "exited")
		writeFakeDockerCLI(t, dir, started, exited)

		dc := &removalWatchDocker{
			watchCalled:    make(chan container.WaitCondition, 4),
			exitedInspects: make(chan struct{}, 8),
			removed:        make(chan container.WaitResponse, 1),
		}
		a := NewApp(zap.NewNop(), "docker run --rm=false --name app-norm-test busybox:1.37", dc, models.SetupOptions{
			Container:   "app-norm-test",
			CommandType: string(utils.DockerRun),
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan models.AppError, 1)
		go func() { done <- a.Run(ctx) }()

		deadline := time.After(5 * time.Second)
		for {
			if _, err := os.Stat(started); err == nil {
				break
			}
			select {
			case appErr := <-done:
				t.Fatalf("App.Run exited before fake docker started: %+v", appErr)
			case <-deadline:
				t.Fatal("timed out waiting for fake docker to start")
			case <-time.After(10 * time.Millisecond):
			}
		}

		dc.exited.Store(true)
		cancel()

		select {
		case appErr := <-done:
			if appErr.AppErrorType != models.ErrCtxCanceled {
				t.Fatalf("App.Run error type = %v, want ErrCtxCanceled", appErr.AppErrorType)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("App.Run hung on --rm=false container")
		}
		select {
		case cond := <-dc.watchCalled:
			t.Fatalf("ContainerWait(%q) was called for --rm=false container", cond)
		default:
		}
	})

	t.Run("silent daemon on ContainerWait withdraws watch and Wait returns immediately without burning appRemovalWaitBudget", func(t *testing.T) {
		prevAsk, prevWait := appRemovalAskBudget, appRemovalWaitBudget
		appRemovalAskBudget = 150 * time.Millisecond
		appRemovalWaitBudget = 5 * time.Second
		t.Cleanup(func() {
			appRemovalAskBudget = prevAsk
			appRemovalWaitBudget = prevWait
		})

		dir := t.TempDir()
		started := filepath.Join(dir, "started")
		exited := filepath.Join(dir, "exited")
		writeFakeDockerCLI(t, dir, started, exited)

		dc := &removalWatchDocker{
			silentWatch:    true,
			watchCalled:    make(chan container.WaitCondition, 4),
			exitedInspects: make(chan struct{}, 8),
			removed:        make(chan container.WaitResponse, 1),
		}
		a := NewApp(zap.NewNop(), "docker run --rm --name app-silent-test busybox:1.37", dc, models.SetupOptions{
			Container:   "app-silent-test",
			CommandType: string(utils.DockerRun),
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan models.AppError, 1)
		go func() { done <- a.Run(ctx) }()

		deadline := time.After(5 * time.Second)
		for {
			if _, err := os.Stat(started); err == nil {
				break
			}
			select {
			case appErr := <-done:
				t.Fatalf("App.Run exited before fake docker started: %+v", appErr)
			case <-deadline:
				t.Fatal("timed out waiting for fake docker to start")
			case <-time.After(10 * time.Millisecond):
			}
		}

		dc.exited.Store(true)
		cancel()

		select {
		case cond := <-dc.watchCalled:
			if cond != container.WaitConditionRemoved {
				t.Fatalf("ContainerWait condition = %q, want %q", cond, container.WaitConditionRemoved)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("App.Run did not subscribe to ContainerWait(removed) on cancel")
		}

		// With appRemovalAskBudget=150ms and appRemovalWaitBudget=5s, returning
		// within 3.5s proves RemovalWatch.Wait short-circuited immediately
		// after the withdrawn Watch instead of burning appRemovalWaitBudget.
		start := time.Now()
		select {
		case appErr := <-done:
			if appErr.AppErrorType != models.ErrCtxCanceled {
				t.Fatalf("App.Run error type = %v (err=%v), want ErrCtxCanceled", appErr.AppErrorType, appErr.Err)
			}
			if elapsed := time.Since(start); elapsed >= 4*time.Second {
				t.Fatalf("App.Run took %v after withdrawn Watch; Wait appears to have burned appRemovalWaitBudget", elapsed)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("App.Run hung when daemon did not answer ContainerWait")
		}
	})
}
