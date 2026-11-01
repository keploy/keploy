package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAppGateLiveReleaseWithoutHealthcheckCliff proves the #4894 park:
// the app does not start while the release file is missing, even for longer
// than the old healthcheck budget would have allowed a flake, and starts
// promptly once the CLI-equivalent writes the release file. Uses
// --abort-on-container-failure so the gate's successful exit does not abort
// the stack (the flag SetupCompose now injects when a completion dependency
// is present).
func TestAppGateLiveReleaseWithoutHealthcheckCliff(t *testing.T) {
	if testing.Short() {
		t.Skip("live docker compose")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not available")
	}

	dir := t.TempDir()
	vol := filepath.Join(dir, "tls")
	if err := os.MkdirAll(vol, 0o755); err != nil {
		t.Fatal(err)
	}
	// Stale marker from a prior run on the persisted volume — the gate must
	// rm this before waiting, or the app would start before the CLI release.
	if err := os.WriteFile(filepath.Join(vol, "agent.ready"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Compose file mounts need forward slashes even on Windows.
	volMount := filepath.ToSlash(vol)
	project := fmt.Sprintf("keploy-gate-%d", time.Now().UnixNano())
	composePath := filepath.Join(dir, "compose.yaml")
	marker := filepath.Join(dir, "app-started")
	markerMount := filepath.ToSlash(marker)

	yaml := fmt.Sprintf(`
services:
  keploy-agent:
    image: alpine:3.21
    command: ["sh", "-c", "sleep 600"]
  %s:
    image: alpine:3.21
    restart: "no"
    volumes:
      - %s:/tmp/keploy-tls
    depends_on:
      keploy-agent:
        condition: service_started
    command: ["sh", "-c", "rm -f /tmp/keploy-tls/agent.ready; while [ ! -f /tmp/keploy-tls/agent.ready ]; do sleep 0.25; done"]
  app:
    image: alpine:3.21
    volumes:
      - %s:/tmp/keploy-tls:ro
      - %s:/out
    depends_on:
      keploy-agent:
        condition: service_started
      %s:
        condition: service_completed_successfully
    command: ["sh", "-c", "echo started > /out/app-started; sleep 600"]
`, KeployAppGateServiceName, volMount, volMount, filepath.ToSlash(dir), KeployAppGateServiceName)

	if err := os.WriteFile(composePath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := exec.CommandContext(ctx, "docker", "compose", "-p", project, "-f", composePath,
		"up", "--abort-on-container-failure", "--pull", "missing")
	up.Stdout = os.Stdout
	up.Stderr = os.Stderr
	if err := up.Start(); err != nil {
		t.Fatalf("compose up: %v", err)
	}
	defer func() {
		cancel()
		_ = up.Wait()
		down := exec.Command("docker", "compose", "-p", project, "-f", composePath, "down", "-v", "--remove-orphans")
		_ = down.Run()
	}()

	// Wait well past a short health budget to show the app stays parked with
	// no release file — 20s is enough to prove ordering without a multi-minute CI.
	park := 20 * time.Second
	deadline := time.Now().Add(park)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(markerMount); err == nil {
			t.Fatalf("app started before release file was written (within %s park)", park)
		}
		time.Sleep(500 * time.Millisecond)
	}

	release := filepath.Join(vol, "agent.ready")
	if err := os.WriteFile(release, []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("write release file: %v", err)
	}

	waitUntil := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(markerMount); err == nil {
			body, _ := os.ReadFile(markerMount)
			if strings.Contains(string(body), "started") {
				return
			}
		}
		if time.Now().After(waitUntil) {
			t.Fatal("app did not start within 60s after release file was written")
		}
		time.Sleep(500 * time.Millisecond)
	}
}
