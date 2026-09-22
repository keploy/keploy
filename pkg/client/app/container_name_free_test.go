package app

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// TestContainerNameFreeIgnoresDockerWarningsOnStderr is the third instance of
// one mistake, and the most expensive.
//
// containerNameFree's verdict is "stdout is empty", so reading the merged
// streams made ANY byte on stderr mean "this name is taken" — for every name,
// permanently. Docker writes to stderr on plenty of healthy exit-0 runs: a
// malformed ~/.docker/config.json, a credential-helper warning, a deprecation
// notice.
//
// The cost is not cosmetic. ensureContainerNameFree then polls its full 90s
// budget before every docker-run start, and isDockerRunNameConflict treats
// every exit-125 as a name conflict — retrying a genuinely broken run
// (bad image, unsatisfiable mount) five times with a 90s removal between
// attempts, so the real error takes minutes to appear.
func TestContainerNameFreeIgnoresDockerWarningsOnStderr(t *testing.T) {
	dir := t.TempDir()
	// Empty stdout (no container holds the name) plus a warning on stderr, at
	// exit 0 — exactly what a malformed docker client config produces.
	script := "#!/bin/sh\n" +
		"printf 'WARNING: Error parsing config file: unexpected end of JSON input\\n' >&2\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("could not write the stand-in docker: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a := &App{logger: zap.NewNop()}

	if !a.containerNameFree("keploy-v3") {
		t.Fatal("a docker warning on stderr made an unused container name read as taken; every " +
			"docker-run start then waits out the full 90s name-free budget, and every exit-125 is " +
			"retried as a name conflict")
	}
}

// TestContainerNameFreeStillDetectsATakenName keeps the guard honest — without
// it, hardcoding `return true` would satisfy the test above.
func TestContainerNameFreeStillDetectsATakenName(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '3f9a1c2b4d5e\\n'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("could not write the stand-in docker: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a := &App{logger: zap.NewNop()}

	if a.containerNameFree("keploy-v3") {
		t.Fatal("a name held by a real container reported as free; the next `docker run --name` " +
			"would hit a conflict")
	}
}
