package app

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
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
		{name: "in memory, run by the compose library", cmd: "COMPOSE_PROJECT_NAME=inline APP_CONTAINER=x docker compose -p psst --compatibility up",
			opts: models.SetupOptions{Container: "psst-1-app", InMemoryCompose: inMemory}, library: true,
			want: docker.ComposeScope{ProjectName: "psst", Compatibility: true, InProcess: true}},
		{name: "in memory, run by the compose library without -p", cmd: "COMPOSE_PROJECT_NAME=inline APP_CONTAINER=x docker compose --compatibility up",
			opts: models.SetupOptions{Container: "psst-1-app", InMemoryCompose: inMemory}, library: true,
			want: docker.ComposeScope{ProjectName: "", Compatibility: true, InProcess: true}},
		{name: "files with inline env assignments", cmd: "COMPOSE_COMPATIBILITY=true APP_CONTAINER=inline-app docker compose -f /srv/ps/docker-compose.yml up",
			opts: models.SetupOptions{Container: "inline-app"},
			want: docker.ComposeScope{Env: map[string]string{"COMPOSE_COMPATIBILITY": "true", "APP_CONTAINER": "inline-app"}}},
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
				scope.Compatibility != tc.want.Compatibility || scope.InProcess != tc.want.InProcess || !maps.Equal(scope.Env, tc.want.Env) {
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
		{"COMPOSE_PROJECT_NAME=Assigned docker compose up", docker.ComposeScope{ProjectName: "assigned", Env: map[string]string{"COMPOSE_PROJECT_NAME": "Assigned"}}},
		{"COMPOSE_PROJECT_NAME=assigned docker compose -p flag up", docker.ComposeScope{ProjectName: "flag", Env: map[string]string{"COMPOSE_PROJECT_NAME": "assigned"}}},
		{"docker compose --project-directory d --env-file a.env --env-file=b.env up", docker.ComposeScope{ProjectDir: "d", EnvFiles: []string{"a.env", "b.env"}}},
		{"docker compose --compatibility up", docker.ComposeScope{Compatibility: true}},
		{"docker-compose --compatibility -p proj up", docker.ComposeScope{ProjectName: "proj", Compatibility: true}},
		{"docker compose --compatibility=true up", docker.ComposeScope{Compatibility: true}},
		{"docker compose --compatibility=false up", docker.ComposeScope{}},
		{"docker compose --compatibility --compatibility=0 up", docker.ComposeScope{}},
		{"docker compose --compatibility=0 --compatibility up", docker.ComposeScope{Compatibility: true}},
		{"docker compose up --compatibility", docker.ComposeScope{}},
		{"./seed.sh --compatibility && docker compose up", docker.ComposeScope{}},
		// Leading inline NAME=value assignments before the program.
		{"COMPOSE_COMPATIBILITY=true docker compose up", docker.ComposeScope{Env: map[string]string{"COMPOSE_COMPATIBILITY": "true"}}},
		{"COMPOSE_COMPATIBILITY=true APP_CONTAINER=my-app docker compose up", docker.ComposeScope{Env: map[string]string{"COMPOSE_COMPATIBILITY": "true", "APP_CONTAINER": "my-app"}}},
		{"COMPOSE_ENV_FILES=ci.env COMPOSE_DISABLE_ENV_FILE=1 docker compose up", docker.ComposeScope{Env: map[string]string{"COMPOSE_ENV_FILES": "ci.env", "COMPOSE_DISABLE_ENV_FILE": "1"}}},
		// Quoted values (double, single, escaped, mixed, empty, backslash).
		{`APP_CONTAINER="my app" docker compose up`, docker.ComposeScope{Env: map[string]string{"APP_CONTAINER": "my app"}}},
		{`APP_CONTAINER='my app' docker compose up`, docker.ComposeScope{Env: map[string]string{"APP_CONTAINER": "my app"}}},
		{`APP_CONTAINER="a\"b\\c\$d" docker compose up`, docker.ComposeScope{Env: map[string]string{"APP_CONTAINER": `a"b\c$d`}}},
		{`APP_CONTAINER='a\"b' docker compose up`, docker.ComposeScope{Env: map[string]string{"APP_CONTAINER": `a\"b`}}},
		{`APP_CONTAINER=a'b c'"d e"f docker compose up`, docker.ComposeScope{Env: map[string]string{"APP_CONTAINER": "ab cd ef"}}},
		{`APP_CONTAINER= docker compose up`, docker.ComposeScope{Env: map[string]string{"APP_CONTAINER": ""}}},
		{`APP_CONTAINER="" docker compose up`, docker.ComposeScope{Env: map[string]string{"APP_CONTAINER": ""}}},
		{`APP_CONTAINER=a\ b docker compose up`, docker.ComposeScope{Env: map[string]string{"APP_CONTAINER": "a b"}}},
		{`APP_CONTAINER=first APP_CONTAINER=second docker compose up`, docker.ComposeScope{Env: map[string]string{"APP_CONTAINER": "second"}}},
		// Non-assignment words and command separators stop the run.
		{"APP_CONTAINER=before docker compose up OTHER=after", docker.ComposeScope{Env: map[string]string{"APP_CONTAINER": "before"}}},
		{"docker compose up APP_CONTAINER=after", docker.ComposeScope{}},
		{"docker APP_CONTAINER=after compose up", docker.ComposeScope{}},
		{`'APP_CONTAINER=quoted' docker compose up`, docker.ComposeScope{}},
		{"1BAD=val docker compose up", docker.ComposeScope{}},
		{"BAD-NAME=val docker compose up", docker.ComposeScope{}},
		{"-f=compose.yml docker compose up", docker.ComposeScope{}},
		{"APP_CONTAINER=before; docker compose up", docker.ComposeScope{}},
		{"APP_CONTAINER=before && docker compose up", docker.ComposeScope{}},
		{"APP_CONTAINER=before\ndocker compose up", docker.ComposeScope{}},
		{`APP_CONTAINER="unclosed docker compose up`, docker.ComposeScope{}},
		{"COMPOSE_PROJECT_NAME=p COMPOSE_COMPATIBILITY=true sudo docker compose up", docker.ComposeScope{}},
	} {
		got := composeScope(tc.cmd)
		if got.ProjectName != tc.want.ProjectName || got.ProjectDir != tc.want.ProjectDir || !slices.Equal(got.EnvFiles, tc.want.EnvFiles) ||
			got.Compatibility != tc.want.Compatibility || got.InProcess || !maps.Equal(got.Env, tc.want.Env) {
			t.Errorf("composeScope(%q) = %+v, want %+v", tc.cmd, got, tc.want)
		}
	}
}

