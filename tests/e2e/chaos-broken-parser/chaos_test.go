// Package chaos is a `go test` wrapper around the chaos-broken-parser
// harness. It exists so CI (and developers) can run the e2e via `go
// test ./tests/e2e/chaos-broken-parser/...` rather than shelling out
// to docker compose directly. The test transparently skips when
// Docker is unavailable, which is the expected state in most
// sandboxed environments (the harness is a docker-compose-driven
// integration test, not a unit test).
package chaos_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestChaosBrokenParser invokes the harness as a subprocess with a
// bounded deadline. It does NOT import the harness package directly —
// the harness is a `package main` with a dependency on the docker CLI
// that only makes sense as a standalone binary. Running it via `go
// run` (rather than compiling it ahead of time) keeps the test
// self-contained.
//
// Skips when:
//   - The `e2e` build tag is NOT set AND KEPLOY_E2E is not truthy
//     (both gates are respected so developers can opt into running
//     this locally without a tag rebuild).
//   - The `docker` CLI or `docker compose` subcommand is missing.
//   - GOOS != linux (the compose stack uses Linux-only images and
//     bind-mounts; macOS and Windows developers should rely on the
//     Linux CI path).
func TestChaosBrokenParser(t *testing.T) {
	if !e2eEnabled() {
		t.Skip("skipping: e2e tests are opt-in (set KEPLOY_E2E=1 or run with `-tags e2e`)")
	}
	if runtime.GOOS != "linux" {
		t.Skipf("skipping: chaos-broken-parser e2e requires Linux (GOOS=%s)", runtime.GOOS)
	}
	if err := probeDocker(t); err != nil {
		t.Skipf("skipping: docker is unavailable (%v)", err)
	}

	// The harness lives at ../harness relative to this test file.
	// `filepath.Dir(runtime.Caller(0))` resolves to the test dir, so we
	// can form an unambiguous path that survives `go test` being
	// invoked from any cwd (particularly: `go test ./...` from repo
	// root).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	testDir := filepath.Dir(thisFile)
	harnessPkg := filepath.Join(testDir, "harness")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "run", "./"+filepath.Base(harnessPkg))
	cmd.Dir = testDir
	cmd.Env = os.Environ()
	// Pipe straight through so failure output lands in `go test -v`
	// without any re-buffering.
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("harness failed: %v", err)
	}
}

// testWriter is a tiny io.Writer that forwards lines into t.Log so
// the harness' stderr/stdout interleave with the test framework's
// output. Avoids the need for a buffered channel + drain goroutine
// pair.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	// Strip a trailing newline so t.Log doesn't emit double blanks;
	// t.Log appends its own newline.
	s := strings.TrimRight(string(p), "\n")
	if s != "" {
		w.t.Log(s)
	}
	return len(p), nil
}

// e2eEnabled returns true when the caller has opted into running the
// (slow, docker-dependent) e2e tests. Honouring both KEPLOY_E2E=1 and
// `-tags e2e` mirrors the convention used by other opt-in integration
// tests in the repo.
func e2eEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("KEPLOY_E2E")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return e2eTagEnabled // set to true in chaos_test_e2e_tag.go, false in chaos_test_no_tag.go
}

// probeDocker is a quick "is docker compose usable?" check. Returns
// nil when both the `docker` binary and the `docker compose` plugin
// are available on PATH.
func probeDocker(t *testing.T) error {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "compose", "version").Run()
}
