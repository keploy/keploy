package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// composeProjectDir writes files into a fresh directory named proj (the
// project compose names after it when nothing else names one) and returns it.
// The variables compose reads its scope from, and the ones the rows set, start
// unset, and are as they were after the test.
func composeProjectDir(t *testing.T, files map[string]string) string {
	t.Helper()
	for _, k := range []string{composeProjectNameEnv, composeEnvFilesEnv, "COMPOSE_DISABLE_ENV_FILE", "COMPOSE_PROFILES", composeCompatibilityEnv, "APP_CONTAINER", "APP_PROJECT", "PARSE_PROJECT", "AGENT_NAME"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, v) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		_ = os.Unsetenv(k)
	}
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func findIn(t *testing.T, dir string, files []string, scope ComposeScope, name string) (*ComposeServiceInfo, error) {
	t.Helper()
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = filepath.Join(dir, f)
	}
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	return idc.FindContainerInComposeFiles(context.Background(), paths, scope, name)
}

func findInContent(t *testing.T, content string, scope ComposeScope, name string) (*ComposeServiceInfo, error) {
	t.Helper()
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	return idc.FindContainerInCompose(context.Background(), []byte(content), scope, name)
}

// The parse-server sample's shape: every name is a variable with a default.
const parseServerShape = `name: ${APP_PROJECT:-parse-server-mongo}
services:
  mongo:
    image: mongo:7
    container_name: ${MONGO_CONTAINER:-parse-server-mongo-mongo}
  parse-server:
    image: ${PARSE_IMAGE:-parse-server-mongo:local}
    container_name: ${APP_CONTAINER:-parse-server-mongo-app}
    networks:
      parse-net:
        ipv4_address: ${PARSE_APP_IP:-172.30.0.11}
    ports:
      - "${PARSE_HOST_PORT:-6100}:${PARSE_CONTAINER_PORT:-6100}"
networks:
  parse-net:
    name: ${PARSE_NETWORK_NAME:-parse-server-mongo-net}
`