// TestComposeScope_ResolvesInlineAssignmentsEndToEnd verifies that composeScope
// feeds leading inline NAME=value assignments from the -c command through to
// compose-go resolution on both file and in-memory paths.
func TestComposeScope_ResolvesInlineAssignmentsEndToEnd(t *testing.T) {
	const composeYAML = `services:
  app:
    image: x
    container_name: ${APP_CONTAINER:-default-app}
  worker:
    image: y
`
	for _, tc := range []struct {
		name        string
		cmd         string
		files       map[string]string
		env         map[string]string
		container   string
		wantService string
		wantErr     string
	}{
		{
			name:        "inline COMPOSE_COMPATIBILITY=true matches underscore separator",
			cmd:         "COMPOSE_COMPATIBILITY=true docker compose -f compose.yml up",
			container:   "proj_worker_1",
			wantService: "worker",
		},
		{
			name:      "inline COMPOSE_COMPATIBILITY=true refuses hyphen separator",
			cmd:       "COMPOSE_COMPATIBILITY=true docker compose -f compose.yml up",
			container: "proj-worker-1",
			wantErr:   `worker: "proj_worker_1"`,
		},
		{
			name:        "inline APP_CONTAINER unset uses default",
			cmd:         "docker compose -f compose.yml up",
			container:   "default-app",
			wantService: "app",
		},
		{
			name:        "inline APP_CONTAINER set resolves to inline value",
			cmd:         "APP_CONTAINER=inline-app docker compose -f compose.yml up",
			container:   "inline-app",
			wantService: "app",
		},
		{
			name:      "inline APP_CONTAINER set refuses default",
			cmd:       "APP_CONTAINER=inline-app docker compose -f compose.yml up",
			container: "default-app",
			wantErr:   `app: "inline-app"`,
		},
		{
			name:        "inline COMPOSE_ENV_FILES loads named env file instead of .env",
			cmd:         "COMPOSE_ENV_FILES=ci.env docker compose -f compose.yml up",
			files:       map[string]string{".env": "APP_CONTAINER=from-dotenv\n", "ci.env": "APP_CONTAINER=from-ci\n"},
			container:   "from-ci",
			wantService: "app",
		},
		{
			name:        "inline COMPOSE_DISABLE_ENV_FILE=1 ignores .env",
			cmd:         "COMPOSE_DISABLE_ENV_FILE=1 docker compose -f compose.yml up",
			files:       map[string]string{".env": "APP_CONTAINER=from-dotenv\n"},
			container:   "default-app",
			wantService: "app",
		},
		{
			name:        "inline APP_CONTAINER beats exported env",
			cmd:         "APP_CONTAINER=inline-wins docker compose -f compose.yml up",
			env:         map[string]string{"APP_CONTAINER": "exported-loses"},
			container:   "inline-wins",
			wantService: "app",
		},
		{
			name:        "quoted inline value",
			cmd:         `APP_CONTAINER="quoted-app" docker compose -f compose.yml up`,
			container:   "quoted-app",
			wantService: "app",
		},
		{
			name:        "non-assignment word stops inline run",
			cmd:         "docker compose -f compose.yml up APP_CONTAINER=after-program",
			container:   "default-app",
			wantService: "app",
		},
		{
			name:        "inline COMPOSE_COMPATIBILITY=$C expands from exported env",
			cmd:         "COMPOSE_COMPATIBILITY=$C docker compose -f compose.yml up",
			env:         map[string]string{"C": "true"},
			container:   "proj_worker_1",
			wantService: "worker",
		},
		{
			name:        "inline COMPOSE_COMPATIBILITY=$C expands from earlier inline assignment",
			cmd:         "C=true COMPOSE_COMPATIBILITY=$C docker compose -f compose.yml up",
			container:   "proj_worker_1",
			wantService: "worker",
		},
		{
			name:      "inline command substitution is refused fail-closed",
			cmd:       "APP_CONTAINER=$(whoami) docker compose -f compose.yml up",
			container: "default-app",
			wantErr:   "command substitution",
		},
		{
			name:      "inline backtick substitution is refused fail-closed",
			cmd:       "APP_CONTAINER=`whoami` docker compose -f compose.yml up",
			container: "default-app",
			wantErr:   "command substitution",
		},
	} {
		for _, inMemory := range []bool{false, true} {
			mode := "files"
			if inMemory {
				mode = "in-memory"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				for _, k := range []string{"APP_CONTAINER", "COMPOSE_COMPATIBILITY", "COMPOSE_ENV_FILES", "COMPOSE_DISABLE_ENV_FILE", "COMPOSE_PROJECT_NAME"} {
					if v, ok := os.LookupEnv(k); ok {
						t.Cleanup(func() { _ = os.Setenv(k, v) })
					} else {
						t.Cleanup(func() { _ = os.Unsetenv(k) })
					}
					_ = os.Unsetenv(k)
				}
				for k, v := range tc.env {
					t.Setenv(k, v)
				}
				dir := filepath.Join(t.TempDir(), "proj")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				composePath := filepath.Join(dir, "compose.yml")
				if err := os.WriteFile(composePath, []byte(composeYAML), 0o644); err != nil {
					t.Fatal(err)
				}
				for name, body := range tc.files {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				t.Chdir(dir)

				idc, err := docker.New(zap.NewNop(), &config.Config{})
				if err != nil {
					t.Fatalf("docker.New: %v", err)
				}
				scope := composeScope(tc.cmd)
				var info *docker.ComposeServiceInfo
				if inMemory {
					info, err = idc.FindContainerInCompose(context.Background(), []byte(composeYAML), scope, tc.container)
				} else {
					info, err = idc.FindContainerInComposeFiles(context.Background(), []string{composePath}, scope, tc.container)
				}
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("got %v, %v; want error with %q", info, err, tc.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if info.AppServiceName != tc.wantService {
					t.Fatalf("got service %q, want %q", info.AppServiceName, tc.wantService)
				}
			})
		}
	}
}

// TestSetupCompose_PreservesProjectDirectoryForSubdirComposeFile ensures that
// when the user's compose file lives in a subdirectory (via -f or inline
// COMPOSE_FILE) and no --project-directory is set, SetupCompose preserves that
// directory via --project-directory on the rewritten command so docker compose
// derives the same project name, reads the same .env, and resolves relative
// paths against the original directory rather than the cwd where
// docker-compose-tmp.yaml is written.
func TestSetupCompose_PreservesProjectDirectoryForSubdirComposeFile(t *testing.T) {
	prev := HookImpl
	t.Cleanup(func() { HookImpl = prev })
	HookImpl = nil

	const subCompose = "services:\n  app:\n    image: alpine:3.21\n"
	for _, tc := range []struct {
		name           string
		cmd            string
		container      string
		wantProjectDir string
		wantContains   string
		wantNotContain string
	}{
		{
			name:           "-f sub/compose.yml adds --project-directory sub",
			cmd:            "docker compose -f sub/compose.yml up",
			container:      "sub-app-1",
			wantProjectDir: "sub",
			wantContains:   "-f ./docker-compose-tmp.yaml --project-directory sub up",
			wantNotContain: "sub/compose.yml",
		},
		{
			name:           "--file sub/compose.yml replaces flag and adds --project-directory sub",
			cmd:            "docker compose --file sub/compose.yml up",
			container:      "sub-app-1",
			wantProjectDir: "sub",
			wantContains:   "-f ./docker-compose-tmp.yaml --project-directory sub up",
			wantNotContain: "sub/compose.yml",
		},
		{
			name:           "-f=sub/compose.yml replaces flag and adds --project-directory sub",
			cmd:            "docker compose -f=sub/compose.yml up",
			container:      "sub-app-1",
			wantProjectDir: "sub",
			wantContains:   "-f ./docker-compose-tmp.yaml --project-directory sub up",
			wantNotContain: "sub/compose.yml",
		},
		{
			name:           "--file=sub/compose.yml replaces flag and adds --project-directory sub",
			cmd:            "docker compose --file=sub/compose.yml up",
			container:      "sub-app-1",
			wantProjectDir: "sub",
			wantContains:   "-f ./docker-compose-tmp.yaml --project-directory sub up",
			wantNotContain: "sub/compose.yml",
		},
		{
			name:           "non-up subcommand (run) places --project-directory before subcommand",
			cmd:            "docker compose -f sub/compose.yml run app",
			container:      "sub-app-1",
			wantProjectDir: "sub",
			wantContains:   "-f ./docker-compose-tmp.yaml --project-directory sub run app",
			wantNotContain: "sub/compose.yml",
		},
		{
			name:           "subcommand outside fixed list (watch) places --project-directory next to -f",
			cmd:            "docker compose -f sub/compose.yml watch",
			container:      "sub-app-1",
			wantProjectDir: "sub",
			wantContains:   "-f ./docker-compose-tmp.yaml --project-directory sub watch",
			wantNotContain: "sub/compose.yml",
		},
		{
			name:           "inline COMPOSE_FILE=sub/compose.yml with watch places -f and --project-directory before watch",
			cmd:            "COMPOSE_FILE=sub/compose.yml docker compose watch",
			container:      "sub-app-1",
			wantProjectDir: "sub",
			wantContains:   "-f ./docker-compose-tmp.yaml --project-directory sub watch",
		},
		{
			name:           "chained subdir -f sub/compose.yml build && -f sub/compose.yml up preserves --project-directory on both",
			cmd:            "docker compose -f sub/compose.yml build && docker compose -f sub/compose.yml up",
			container:      "sub-app-1",
			wantProjectDir: "sub",
			wantContains:   "docker compose -f ./docker-compose-tmp.yaml --project-directory sub build && docker compose -f ./docker-compose-tmp.yaml --project-directory sub up",
			wantNotContain: "sub/compose.yml",
		},
		{
			name:           "inline COMPOSE_FILE=sub/compose.yml adds --project-directory sub",
			cmd:            "COMPOSE_FILE=sub/compose.yml docker compose up",
			container:      "sub-app-1",
			wantProjectDir: "sub",
			wantContains:   "-f ./docker-compose-tmp.yaml --project-directory sub up",
		},
		{
			name:           "explicit --project-directory is kept without duplicating",
			cmd:            "docker compose --project-directory custom -f sub/compose.yml up",
			container:      "custom-app-1",
			wantProjectDir: "custom",
			wantContains:   "--project-directory custom -f ./docker-compose-tmp.yaml up",
			wantNotContain: "--project-directory sub",
		},
		{
			name:           "cwd compose file adds no --project-directory",
			cmd:            "docker compose -f compose.yml up",
			container:      "rootproj-app-1",
			wantNotContain: "--project-directory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR", "COMPOSE_COMPATIBILITY"} {
				t.Setenv(k, "")
			}
			root := filepath.Join(t.TempDir(), "rootproj")
			sub := filepath.Join(root, "sub")
			custom := filepath.Join(root, "custom")
			for _, d := range []string{root, sub, custom} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, p := range []string{filepath.Join(root, "compose.yml"), filepath.Join(sub, "compose.yml")} {
				if err := os.WriteFile(p, []byte(subCompose), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(root)

			idc, err := docker.New(zap.NewNop(), &config.Config{})
			if err != nil {
				t.Fatalf("docker.New: %v", err)
			}
			opts := models.SetupOptions{Container: tc.container, AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791}
			a := NewApp(zap.NewNop(), tc.cmd, idc, opts)
			if err := a.SetupCompose(context.Background(), nil); err != nil {
				t.Fatalf("SetupCompose(%q): %v", tc.cmd, err)
			}
			gotCmd := a.cmd.WholeUnredacted()
			if tc.wantContains != "" && !strings.Contains(gotCmd, tc.wantContains) {
				t.Fatalf("rewritten cmd = %q, want substring %q", gotCmd, tc.wantContains)
			}
			if tc.wantNotContain != "" && strings.Contains(gotCmd, tc.wantNotContain) {
				t.Fatalf("rewritten cmd = %q, must not contain %q", gotCmd, tc.wantNotContain)
			}
			_, gotDir := composeProjectScope(gotCmd)
			if gotDir != tc.wantProjectDir {
				t.Fatalf("composeProjectScope(%q).projectDir = %q, want %q", gotCmd, gotDir, tc.wantProjectDir)
			}
		})
	}

	for _, tc := range []struct {
		dir  string
		want string
	}{
		{"my sub", "--project-directory 'my sub' up"},
		{"#sub", "--project-directory '#sub' up"},
		{"sub[1]", "--project-directory 'sub[1]' up"},
		{"~sub", "--project-directory '~sub' up"},
	} {
		if got := withComposeProjectDirectory("docker compose -f ./docker-compose-tmp.yaml up", "docker-compose-tmp.yaml", tc.dir); !strings.Contains(got, tc.want) {
			t.Fatalf("withComposeProjectDirectory(%q) = %q, want substring %q", tc.dir, got, tc.want)
		}
	}
}

// TestSetupCompose_PreservesAllFilesFromComposeFileAndOverride ensures that
// when the command has no explicit -f/--file flag (multi-file COMPOSE_FILE or
// default docker-compose.yml + docker-compose.override.yml), rewriting the
// command emits -f for every discovered file in order with keploy's temp file
// taking the app file's slot. Passing a single -f would make compose ignore
// COMPOSE_FILE and skip docker-compose.override.yml, dropping overrides such
// as container_name that keploy just matched against.
func TestSetupCompose_PreservesAllFilesFromComposeFileAndOverride(t *testing.T) {
	prev := HookImpl
	t.Cleanup(func() { HookImpl = prev })
	HookImpl = nil

	for _, tc := range []struct {
		name           string
		files          map[string]string
		cmd            string
		container      string
		wantContains   string
		wantNotContain string
		wantProjectDir string
	}{
		{
			name: "inline COMPOSE_FILE two files, app defined in first, container_name in second",
			files: map[string]string{
				"base.yml":  "services:\n  app:\n    image: alpine:3.21\n",
				"extra.yml": "services:\n  app:\n    container_name: prod-app\n",
			},
			cmd:            "COMPOSE_FILE=base.yml:extra.yml docker compose up",
			container:      "prod-app",
			wantContains:   "-f ./docker-compose-tmp.yaml -f extra.yml up",
			wantNotContain: "-f base.yml",
		},
		{
			name: "inline COMPOSE_FILE two files, app defined in second keeps first in order",
			files: map[string]string{
				"base.yml":  "services:\n  db:\n    image: alpine:3.21\n",
				"extra.yml": "services:\n  app:\n    image: alpine:3.21\n    container_name: second-app\n",
			},
			cmd:            "COMPOSE_FILE=base.yml:extra.yml docker compose up",
			container:      "second-app",
			wantContains:   "-f base.yml -f ./docker-compose-tmp.yaml up",
			wantNotContain: "-f extra.yml",
		},
		{
			name: "default docker-compose.yml plus docker-compose.override.yml keeps override",
			files: map[string]string{
				"docker-compose.yml":          "services:\n  app:\n    image: alpine:3.21\n",
				"docker-compose.override.yml": "services:\n  app:\n    container_name: override-app\n",
			},
			cmd:            "docker compose up",
			container:      "override-app",
			wantContains:   "-f ./docker-compose-tmp.yaml -f docker-compose.override.yml up",
			wantNotContain: "-f docker-compose.yml",
		},
		{
			name: "subdirectory multi-file COMPOSE_FILE preserves project dir and second file",
			files: map[string]string{
				"sub/base.yml":  "services:\n  app:\n    image: alpine:3.21\n",
				"sub/extra.yml": "services:\n  app:\n    container_name: sub-multi-app\n",
			},
			cmd:            "COMPOSE_FILE=sub/base.yml:sub/extra.yml docker compose up",
			container:      "sub-multi-app",
			wantContains:   "-f ./docker-compose-tmp.yaml --project-directory sub -f sub/extra.yml up",
			wantNotContain: "-f sub/base.yml",
			wantProjectDir: "sub",
		},
		{
			name: "inline COMPOSE_FILE two files with non-up subcommand (run) inserts -f flags before run",
			files: map[string]string{
				"base.yml":  "services:\n  app:\n    image: alpine:3.21\n",
				"extra.yml": "services:\n  app:\n    container_name: prod-run-app\n",
			},
			cmd:            "COMPOSE_FILE=base.yml:extra.yml docker compose run app",
			container:      "prod-run-app",
			wantContains:   "-f ./docker-compose-tmp.yaml -f extra.yml run app",
			wantNotContain: "-f base.yml",
		},
		{
			name: "chained build && up splices -f flags before up, not before build",
			files: map[string]string{
				"docker-compose.yml":          "services:\n  app:\n    image: alpine:3.21\n",
				"docker-compose.override.yml": "services:\n  app:\n    container_name: chained-app\n",
			},
			cmd:            "docker compose build && docker compose up",
			container:      "chained-app",
			wantContains:   "docker compose build && docker compose -f ./docker-compose-tmp.yaml -f docker-compose.override.yml up",
			wantNotContain: "-f ./docker-compose-tmp.yaml build",
		},
		{
			name: "chained explicit -f compose.yml build && -f compose.yml up replaces both without clobbering primary",
			files: map[string]string{
				"compose.yml": "services:\n  app:\n    image: alpine:3.21\n    ports:\n      - \"8080:8080\"\n",
			},
			cmd:            "docker compose -f compose.yml build && docker compose -f compose.yml up",
			container:      "rootproj-app-1",
			wantContains:   "docker compose -f ./docker-compose-tmp.yaml build && docker compose -f ./docker-compose-tmp.yaml up",
			wantNotContain: "docker-compose-tmp-1.yaml",
		},
		{
			name: "duplicate inline COMPOSE_FILE replaces every occurrence with primary temp file",
			files: map[string]string{
				"compose.yml": "services:\n  app:\n    image: alpine:3.21\n    ports:\n      - \"8080:8080\"\n",
			},
			cmd:            "COMPOSE_FILE=compose.yml:compose.yml docker compose up",
			container:      "rootproj-app-1",
			wantContains:   "-f ./docker-compose-tmp.yaml -f ./docker-compose-tmp.yaml up",
			wantNotContain: "-f compose.yml",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR", "COMPOSE_COMPATIBILITY"} {
				t.Setenv(k, "")
			}
			root := filepath.Join(t.TempDir(), "rootproj")
			for rel, body := range tc.files {
				full := filepath.Join(root, rel)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(root)

			idc, err := docker.New(zap.NewNop(), &config.Config{})
			if err != nil {
				t.Fatalf("docker.New: %v", err)
			}
			opts := models.SetupOptions{Container: tc.container, AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791}
			a := NewApp(zap.NewNop(), tc.cmd, idc, opts)
			if err := a.SetupCompose(context.Background(), nil); err != nil {
				t.Fatalf("SetupCompose(%q): %v", tc.cmd, err)
			}
			gotCmd := a.cmd.WholeUnredacted()
			if !strings.Contains(gotCmd, tc.wantContains) {
				t.Fatalf("rewritten cmd = %q, want substring %q", gotCmd, tc.wantContains)
			}
			if tc.wantNotContain != "" && strings.Contains(gotCmd, tc.wantNotContain) {
				t.Fatalf("rewritten cmd = %q, must not contain %q", gotCmd, tc.wantNotContain)
			}
			_, gotDir := composeProjectScope(gotCmd)
			if gotDir != tc.wantProjectDir {
				t.Fatalf("composeProjectScope(%q).projectDir = %q, want %q", gotCmd, gotDir, tc.wantProjectDir)
			}
		})
	}
}

