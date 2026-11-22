package app

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// slowDaemon is a loaded docker daemon in front of another (Client): its
// first failInspects inspects and first failLists lists fail, as reads that
// gave up do, and each inspect takes latency to answer, failing with its
// context's error when that ends first. answered counts the inspects that
// reached Client.
type slowDaemon struct {
	docker.Client
	mu                      sync.Mutex
	failInspects, failLists int
	latency                 time.Duration
	answered                int
}

var errSlowRead = errors.New("context deadline exceeded (a loaded daemon's read that gave up)")

func (d *slowDaemon) ContainerInspect(ctx context.Context, ref string) (container.InspectResponse, error) {
	d.mu.Lock()
	fail := d.failInspects > 0
	if fail {
		d.failInspects--
	}
	latency := d.latency
	d.mu.Unlock()
	if fail {
		return container.InspectResponse{}, errSlowRead
	}
	if latency > 0 {
		select {
		case <-ctx.Done():
			return container.InspectResponse{}, ctx.Err()
		case <-time.After(latency):
		}
	}
	d.mu.Lock()
	d.answered++
	d.mu.Unlock()
	return d.Client.ContainerInspect(ctx, ref)
}

func (d *slowDaemon) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	d.mu.Lock()
	fail := d.failLists > 0
	if fail {
		d.failLists--
	}
	d.mu.Unlock()
	if fail {
		return nil, errSlowRead
	}
	return d.Client.ContainerList(ctx, o)
}

func (d *slowDaemon) answers() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.answered
}

// A read of the daemon ends on its answer: one it failed is asked again
// (five failures, then the answer), and a "no such container" is an answer,
// not asked again. With no answer, it ends at the caller's ctx, or once
// keploy's runnable-time bound on no answer passes, and says it had none.
func TestAReadOfTheDaemonEndsOnItsAnswer(t *testing.T) {
	asks := func(fails int, last error) (func(context.Context) (string, error), *int) {
		n := 0
		return func(context.Context) (string, error) {
			n++
			if n <= fails {
				return "", errSlowRead
			}
			if last != nil {
				return "", last
			}
			return "the answer", nil
		}, &n
	}
	t.Run("asked again until it answers", func(t *testing.T) {
		ask, n := asks(5, nil)
		if v, err := askDaemon(context.Background(), zap.NewNop(), "a test read", ask); err != nil || v != "the answer" || *n != 6 {
			t.Fatalf("askDaemon = %q, %v after %d asks; want the answer after 6", v, err, *n)
		}
	})
	t.Run("no such container is an answer", func(t *testing.T) {
		ask, n := asks(2, errdefs.NotFound(errors.New("no such container")))
		if _, err := askDaemon(context.Background(), zap.NewNop(), "a test read", ask); !cerrdefs.IsNotFound(err) || *n != 3 {
			t.Fatalf("askDaemon's error %v after %d asks; want the daemon's not-found after 3", err, *n)
		}
	})
	t.Run("the caller stops waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		n := 0
		_, err := askDaemon(ctx, zap.NewNop(), "a test read", func(context.Context) (string, error) {
			if n++; n == 3 {
				cancel()
			}
			return "", errSlowRead
		})
		if err == nil || cerrdefs.IsNotFound(err) || !strings.Contains(err.Error(), "caller stopped waiting") || n != 3 {
			t.Fatalf("askDaemon's error %v after %d asks; want no answer, its caller stopped waiting, after 3", err, n)
		}
	})
	t.Run("no answer for the stall bound", func(t *testing.T) {
		stallBudget(t, 300*time.Millisecond)
		_, err := askDaemon(context.Background(), zap.NewNop(), "a test read", func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		})
		if err == nil || !strings.Contains(err.Error(), "no answer for") {
			t.Fatalf("askDaemon's error %v; want no answer for the stall bound", err)
		}
	})
}

// exitedApp is a daemon whose container "app" has exited.
func exitedApp() *inspectRecorder {
	return &inspectRecorder{inspects: map[string]container.InspectResponse{"app": {ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Status: "exited"}}}}}
}

// The wait for a docker run app's container to exit after keploy's interrupt
// reads the daemon's answer, however slow (2.2 s, inside the wait's 8 s), and
// asks again a read that failed: with a 2 s budget per read, the first slow or
// failed read ended the wait at once, the container's state never read.
func TestTheWaitForADockerRunAppsExitReadsTheDaemonsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    *slowDaemon
	}{
		{"a slow answer", &slowDaemon{Client: exitedApp(), latency: 2200 * time.Millisecond}},
		{"failed reads", &slowDaemon{Client: exitedApp(), failInspects: 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{logger: zap.NewNop(), docker: tc.d, container: "app"}
			a.waitTillExit()
			if n := tc.d.answers(); n != 1 {
				t.Fatalf("the wait read %d answers of the daemon; want 1, that the container exited", n)
			}
		})
	}
}

// A read of the daemon that ends with no answer ends only once the ask it
// cancelled has returned: it ended at once, its ask still in flight, so the
// read after it (the docker run stop's last look at the container, as its
// grace ran out mid-read) ran beside the first, and so did what the stop did
// next (its kill). Each round's ask takes some turns of the scheduler to
// return once cancelled, as a call to the daemon takes to unwind, and counts
// itself left running if the read had returned by then.
func TestAReadThatEndsWithNoAnswerEndsWithItsAsk(t *testing.T) {
	rounds := func(ctx context.Context, t *testing.T, n int, want string) {
		t.Helper()
		for round := 0; round < n; round++ {
			var readDone, leftRunning atomic.Bool
			var asks sync.WaitGroup
			asks.Add(1)
			_, err := askDaemon(ctx, zap.NewNop(), "a test read", func(actx context.Context) (string, error) {
				defer asks.Done()
				<-actx.Done()
				for i := 0; i < 1000 && !readDone.Load(); i++ {
					runtime.Gosched()
				}
				leftRunning.Store(readDone.Load())
				return "", actx.Err()
			})
			readDone.Store(true)
			asks.Wait()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("round %d: askDaemon's error %v; want no answer, %s", round, err, want)
			}
			if leftRunning.Load() {
				t.Fatalf("round %d: the read ended with its ask still running", round)
			}
		}
	}
	t.Run("its caller stopped waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rounds(ctx, t, 200, "caller stopped waiting")
	})
	t.Run("no answer for the stall bound", func(t *testing.T) {
		stallBudget(t, 20*time.Millisecond)
		rounds(context.Background(), t, 10, "no answer for")
	})
}
