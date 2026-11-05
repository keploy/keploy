// The two sweep cases that take the compose LIBRARY path. Split out of
// compose_project_sweep_test.go rather than tagging that whole file, which
// also defines helpers the untagged tests in this package use.

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/network"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// TestSweepUsesTheComposeLibraryForInMemoryProjects covers the in-memory path
// (the enterprise cloud flow), where the compose document is never written to
// disk. That path no longer shells out at all — it drives the compose library —
// so the failure this guards is that the sweep enumerates NOTHING and silently
// becomes a no-op on the very path it was written for.
//
// It also pins that no `docker` binary is invoked: the whole point of the
// library path is that the runner image needs no docker CLI.
func TestSweepUsesTheComposeLibraryForInMemoryProjects(t *testing.T) {
	forceComposeLibrary(t)
	argvLog := stubDockerCLI(t, "", 0)
	stack := &fakeComposeStack{
		ps: []docker.ServiceState{{ID: "id-app", Service: "app", State: "exited"}},
	}
	a := &App{
		logger:          zap.NewNop(),
		docker:          &inspectRecorder{},
		composeContent:  []byte("services:\n  app:\n    image: alpine\n"),
		cmd:             "docker compose -f - up",
		composeServices: []string{"app"},
		newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
	}

	ids := a.sweepStragglingProjectContainers()

	if stack.psCalls == 0 {
		t.Fatal("the in-memory sweep never asked the compose library what was left standing, " +
			"so it silently removes nothing on the path it exists for")
	}
	if len(ids) != 1 || ids[0] != "id-app" {
		t.Fatalf("sweep returned %v, want [id-app]", ids)
	}
	if log := recordedLog(t, argvLog); strings.Contains(log, "ARG") {
		t.Fatalf("the in-memory sweep shelled out to a `docker` binary; the library path must not.\n%s", log)
	}
}

// TestSweepSkipsComposeRunOneOffsOnTheLibraryPath pins that the label carried
// through the library is still read. A `compose run` container shares the
// service label with a `compose up` one, so without the oneoff label the sweep
// would remove a container that is the user's, not keploy's.
func TestSweepSkipsComposeRunOneOffsOnTheLibraryPath(t *testing.T) {
	forceComposeLibrary(t)
	stubDockerCLI(t, "", 0)
	stack := &fakeComposeStack{ps: []docker.ServiceState{
		{ID: "id-oneoff", Service: "app", State: "exited",
			Labels: map[string]string{"com.docker.compose.oneoff": "True"}},
		{ID: "id-real", Service: "app", State: "exited"},
	}}
	a := &App{
		logger:          zap.NewNop(),
		docker:          &inspectRecorder{},
		composeContent:  []byte("services:\n  app:\n    image: alpine\n"),
		cmd:             "docker compose -f - up",
		composeServices: []string{"app"},
		newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
	}

	ids := a.sweepStragglingProjectContainers()

	for _, id := range ids {
		if id == "id-oneoff" {
			t.Fatalf("the sweep removed a `compose run` one-off, which is the user's container: %v", ids)
		}
	}
	if len(ids) != 1 || ids[0] != "id-real" {
		t.Fatalf("sweep returned %v, want [id-real]", ids)
	}
}

// libraryDownApp is an App on the in-process path whose runner build answers
// build, with the leftovers a cut-short down leaves: a db container, and the
// project's network.
func libraryDownApp(t *testing.T, build func(context.Context) (composeStack, error)) (*App, *inspectRecorder, string) {
	t.Helper()
	forceComposeLibrary(t)
	argvLog := stubDockerCLI(t, "", 0)
	rec := &inspectRecorder{networks: []network.Summary{{ID: "net-1", Name: "fake_default"}}}
	a := &App{
		logger:          zap.NewNop(),
		docker:          rec,
		composeContent:  []byte("services:\n  app:\n    image: alpine\n"),
		cmd:             "docker compose -p orderflow -f - up",
		keployContainer: "keploy-v3",
		container:       "user-app",
		composeServices: []string{"app", "db"},
		newComposeStack: build,
	}
	return a, rec, argvLog
}