// TestSetupCompose_StripsConflictingKeysFromOverrideAndMovesToAgent verifies
// that when a companion compose file (default docker-compose.override.yml,
// multi-file COMPOSE_FILE, explicit -f a -f b, or wrapper `make up`) sets
// ports, networks, dns, or extra_hosts on the app service, SetupCompose moves
// those settings onto keploy-agent, strips the conflicting keys into a
// companion temp file (docker-compose-tmp-1.yaml), and passes both files to
// compose (or in COMPOSE_FILE for wrappers).
func TestSetupCompose_StripsConflictingKeysFromOverrideAndMovesToAgent(t *testing.T) {
	prev := HookImpl
	t.Cleanup(func() { HookImpl = prev })
	HookImpl = nil

	const baseYAML = `services:
  app:
    image: alpine:3.21
    ports:
      - "8080:8080"
`
	const overrideYAML = `services:
  app:
    container_name: override-app
    environment:
      - FOO=bar
    ports:
      - "8883:80"
    networks:
      - overnet
    dns:
      - "1.1.1.1"
    extra_hosts:
      - "api.local:127.0.0.1"
  sidecar:
    image: alpine:3.21
networks:
  overnet: {}
`

	for _, tc := range []struct {
		name           string
		baseName       string
		overrideName   string
		cmd            string
		isWrapper      bool
		wantContains   string
		wantNotContain string
	}{
		{
			name:           "default docker-compose.yml plus conflicting docker-compose.override.yml",
			baseName:       "docker-compose.yml",
			overrideName:   "docker-compose.override.yml",
			cmd:            "docker compose up",
			wantContains:   "-f ./docker-compose-tmp.yaml -f ./docker-compose-tmp-1.yaml up",
			wantNotContain: "docker-compose.override.yml",
		},
		{
			name:           "explicit -f base.yml -f extra.yml replaces both files",
			baseName:       "base.yml",
			overrideName:   "extra.yml",
			cmd:            "docker compose -f base.yml -f extra.yml up",
			wantContains:   "-f ./docker-compose-tmp.yaml -f ./docker-compose-tmp-1.yaml up",
			wantNotContain: "extra.yml",
		},
		{
			name:         "wrapper make up preserves both files in COMPOSE_FILE",
			baseName:     "docker-compose.yml",
			overrideName: "docker-compose.override.yml",
			cmd:          "make up",
			isWrapper:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR", "COMPOSE_COMPATIBILITY"} {
				t.Setenv(k, "")
			}
			root := filepath.Join(t.TempDir(), "rootproj")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, tc.baseName), []byte(baseYAML), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, tc.overrideName), []byte(overrideYAML), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)

			idc, err := docker.New(zap.NewNop(), &config.Config{})
			if err != nil {
				t.Fatalf("docker.New: %v", err)
			}
			opts := models.SetupOptions{Container: "override-app", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791}
			a := NewApp(zap.NewNop(), tc.cmd, idc, opts)
			if err := a.SetupCompose(context.Background(), nil); err != nil {
				t.Fatalf("SetupCompose(%q): %v", tc.cmd, err)
			}

			if tc.isWrapper {
				sep := string(filepath.ListSeparator)
				wantEnv := filepath.Join(root, "docker-compose-tmp.yaml") + sep + filepath.Join(root, "docker-compose-tmp-1.yaml")
				if a.composeFileEnv != wantEnv {
					t.Fatalf("composeFileEnv = %q, want %q", a.composeFileEnv, wantEnv)
				}
			} else {
				gotCmd := a.cmd.WholeUnredacted()
				if !strings.Contains(gotCmd, tc.wantContains) {
					t.Fatalf("rewritten cmd = %q, want substring %q", gotCmd, tc.wantContains)
				}
				if tc.wantNotContain != "" && strings.Contains(gotCmd, tc.wantNotContain) {
					t.Fatalf("rewritten cmd = %q, must not contain %q", gotCmd, tc.wantNotContain)
				}
			}

			primaryBytes, err := os.ReadFile(filepath.Join(root, "docker-compose-tmp.yaml"))
			if err != nil {
				t.Fatalf("read docker-compose-tmp.yaml: %v", err)
			}
			overrideBytes, err := os.ReadFile(filepath.Join(root, "docker-compose-tmp-1.yaml"))
			if err != nil {
				t.Fatalf("read docker-compose-tmp-1.yaml: %v", err)
			}
			primaryText := string(primaryBytes)
			overrideText := string(overrideBytes)

			for _, want := range []string{"8080:8080", "8883:80", "overnet", "1.1.1.1", "api.local:127.0.0.1"} {
				if !strings.Contains(primaryText, want) {
					t.Errorf("docker-compose-tmp.yaml missing %q moved to keploy-agent:\n%s", want, primaryText)
				}
			}
			for _, want := range []string{"override-app", "FOO=bar", "overnet"} {
				if !strings.Contains(overrideText, want) {
					t.Errorf("docker-compose-tmp-1.yaml lost %q:\n%s", want, overrideText)
				}
			}
			for _, forbidden := range []string{"8883:80", "1.1.1.1", "api.local:127.0.0.1", "ports:"} {
				if strings.Contains(overrideText, forbidden) {
					t.Errorf("docker-compose-tmp-1.yaml still has conflicting %q:\n%s", forbidden, overrideText)
				}
			}

			// Teardown and ps probes must receive BOTH rewritten files and track
			// services from every merged file (including companion-only sidecar).
			wantFileArgs := []string{"-f", "docker-compose-tmp.yaml", "-f", "docker-compose-tmp-1.yaml"}
			if gotArgs := a.composeFileArgs(); strings.Join(gotArgs, " ") != strings.Join(wantFileArgs, " ") {
				t.Fatalf("composeFileArgs() = %v, want %v", gotArgs, wantFileArgs)
			}
			for _, wantSvc := range []string{"app", "sidecar", "keploy-agent"} {
				foundSvc := false
				for _, s := range a.composeServices {
					if s == wantSvc {
						foundSvc = true
						break
					}
				}
				if !foundSvc {
					t.Fatalf("composeServices = %v, missing %q", a.composeServices, wantSvc)
				}
			}
		})
	}
}