func TestComposeIdentity_ContainerNameFromAVariable(t *testing.T) {
	cases := []struct {
		name        string
		files       map[string]string
		env         map[string]string
		scope       ComposeScope
		container   string
		wantService string
		wantErr     string
	}{
		{name: "${VAR:-default}, unset: the default", files: map[string]string{"compose.yml": parseServerShape},
			container: "parse-server-mongo-app", wantService: "parse-server"},
		{name: "${VAR:-default}, set: the value", files: map[string]string{"compose.yml": parseServerShape},
			env: map[string]string{"APP_CONTAINER": "psst-1-app"}, container: "psst-1-app", wantService: "parse-server"},
		{name: "${VAR:-default}, set: the default no longer names it", files: map[string]string{"compose.yml": parseServerShape},
			env: map[string]string{"APP_CONTAINER": "psst-1-app"}, container: "parse-server-mongo-app", wantErr: `parse-server: "psst-1-app"`},
		{name: "${VAR:-default}, set empty: the default", files: map[string]string{"compose.yml": parseServerShape},
			env: map[string]string{"APP_CONTAINER": ""}, container: "parse-server-mongo-app", wantService: "parse-server"},
		{name: "${VAR-default}, set empty: no container_name, so the project's name",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: ${APP_CONTAINER-dflt}\n"},
			env:       map[string]string{"APP_CONTAINER": ""},
			container: "proj-app-1", wantService: "app"},
		{name: "${VAR-default}, unset: the default",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: ${APP_CONTAINER-dflt}\n"},
			container: "dflt", wantService: "app"},
		{name: "${VAR}, unset, no default: no container_name, so the project's name",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: ${APP_CONTAINER}\n"},
			container: "proj-app-1", wantService: "app"},
		{name: "$VAR, set", files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: $APP_CONTAINER\n"},
			env: map[string]string{"APP_CONTAINER": "bare"}, container: "bare", wantService: "app"},
		{name: "$$ is a literal dollar", files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: a$$b\n"},
			container: "a$b", wantService: "app"},
		{name: "${VAR:?err}, unset: refused with compose's reason",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: ${APP_CONTAINER:?set APP_CONTAINER}\n"},
			container: "app", wantErr: "set APP_CONTAINER"},
		{name: "a malformed template: refused", files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: ${APP_CONTAINER\n"},
			container: "app", wantErr: "could not resolve"},
		{name: "the project's .env sets it", files: map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n"},
			container: "from-dotenv", wantService: "parse-server"},
		{name: "keploy's environment beats the .env", files: map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n"},
			env: map[string]string{"APP_CONTAINER": "from-env"}, container: "from-env", wantService: "parse-server"},
		{name: "--env-file replaces the .env",
			files: map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n", "ci.env": "APP_CONTAINER=from-env-file\n"},
			scope: ComposeScope{EnvFiles: []string{"ci.env"}}, container: "from-env-file", wantService: "parse-server"},
		{name: "--env-file: the .env is not read",
			files: map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n", "ci.env": "OTHER=1\n"},
			scope: ComposeScope{EnvFiles: []string{"ci.env"}}, container: "from-dotenv", wantErr: "not found"},
		{name: "a later --env-file wins",
			files: map[string]string{"compose.yml": parseServerShape, "a.env": "APP_CONTAINER=from-a\n", "b.env": "APP_CONTAINER=from-b\n"},
			scope: ComposeScope{EnvFiles: []string{"a.env", "b.env"}}, container: "from-b", wantService: "parse-server"},
		{name: "COMPOSE_ENV_FILES when the command names none",
			files: map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n", "ci.env": "APP_CONTAINER=from-compose-env-files\n"},
			env:   map[string]string{composeEnvFilesEnv: "ci.env"}, container: "from-compose-env-files", wantService: "parse-server"},
		{name: "an --env-file that cannot be read: refused", files: map[string]string{"compose.yml": parseServerShape},
			scope: ComposeScope{EnvFiles: []string{"missing.env"}}, container: "parse-server-mongo-app", wantErr: "missing.env"},
		{name: "no container_name: <project>-<service>-1, the project from name:",
			files:     map[string]string{"compose.yml": "name: ${APP_PROJECT:-named}\nservices:\n  app:\n    image: x\n"},
			container: "named-app-1", wantService: "app"},
		{name: "no container_name: the project from -p",
			files: map[string]string{"compose.yml": "name: named\nservices:\n  app:\n    image: x\n"},
			scope: ComposeScope{ProjectName: "flagged"}, container: "flagged-app-1", wantService: "app"},
		{name: "no container_name: the project from COMPOSE_PROJECT_NAME in the .env",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n", ".env": "COMPOSE_PROJECT_NAME=fromdotenv\n"},
			container: "fromdotenv-app-1", wantService: "app"},
		{name: "no container_name: the project from the directory",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			container: "proj-app-1", wantService: "app"},
		{name: "no container_name: another replica", files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			container: "proj-app-12", wantService: "app"},
		{name: "no container_name: no replica 0", files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			container: "proj-app-0", wantErr: "not found"},
		{name: "no container_name: no zero-padded replica", files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			container: "proj-app-01", wantErr: "not found"},
		{name: "no container_name: no word for a replica", files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			container: "proj-app-x", wantErr: "not found"},
		{name: "no container_name: another project's", files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			container: "other-app-1", wantErr: "not found"},
		{name: "a container_name set: the project's name is not the container's",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: mine\n"},
			container: "proj-app-1", wantErr: `app: "mine"`},
		{name: "the service's key, as keploy has always taken it",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: mine\n"},
			container: "app", wantService: "app"},
		{name: "the container named so beats a service keyed so",
			files:     map[string]string{"compose.yml": "services:\n  db:\n    image: x\n    container_name: web\n  web:\n    image: y\n"},
			container: "web", wantService: "db"},
		{name: "two services' containers named the same: refused",
			files:     map[string]string{"compose.yml": "services:\n  a:\n    image: x\n    container_name: ${APP_CONTAINER:-same}\n  b:\n    image: y\n    container_name: same\n"},
			container: "same", wantErr: "more than one compose service (a, b)"},
		{name: "a service behind an inactive profile",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    profiles: [debug]\n    container_name: ${APP_CONTAINER:-profiled}\n"},
			container: "profiled", wantService: "app"},
		{name: "a service's env_file missing on this machine does not matter",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    env_file: [absent.env]\n    container_name: ${APP_CONTAINER:-withenvfile}\n"},
			container: "withenvfile", wantService: "app"},
		{name: "a container_name inherited through extends",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    extends:\n      file: base.yml\n      service: base\n", "base.yml": "services:\n  base:\n    image: x\n    container_name: ${APP_CONTAINER:-extended}\n"},
			container: "extended", wantService: "app"},
		{name: "no container_name: the project from name:, lowercased as compose names it",
			files:     map[string]string{"compose.yml": "name: ${APP_PROJECT:-Parse.Server}\nservices:\n  app:\n    image: x\n"},
			container: "parseserver-app-1", wantService: "app"},
		{name: "no container_name: name: as written is not the project's name",
			files:     map[string]string{"compose.yml": "name: Parse-Server\nservices:\n  app:\n    image: x\n"},
			container: "Parse-Server-app-1", wantErr: "not found"},
		{name: "no container_name: a bare replica number is not a container",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			container: "12", wantErr: "not found"},
		{name: "no container_name: the project's prefix then a bare number is not a container",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			container: "proj-12", wantErr: "not found"},
		{name: "no container_name: <service>-<number> without the project's prefix is not a container",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			container: "app-1", wantErr: "not found"},
		{name: "name: from an unset variable: refused, as compose refuses it",
			files:     map[string]string{"compose.yml": "name: ${APP_PROJECT}\nservices:\n  app:\n    image: x\n"},
			container: "proj-app-1", wantErr: "project name can't be empty"},
		{name: "name: that is not a string: refused, as compose refuses it",
			files:     map[string]string{"compose.yml": "name: [a, b]\nservices:\n  app:\n    image: x\n"},
			container: "proj-app-1", wantErr: "into string"},
		{name: "COMPOSE_COMPATIBILITY=y, a yes as compose reads one",
			files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			env:   map[string]string{composeCompatibilityEnv: " Y "}, container: "proj_app_1", wantService: "app"},
		{name: "a dependency on a service another file brings does not matter",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    depends_on: [elsewhere]\n    container_name: ${APP_CONTAINER:-depends}\n"},
			container: "depends", wantService: "app"},
		{name: "a service's label_file missing on this machine does not matter",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    label_file: absent.labels\n    container_name: ${APP_CONTAINER:-withlabelfile}\n"},
			container: "withlabelfile", wantService: "app"},
		{name: "a field newer than keploy's compose schema does not matter",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    a_field_from_a_later_compose: true\n    container_name: ${APP_CONTAINER:-newschema}\n"},
			container: "newschema", wantService: "app"},
		{name: "--compatibility: <project>_<service>_<number>",
			files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			scope: ComposeScope{Compatibility: true}, container: "proj_app_1", wantService: "app"},
		{name: "--compatibility: not <project>-<service>-<number>",
			files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			scope: ComposeScope{Compatibility: true}, container: "proj-app-1", wantErr: `app: "proj_app_1"`},
		{name: "no compatibility: not <project>_<service>_<number>",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			container: "proj_app_1", wantErr: `app: "proj-app-1"`},
		{name: "COMPOSE_COMPATIBILITY in keploy's environment",
			files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			env:   map[string]string{composeCompatibilityEnv: "true"}, container: "proj_app_2", wantService: "app"},
		{name: "COMPOSE_COMPATIBILITY in the .env",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n", ".env": "COMPOSE_COMPATIBILITY=1\n"},
			container: "proj_app_1", wantService: "app"},
		{name: "COMPOSE_COMPATIBILITY false",
			files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			env:   map[string]string{composeCompatibilityEnv: "false"}, container: "proj-app-1", wantService: "app"},
		{name: "compatibility mode leaves a container_name as it is",
			files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: ${APP_CONTAINER:-my-app}\n"},
			scope: ComposeScope{Compatibility: true}, container: "my-app", wantService: "app"},
		{name: "the in-process library has no compatibility mode",
			files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			env:   map[string]string{composeCompatibilityEnv: "true"},
			scope: ComposeScope{Compatibility: true, InProcess: true}, container: "proj-app-1", wantService: "app"},
		{name: "-p that compose refuses: refused",
			files: map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			scope: ComposeScope{ProjectName: "Not.Valid"}, container: "app", wantErr: "Not.Valid"},
		{name: "an --env-file named from the home directory",
			files: map[string]string{"compose.yml": parseServerShape, "home.env": "APP_CONTAINER=from-home\n"},
			env:   map[string]string{"HOME": "<dir>"}, scope: ComposeScope{EnvFiles: []string{"~/home.env"}},
			container: "from-home", wantService: "parse-server"},
		// Inline shell assignments from the compose command (scope.Env).
		{name: "inline COMPOSE_COMPATIBILITY=true: <project>_<service>_<number>",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			scope:     ComposeScope{Env: map[string]string{composeCompatibilityEnv: "true"}},
			container: "proj_app_1", wantService: "app"},
		{name: "inline COMPOSE_COMPATIBILITY=true: refuses <project>-<service>-<number>",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			scope:     ComposeScope{Env: map[string]string{composeCompatibilityEnv: "true"}},
			container: "proj-app-1", wantErr: `app: "proj_app_1"`},
		{name: "inline COMPOSE_COMPATIBILITY=false beats exported true",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			env:       map[string]string{composeCompatibilityEnv: "true"},
			scope:     ComposeScope{Env: map[string]string{composeCompatibilityEnv: "false"}},
			container: "proj-app-1", wantService: "app"},
		{name: "inline APP_CONTAINER set: matches the inline value",
			files:     map[string]string{"compose.yml": parseServerShape},
			scope:     ComposeScope{Env: map[string]string{"APP_CONTAINER": "from-inline"}},
			container: "from-inline", wantService: "parse-server"},
		{name: "inline APP_CONTAINER set: default no longer names it",
			files:     map[string]string{"compose.yml": parseServerShape},
			scope:     ComposeScope{Env: map[string]string{"APP_CONTAINER": "from-inline"}},
			container: "parse-server-mongo-app", wantErr: `parse-server: "from-inline"`},
		{name: "inline unrelated var: APP_CONTAINER keeps its default",
			files:     map[string]string{"compose.yml": parseServerShape},
			scope:     ComposeScope{Env: map[string]string{"OTHER": "1"}},
			container: "parse-server-mongo-app", wantService: "parse-server"},
		{name: "inline APP_CONTAINER beats exported env and .env",
			files:     map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n"},
			env:       map[string]string{"APP_CONTAINER": "from-env"},
			scope:     ComposeScope{Env: map[string]string{"APP_CONTAINER": "from-inline"}},
			container: "from-inline", wantService: "parse-server"},
		{name: "inline empty APP_CONTAINER beats exported env for ${VAR-default}",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: ${APP_CONTAINER-dflt}\n"},
			env:       map[string]string{"APP_CONTAINER": "from-env"},
			scope:     ComposeScope{Env: map[string]string{"APP_CONTAINER": ""}},
			container: "proj-app-1", wantService: "app"},
		{name: "inline COMPOSE_PROJECT_NAME beats exported env",
			files:     map[string]string{"compose.yml": "services:\n  app:\n    image: x\n"},
			env:       map[string]string{composeProjectNameEnv: "from-env"},
			scope:     ComposeScope{Env: map[string]string{composeProjectNameEnv: "from-inline"}},
			container: "from-inline-app-1", wantService: "app"},
		{name: "inline COMPOSE_ENV_FILES replaces .env",
			files:     map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n", "ci.env": "APP_CONTAINER=from-inline-env-file\n"},
			scope:     ComposeScope{Env: map[string]string{composeEnvFilesEnv: "ci.env"}},
			container: "from-inline-env-file", wantService: "parse-server"},
		{name: "inline COMPOSE_ENV_FILES beats exported COMPOSE_ENV_FILES",
			files:     map[string]string{"compose.yml": parseServerShape, "other.env": "APP_CONTAINER=from-other\n", "ci.env": "APP_CONTAINER=from-inline-env-file\n"},
			env:       map[string]string{composeEnvFilesEnv: "other.env"},
			scope:     ComposeScope{Env: map[string]string{composeEnvFilesEnv: "ci.env"}},
			container: "from-inline-env-file", wantService: "parse-server"},
		{name: "inline COMPOSE_DISABLE_ENV_FILE=1 skips .env",
			files:     map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n"},
			scope:     ComposeScope{Env: map[string]string{composeDisableEnvFileEnv: "1"}},
			container: "parse-server-mongo-app", wantService: "parse-server"},
		{name: "inline COMPOSE_DISABLE_ENV_FILE=1 refuses .env name",
			files:     map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n"},
			scope:     ComposeScope{Env: map[string]string{composeDisableEnvFileEnv: "1"}},
			container: "from-dotenv", wantErr: "not found"},
		{name: "inline COMPOSE_DISABLE_ENV_FILE=0 beats exported 1 and reads .env",
			files:     map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n"},
			env:       map[string]string{composeDisableEnvFileEnv: "1"},
			scope:     ComposeScope{Env: map[string]string{composeDisableEnvFileEnv: "0"}},
			container: "from-dotenv", wantService: "parse-server"},
		{name: "inline COMPOSE_DISABLE_ENV_FILE=1 still honours explicit --env-file",
			files:     map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n", "ci.env": "APP_CONTAINER=from-ci\n"},
			scope:     ComposeScope{EnvFiles: []string{"ci.env"}, Env: map[string]string{composeDisableEnvFileEnv: "1"}},
			container: "from-ci", wantService: "parse-server"},
		{name: "inline COMPOSE_DISABLE_ENV_FILE=1 still honours COMPOSE_ENV_FILES",
			files:     map[string]string{"compose.yml": parseServerShape, ".env": "APP_CONTAINER=from-dotenv\n", "ci.env": "APP_CONTAINER=from-ci\n"},
			scope:     ComposeScope{Env: map[string]string{composeDisableEnvFileEnv: "1", composeEnvFilesEnv: "ci.env"}},
			container: "from-ci", wantService: "parse-server"},
		{name: "inline COMPOSE_DISABLE_ENV_FILE invalid bool is refused",
			files:     map[string]string{"compose.yml": parseServerShape},
			scope:     ComposeScope{Env: map[string]string{composeDisableEnvFileEnv: "notabool"}},
			container: "parse-server-mongo-app", wantErr: "invalid syntax"},
		{name: "in-process library ignores inline scope.Env",
			files:     map[string]string{"compose.yml": parseServerShape},
			scope:     ComposeScope{InProcess: true, Env: map[string]string{"APP_CONTAINER": "from-inline", composeCompatibilityEnv: "true"}},
			container: "parse-server-mongo-app", wantService: "parse-server"},
	}
	// Each row runs on the file (-f compose.yml) and on its content as the
	// one in-memory document (-f -) from the same directory: compose names
	// both alike, and so must keploy.
	for _, tc := range cases {
		for _, inMemory := range []bool{false, true} {
			mode := "files"
			if inMemory {
				mode = "in memory"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				dir := composeProjectDir(t, tc.files)
				for k, v := range tc.env {
					t.Setenv(k, strings.ReplaceAll(v, "<dir>", dir))
				}
				t.Chdir(dir)
				var info *ComposeServiceInfo
				var err error
				if inMemory {
					info, err = findInContent(t, tc.files["compose.yml"], tc.scope, tc.container)
				} else {
					info, err = findIn(t, dir, []string{"compose.yml"}, tc.scope, tc.container)
				}
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("find %q = %v, %v; want an error with %q", tc.container, info, err, tc.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("find %q: %v", tc.container, err)
				}
				if info.AppServiceName != tc.wantService {
					t.Fatalf("find %q found service %q, want %q", tc.container, info.AppServiceName, tc.wantService)
				}
				if info.Compose == nil || (inMemory && info.ComposePath != "") {
					t.Fatalf("find %q gave document %v from %q", tc.container, info.Compose, info.ComposePath)
				}
			})
		}
	}
}

