//go:build composelib && !darwin

// The two sweep cases that take the compose LIBRARY path. Split out of
// compose_project_sweep_test.go rather than tagging that whole file, which
// also defines helpers the untagged tests in this package use.

package app

import (
	"context"
	"strings"
	"testing"

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
