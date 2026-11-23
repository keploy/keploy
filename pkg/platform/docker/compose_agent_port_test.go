package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// agentComposeFor is in, an application's compose document, as keploy runs
// it in mock mode with its agent on port 16789 (ModifyComposeForAgent).
func agentComposeFor(t *testing.T, in string) (*Impl, *Compose) {
	t.Helper()
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatal(err)
	}
	opts := buildAgentOpts()
	opts.MockMode = true
	opts.AppPorts = []string{"8080:8080"}
	if err := idc.ModifyComposeForAgent(&compose, opts, "app"); err != nil {
		t.Fatal(err)
	}
	return idc, &compose
}

// A move of keploy's agent, in the compose document keploy runs, moves its
// publish on the host's loopback, the port its server is told, and the
// address a mock run's tests are handed, whatever shape the application's
// environment has; nothing else moves: the application's own ports, the
// proxy's and DNS server's ports, another service's environment.
func TestMoveComposeAgentPortMovesThePublishThePortAndTheTestsAddress(t *testing.T) {
	for name, in := range map[string]string{
		"a list environment": "services:\n  app:\n    image: alpine:3.21\n    ports: [\"8080:8080\"]\n    environment:\n      - A=1\n  db:\n    image: alpine:3.21\n    environment:\n      - KEPLOY_MOCK_AGENT=http://localhost:16789\n  other:\n    image: alpine:3.21\n    environment:\n      - KEPLOY_MOCK_AGENT=http://elsewhere:16789\n",
		"a map environment":  "services:\n  app:\n    image: alpine:3.21\n    ports: [\"8080:8080\"]\n    environment:\n      A: \"1\"\n  db:\n    image: alpine:3.21\n    environment:\n      KEPLOY_MOCK_AGENT: http://localhost:16789\n  other:\n    image: alpine:3.21\n    environment:\n      KEPLOY_MOCK_AGENT: http://elsewhere:16789\n",
	} {
		t.Run(name, func(t *testing.T) {
			idc, compose := agentComposeFor(t, in)
			if err := MoveComposeAgentPort(compose, 16789, 20001); err != nil {
				t.Fatal(err)
			}
			// Each value is written as the one it replaced: --port's as
			// --proxy-port's, which was written alike and stays.
			cmd := agentList(compose, "command").Content
			for i := 0; i+1 < len(cmd); i++ {
				if cmd[i].Value == "--port" && (cmd[i+1].Tag != cmd[i+3].Tag || cmd[i+1].Style != cmd[i+3].Style || cmd[i+2].Value != "--proxy-port") {
					t.Fatalf("--port's value is written %q/%v, --proxy-port's %q/%v; want them alike", cmd[i+1].Tag, cmd[i+1].Style, cmd[i+3].Tag, cmd[i+3].Style)
				}
			}
			svcs := composeOut(t, idc, compose)
			ports, _ := svcs["keploy-agent"]["ports"].([]interface{})
			if !slices.Contains(ports, interface{}("127.0.0.1:20001:20001")) || !slices.Contains(ports, interface{}("8080:8080")) || len(ports) != 2 {
				t.Fatalf("keploy-agent publishes %q, want 127.0.0.1:20001:20001 and the app's 8080:8080", ports)
			}
			var command []string
			for _, c := range svcs["keploy-agent"]["command"].([]interface{}) {
				command = append(command, fmt.Sprint(c))
			}
			joined := " " + strings.Join(command, " ") + " "
			if !strings.Contains(joined, " --port 20001 ") || !strings.Contains(joined, " --proxy-port 16790 ") || !strings.Contains(joined, " --dns-port 16791 ") || strings.Contains(joined, "16789") {
				t.Fatalf("keploy-agent is told %q, want --port 20001 and the proxy's and DNS server's ports as they were", joined)
			}
			env := envOf(svcs["app"]["environment"])
			if env["KEPLOY_MOCK_AGENT"] != "http://localhost:20001" || env["A"] != "1" {
				t.Fatalf("the app's environment %v, want KEPLOY_MOCK_AGENT=http://localhost:20001 beside A=1", env)
			}
			if got := envOf(svcs["db"]["environment"])["KEPLOY_MOCK_AGENT"]; got != "http://localhost:20001" {
				t.Fatalf("db's KEPLOY_MOCK_AGENT %q, want the agent's new address", got)
			}
			if got := envOf(svcs["other"]["environment"])["KEPLOY_MOCK_AGENT"]; got != "http://elsewhere:16789" {
				t.Fatalf("another address in KEPLOY_MOCK_AGENT became %q; want it kept", got)
			}
		})
	}
}