// TestComposeCLICommand_InlineEnvAndMoveComposeAgent verifies that leading
// inline env assignments from -c are overlaid on keploy's compose CLI builder
// (including empty-value overrides), left nil when none are set or placed
// before sudo, and preserved when moveComposeAgent rewrites an in-memory
// compose document after a host port collision.
func TestComposeCLICommand_InlineEnvAndMoveComposeAgent(t *testing.T) {
	t.Run("SetupCompose records inline env and composeCLICommand overlays it over process env", func(t *testing.T) {
		for _, k := range []string{"COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR", "COMPOSE_COMPATIBILITY", "REQ_VAR"} {
			t.Setenv(k, "")
		}
		t.Setenv("APP_NETWORK", "exported-net")
		t.Setenv("EMPTY_OVERRIDE", "nonempty-exported")
		root := filepath.Join(t.TempDir(), "envproj")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		const doc = "services:\n  app:\n    image: alpine:3.21\n    container_name: env-app\n    environment:\n      - REQ=${REQ_VAR:?required}\n    networks: [appnet]\nnetworks:\n  appnet:\n    name: ${APP_NETWORK}\n"
		if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)
		idc, err := docker.New(zap.NewNop(), &config.Config{})
		if err != nil {
			t.Fatal(err)
		}
		opts := models.SetupOptions{Container: "env-app", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791}
		a := NewApp(zap.NewNop(), "APP_NETWORK=inline-net REQ_VAR=ok EMPTY_OVERRIDE= docker compose up", idc, opts)
		if err := a.SetupCompose(context.Background(), nil); err != nil {
			t.Fatalf("SetupCompose: %v", err)
		}
		cmd := a.composeCLICommand(context.Background(), "envproj", "down")
		if cmd == nil {
			t.Fatal("composeCLICommand returned nil")
		}
		if len(cmd.Env) == 0 {
			t.Fatalf("composeCLICommand.Env is empty, want overlaid inline env")
		}
		foundNet, foundReq, foundEmpty := false, false, false
		for _, kv := range cmd.Env {
			if kv == "APP_NETWORK=exported-net" || kv == "EMPTY_OVERRIDE=nonempty-exported" {
				t.Fatalf("composeCLICommand.Env kept exported value alongside inline override: %q", kv)
			}
			switch kv {
			case "APP_NETWORK=inline-net":
				foundNet = true
			case "REQ_VAR=ok":
				foundReq = true
			case "EMPTY_OVERRIDE=":
				foundEmpty = true
			}
		}
		if !foundNet || !foundReq || !foundEmpty {
			t.Fatalf("composeCLICommand.Env missing inline assignments (net=%v req=%v empty=%v)", foundNet, foundReq, foundEmpty)
		}
		// Plain command with no inline env leaves cmd.Env nil (inherits parent env).
		plain := &App{composeFile: "docker-compose-tmp.yaml"}
		if got := plain.composeCLICommand(context.Background(), "", "down").Env; got != nil {
			t.Fatalf("plain composeCLICommand.Env = %v, want nil", got)
		}
		// Assignments before sudo are ignored by composeScope so composeInlineEnv stays nil.
		sudoScope := composeScope("REQ_VAR=ok sudo docker compose up")
		sudoApp := &App{composeFile: "docker-compose-tmp.yaml", composeInlineEnv: sudoScope.Env}
		if got := sudoApp.composeCLICommand(context.Background(), "", "down").Env; got != nil {
			t.Fatalf("sudo composeCLICommand.Env = %v, want nil", got)
		}
	})

	t.Run("in-memory moveComposeAgent preserves inline env and updates stdin", func(t *testing.T) {
		for _, k := range []string{"COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR", "COMPOSE_COMPATIBILITY", "REQ_VAR"} {
			t.Setenv(k, "")
		}
		t.Setenv("APP_NETWORK", "exported-net")
		const doc = "name: memproj\nservices:\n  app:\n    image: alpine:3.21\n    container_name: mem-app\n    environment:\n      - REQ=${REQ_VAR:?required}\n    networks: [appnet]\nnetworks:\n  appnet:\n    name: ${APP_NETWORK}\n"
		idc, err := docker.New(zap.NewNop(), &config.Config{})
		if err != nil {
			t.Fatal(err)
		}
		opts := models.SetupOptions{
			Container:       "mem-app",
			InMemoryCompose: []byte(doc),
			AgentPort:       16789,
			ProxyPort:       16790,
			DnsPort:         16791,
		}
		a := NewApp(zap.NewNop(), "APP_NETWORK=inline-net REQ_VAR=ok docker compose up", idc, opts)
		if err := a.setupComposeInMemory(context.Background(), nil); err != nil {
			t.Fatalf("setupComposeInMemory: %v", err)
		}
		if err := a.moveComposeAgent(16789, 16799); err != nil {
			t.Fatalf("moveComposeAgent: %v", err)
		}
		cmd := a.composeCLICommand(context.Background(), "memproj", "down")
		if cmd == nil {
			t.Fatal("composeCLICommand returned nil after moveComposeAgent")
		}
		if cmd.Stdin == nil {
			t.Fatal("composeCLICommand.Stdin is nil for in-memory compose")
		}
		stdinBytes, err := io.ReadAll(cmd.Stdin)
		if err != nil {
			t.Fatalf("read cmd.Stdin: %v", err)
		}
		if !strings.Contains(string(stdinBytes), "16799") {
			t.Fatalf("moved compose stdin missing new agent port 16799:\n%s", string(stdinBytes))
		}
		if composeLibrarySupported() {
			return
		}
		foundNet, foundReq := false, false
		for _, kv := range cmd.Env {
			if kv == "APP_NETWORK=inline-net" {
				foundNet = true
			}
			if kv == "REQ_VAR=ok" {
				foundReq = true
			}
		}
		if !foundNet || !foundReq {
			t.Fatalf("composeCLICommand.Env lost inline env after moveComposeAgent (net=%v req=%v): %v", foundNet, foundReq, cmd.Env)
		}
	})
}
