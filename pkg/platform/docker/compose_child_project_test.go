package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serviceOfOutput reads compose's container naming backwards, from the name
// compose prints a container's lines under (its container_name, else its
// name without the project's prefix).
//
// The cases that matter are the ambiguous ones. "app-1" is BOTH the default
// name of service "app" replica 1 and a service in its own right, and an
// explicit container_name must win over any default that could also claim it,
// which is why every container_name is looked at before any default.
func TestServiceOfOutputReadsComposeNamesBackwards(t *testing.T) {
	ci := &composeIdentity{project: "p", separator: composeContainerSeparator, containerNames: map[string]string{
		"app":   "",
		"app-1": "",
		"db":    "orders-db",
		"web":   "app-2",
	}}
	for name, want := range map[string]string{
		"app-1":     "app",
		"app-12":    "app",
		"app-1-1":   "app-1",
		"orders-db": "db",
		"app-2":     "web", // web names its container so; app's replica 2 cannot claim it
		"db-1":      "",    // db names its container, so the default cannot claim it
		"app-":      "",    // a number is required
		"app-0":     "",    // compose numbers replicas from 1
		"app-01":    "",    // written as %d
		"app-x":     "",
		"app_1":     "", // the separator outside compatibility mode is -
		"p-app-1":   "", // compose prints a default name without the project's prefix
		"other-1":   "",
	} {
		if got := ci.serviceOfOutput(name); got != want {
			t.Errorf("serviceOfOutput(%q) = %q, want %q", name, got, want)
		}
	}
	compat := &composeIdentity{project: "p", separator: composeCompatibilitySeparator, containerNames: map[string]string{"app": "", "db": "orders-db"}}
	for name, want := range map[string]string{"app_1": "app", "app_3": "app", "app-1": "", "orders-db": "db", "app_": ""} {
		if got := compat.serviceOfOutput(name); got != want {
			t.Errorf("in compatibility mode serviceOfOutput(%q) = %q, want %q", name, got, want)
		}
	}
	if (*composeIdentity)(nil).serviceOfOutput("app-1") != "" {
		t.Error("a nil project answered a service")
	}
}

// childProject starts a compose child's output for doc in scope, with env set,
// from a fresh current directory named My.Child holding files.
func childProject(t *testing.T, doc string, files, env map[string]string, scope ComposeScope) (*ComposeChildOutput, error) {
	t.Helper()
	ForgetAppOutput()
	t.Cleanup(ForgetAppOutput)
	dir := composeProjectDir(t, files)
	for k, v := range env {
		t.Setenv(k, v)
	}
	named := filepath.Join(filepath.Dir(dir), "My.Child")
	if err := os.Rename(dir, named); err != nil {
		t.Fatal(err)
	}
	t.Chdir(named)
	return StartComposeChildOutput(context.Background(), []byte(doc), scope)
}

