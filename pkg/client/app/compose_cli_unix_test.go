//go:build unix

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// offlineComposeDocker wraps a real docker.Client so compose parsing/rewriting
// runs verbatim while ContainerRemove/ContainerInspect return NotFound without
// talking to a Docker daemon.
type offlineComposeDocker struct {
	docker.Client
}

func (offlineComposeDocker) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	return errdefs.NotFound(errors.New("not found"))
}

func (offlineComposeDocker) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{}, errdefs.NotFound(errors.New("not found"))
}

// installComposeEnvRecorder puts a stand-in `docker` binary at the front of
// PATH that logs each call's subcommand arguments, CECN_REQ and APP_NETWORK
// env values, and stdin (when `-f -` is passed), and answers `ps` with either
// an agent container ID (`ps -aq`) or JSON state (`ps -a --format json`).
func installComposeEnvRecorder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
{
  printf 'CALL'
  for a in "$@"; do printf ' %s' "$a"; done
  printf '\n'
  printf 'ENV CECN_REQ=%s APP_NETWORK=%s\n' "${CECN_REQ:-<unset>}" "${APP_NETWORK:-<unset>}"
  for a in "$@"; do
    if [ "$a" = "-" ]; then
      printf 'STDIN_HAS_16799='
      if grep -q '16799'; then printf 'yes\n'; else printf 'no\n'; fi
      break
    fi
  done
} >> "` + logPath + `"
for a in "$@"; do
  if [ "$a" = "-aq" ]; then
    printf 'agent-id-123\n'
    exit 0
  fi
  if [ "$a" = "json" ]; then
    printf '{"ID":"db-id-1","Name":"proj-db-1","Service":"db","State":"running"}\n'
    exit 0
  fi
