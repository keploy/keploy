package app

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// NewApp used to derive its kind from the command string, which is why
// --cmd-type could not work end to end (#4399): agent.go honoured
// opts.CommandType while this App did not, so `--cmd-type docker-compose -c
// "make up"` left the agent expecting a compose project SetupCompose was
// never asked to create. a.kind drives Setup / SetupCompose / Run / Stop, so
// this is the value that decides what actually happens.
func TestResolveKind(t *testing.T) {
	cases := []struct {
		name        string
		commandType string
		cmd         string
		want        utils.CmdType
	}{
		// The case the whole feature exists for.
		{"explicit compose beats an undetectable command", "docker-compose", "make up", utils.DockerCompose},
		{"explicit docker-run beats a plain command", "docker-run", "./start.sh", utils.DockerRun},
		{"explicit native beats a docker-looking command", "native", "docker compose up", utils.Native},

		// Unset or unusable values must behave exactly as before.
		{"unset falls back to detection", "", "docker compose up", utils.DockerCompose},
		{"unset falls back to detection, plain", "", "python app.py", utils.Native},
		{"garbage falls back to detection", "not-a-kind", "docker run --name x img", utils.DockerRun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveKind(tc.commandType, tc.cmd); got != tc.want {
				t.Errorf("resolveKind(%q, %q) = %q, want %q", tc.commandType, tc.cmd, got, tc.want)
			}
		})
	}
}

// resolveKind being correct is useless if NewApp does not call it, so pin
// the wiring too — a revert to FindDockerCmd(cmd) here would leave the
// function above green while the feature stayed broken.
func TestNewApp_UsesResolvedCommandType(t *testing.T) {
	app := NewApp(zap.NewNop(), "make up", nil, models.SetupOptions{CommandType: "docker-compose"})
	if got := app.Kind(context.Background()); got != utils.DockerCompose {
		t.Errorf("App.Kind = %q, want %q — NewApp is deriving the kind from the command "+
			"string again and ignoring the resolved --cmd-type", got, utils.DockerCompose)
	}

	// Unset CommandType must still auto-detect, as every existing caller relies on.
	app = NewApp(zap.NewNop(), "docker compose up", nil, models.SetupOptions{})
	if got := app.Kind(context.Background()); got != utils.DockerCompose {
		t.Errorf("App.Kind = %q, want %q from auto-detection", got, utils.DockerCompose)
	}
}