// envOf is a service's environment, list or map, as a map.
func envOf(v interface{}) map[string]string {
	env := map[string]string{}
	switch e := v.(type) {
	case []interface{}:
		for _, kv := range e {
			k, val, _ := strings.Cut(kv.(string), "=")
			env[k] = val
		}
	case map[string]interface{}:
		for k, val := range e {
			env[k], _ = val.(string)
		}
	}
	return env
}

// A document in which keploy's agent is not published on the port it is
// moved from, or not told it, exactly once each, is not moved at all, and
// the move's error says so: an agent half moved would be told one port and
// published on another.
func TestMoveComposeAgentPortLeavesADocumentItCannotMoveWhole(t *testing.T) {
	for name, edit := range map[string]func(c *Compose){
		"no agent service": func(c *Compose) {
			for i := 0; i+1 < len(c.Services.Content); i += 2 {
				if c.Services.Content[i].Value == "keploy-agent" {
					c.Services.Content = append(c.Services.Content[:i], c.Services.Content[i+2:]...)
					return
				}
			}
		},
		"published on another port": func(c *Compose) { agentList(c, "ports").Content[0].Value = "127.0.0.1:16788:16789" },
		"published twice": func(c *Compose) {
			l := agentList(c, "ports")
			l.Content = append(l.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "127.0.0.1:16789:16789"})
		},
		"told another port": func(c *Compose) {
			l := agentList(c, "command")
			for i, n := range l.Content {
				if n.Value == "--port" {
					l.Content[i+1].Value = "16788"
				}
			}
		},
		"told the port twice": func(c *Compose) {
			l := agentList(c, "command")
			l.Content = append(l.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "--port"}, &yaml.Node{Kind: yaml.ScalarNode, Value: "16789"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			idc, compose := agentComposeFor(t, "services:\n  app:\n    image: alpine:3.21\n")
			edit(compose)
			before, err := idc.MarshalCompose(compose)
			if err != nil {
				t.Fatal(err)
			}
			if err := MoveComposeAgentPort(compose, 16789, 20001); err == nil {
				t.Fatal("the move reported success")
			}
			after, _ := idc.MarshalCompose(compose)
			if string(after) != string(before) {
				t.Fatalf("the move changed a document it refused:\n%s\nwas\n%s", after, before)
			}
		})
	}
	if err := MoveComposeAgentPort(nil, 16789, 20001); err == nil {
		t.Fatal("a move with no document reported success")
	}
}

// agentList is keploy-agent's list under key in c.
func agentList(c *Compose, key string) *yaml.Node {
	for i := 0; i+1 < len(c.Services.Content); i += 2 {
		if c.Services.Content[i].Value != "keploy-agent" {
			continue
		}
		svc := c.Services.Content[i+1]
		for j := 0; j+1 < len(svc.Content); j += 2 {
			if svc.Content[j].Value == key {
				return svc.Content[j+1]
			}
		}
	}
	return &yaml.Node{}
}