// A down through the compose library that did not succeed is finished as a
// failed shell-out down is: what it did not reach of the project, its
// containers and then its networks, is swept, or the next up reuses the
// previous test-set's database (#4614). One that succeeded is not swept, and
// it stops containers with the shell-out's `--timeout 1`, not compose's 10 s
// per container, which overran the teardown's drain on a loaded daemon.
func TestALibraryDownThatDidNotSucceedIsSwept(t *testing.T) {
	leftovers := []docker.ServiceState{{ID: "id-db", Service: "db", State: "running"}}

	t.Run("the down errs", func(t *testing.T) {
		stack := &fakeComposeStack{ps: leftovers, downErr: errors.New("cut: no progress")}
		a, rec, argvLog := libraryDownApp(t, func(context.Context) (composeStack, error) { return stack, nil })
		if _, err := a.composeRunner(context.Background()); err != nil { // the up built it
			t.Fatal(err)
		}

		a.ComposeDown()

		if stack.downCall != 1 {
			t.Fatalf("the library down ran %d times; want once", stack.downCall)
		}
		if !rec.didRemove("id-db") {
			t.Fatalf("the db container a failed library down left was not swept; removed %v", rec.removedIDs())
		}
		if strings.Join(rec.netLabels, ",") != "com.docker.compose.project=fake" || strings.Join(rec.netRemoved, ",") != "net-1" {
			t.Fatalf("after a failed library down keploy listed networks by %v and removed %v; want the runner's project fake and net-1",
				rec.netLabels, rec.netRemoved)
		}
		if log := recordedLog(t, argvLog); strings.Contains(log, "ARG") {
			t.Fatalf("the library teardown shelled out to a `docker` binary:\n%s", log)
		}
	})

	t.Run("the runner cannot be built", func(t *testing.T) {
		a, rec, _ := libraryDownApp(t, func(context.Context) (composeStack, error) {
			return nil, errors.New("engine unreachable")
		})

		a.ComposeDown()

		if strings.Join(rec.netLabels, ",") != "com.docker.compose.project=orderflow" || strings.Join(rec.netRemoved, ",") != "net-1" {
			t.Fatalf("with no runner to take the stack down keploy listed networks by %v and removed %v; want the command's project orderflow and net-1",
				rec.netLabels, rec.netRemoved)
		}
	})

	t.Run("the down succeeds", func(t *testing.T) {
		stack := &fakeComposeStack{ps: leftovers}
		a, rec, _ := libraryDownApp(t, func(context.Context) (composeStack, error) { return stack, nil })

		a.ComposeDown()

		if stack.downGrace != time.Second {
			t.Fatalf("the library down stopped containers with a %v grace; want the shell-out's --timeout 1", stack.downGrace)
		}
		if rec.didRemove("id-db") || len(rec.netLabels) != 0 {
			t.Fatalf("a library down that succeeded was swept: removed %v, networks listed by %v", rec.removedIDs(), rec.netLabels)
		}
	})
}

// A down through the compose library runs under the project's progress bound,
// as a shell-out down does: one whose project never moves is cut at the stall
// bound and its leftovers swept, never left to hang the teardown.
func TestALibraryDownThatMakesNoProgressIsCut(t *testing.T) {
	shortDownStall(t, 500*time.Millisecond)
	stack := &fakeComposeStack{downBlocks: true}
	a, _, _ := libraryDownApp(t, func(context.Context) (composeStack, error) { return stack, nil })

	done := make(chan struct{})
	go func() {
		a.ComposeDown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("a library down whose project never moved was not cut at the stall bound of %s", composeDownStall)
	}
	if stack.downCall != 1 || !stack.downCtxEnded.Load() {
		t.Fatalf("the library down ran %d times and saw its context end: %v; want once, cut", stack.downCall, stack.downCtxEnded.Load())
	}
}
