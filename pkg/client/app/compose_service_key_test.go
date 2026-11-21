package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
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

func (k keyedCompose) FindContainerInCompose(_ context.Context, content []byte, scope docker.ComposeScope, _ string) (*docker.ComposeServiceInfo, error) {
	*k.scope = scope
	return &docker.ComposeServiceInfo{AppServiceName: "parse-server", Compose: parsedCompose(content)}, nil
}

func (k keyedCompose) ModifyComposeForAgent(_ *docker.Compose, _ models.SetupOptions, service string) error {
	*k.modified = service
	return nil
}

// parsedCompose is content parsed as the in-memory lookup parses it, for the
// fakes that answer it: nil when content is not a compose document.
func parsedCompose(content []byte) *docker.Compose {
	var c docker.Compose
	if err := yaml.Unmarshal(content, &c); err != nil {
		return nil
	}
	return &c
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
	prevLibrary := composeLibrarySupported
	t.Cleanup(func() { composeLibrarySupported = prevLibrary })
	inMemory := []byte("services:\n  parse-server:\n    image: x\n")
	for _, tc := range []struct {
		name    string
		cmd     string
		opts    models.SetupOptions
		library bool
		want    docker.ComposeScope
	}{
		{name: "files", cmd: "docker compose -p psst --project-directory /srv/ps --env-file ci.env --compatibility -f /srv/ps/docker-compose.yml up",
			opts: models.SetupOptions{Container: "psst-1-app"},
			want: docker.ComposeScope{ProjectName: "psst", ProjectDir: "/srv/ps", EnvFiles: []string{"ci.env"}, Compatibility: true}},
		{name: "files, a build with the compose library", cmd: "docker compose -p psst -f /srv/ps/docker-compose.yml up",
			opts: models.SetupOptions{Container: "psst-1-app"}, library: true,
			want: docker.ComposeScope{ProjectName: "psst"}},
		{name: "in memory, run by the docker compose CLI", cmd: "docker compose -p psst --project-directory /srv/ps --env-file ci.env --compatibility up",
			opts: models.SetupOptions{Container: "psst-1-app", InMemoryCompose: inMemory},
			want: docker.ComposeScope{ProjectName: "psst", ProjectDir: "/srv/ps", EnvFiles: []string{"ci.env"}, Compatibility: true}},
		{name: "in memory, run by the compose library", cmd: "docker compose -p psst --compatibility up",
			opts: models.SetupOptions{Container: "psst-1-app", InMemoryCompose: inMemory}, library: true,
			want: docker.ComposeScope{ProjectName: "psst", Compatibility: true, InProcess: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			composeLibrarySupported = func() bool { return tc.library }
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
			if scope.ProjectName != tc.want.ProjectName || scope.ProjectDir != tc.want.ProjectDir || !slices.Equal(scope.EnvFiles, tc.want.EnvFiles) ||
				scope.Compatibility != tc.want.Compatibility || scope.InProcess != tc.want.InProcess {
				t.Fatalf("the lookup was given scope %+v, want %+v", scope, tc.want)
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
		{"docker compose --compatibility up", docker.ComposeScope{Compatibility: true}},
		{"docker-compose --compatibility -p proj up", docker.ComposeScope{ProjectName: "proj", Compatibility: true}},
		{"docker compose --compatibility=true up", docker.ComposeScope{Compatibility: true}},
		{"docker compose --compatibility=false up", docker.ComposeScope{}},
		{"docker compose --compatibility --compatibility=0 up", docker.ComposeScope{}},
		{"docker compose --compatibility=0 --compatibility up", docker.ComposeScope{Compatibility: true}},
		{"docker compose up --compatibility", docker.ComposeScope{}},
		{"./seed.sh --compatibility && docker compose up", docker.ComposeScope{}},
	} {
		got := composeScope(tc.cmd)
		if got.ProjectName != tc.want.ProjectName || got.ProjectDir != tc.want.ProjectDir || !slices.Equal(got.EnvFiles, tc.want.EnvFiles) ||
			got.Compatibility != tc.want.Compatibility || got.InProcess {
			t.Errorf("composeScope(%q) = %+v, want %+v", tc.cmd, got, tc.want)
		}
	}
}