// ComposePublishedHostPorts extracts every static host port published by
// non-agent services (short syntax with or without bind IP/IPv6/proto, port
// ranges, long syntax, and YAML aliases) while ignoring dynamic publishes and
// keploy-agent itself.
func TestComposePublishedHostPortsExtractsStaticHostPorts(t *testing.T) {
	doc := `
x-shared-ports: &shared_ports
  - "61509:9000"
services:
  keploy-agent:
    image: keploy:dev
    ports:
      - "127.0.0.1:61999:61999"
  app:
    image: alpine:3.21
    ports:
      - "61500:80"
      - "127.0.0.1:61501:80/tcp"
      - "[::1]:61502:80/udp"
      - "61503-61505:8000-8002"
      - "8080"
      - ":8081"
      - "127.0.0.1::8082"
      - target: 90
        published: 61506
        protocol: tcp
      - target: 91
        published: "61507-61508"
  worker:
    image: alpine:3.21
    ports: *shared_ports
`
	got := ComposePublishedHostPortsFromYAML([]byte(doc))
	want := []uint32{61500, 61501, 61502, 61503, 61504, 61505, 61506, 61507, 61508, 61509}
	if !slices.Equal(got, want) {
		t.Fatalf("ComposePublishedHostPortsFromYAML = %v; want %v", got, want)
	}
}

// FindComposeFiles follows Docker Compose precedence: explicit -f/--file flags
// first, then COMPOSE_FILE (split by COMPOSE_PATH_SEPARATOR or
// filepath.ListSeparator), then default base + override files in the working
// directory.
func TestFindComposeFilesPrecedenceOverrideAndEnv(t *testing.T) {
	writeFile := func(t *testing.T, dir, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("explicit -f and --file override COMPOSE_FILE and default files", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "docker-compose.yml")
		writeFile(t, dir, "docker-compose.override.yml")
		t.Chdir(dir)
		t.Setenv("COMPOSE_FILE", "from-env.yml")
		t.Setenv("COMPOSE_PATH_SEPARATOR", "")

		got := FindComposeFiles("docker compose -f a.yml --file b.yml -f=c.yml --file=d.yml up")
		want := []string{"a.yml", "b.yml", "c.yml", "d.yml"}
		if !slices.Equal(got, want) {
			t.Fatalf("FindComposeFiles = %v; want %v", got, want)
		}
	})

	t.Run("COMPOSE_FILE split by default filepath.ListSeparator", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "docker-compose.yml")
		writeFile(t, dir, "docker-compose.override.yml")
		t.Chdir(dir)
		sep := string(filepath.ListSeparator)
		t.Setenv("COMPOSE_FILE", "first.yml"+sep+sep+" second.yml ")
		t.Setenv("COMPOSE_PATH_SEPARATOR", "")

		got := FindComposeFiles("make up")
		want := []string{"first.yml", "second.yml"}
		if !slices.Equal(got, want) {
			t.Fatalf("FindComposeFiles = %v; want %v", got, want)
		}
	})

	t.Run("COMPOSE_FILE split by custom COMPOSE_PATH_SEPARATOR", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		t.Setenv("COMPOSE_FILE", "base.yml,extra.yml")
		t.Setenv("COMPOSE_PATH_SEPARATOR", ",")

		got := FindComposeFiles("make up")
		want := []string{"base.yml", "extra.yml"}
		if !slices.Equal(got, want) {
			t.Fatalf("FindComposeFiles = %v; want %v", got, want)
		}
	})

	t.Run("default discovery includes docker-compose.override.yml", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "docker-compose.yml")
		writeFile(t, dir, "docker-compose.override.yml")
		t.Chdir(dir)
		t.Setenv("COMPOSE_FILE", "")
		t.Setenv("COMPOSE_PATH_SEPARATOR", "")

		got := FindComposeFiles("docker compose up")
		want := []string{"docker-compose.yml", "docker-compose.override.yml"}
		if !slices.Equal(got, want) {
			t.Fatalf("FindComposeFiles = %v; want %v", got, want)
		}
	})

	t.Run("default discovery includes compose.override.yml", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "compose.yml")
		writeFile(t, dir, "compose.override.yml")
		t.Chdir(dir)
		t.Setenv("COMPOSE_FILE", "")
		t.Setenv("COMPOSE_PATH_SEPARATOR", "")

		got := FindComposeFiles("docker compose up")
		want := []string{"compose.yml", "compose.override.yml"}
		if !slices.Equal(got, want) {
			t.Fatalf("FindComposeFiles = %v; want %v", got, want)
		}
	})

	t.Run("default discovery without override returns base only", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "docker-compose.yml")
		t.Chdir(dir)
		t.Setenv("COMPOSE_FILE", "")
		t.Setenv("COMPOSE_PATH_SEPARATOR", "")

		got := FindComposeFiles("docker compose up")
		want := []string{"docker-compose.yml"}
		if !slices.Equal(got, want) {
			t.Fatalf("FindComposeFiles = %v; want %v", got, want)
		}
	})

	t.Run("orphan override without base returns nil", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "docker-compose.override.yml")
		t.Chdir(dir)
		t.Setenv("COMPOSE_FILE", "")
		t.Setenv("COMPOSE_PATH_SEPARATOR", "")

		if got := FindComposeFiles("docker compose up"); len(got) != 0 {
			t.Fatalf("FindComposeFiles = %v; want empty", got)
		}
	})
}

