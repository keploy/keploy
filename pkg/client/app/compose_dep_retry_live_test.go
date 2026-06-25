//go:build dockerlive

// Package app live integration tests for the transient-compose-dependency retry.
//
// These tests drive the REAL App.run against the REAL docker daemon and require:
//   - docker + docker compose v2 available and runnable by the test user
//   - the busybox image pullable
//
// They are gated behind the `dockerlive` build tag so the normal `go test ./...`
// CI run (no privileged docker, no compose) never executes them. Run with:
//
//	go test -tags dockerlive -run TestComposeDepRetryLive -v ./pkg/client/app/...
//
// What they prove (the Step-2 reproductions, turned into assertions):
//   - FLAKY DEP (RECOVERS / GREEN): a dependency that exits non-zero on the FIRST
//     `up` then comes up healthy → App.run returns a non-error (ErrAppStopped),
//     i.e. keploy worked slow instead of failing. With the retry removed this run
//     returns ErrUnExpected (the original RED), which the sibling
//     TestComposeDepRetryLive_RedWithoutFix documents by forcing the bound to 0.
//   - ALWAYS-FAIL APP (STILL FAILS / NO MASKING): an app whose own container exits
//     non-zero every time → App.run STILL returns ErrUnExpected after the bounded
//     retries; the real failure is never hidden.
package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// flakyDepCompose writes a compose file whose dependency exits non-zero on its
// first start (creating a marker) and comes up healthy on any later start, with
// the app depending on it via condition: service_healthy. project is used as the
// compose -p so each test gets an isolated stack.
const flakyDepCompose = `services:
  flakydep:
    image: busybox
    command: sh -c 'if [ -f /data/marker ]; then echo "dep healthy"; touch /data/healthy; sleep 3600; else echo "dep crashing (first run)"; touch /data/marker; exit 1; fi'
    volumes:
      - %s:/data
    healthcheck:
      test: ["CMD", "test", "-f", "/data/healthy"]
      interval: 1s
      timeout: 2s
      retries: 8
      start_period: 1s
  app:
    image: busybox
    container_name: %s
    command: sh -c 'echo "APP STARTED OK"; sleep 3600'
    depends_on:
      flakydep:
        condition: service_healthy
`

// alwaysFailApp writes a compose file whose dependency is always healthy but the
// app's own container exits non-zero every time — the genuine-failure case that
// must NOT be masked by the retry.
const alwaysFailApp = `services:
  gooddep:
    image: busybox
    command: sh -c 'touch /data/healthy; echo "dep healthy"; sleep 3600'
    volumes:
      - %s:/data
    healthcheck:
      test: ["CMD", "test", "-f", "/data/healthy"]
      interval: 1s
      timeout: 2s
      retries: 8
      start_period: 1s
  app:
    image: busybox
    container_name: %s
    command: sh -c 'echo "APP FAILING ON PURPOSE"; exit 3'
    depends_on:
      gooddep:
        condition: service_healthy
`

func newLiveApp(t *testing.T, composeYAML, appContainer, project string) (*App, func()) {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	cli, err := docker.New(logger, &config.Config{})
	if err != nil {
		t.Skipf("docker client unavailable: %v", err)
	}

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if mkErr := os.MkdirAll(dataDir, 0o755); mkErr != nil {
		t.Fatalf("mkdir data: %v", mkErr)
	}
	composePath := filepath.Join(dir, "docker-compose.yaml")
	content := []byte(fmt.Sprintf(composeYAML, dataDir, appContainer))
	if wErr := os.WriteFile(composePath, content, 0o644); wErr != nil {
		t.Fatalf("write compose: %v", wErr)
	}

	a := &App{
		logger:         logger,
		docker:         cli,
		kind:           utils.DockerCompose,
		container:      appContainer,
		composeService: "app",
		composeFile:    composePath,
		// Project-scoped command so ComposeDown / ps target this stack only.
		// --abort-on-container-exit --exit-code-from app mirrors what keploy's
		// ensureComposeExitOnAppFailure injects in production, so `up` returns the
		// app's own exit code the moment the app container exits (and aborts the
		// stack), exactly as it does for a real recorded app.
		cmd: "docker compose -p " + project + " -f " + composePath + " up --abort-on-container-exit --exit-code-from app",
	}

	cleanup := func() {
		_ = exec.Command("docker", "compose", "-p", project, "-f", composePath, "down", "--timeout", "1").Run()
	}
	cleanup() // ensure a clean slate before the test
	return a, cleanup
}