// The files merge in order, as compose merges them: a later file's
// container_name is the container's, and the file keploy rewrites is the first
// that keys the service, with that file's ports and networks.
func TestComposeIdentity_MultipleFiles(t *testing.T) {
	base := "services:\n  app:\n    image: x\n    container_name: base-name\n    ports: [\"8080:8080\"]\n    networks: [front]\nnetworks:\n  front: {}\n"
	override := "services:\n  app:\n    container_name: ${APP_CONTAINER:-override-name}\n"
	dir := composeProjectDir(t, map[string]string{"base.yml": base, "override.yml": override})
	t.Chdir(dir)

	info, err := findIn(t, dir, []string{"base.yml", "override.yml"}, ComposeScope{}, "override-name")
	if err != nil {
		t.Fatalf("the later file's container_name: %v", err)
	}
	if info.AppServiceName != "app" || info.ComposePath != filepath.Join(dir, "base.yml") {
		t.Fatalf("found %q in %s, want app in the first file that keys it, base.yml", info.AppServiceName, info.ComposePath)
	}
	if len(info.Ports) != 1 || info.Ports[0] != "8080:8080" || len(info.Networks) != 1 || info.Networks[0] != "front" {
		t.Fatalf("ports %v, networks %v; want base.yml's", info.Ports, info.Networks)
	}
	if _, err := findIn(t, dir, []string{"base.yml", "override.yml"}, ComposeScope{}, "base-name"); err == nil {
		t.Fatalf("the earlier file's container_name names no container once a later one sets another")
	}
	// The other order: base.yml's name is the container's.
	if _, err := findIn(t, dir, []string{"override.yml", "base.yml"}, ComposeScope{}, "base-name"); err != nil {
		t.Fatalf("override.yml then base.yml: %v", err)
	}
	// A file the command does not name is not merged.
	if _, err := findIn(t, dir, []string{"base.yml"}, ComposeScope{}, "override-name"); err == nil {
		t.Fatalf("override.yml is not in the command, so its name is no container's")
	}
}

