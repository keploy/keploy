package replay

import (
	"os"
	"path/filepath"
	"testing"
)

// A compose replay had no readiness gate at all: dockerPublishedHostPort only
// recognises -p/--publish, which `docker compose up` never carries, so the
// fixed --delay was the whole protection — and keploy's own generated compose
// holds the app behind the agent's healthcheck, routinely spending it.
func TestComposePublishedHostPort(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "docker-compose.yml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	for _, tc := range []struct {
		name          string
		compose       string
		containerName string
		wantHost      string
		wantPort      string
		wantOK        bool
	}{
		{
			name: "app identified by container_name among several services",
			compose: `services:
  api:
    container_name: parse_app
    ports: ["6076:1337"]
  mongo:
    container_name: parse_mongo
    ports: ["27017:27017"]
`,
			containerName: "parse_app", wantHost: "127.0.0.1", wantPort: "6076", wantOK: true,
		},
		{
			name: "a db port is never mistaken for the app when the name does not match",
			compose: `services:
  api:
    container_name: parse_app
    ports: ["6076:1337"]
  mongo:
    container_name: parse_mongo
    ports: ["27017:27017"]
`,
			containerName: "something_else", wantOK: false,
		},
		{
			name: "lone publishing service is unambiguous even with no container_name",
			compose: `services:
  api:
    ports: ["8080:8080"]
  mongo:
    image: mongo:7
`,
			wantHost: "127.0.0.1", wantPort: "8080", wantOK: true,
		},
		{
			name: "explicit host ip is honoured",
			compose: `services:
  api:
    container_name: app
    ports: ["192.168.1.5:9090:80"]
`,
			containerName: "app", wantHost: "192.168.1.5", wantPort: "9090", wantOK: true,
		},
		{
			name: "long form",
			compose: `services:
  api:
    container_name: app
    ports:
      - target: 80
        published: 8081
        protocol: tcp
`,
			containerName: "app", wantHost: "127.0.0.1", wantPort: "8081", wantOK: true,
		},
		{
			name: "container-only publish names no host port",
			compose: `services:
  api:
    container_name: app
    ports: ["8080"]
`,
			containerName: "app", wantOK: false,
		},
		{
			name: "ranges are refused rather than guessed",
			compose: `services:
  api:
    container_name: app
    ports: ["8000-8010:8000-8010"]
`,
			containerName: "app", wantOK: false,
		},
		{
			name: "two publishing services and no name match stays ambiguous",
			compose: `services:
  a:
    ports: ["1111:1111"]
  b:
    ports: ["2222:2222"]
`,
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, tc.compose)
			host, port, ok := composePublishedHostPort("docker compose -f "+path+" up", tc.containerName)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (host=%q port=%q)", ok, tc.wantOK, host, port)
			}
			if !tc.wantOK {
				return
			}
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("got %s:%s, want %s:%s", host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

// A non-compose command must be left exactly as it was: the -p gate owns that
// case, and guessing a port here could make replay wait on something unrelated.
func TestComposePublishedHostPortIgnoresNonCompose(t *testing.T) {
	for _, cmd := range []string{
		"docker run -p 8080:8080 myapp",
		"go run ./cmd/app",
		"npm start",
		"",
	} {
		if _, _, ok := composePublishedHostPort(cmd, "app"); ok {
			t.Errorf("claimed a port for a non-compose command: %q", cmd)
		}
	}
}

// A missing or unparseable compose file must decline, never panic or guess.
func TestComposePublishedHostPortDegradesQuietly(t *testing.T) {
	if _, _, ok := composePublishedHostPort("docker compose -f /nonexistent/compose.yml up", "app"); ok {
		t.Error("claimed a port from a file that does not exist")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(bad, []byte("services: [this is not: valid: yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := composePublishedHostPort("docker compose -f "+bad+" up", "app"); ok {
		t.Error("claimed a port from unparseable YAML")
	}
}
