package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	for _, k := range []string{composeProjectNameEnv, composeEnvFilesEnv, "COMPOSE_DISABLE_ENV_FILE", "COMPOSE_PROFILES", composeCompatibilityEnv, "APP_CONTAINER", "APP_PROJECT", "PARSE_PROJECT"} {
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

// An empty in-memory document names no container.
func TestComposeIdentity_EmptyDocumentIsRefused(t *testing.T) {
	if _, err := findInContent(t, "", ComposeScope{}, "app"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("got %v, want the empty document refused", err)
	}
	if _, err := findInContent(t, "services: [not a map", ComposeScope{}, "app"); err == nil {
		t.Fatal("a document that is not compose YAML was read")
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
