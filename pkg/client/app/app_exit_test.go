package app

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// ExitedState is what tells an agent that went with the app (compose stops
// the agent the moment the app exits) from a replaced one: only an app
// container that no longer runs may count as an exit, and only as docker
// reports it.
func TestExitedState(t *testing.T) {
	const at, later = "2026-10-09T11:06:52Z", "2026-10-09T11:06:53Z"
	restarting := ended("restarting", 1, at, later)
	restarting.Running, restarting.Restarting = true, true
	for _, tc := range []struct {
		name      string
		container string
		state     *container.State // nil: docker does not know the container
		exited    bool
		code      int
	}{
		{"exited with a code of its own", "app", ended("exited", 1, at, later), true, 1},
		{"dead", "app", ended("dead", 137, at, later), true, 137},
		{"restarting after an exit", "app", restarting, true, 1},
		{"running", "app", ended("running", 0, at, ""), false, 0},
		{"never started", "app", ended("created", 0, neverStarted, neverStarted), false, 0},
		{"gone", "app", nil, false, 0},
		{"no app container", "", ended("exited", 1, at, later), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &stoppedAgentDocker{states: map[string]*container.State{}}
			if tc.state != nil {
				d.states["app"] = tc.state
			}
			a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose, container: tc.container}

			name, state, exited := a.ExitedState(context.Background())
			if exited != tc.exited {
				t.Fatalf("ExitedState exited = %v, want %v", exited, tc.exited)
			}
			if !exited {
				return
			}
			if name != "app" || state.ExitCode != tc.code {
				t.Fatalf("ExitedState = %q, code %d; want %q, code %d", name, state.ExitCode, "app", tc.code)
			}
		})
	}
}
