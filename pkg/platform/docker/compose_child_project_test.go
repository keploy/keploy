package docker

import (
	"os"
	"path/filepath"
	"testing"
)

// serviceOf reads compose's container naming backwards from the document.
//
// Ported from apptail_test.go's TestServiceOfReadsComposeNamesBackwards, which
// asked the same question of the compose-go project the in-process library
// loaded. The library is gone; the question is not, because compose still
// prints `<container> | line` and the tail still has to say which service that
// is.
//
// The cases that matter are the ambiguous ones. "app-1" is BOTH the default
// name of service "app" replica 1 and a service in its own right, and an
// explicit container_name must win over any default that could also claim it —
// which is why the explicit pass runs over every service before the default
// pass runs at all.
func TestServiceOfReadsComposeNamesBackwards(t *testing.T) {
	p := &composeChildProject{name: "p", containerNames: map[string]string{
		"app":   "",
		"app-1": "",
		"db":    "orders-db",
	}}
	for name, want := range map[string]string{
		"app-1":     "app",
		"app-12":    "app",
		"app-1-1":   "app-1",
		"orders-db": "db",
		"db-1":      "", // db names its container, so the default cannot claim it
		"app-":      "", // a number is required
		"app-x":     "",
		"other-1":   "",
	} {
		if got := p.serviceOf(name); got != want {
			t.Errorf("serviceOf(%q) = %q, want %q", name, got, want)
		}
	}
	if (*composeChildProject)(nil).serviceOf("app-1") != "" {
		t.Error("a nil project answered a service")
	}
}

// THE DOCUMENT IS READ WITHOUT compose-go, so this pins what is read out of it:
// every service, and the container_name only where one is set. A service that
// sets none must still be PRESENT with "", because the <service>-<number>
// fallback applies only to those.
func TestLoadComposeChildProjectReadsServicesAndContainerNames(t *testing.T) {
	doc := []byte(`
services:
  app:
    image: shop:v1
  db:
    image: postgres:16
    container_name: orders-db
`)
	p, err := loadComposeChildProject(doc, "proj", "/tmp/whatever", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.name != "proj" {
		t.Errorf("name = %q, want proj", p.name)
	}
	if got, ok := p.containerNames["app"]; !ok || got != "" {
		t.Errorf(`containerNames["app"] = %q, present=%v; a service with no container_name must be present with ""`, got, ok)
	}
	if p.containerNames["db"] != "orders-db" {
		t.Errorf(`containerNames["db"] = %q, want orders-db`, p.containerNames["db"])
	}
	if p.serviceOf("app-1") != "app" || p.serviceOf("orders-db") != "db" {
		t.Error("the loaded document does not answer the names compose prints")
	}
}

// The project name follows compose's precedence. The caller's name wins,
// because app.go has already resolved it from the command; the directory base
// is the last resort and is normalised as compose normalises it.
func TestComposeChildProjectNamePrecedence(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "from-env")
	if got := composeChildProjectName("explicit", "fromdoc", "/srv/My_App", nil); got != "explicit" {
		t.Errorf("explicit name = %q, want explicit", got)
	}
	if got := composeChildProjectName("  ", "fromdoc", "/srv/My_App", nil); got != "from-env" {
		t.Errorf("env name = %q, want from-env", got)
	}
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	// The DOCUMENT's own name sits above the directory and below the
	// environment -- the level the first cut of this reader missed, which
	// TestComposeChildOutputProjectFollowsComposePrecedence caught.
	if got := composeChildProjectName("", "fromdoc", "/srv/My_App", nil); got != "fromdoc" {
		t.Errorf("document name = %q, want fromdoc", got)
	}
	if got := composeChildProjectName("", "", "/srv/My_App", nil); got != "my_app" {
		t.Errorf("dir name = %q, want my_app (lower-cased, non-[a-z0-9_-] dropped)", got)
	}
}

// A document keploy cannot read is an error, not an empty project: answering
// with no services would make every container line unattributed and look like
// a document that simply names none.
func TestLoadComposeChildProjectRefusesAnUnreadableDocument(t *testing.T) {
	if _, err := loadComposeChildProject([]byte("services: [this is not a map"), "p", "/tmp", nil); err == nil {
		t.Error("an unparsable document loaded without error")
	}
}

// THE ENV FILES COUNT TOWARDS THE PROJECT NAME, and this reader's first cut
// dropped them -- caught by TestComposeChildTeeTakesTheProjectComposeTakes's
// --env-file row ("project = app, want envfileproj").
//
// It matters because the name is what the tails are recorded UNDER: get it
// wrong and the output is kept against a project nothing reads back, so a
// crashed app shows no recent output at all.
func TestComposeChildProjectNameReadsTheEnvFiles(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "keploy.env")
	if err := os.WriteFile(envFile, []byte("# a comment\n\nexport COMPOSE_PROJECT_NAME=\"envfileproj\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(composeProjectNameEnv, "")
	t.Setenv(composeEnvFilesEnv, "")

	if got := composeChildProjectName("", "", dir, []string{envFile}); got != "envfileproj" {
		t.Errorf("--env-file name = %q, want envfileproj", got)
	}
	// The SHELL beats a file: the caller checks the process environment first.
	t.Setenv(composeProjectNameEnv, "fromshell")
	if got := composeChildProjectName("", "", dir, []string{envFile}); got != "fromshell" {
		t.Errorf("with the shell set, name = %q, want fromshell", got)
	}
	// And COMPOSE_ENV_FILES is read when no --env-file was given.
	t.Setenv(composeProjectNameEnv, "")
	t.Setenv(composeEnvFilesEnv, envFile)
	if got := composeChildProjectName("", "", dir, nil); got != "envfileproj" {
		t.Errorf("COMPOSE_ENV_FILES name = %q, want envfileproj", got)
	}
}

// The project directory's .env is the last file compose reads, and only when
// nothing named one. A missing .env is not an error -- compose runs without it.
func TestComposeChildProjectNameFallsBackToDotEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(composeProjectNameEnv, "")
	t.Setenv(composeEnvFilesEnv, "")
	t.Setenv("COMPOSE_DISABLE_ENV_FILE", "")

	if got := composeChildProjectName("", "", filepath.Join(dir, "My_Dir"), nil); got != "my_dir" {
		t.Errorf("with no .env, name = %q, want the directory base", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("COMPOSE_PROJECT_NAME=dotenvproj\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := composeChildProjectName("", "", dir, nil); got != "dotenvproj" {
		t.Errorf(".env name = %q, want dotenvproj", got)
	}
}