// The project the tails are kept under is the one `docker compose -f -` makes
// of keploy's document: -p, else COMPOSE_PROJECT_NAME (keploy's environment,
// else the env files'), else the document's name: interpolated and lowercased
// as compose names containers, else the current directory's name, normalized.
// The parse-server shape names its project from a variable with a default.
func TestComposeChildOutputProjectIsTheOneComposeMakes(t *testing.T) {
	const parseShape = "name: ${PARSE_PROJECT:-parse-server-mongo}\nservices:\n  parse-server:\n    image: x\n    container_name: ${APP_CONTAINER:-parse-server-mongo-app}\n"
	for _, tc := range []struct {
		name  string
		doc   string
		files map[string]string
		env   map[string]string
		scope ComposeScope
		want  string
	}{
		{name: "name: ${VAR:-default}, unset: the default", doc: parseShape, want: "parse-server-mongo"},
		{name: "name: ${VAR:-default}, set: the value, lowercased", doc: parseShape,
			env: map[string]string{"PARSE_PROJECT": "PS.Lane"}, want: "pslane"},
		{name: "name: ${VAR:-default}, set in the .env", doc: parseShape,
			files: map[string]string{".env": "PARSE_PROJECT=from-dotenv\n"}, want: "from-dotenv"},
		{name: "name: as written, lowercased", doc: "name: Parse.Server\nservices:\n  app:\n    image: x\n", want: "parseserver"},
		{name: "-p beats name:", doc: parseShape, scope: ComposeScope{ProjectName: "flagged"}, want: "flagged"},
		{name: "COMPOSE_PROJECT_NAME beats name:", doc: parseShape, env: map[string]string{composeProjectNameEnv: "fromenv"}, want: "fromenv"},
		{name: "COMPOSE_PROJECT_NAME in an --env-file", doc: parseShape,
			files: map[string]string{"keploy.env": "# a comment\n\nexport COMPOSE_PROJECT_NAME=\"envfileproj\"\n"},
			scope: ComposeScope{EnvFiles: []string{"keploy.env"}}, want: "envfileproj"},
		{name: "keploy's environment beats an --env-file", doc: parseShape,
			files: map[string]string{"keploy.env": "COMPOSE_PROJECT_NAME=envfileproj\n"},
			env:   map[string]string{composeProjectNameEnv: "fromshell"},
			scope: ComposeScope{EnvFiles: []string{"keploy.env"}}, want: "fromshell"},
		{name: "COMPOSE_PROJECT_NAME in COMPOSE_ENV_FILES", doc: parseShape,
			files: map[string]string{"keploy.env": "COMPOSE_PROJECT_NAME=envfilesproj\n"},
			env:   map[string]string{composeEnvFilesEnv: "keploy.env"}, want: "envfilesproj"},
		{name: "COMPOSE_PROJECT_NAME in the .env", doc: parseShape,
			files: map[string]string{".env": "COMPOSE_PROJECT_NAME=dotenvproj\n"}, want: "dotenvproj"},
		{name: "no name: the directory's, normalized", doc: "services:\n  app:\n    image: x\n", want: "mychild"},
		{name: "--project-directory names the directory", doc: "services:\n  app:\n    image: x\n",
			files: map[string]string{"Sub_Dir/keep": ""}, scope: ComposeScope{ProjectDir: "Sub_Dir"}, want: "sub_dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := childProject(t, tc.doc, tc.files, tc.env, tc.scope)
			if err != nil {
				t.Fatalf("StartComposeChildOutput: %v", err)
			}
			if o.Project() != tc.want {
				t.Fatalf("Project() = %q, want %q", o.Project(), tc.want)
			}
		})
	}
}

// A line is kept against the service compose printed it for: the
// container_name the document sets from a variable, as compose resolves it,
// and in compatibility mode a replica's <service>_<number>.
func TestComposeChildOutputKeepsALineAgainstTheResolvedService(t *testing.T) {
	const doc = "services:\n  parse-server:\n    image: x\n    container_name: ${APP_CONTAINER:-parse-server-mongo-app}\n  worker:\n    image: y\n"
	for _, tc := range []struct {
		name    string
		files   map[string]string
		env     map[string]string
		scope   ComposeScope
		printed string
		service string
	}{
		{name: "a container_name's default", printed: "parse-server-mongo-app", service: "parse-server"},
		{name: "a container_name from keploy's environment", env: map[string]string{"APP_CONTAINER": "psst-app"},
			printed: "psst-app", service: "parse-server"},
		{name: "a replica", printed: "worker-1", service: "worker"},
		{name: "a replica under --compatibility", scope: ComposeScope{Compatibility: true}, printed: "worker_2", service: "worker"},
		{name: "a replica under COMPOSE_COMPATIBILITY in the .env", files: map[string]string{".env": "COMPOSE_COMPATIBILITY=true\n"},
			printed: "worker_1", service: "worker"},
		{name: "a replica under COMPOSE_COMPATIBILITY=y", env: map[string]string{composeCompatibilityEnv: " Y "},
			printed: "worker_1", service: "worker"},
		{name: "COMPOSE_COMPATIBILITY false", env: map[string]string{composeCompatibilityEnv: "false"},
			printed: "worker-1", service: "worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := childProject(t, doc, tc.files, tc.env, tc.scope)
			if err != nil {
				t.Fatalf("StartComposeChildOutput: %v", err)
			}
			write(t, o.Stream(), tc.printed+"  | boom\n")
			if got := string(ServiceOutputTail(o.Project(), tc.service)); !strings.Contains(got, "boom") {
				t.Fatalf("service %q's tail = %q, want the line %s printed", tc.service, got, tc.printed)
			}
		})
	}
}

// A document keploy cannot resolve is an error, not an empty project:
// answering with no services would make every container line unattributed and
// look like a document that simply names none.
func TestStartComposeChildOutputRefusesADocumentComposeRefuses(t *testing.T) {
	for name, doc := range map[string]string{
		"not YAML":                  "services: [this is not a map",
		"a required variable unset": "services:\n  app:\n    image: x\n    container_name: ${APP_CONTAINER:?set APP_CONTAINER}\n",
		"empty":                     "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := childProject(t, doc, nil, nil, ComposeScope{}); err == nil {
				t.Fatal("the document loaded without error")
			}
		})
	}
}
