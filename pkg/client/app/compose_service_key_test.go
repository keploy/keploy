package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// keyedCompose answers SetupCompose's lookups with a service whose key is not
// the name keploy was given, and records what it is asked.
type keyedCompose struct {
	docker.Client
	scope    *docker.ComposeScope
	modified *string
}

func (k keyedCompose) FindContainerInComposeFiles(_ context.Context, paths []string, scope docker.ComposeScope, _ string) (*docker.ComposeServiceInfo, error) {
	*k.scope = scope
	return &docker.ComposeServiceInfo{AppServiceName: "parse-server", ComposePath: paths[0], Compose: &docker.Compose{}}, nil
}

func (k keyedCompose) FindContainerInCompose(c *docker.Compose, _ string) (*docker.ComposeServiceInfo, error) {
	return &docker.ComposeServiceInfo{AppServiceName: "parse-server", Compose: c}, nil
}

func (k keyedCompose) ModifyComposeForAgent(_ *docker.Compose, _ models.SetupOptions, service string) error {
	*k.modified = service
	return nil
}

var errHookStop = errors.New("stop after the hook")

type serviceHook struct {
	Hooks
	got *string
}

func (h serviceHook) BeforeDockerComposeSetup(_ context.Context, _ *docker.Compose, service string) (bool, error) {
	*h.got = service
	return false, errHookStop
}

// After the lookup, the app's service is named by its key: the compose file
// may write its container_name as a variable, which only the resolved model
// matches, so the name keploy was given finds nothing in the document.
func TestSetupCompose_NamesTheServiceByItsKey(t *testing.T) {
	prev := HookImpl
	t.Cleanup(func() { HookImpl = prev })
	for _, tc := range []struct {
		name string
		cmd  string
		opts models.SetupOptions
	}{
		{"files", "docker compose -p psst --project-directory /srv/ps --env-file ci.env -f /srv/ps/docker-compose.yml up",
			models.SetupOptions{Container: "psst-1-app"}},
		{"in memory", "docker compose up",
			models.SetupOptions{Container: "psst-1-app", InMemoryCompose: []byte("services:\n  parse-server:\n    image: x\n")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var scope docker.ComposeScope
			var modified, hooked string
			HookImpl = serviceHook{got: &hooked}
			a := NewApp(zap.NewNop(), tc.cmd, keyedCompose{scope: &scope, modified: &modified}, tc.opts)
			if err := a.SetupCompose(context.Background(), nil); !errors.Is(err, errHookStop) {
				t.Fatalf("SetupCompose: %v, want the hook's stop", err)
			}
			if modified != "parse-server" || hooked != "parse-server" {
				t.Fatalf("ModifyComposeForAgent got %q and the hook %q, want both the key %q", modified, hooked, "parse-server")
			}
			if tc.opts.InMemoryCompose == nil {
				want := docker.ComposeScope{ProjectName: "psst", ProjectDir: "/srv/ps", EnvFiles: []string{"ci.env"}}
				if scope.ProjectName != want.ProjectName || scope.ProjectDir != want.ProjectDir || !slices.Equal(scope.EnvFiles, want.EnvFiles) {
					t.Fatalf("the lookup was given scope %+v, want %+v", scope, want)
				}
			}
		})
	}
}

func TestComposeScope(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want docker.ComposeScope
	}{
		{"docker compose up", docker.ComposeScope{}},
		{"docker compose -p proj up", docker.ComposeScope{ProjectName: "proj"}},
		{"COMPOSE_PROJECT_NAME=Assigned docker compose up", docker.ComposeScope{ProjectName: "assigned"}},
		{"COMPOSE_PROJECT_NAME=assigned docker compose -p flag up", docker.ComposeScope{ProjectName: "flag"}},
		{"docker compose --project-directory d --env-file a.env --env-file=b.env up", docker.ComposeScope{ProjectDir: "d", EnvFiles: []string{"a.env", "b.env"}}},
	} {
		got := composeScope(tc.cmd)
		if got.ProjectName != tc.want.ProjectName || got.ProjectDir != tc.want.ProjectDir || !slices.Equal(got.EnvFiles, tc.want.EnvFiles) {
			t.Errorf("composeScope(%q) = %+v, want %+v", tc.cmd, got, tc.want)
		}
	}
}