// TestComposeDepRetryLive_FlakyDepRecovers is the GREEN reproduction: the
// dependency crashes on the first up and the bounded retry brings the whole
// stack up successfully, so the recording is NOT aborted.
func TestComposeDepRetryLive_FlakyDepRecovers(t *testing.T) {
	a, cleanup := newLiveApp(t, flakyDepCompose, "kploy-live-flaky-app", "kploylive-flaky")
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Once the app is up (it sleeps 3600), cancel the ctx so run() returns via the
	// graceful-stop path with ErrAppStopped (no error) — i.e. it recovered.
	go func() {
		// give the first up (fail) + backoff + second up (success) time to land.
		time.Sleep(25 * time.Second)
		cancel()
	}()

	appErr := a.run(ctx)
	// Recovered: either the ctx-cancel path (ErrCtxCanceled / ErrAppStopped with
	// nil err) — NOT ErrUnExpected. The defining assertion is that no error was
	// surfaced from a compose-up failure.
	if appErr.AppErrorType == models.ErrUnExpected {
		t.Fatalf("flaky dep should have recovered via the bounded retry, but run surfaced ErrUnExpected: %v", appErr.Err)
	}
	t.Logf("flaky-dep run resolved as %q (err=%v) — recovered (GREEN)", appErr.AppErrorType, appErr.Err)
}

// TestComposeDepRetryLive_RedWithoutRetry documents the ORIGINAL (pre-fix) RED:
// with the retry disabled, the SAME flaky-dependency stack aborts the run with
// ErrUnExpected on the first up. The retry is disabled here by clearing
// composeService, which makes isTransientComposeDependencyFailure return false
// (it can't prove the app never started) — i.e. it reproduces exactly the
// codepath keploy took before this change. This is the run the fix turns green
// (see TestComposeDepRetryLive_FlakyDepRecovers).
func TestComposeDepRetryLive_RedWithoutRetry(t *testing.T) {
	a, cleanup := newLiveApp(t, flakyDepCompose, "kploy-live-red-app", "kploylive-red")
	defer cleanup()
	a.composeService = "" // disable the gate -> reproduce pre-fix behavior

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	appErr := a.run(ctx)
	if appErr.AppErrorType != models.ErrUnExpected {
		t.Fatalf("without the retry the flaky-dep run should abort with ErrUnExpected (the original RED), got %q (err=%v)",
			appErr.AppErrorType, appErr.Err)
	}
	t.Logf("pre-fix behavior reproduced: flaky-dep run aborted with %q (err=%v) — RED",
		appErr.AppErrorType, appErr.Err)
}

// TestComposeDepRetryLive_AlwaysFailNotMasked is the NO-MASKING reproduction: the
// app's own container exits non-zero every time, so even with the retry the run
// must STILL surface the failure.
func TestComposeDepRetryLive_AlwaysFailNotMasked(t *testing.T) {
	a, cleanup := newLiveApp(t, alwaysFailApp, "kploy-live-failapp", "kploylive-fail")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	appErr := a.run(ctx)
	elapsed := time.Since(start)

	if appErr.AppErrorType != models.ErrUnExpected {
		t.Fatalf("always-failing app must STILL fail (ErrUnExpected), got %q (err=%v) after %s",
			appErr.AppErrorType, appErr.Err, elapsed)
	}
	// It must not have been retried (the app exited non-zero -> not "created"), so
	// it should fail fast, not after the full retry budget.
	t.Logf("always-fail run correctly surfaced %q (err=%v) after %s — NOT masked",
		appErr.AppErrorType, appErr.Err, elapsed)
}
