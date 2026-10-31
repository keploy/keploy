package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

// The grace keploy gives a compose app it interrupted ends once the daemon
// says the app's container exited, however slow its answer (here 2.2 s, well
// inside the 5 s grace): the reads had 2 s budgets, every slow answer was cut,
// and the app that had exited at once was given the whole grace. cmdCancel's
// compose branch gives the same grace (composeAppGrace).
func TestTheComposeGraceEndsOnTheDaemonsSlowAnswer(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	d := &slowDaemon{Client: exitedApp(), latency: 2200 * time.Millisecond}
	a := &App{logger: zap.New(core), docker: d, container: "app"}
	downs := 0
	a.composeGraceThenDown(func() { downs++ })
	if downs != 1 || logs.FilterMessage("app container stopped gracefully; tearing down the rest of the compose stack").Len() != 1 {
		t.Fatalf("downs %d, %d answers read; want the grace ended on the daemon's answer that the app exited, then one down", downs, d.answers())
	}
}

// The grace keploy gives a compose app it interrupted ends at compose's up
// returning (here at 0.6 s), while the daemon fails every read of the app's
// container: the up's return was looked at only between reads, and a read
// asked again until the grace's end held the grace to its 5 s.
func TestTheComposeGraceEndsAtTheUpsReturnWhileTheDaemonFailsItsReads(t *testing.T) {
	stallBudget(t, time.Hour)
	core, logs := observer.New(zap.DebugLevel)
	d := &slowDaemon{Client: exitedApp(), failInspects: 1 << 30}
	a := &App{logger: zap.New(core), docker: d, container: "app"}
	began := time.Now()
	returnedAt := began.Add(600 * time.Millisecond)
	a.composeAppGrace(func() bool { return time.Now().After(returnedAt) })
	if took := time.Since(began); took >= graceBudget {
		t.Fatalf("the grace took %v, its whole %v, after the up returned at 0.6 s; want it ended at the up's return", took, graceBudget)
	}
	if logs.FilterMessage("docker compose process exited").Len() != 1 {
		t.Fatal("the grace did not say it ended at the up's return")
	}
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