// ComposePublishedHostPortsFromFiles skips unreadable or unparseable compose
// files with a single Debug log naming the skipped file, while still returning
// the published host ports from every readable file (including overrides).
func TestComposePublishedHostPortsFromFilesLogsUnreadableAndReadsOverrides(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "docker-compose.yml")
	overridePath := filepath.Join(dir, "docker-compose.override.yml")
	missingPath := filepath.Join(dir, "missing-compose.yml")
	brokenPath := filepath.Join(dir, "broken-compose.yml")

	if err := os.WriteFile(basePath, []byte("services:\n  app:\n    image: alpine:3.21\n    ports:\n      - \"61520:8080\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overridePath, []byte("services:\n  admin:\n    image: alpine:3.21\n    ports:\n      - \"127.0.0.1:61521:9090\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brokenPath, []byte("services:\n  app: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	got := ComposePublishedHostPortsFromFiles(logger, []string{basePath, missingPath, overridePath})
	want := []uint32{61520, 61521}
	if !slices.Equal(got, want) {
		t.Fatalf("ComposePublishedHostPortsFromFiles = %v; want %v", got, want)
	}

	entries := logs.FilterLevelExact(zap.DebugLevel).All()
	if len(entries) != 1 {
		t.Fatalf("unreadable file emitted %d Debug logs (%v); want exactly 1", len(entries), entries)
	}
	if gotPath, _ := entries[0].ContextMap()["path"].(string); gotPath != missingPath {
		t.Fatalf("Debug log path = %q; want %q", gotPath, missingPath)
	}

	// Unparseable YAML also emits one Debug log naming the file.
	core2, logs2 := observer.New(zap.DebugLevel)
	got2 := ComposePublishedHostPortsFromFiles(zap.New(core2), []string{brokenPath, basePath})
	if !slices.Equal(got2, []uint32{61520}) {
		t.Fatalf("ComposePublishedHostPortsFromFiles with broken file = %v; want [61520]", got2)
	}
	entries2 := logs2.FilterLevelExact(zap.DebugLevel).All()
	if len(entries2) != 1 {
		t.Fatalf("unparseable file emitted %d Debug logs (%v); want exactly 1", len(entries2), entries2)
	}
	if gotPath, _ := entries2[0].ContextMap()["path"].(string); gotPath != brokenPath {
		t.Fatalf("Debug log path = %q; want %q", gotPath, brokenPath)
	}

	// Nil logger must not panic on unreadable files.
	if gotNil := ComposePublishedHostPortsFromFiles(nil, []string{missingPath, basePath}); !slices.Equal(gotNil, []uint32{61520}) {
		t.Fatalf("ComposePublishedHostPortsFromFiles(nil, ...) = %v; want [61520]", gotNil)
	}
}