// A service only an include brings is in no file keploy rewrites: refused,
// saying so, rather than rewriting a file the service is not in.
func TestComposeIdentity_IncludedServiceIsRefused(t *testing.T) {
	dir := composeProjectDir(t, map[string]string{
		"compose.yml": "include:\n  - inc.yml\nservices:\n  web:\n    image: x\n",
		"inc.yml":     "services:\n  app:\n    image: x\n    container_name: ${APP_CONTAINER:-included}\n",
	})
	t.Chdir(dir)
	_, err := findIn(t, dir, []string{"compose.yml"}, ComposeScope{}, "included")
	if err == nil || !strings.Contains(err.Error(), "not keyed in any of the files") {
		t.Fatalf("got %v, want the include refused", err)
	}
	if _, err := findIn(t, dir, []string{"compose.yml"}, ComposeScope{}, "web"); err != nil {
		t.Fatalf("the including file's own service: %v", err)
	}
	// So too for the in-memory document, its include read from the current
	// directory as compose reads it.
	_, err = findInContent(t, "include:\n  - inc.yml\nservices:\n  web:\n    image: x\n", ComposeScope{}, "included")
	if err == nil || !strings.Contains(err.Error(), "not keyed in the in-memory compose document") {
		t.Fatalf("in memory: got %v, want the include refused", err)
	}
}

// An empty in-memory document names no container, and none is read from
// keploy's own standard input in its place (what `-f -` reads to compose).
func TestComposeIdentity_EmptyDocumentIsRefused(t *testing.T) {
	const empty = "the compose document is empty"
	if _, err := findInContent(t, "", ComposeScope{}, "app"); err == nil || !strings.HasSuffix(err.Error(), ": "+empty) {
		t.Fatalf("got %v, want the empty document refused", err)
	}
	if _, err := StartComposeChildOutput(context.Background(), nil, ComposeScope{}); err == nil || err.Error() != empty {
		t.Fatalf("child output of no document: got %v, want %q", err, empty)
	}
	if _, err := findInContent(t, "services: [not a map", ComposeScope{}, "app"); err == nil || !strings.Contains(err.Error(), "failed to parse in-memory compose YAML") {
		t.Fatalf("got %v, want a document that is not YAML refused as such", err)
	}
}

// --project-directory names the project and holds the .env compose reads, as
// for compose, wherever the files are.
func TestComposeIdentity_ProjectDirectory(t *testing.T) {
	dir := composeProjectDir(t, map[string]string{
		"compose.yml": "services:\n  app:\n    image: x\n  web:\n    image: y\n    container_name: ${APP_CONTAINER:-dflt}\n",
		"pd/.env":     "APP_CONTAINER=from-pd\n",
	})
	t.Chdir(dir)
	scope := ComposeScope{ProjectDir: filepath.Join(dir, "pd")}
	for container, want := range map[string]string{"pd-app-1": "app", "from-pd": "web"} {
		info, err := findIn(t, dir, []string{"compose.yml"}, scope, container)
		if err != nil || info.AppServiceName != want {
			t.Fatalf("FindContainerInComposeFiles(%q) = %v, %v; want service %q", container, info, err, want)
		}
		content, err := os.ReadFile(filepath.Join(dir, "compose.yml"))
		if err != nil {
			t.Fatal(err)
		}
		info, err = findInContent(t, string(content), scope, container)
		if err != nil || info.AppServiceName != want {
			t.Fatalf("FindContainerInCompose(%q) = %v, %v; want service %q", container, info, err, want)
		}
	}
}