done
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write docker stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestComposeDownAndPSProbes_PassAllFilesAndInlineEnv exercises the three real
// compose CLI call sites (ComposeDown, composeAgentContainerIDs, and
// composeServiceStatesOf) through a recording `docker` binary on PATH, for
// both multi-file SetupCompose and in-memory setupComposeInMemory +
// moveComposeAgent.
func TestComposeDownAndPSProbes_PassAllFilesAndInlineEnv(t *testing.T) {
	if composeLibrarySupported() {
		// In-process compose library builds do not shell out to the docker CLI
		// for down/ps.
		return
	}

	t.Run("multi-file SetupCompose forwards all rewritten files and inline env to down and both ps probes", func(t *testing.T) {
		for _, k := range []string{"COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR", "COMPOSE_COMPATIBILITY", "CECN_REQ"} {
			t.Setenv(k, "")
		}
		t.Setenv("APP_NETWORK", "exported-net")
		logPath := installComposeEnvRecorder(t)

		root := filepath.Join(t.TempDir(), "multiproj")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		const baseYML = "name: cecn-cli-proj\nservices:\n  app:\n    image: alpine:3.21\n  db:\n    image: alpine:3.21\n    environment:\n      - REQ=${CECN_REQ:?set CECN_REQ}\n"
		const overYML = "services:\n  app:\n    container_name: cli-app\n    ports:\n      - \"8080:8080\"\n    networks: [overnet]\n  sidecar:\n    image: alpine:3.21\nnetworks:\n  overnet:\n    name: ${APP_NETWORK}\n"
		if err := os.WriteFile(filepath.Join(root, "base.yml"), []byte(baseYML), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "override.yml"), []byte(overYML), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)

		idc, err := docker.New(zap.NewNop(), &config.Config{})
		if err != nil {
			t.Fatal(err)
		}
		opts := models.SetupOptions{Container: "cli-app", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791}
		a := NewApp(zap.NewNop(), "CECN_REQ=1 APP_NETWORK=inline-net COMPOSE_FILE=base.yml:override.yml docker compose up", offlineComposeDocker{idc}, opts)
		if err := a.SetupCompose(context.Background(), nil); err != nil {
			t.Fatalf("SetupCompose: %v", err)
		}

		// 1. ComposeDown
		a.ComposeDown()
		// 2. composeAgentContainerIDs (`ps -aq keploy-agent`)
		ids := a.composeAgentContainerIDs(context.Background())
		if len(ids) != 1 || ids[0] != "agent-id-123" {
			t.Fatalf("composeAgentContainerIDs = %v, want [agent-id-123]", ids)
		}
		// 3. composeServiceStatesOf (`ps -a --format json`)
		states := a.composeServiceStatesOf(context.Background(), "cecn-cli-proj")
		if len(states) != 1 || states[0].Service != "db" {
			t.Fatalf("composeServiceStatesOf = %+v, want single db service", states)
		}

		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read calls.log: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		wantSubs := []string{"down --timeout 1", "ps -aq keploy-agent", "ps -a --format json"}
		callIdx := 0
		for i := 0; i < len(lines); i++ {
			if !strings.HasPrefix(lines[i], "CALL ") {
				continue
			}
			if callIdx >= len(wantSubs) {
				t.Fatalf("unexpected extra docker call: %s\nfull log:\n%s", lines[i], string(raw))
			}
			callLine := lines[i]
			if !strings.Contains(callLine, "-f docker-compose-tmp.yaml -f docker-compose-tmp-1.yaml") {
				t.Errorf("call %d missing both rewritten -f files: %s", callIdx, callLine)
			}
			if !strings.Contains(callLine, wantSubs[callIdx]) {
				t.Errorf("call %d = %q, want subcommand %q", callIdx, callLine, wantSubs[callIdx])
			}
			if i+1 >= len(lines) || lines[i+1] != "ENV CECN_REQ=1 APP_NETWORK=inline-net" {
				t.Errorf("call %d (%s) saw wrong env: %q\nfull log:\n%s", callIdx, wantSubs[callIdx], lines[i+1], string(raw))
			}
			callIdx++
		}
		if callIdx != len(wantSubs) {
			t.Fatalf("saw %d docker calls, want %d\nfull log:\n%s", callIdx, len(wantSubs), string(raw))
		}
	})

	t.Run("in-memory setupComposeInMemory + moveComposeAgent forwards -f -, moved stdin, and inline env to down and both ps probes", func(t *testing.T) {
		for _, k := range []string{"COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR", "COMPOSE_COMPATIBILITY", "CECN_REQ"} {
			t.Setenv(k, "")
		}
		t.Setenv("APP_NETWORK", "exported-net")
		logPath := installComposeEnvRecorder(t)

		const doc = "name: cecn-mem-proj\nservices:\n  app:\n    image: alpine:3.21\n    container_name: mem-cli-app\n    environment:\n      - REQ=${CECN_REQ:?set CECN_REQ}\n    networks: [appnet]\nnetworks:\n  appnet:\n    name: ${APP_NETWORK}\n"
		idc, err := docker.New(zap.NewNop(), &config.Config{})
		if err != nil {
			t.Fatal(err)
		}
		opts := models.SetupOptions{
			Container:       "mem-cli-app",
			InMemoryCompose: []byte(doc),
			AgentPort:       16789,
			ProxyPort:       16790,
			DnsPort:         16791,
		}
		a := NewApp(zap.NewNop(), "CECN_REQ=1 APP_NETWORK=inline-net docker compose up", offlineComposeDocker{idc}, opts)
		if err := a.setupComposeInMemory(context.Background(), nil); err != nil {
			t.Fatalf("setupComposeInMemory: %v", err)
		}
		if err := a.moveComposeAgent(16789, 16799); err != nil {
			t.Fatalf("moveComposeAgent: %v", err)
		}

		a.ComposeDown()
		ids := a.composeAgentContainerIDs(context.Background())
		if len(ids) != 1 || ids[0] != "agent-id-123" {
			t.Fatalf("composeAgentContainerIDs = %v, want [agent-id-123]", ids)
		}
		states := a.composeServiceStatesOf(context.Background(), "cecn-mem-proj")
		if len(states) != 1 || states[0].Service != "db" {
			t.Fatalf("composeServiceStatesOf = %+v, want single db service", states)
		}

		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read calls.log: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		wantSubs := []string{"down --timeout 1", "ps -aq keploy-agent", "ps -a --format json"}
		callIdx := 0
		for i := 0; i < len(lines); i++ {
			if !strings.HasPrefix(lines[i], "CALL ") {
				continue
			}
			if callIdx >= len(wantSubs) {
				t.Fatalf("unexpected extra docker call: %s\nfull log:\n%s", lines[i], string(raw))
			}
			if !strings.Contains(lines[i], "-f -") || !strings.Contains(lines[i], wantSubs[callIdx]) {
				t.Errorf("call %d = %q, want -f - and %q", callIdx, lines[i], wantSubs[callIdx])
			}
			if i+2 >= len(lines) || lines[i+1] != "ENV CECN_REQ=1 APP_NETWORK=inline-net" || lines[i+2] != "STDIN_HAS_16799=yes" {
				t.Errorf("call %d (%s) lost inline env or moved stdin:\n%s", callIdx, wantSubs[callIdx], string(raw))
			}
			callIdx++
		}
		if callIdx != len(wantSubs) {
			t.Fatalf("saw %d docker calls, want %d\nfull log:\n%s", callIdx, len(wantSubs), string(raw))
		}
	})
}