func TestComposeIdentity_StdinIsRefused(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	_, err := idc.FindContainerInComposeFiles(context.Background(), []string{"-"}, ComposeScope{}, "app")
	if err == nil || !strings.Contains(err.Error(), "standard input") {
		t.Fatalf("got %v, want -f - refused", err)
	}
}

// A file compose's library loads but keploy's own reader cannot is an error,
// not a file skipped.
func TestComposeIdentity_UnreadableByKeployIsAnError(t *testing.T) {
	dir := composeProjectDir(t, map[string]string{"compose.yml": "version: [3]\nservices:\n  app:\n    image: x\n"})
	t.Chdir(dir)
	_, err := findIn(t, dir, []string{"compose.yml"}, ComposeScope{}, "app")
	if err == nil || !strings.Contains(err.Error(), "read compose file") {
		t.Fatalf("got %v, want keploy's read error", err)
	}
}

// ModifyComposeForAgent takes a service by its key before any container_name,
// so the key keploy's setup hands it is the service it wires.
func TestModifyComposeForAgent_KeyBeforeAnotherServicesContainerName(t *testing.T) {
	const in = "services:\n  db:\n    image: x\n    container_name: web\n  web:\n    image: y\n"
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatal(err)
	}
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	if err := idc.ModifyComposeForAgent(&compose, models.SetupOptions{}, "web"); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}
	if got := idc.getServiceProperty(findServiceByName(&compose, "web"), "network_mode"); got == nil || got.Value != "service:keploy-agent" {
		t.Fatalf("web was not wired: network_mode %v", got)
	}
	if got := idc.getServiceProperty(findServiceByName(&compose, "db"), "network_mode"); got != nil {
		t.Fatalf("db was wired in web's place: network_mode %q", got.Value)
	}
}

// The app hook is handed the run's --container-name (opts.Container), and the
// name ModifyComposeForAgent was given only when the run gives none.
func TestModifyComposeForAgent_HookGetsTheContainerName(t *testing.T) {
	const in = "services:\n  web:\n    image: y\n    container_name: ${APP_CONTAINER:-my-app}\n"
	for _, tc := range []struct {
		container, want string
	}{{"my-app", "my-app"}, {"", "web"}} {
		var compose Compose
		if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
			t.Fatal(err)
		}
		var got []string
		prev := ComposeServiceHook
		ComposeServiceHook = func(name string, _ *yaml.Node) { got = append(got, name) }
		idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
		err := idc.ModifyComposeForAgent(&compose, models.SetupOptions{Container: tc.container}, "web")
		ComposeServiceHook = prev
		if err != nil {
			t.Fatalf("ModifyComposeForAgent: %v", err)
		}
		if len(got) != 2 || got[0] != "keploy-agent" || got[1] != tc.want {
			t.Fatalf("opts.Container %q: the hook was handed %v, want [keploy-agent %s]", tc.container, got, tc.want)
		}
	}
}

// keploy's agent service is keploy-agent, so a service compose names
// keploy-agent is refused however the files name it: through a variable, from
// the project's .env, or in another -f file. Each lookup hands its resolution
// on with the document ModifyComposeForAgent rewrites. A pure tuning override
// for keploy-agent in a secondary -f file (such as `cpus: 0.02` in
// agent-cpu.yml) is allowed so compose merges it onto keploy's generated
// service, while any primary-file `keploy-agent` key or secondary entry that
// sets image, build, container_name, entrypoint, or command is refused.
func TestModifyComposeForAgent_AgentNameAsComposeResolvesIt(t *testing.T) {
	const web = "services:\n  web:\n    image: x\n"
	const side = web + "  side:\n    image: x\n    container_name: ${AGENT_NAME:-keploy-agent}\n"
	const named = `service "side" already uses container_name "keploy-agent"`
	const projectDefined = `the compose project already defines or resets a service named "keploy-agent"`
	for _, tc := range []struct {
		name     string
		files    map[string]string
		paths    []string
		inMemory bool
		want     string // "": accepted; else the refusal says it
	}{
		{"a variable's default", map[string]string{"compose.yml": side}, []string{"compose.yml"}, false, named},
		{"a variable's default, in memory", map[string]string{"compose.yml": side}, nil, true, named},
		{"the .env sets another", map[string]string{"compose.yml": side, ".env": "AGENT_NAME=side-1\n"}, []string{"compose.yml"}, false, ""},
		{"the .env sets another, in memory", map[string]string{"compose.yml": side, ".env": "AGENT_NAME=side-1\n"}, nil, true, ""},
		{"the .env sets it", map[string]string{
			"compose.yml": web + "  side:\n    image: x\n    container_name: ${AGENT_NAME}\n",
			".env":        "AGENT_NAME=keploy-agent\n",
		}, []string{"compose.yml"}, false, named},
		{"another file tunes keploy-agent with cpus", map[string]string{
			"compose.yml": web, "agent-cpu.yml": "services:\n  keploy-agent:\n    cpus: 0.02\n",
		}, []string{"compose.yml", "agent-cpu.yml"}, false, ""},
		{"earlier override file tunes keploy-agent with cpus", map[string]string{
			"compose.yml": web, "agent-cpu.yml": "services:\n  keploy-agent:\n    cpus: 0.02\n",
		}, []string{"agent-cpu.yml", "compose.yml"}, false, ""},
		{"another file tunes keploy-agent with labels and env", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    labels:\n      keploy.test: \"1\"\n    environment:\n      - FOO=bar\n",
		}, []string{"compose.yml", "override.yml"}, false, ""},
		{"another file tunes keploy-agent; the app extends an included service", map[string]string{
			"compose.yml":   "include:\n  - common.yml\nservices:\n  web:\n    extends:\n      service: base\n",
			"common.yml":    "services:\n  base:\n    image: x\n",
			"agent-cpu.yml": "services:\n  keploy-agent:\n    cpus: 0.02\n",
		}, []string{"compose.yml", "agent-cpu.yml"}, false, ""},
		{"another file clears command with null", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    command: null\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (command)"},
		{"another file clears entrypoint with null", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    entrypoint: null\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (entrypoint)"},
		{"another file keys it with image", map[string]string{
			"compose.yml": web, "agent.yml": "services:\n  keploy-agent:\n    image: mine\n",
		}, []string{"compose.yml", "agent.yml"}, false, projectDefined + " (image)"},
		{"another file keys it with empty image", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    image: \"\"\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (image)"},
		{"another file resets image with !reset", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    image: !reset \"\"\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (image)"},
		{"another file keys it with build", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    build: .\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (build)"},
		{"another file keys it with container_name", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    container_name: other\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (container_name)"},
		{"another file keys it with empty container_name", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    container_name: \"\"\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (container_name)"},
		{"another file resets container_name with !reset", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    container_name: !reset null\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (container_name)"},
		{"another file keys it with container_name keploy-agent", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    container_name: keploy-agent\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (container_name)"},
		{"another file keys it with container_name from env", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    container_name: ${AGENT_NAME:-other}\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (container_name)"},
		{"another file keys it with entrypoint", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    entrypoint: [\"/bin/sh\"]\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (entrypoint)"},
		{"another file keys it with empty entrypoint", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    entrypoint: []\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (entrypoint)"},
		{"another file resets entrypoint with !reset", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    entrypoint: !reset []\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (entrypoint)"},
		{"another file keys it with command", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    command: [\"sleep\", \"99\"]\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (command)"},
		{"another file keys it with empty command", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    command: []\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (command)"},
		{"another file resets command with !reset", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    command: !reset []\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (command)"},
		{"another file overrides command with !override", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent:\n    command: !override [\"sleep\", \"99\"]\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (command)"},
		{"another file resets whole keploy-agent service with !reset", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent: !reset {}\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (service)"},
		{"another file overrides whole keploy-agent service with !override", map[string]string{
			"compose.yml": web, "override.yml": "services:\n  keploy-agent: !override\n    cpus: 0.02\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (image, container_name, entrypoint, command)"},
		{"another file extends a service with image onto keploy-agent", map[string]string{
			"compose.yml":  web,
			"base.yml":     "services:\n  base:\n    image: mine\n",
			"override.yml": "services:\n  keploy-agent:\n    extends:\n      file: base.yml\n      service: base\n",
		}, []string{"compose.yml", "override.yml"}, false, projectDefined + " (image)"},
		{"included file defines keploy-agent with only cpus", map[string]string{
			"compose.yml": "include:\n  - agent.yml\n" + web,
			"agent.yml":   "services:\n  keploy-agent:\n    cpus: 0.02\n",
		}, []string{"compose.yml"}, false, projectDefined + " (include)"},
		{"included file defines keploy-agent with only cpus, in memory", map[string]string{
			"compose.yml": "include:\n  - agent.yml\n" + web,
			"agent.yml":   "services:\n  keploy-agent:\n    cpus: 0.02\n",
		}, nil, true, projectDefined + " (include)"},
		{"multi-file project with included keploy-agent", map[string]string{
			"compose.yml": "include:\n  - agent.yml\n" + web,
			"agent.yml":   "services:\n  keploy-agent:\n    cpus: 0.02\n",
			"side.yml":    "services:\n  side:\n    image: y\n",
		}, []string{"compose.yml", "side.yml"}, false, projectDefined + " (include)"},
		{"same file keys keploy-agent even with only cpus", map[string]string{
			"compose.yml": web + "  keploy-agent:\n    cpus: 0.02\n",
		}, []string{"compose.yml"}, false, `the compose file already defines a service named "keploy-agent"`},
		{"another file names it", map[string]string{
			"compose.yml": web, "side.yml": "services:\n  side:\n    image: x\n    container_name: keploy-agent\n",
		}, []string{"compose.yml", "side.yml"}, false, named},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := composeProjectDir(t, tc.files)
			t.Chdir(dir)
			var info *ComposeServiceInfo
			var err error
			if tc.inMemory {
				info, err = findInContent(t, tc.files["compose.yml"], ComposeScope{}, "web")
			} else {
				info, err = findIn(t, dir, tc.paths, ComposeScope{}, "web")
			}
			if err != nil {
				t.Fatalf("finding web: %v", err)
			}
			idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
			err = idc.ModifyComposeForAgent(info.Compose, models.SetupOptions{}, info.AppServiceName)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want the refusal %q", err, tc.want)
			}
		})
	}
}

// TestModifyComposeForAgent_SecondaryAgentOverrideMergesInComposeGo verifies
// that a secondary override file tuning keploy-agent (as compose-agent-boot
// passes `-f docker-compose.yml -f agent-cpu.yml`) is accepted and merges its
// resource limit onto keploy's generated keploy-agent service under compose-go
// full validation.
func TestModifyComposeForAgent_SecondaryAgentOverrideMergesInComposeGo(t *testing.T) {
	dir := composeProjectDir(t, map[string]string{
		"docker-compose.yml": "services:\n  app:\n    image: alpine:3.21\n    container_name: cab-app\n",
		"agent-cpu.yml":      "services:\n  keploy-agent:\n    cpus: 0.02\n",
	})
	basePath := filepath.Join(dir, "docker-compose.yml")
	overridePath := filepath.Join(dir, "agent-cpu.yml")

	idc, err := New(zap.NewNop(), &config.Config{})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	info, err := idc.FindContainerInComposeFiles(context.Background(), []string{basePath, overridePath}, ComposeScope{}, "cab-app")
	if err != nil {
		t.Fatalf("FindContainerInComposeFiles: %v", err)
	}
	opts := buildAgentOpts()
	opts.Container = "cab-app"
	if err := idc.ModifyComposeForAgent(info.Compose, opts, info.AppServiceName); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}
	outPrimary := filepath.Join(dir, "docker-compose-tmp.yaml")
	if err := idc.WriteComposeFile(info.Compose, outPrimary); err != nil {
		t.Fatalf("WriteComposeFile: %v", err)
	}
	po, err := cli.NewProjectOptions(
		[]string{outPrimary, overridePath},
		cli.WithWorkingDirectory(dir),
		cli.WithName("proj"),
	)
	if err != nil {
		t.Fatalf("cli.NewProjectOptions: %v", err)
	}
	proj, err := po.LoadProject(context.Background())
	if err != nil {
		t.Fatalf("compose-go rejected rewritten compose + agent-cpu.yml: %v", err)
	}
	agentSvc := proj.Services["keploy-agent"]
	if agentSvc.CPUS != 0.02 {
		t.Fatalf("keploy-agent.CPUS = %v, want 0.02", agentSvc.CPUS)
	}
	if agentSvc.ContainerName != opts.KeployContainer {
		t.Fatalf("keploy-agent.ContainerName = %q, want %q", agentSvc.ContainerName, opts.KeployContainer)
	}
	if agentSvc.Image == "" {
		t.Fatalf("keploy-agent.Image is empty")
	}
}

// A document compose's schema refuses is refused with compose's own message,
// never a panic in its loader, which keploy runs with the schema unchecked.
func TestComposeIdentity_SchemaRefusalIsComposesError(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"the app's networks left empty", "services:\n  web:\n    image: x\n    networks:\n", "services.web.networks must be a array"},
		{"another service's volumes left empty", "services:\n  web:\n    image: x\n  db:\n    image: x\n    volumes:\n", "services.db.volumes must be a array"},
		{"a service with no body", "services:\n  web:\n    image: x\n  db:\n", "services.db must be a mapping"},
	} {
		for _, inMemory := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/in-memory=%v", tc.name, inMemory), func(t *testing.T) {
				dir := composeProjectDir(t, map[string]string{"compose.yml": tc.doc})
				t.Chdir(dir)
				var err error
				func() {
					defer func() {
						if r := recover(); r != nil {
							err = fmt.Errorf("keploy panicked: %v", r)
						}
					}()
					if inMemory {
						_, err = findInContent(t, tc.doc, ComposeScope{}, "web")
					} else {
						_, err = findIn(t, dir, []string{"compose.yml"}, ComposeScope{}, "web")
					}
				}()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("got %v, want compose's %q", err, tc.want)
				}
			})
		}
	}
}

// loadSchemaUnchecked recovers panics on both the unchecked load and the
// schema-checked retry, returning compose's loader message rather than crashing
// keploy.
func TestLoadSchemaUnchecked_RecoversSecondPanic(t *testing.T) {
	runNoPanic := func(load func(extra ...func(*loader.Options)) (*composetypes.Project, error)) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("loadSchemaUnchecked panicked: %v", r)
			}
		}()
		_, err = loadSchemaUnchecked(load)
		return err
	}

	calls := 0
	err := runNoPanic(func(extra ...func(*loader.Options)) (*composetypes.Project, error) {
		calls++
		if calls == 1 {
			panic("first boom")
		}
		panic("second boom")
	})
	if calls != 2 {
		t.Fatalf("load called %d times, want 2", calls)
	}
	if err == nil || !strings.Contains(err.Error(), "compose's loader failed on the document: second boom") {
		t.Fatalf("got %v, want second loader panic message", err)
	}

	calls = 0
	err = runNoPanic(func(extra ...func(*loader.Options)) (*composetypes.Project, error) {
		calls++
		if calls == 1 {
			panic("first boom")
		}
		return &composetypes.Project{Name: "p"}, nil
	})
	if calls != 2 {
		t.Fatalf("load called %d times, want 2", calls)
	}
	if err == nil || !strings.Contains(err.Error(), "compose's loader failed on the document: first boom") {
		t.Fatalf("got %v, want first loader panic message", err)
	}
}

// TestComposeIdentity_MultiFileOverrideMovesConflictsAndPassesComposeValidation
// verifies that when a companion compose file sets ports, networks, dns, and
// extra_hosts on the app service, FindContainerInComposeFiles collects them
// and ModifyComposeForAgent moves them to keploy-agent and strips them from the
// companion file so compose-go's own full validation accepts the rewritten pair.
func TestComposeIdentity_MultiFileOverrideMovesConflictsAndPassesComposeValidation(t *testing.T) {
	dir := composeProjectDir(t, map[string]string{
		"base.yml": `services:
  app:
    image: alpine:3.21
    networks: []
    ports:
      - "8080:8080"
  db:
    image: alpine:3.21
`,
		"override.yml": `x-override-dns: &override_dns
  - "1.1.1.1"
services:
  app:
    container_name: override-app
    network_mode: bridge
    pid: host
    hostname: myhost
    domainname: example.local
    mac_address: "02:42:ac:11:00:02"
    links:
      - db
    expose:
      - "9090"
    ports:
      - "8883:80"
    networks:
      - overnet
    dns: *override_dns
    extra_hosts:
      - "api.local:127.0.0.1"
      - "host.docker.internal:10.0.0.9"
  sidecar:
    image: alpine:3.21
networks:
  overnet: {}
`,
	})
	basePath := filepath.Join(dir, "base.yml")
	overridePath := filepath.Join(dir, "override.yml")

	idc, err := New(zap.NewNop(), &config.Config{})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	// Duplicate path must never put primaryPath into ExtraFiles.
	dupInfo, err := idc.FindContainerInComposeFiles(context.Background(), []string{basePath, basePath}, ComposeScope{}, "proj-app-1")
	if err != nil {
		t.Fatalf("FindContainerInComposeFiles(dup): %v", err)
	}
	if len(dupInfo.ExtraFiles) != 0 {
		t.Fatalf("duplicate path produced ExtraFiles = %v, want empty", dupInfo.ExtraFiles)
	}

	info, err := idc.FindContainerInComposeFiles(context.Background(), []string{basePath, overridePath}, ComposeScope{}, "override-app")
	if err != nil {
		t.Fatalf("FindContainerInComposeFiles: %v", err)
	}
	if len(info.ExtraFiles) != 1 || info.ExtraFiles[0].Path != overridePath {
		t.Fatalf("ExtraFiles = %v, want single companion %q", info.ExtraFiles, overridePath)
	}
	if len(info.Networks) != 1 || info.Networks[0] != "overnet" {
		t.Fatalf("Networks = %v, want [overnet] (empty base networks: [] must not add default)", info.Networks)
	}
	gotServices := info.Compose.ResolvedServiceNames()
	if strings.Join(gotServices, ",") != "app,db,sidecar" {
		t.Fatalf("ResolvedServiceNames() = %v, want [app db sidecar]", gotServices)
	}
	opts := buildAgentOpts()
	opts.Container = "override-app"
	opts.AppPorts = info.Ports
	opts.AppNetworks = info.Networks
	opts.HostGateway = true
	if err := idc.ModifyComposeForAgent(info.Compose, opts, info.AppServiceName); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}
	outPrimary := filepath.Join(dir, "docker-compose-tmp.yaml")
	outOverride := filepath.Join(dir, "docker-compose-tmp-1.yaml")
	if err := idc.WriteComposeFile(info.Compose, outPrimary); err != nil {
		t.Fatalf("WriteComposeFile(primary): %v", err)
	}
	if err := idc.WriteComposeFile(info.ExtraFiles[0].Compose, outOverride); err != nil {
		t.Fatalf("WriteComposeFile(override): %v", err)
	}

	// Load the generated pair with compose-go's full validation enabled.
	po, err := cli.NewProjectOptions(
		[]string{outPrimary, outOverride},
		cli.WithWorkingDirectory(dir),
		cli.WithName("proj"),
	)
	if err != nil {
		t.Fatalf("cli.NewProjectOptions: %v", err)
	}
	proj, err := po.LoadProject(context.Background())
	if err != nil {
		t.Fatalf("compose-go rejected rewritten multi-file pair: %v", err)
	}

	appSvc := proj.Services["app"]
	if appSvc.NetworkMode != "service:keploy-agent" {
		t.Fatalf("app.NetworkMode = %q, want service:keploy-agent", appSvc.NetworkMode)
	}
	if appSvc.Pid != "service:keploy-agent" {
		t.Fatalf("app.Pid = %q, want service:keploy-agent", appSvc.Pid)
	}
	if appSvc.Hostname != "" || appSvc.DomainName != "" || appSvc.MacAddress != "" || len(appSvc.Links) != 0 || len(appSvc.Expose) != 0 {
		t.Fatalf("app still carries stripped host/link/expose fields: hostname=%q domainname=%q mac=%q links=%v expose=%v",
			appSvc.Hostname, appSvc.DomainName, appSvc.MacAddress, appSvc.Links, appSvc.Expose)
	}
	if appSvc.ContainerName != "override-app" {
		t.Fatalf("app.ContainerName = %q, want override-app", appSvc.ContainerName)
	}
	if len(appSvc.Ports) != 0 || len(appSvc.Networks) != 0 || len(appSvc.DNS) != 0 || len(appSvc.ExtraHosts) != 0 {
		t.Fatalf("app still carries conflicting keys: ports=%v networks=%v dns=%v extra_hosts=%v",
			appSvc.Ports, appSvc.Networks, appSvc.DNS, appSvc.ExtraHosts)
	}

	agentSvc := proj.Services["keploy-agent"]
	if _, ok := agentSvc.Networks["overnet"]; !ok || len(agentSvc.Networks) != 1 {
		t.Fatalf("keploy-agent.Networks = %v, want only overnet", agentSvc.Networks)
	}
	var gotPublished []string
	for _, p := range agentSvc.Ports {
		gotPublished = append(gotPublished, p.Published)
	}
	if !strings.Contains(strings.Join(gotPublished, ","), "8080") || !strings.Contains(strings.Join(gotPublished, ","), "8883") {
		t.Fatalf("keploy-agent published ports = %v, want both 8080 and 8883", gotPublished)
	}
	if len(agentSvc.DNS) != 1 || agentSvc.DNS[0] != "1.1.1.1" {
		t.Fatalf("keploy-agent.DNS = %v, want [1.1.1.1]", agentSvc.DNS)
	}
	if got := agentSvc.ExtraHosts["api.local"]; len(got) != 1 || got[0] != "127.0.0.1" {
		t.Fatalf("keploy-agent.ExtraHosts[api.local] = %v, want [127.0.0.1]", got)
	}
	// User's override mapping for host.docker.internal must beat host-gateway.
	if got := agentSvc.ExtraHosts["host.docker.internal"]; len(got) != 1 || got[0] != "10.0.0.9" {
		t.Fatalf("keploy-agent.ExtraHosts[host.docker.internal] = %v, want [10.0.0.9]", got)
	}

	// Companion setting ONLY network_mode: host (no ports/networks/dns/extra_hosts)
	// must still be detected as conflicting and stripped so it cannot overwrite
	// keploy's network_mode: service:keploy-agent on merge.
	t.Run("solo network_mode override is detected and stripped", func(t *testing.T) {
		soloDir := composeProjectDir(t, map[string]string{
			"base.yml":     "services:\n  app:\n    image: alpine:3.21\n",
			"override.yml": "services:\n  app:\n    network_mode: host\n",
		})
		bp := filepath.Join(soloDir, "base.yml")
		op := filepath.Join(soloDir, "override.yml")
		soloInfo, err := idc.FindContainerInComposeFiles(context.Background(), []string{bp, op}, ComposeScope{}, "proj-app-1")
		if err != nil {
			t.Fatalf("FindContainerInComposeFiles(solo): %v", err)
		}
		if len(soloInfo.ExtraFiles) != 1 || soloInfo.ExtraFiles[0].Path != op {
			t.Fatalf("solo network_mode override ExtraFiles = %v, want [%q]", soloInfo.ExtraFiles, op)
		}
		soloOpts := buildAgentOpts()
		soloOpts.Container = "proj-app-1"
		if err := idc.ModifyComposeForAgent(soloInfo.Compose, soloOpts, soloInfo.AppServiceName); err != nil {
			t.Fatalf("ModifyComposeForAgent(solo): %v", err)
		}
		p1 := filepath.Join(soloDir, "docker-compose-tmp.yaml")
		p2 := filepath.Join(soloDir, "docker-compose-tmp-1.yaml")
		if err := idc.WriteComposeFile(soloInfo.Compose, p1); err != nil {
			t.Fatal(err)
		}
		if err := idc.WriteComposeFile(soloInfo.ExtraFiles[0].Compose, p2); err != nil {
			t.Fatal(err)
		}
		po2, err := cli.NewProjectOptions([]string{p1, p2}, cli.WithWorkingDirectory(soloDir), cli.WithName("proj"))
		if err != nil {
			t.Fatal(err)
		}
		proj2, err := po2.LoadProject(context.Background())
		if err != nil {
			t.Fatalf("compose-go rejected solo network_mode rewrite: %v", err)
		}
		if got := proj2.Services["app"].NetworkMode; got != "service:keploy-agent" {
			t.Fatalf("solo override app.NetworkMode = %q, want service:keploy-agent", got)
		}
	})
}
