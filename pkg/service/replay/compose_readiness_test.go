package replay

import (
	"os"
	"path/filepath"
	"strings"
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
			name: "a bracketed IPv6 host ip is read as docker reads it",
			compose: `services:
  api:
    container_name: app
    ports: ["[::1]:8080:80"]
`,
			containerName: "app", wantHost: "::1", wantPort: "8080", wantOK: true,
		},
		{
			name: "an IPv6 host ip with a random host port names none",
			compose: `services:
  api:
    container_name: app
    ports: ["[::1]::80"]
`,
			containerName: "app", wantOK: false,
		},
		{
			name: "a UDP port is passed over for the TCP one",
			compose: `services:
  api:
    container_name: app
    ports: ["5353:5353/udp", "8080:80"]
`,
			containerName: "app", wantHost: "127.0.0.1", wantPort: "8080", wantOK: true,
		},
		{
			name: "a long form UDP port is passed over for the TCP one",
			compose: `services:
  api:
    container_name: app
    ports:
      - target: 53
        published: 5353
        protocol: udp
      - target: 80
        published: 8081
`,
			containerName: "app", wantHost: "127.0.0.1", wantPort: "8081", wantOK: true,
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
			host, port, ok, err := composePublishedHostPort("docker compose -f "+path+" up", tc.containerName)
			if err != nil || ok != tc.wantOK {
				t.Fatalf("ok = %v, %v, want %v, nil (host=%q port=%q)", ok, err, tc.wantOK, host, port)
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
		if _, _, ok, err := composePublishedHostPort(cmd, "app"); ok || err != nil {
			t.Errorf("claimed a port for a non-compose command: %q (%v)", cmd, err)
		}
	}
}

// The app's port entry keploy cannot read a host address from is an error,
// never an address to wait on: split on its colons, "abc:8080" named port
// "abc", where no wait ever ends. Another service's is not the app's, and
// says nothing.
func TestComposePublishedHostPortRefusesAnEntryItCannotRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	body := `services:
  api:
    container_name: app
    ports: ["abc:8080"]
  db:
    container_name: db
    ports: ["[::1]:5432"]
  cache:
    container_name: cache
    ports: ["6379:6379"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := "docker compose -f " + path + " up"
	host, port, ok, err := composePublishedHostPort(cmd, "app")
	if err == nil || ok || !strings.Contains(err.Error(), "abc:8080") {
		t.Errorf("app: (%q,%q,%v,%v), want an error naming its entry", host, port, ok, err)
	}
	if host, port, ok, err := composePublishedHostPort(cmd, "cache"); err != nil || !ok || host != "127.0.0.1" || port != "6379" {
		t.Errorf("cache: (%q,%q,%v,%v), want 127.0.0.1:6379", host, port, ok, err)
	}
}

// A missing or unparseable compose file must decline, never panic or guess.
func TestComposePublishedHostPortDegradesQuietly(t *testing.T) {
	if _, _, ok, err := composePublishedHostPort("docker compose -f /nonexistent/compose.yml up", "app"); ok || err != nil {
		t.Errorf("claimed a port from a file that does not exist (%v)", err)
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(bad, []byte("services: [this is not: valid: yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := composePublishedHostPort("docker compose -f "+bad+" up", "app"); ok || err != nil {
		t.Errorf("claimed a port from unparseable YAML (%v)", err)
	}
}

// The ports are read as compose reads them: variables expanded from the
// environment, then the project's .env, and a long-form entry by the short
// form's rules (docker's). Read as written, a long-form published "${PORT}",
// 0, "abc" or 70000 was a port to wait on, a bracketed host_ip was joined
// as "[[::1]]", and a short "${PORT}:8080" could not be read.
func TestComposePublishedHostPortReadsThePortsAsComposeDoes(t *testing.T) {
	t.Setenv("KEPLOY_ROW_PORT", "18080")
	t.Setenv("KEPLOY_ROW_UNSET", "")
	if err := os.Unsetenv("KEPLOY_ROW_UNSET"); err != nil {
		t.Fatal(err)
	}
	long := func(fields string) string { return "\n      - target: 80\n" + fields }
	for _, c := range []struct {
		name, ports        string
		wantHost, wantPort string
		wantOK, wantErr    bool
	}{
		{name: "short, a set variable", ports: ` ["${KEPLOY_ROW_PORT}:8080"]`, wantHost: "127.0.0.1", wantPort: "18080", wantOK: true},
		{name: "short, a default", ports: ` ["${KEPLOY_ROW_UNSET:-8081}:8080"]`, wantHost: "127.0.0.1", wantPort: "8081", wantOK: true},
		{name: "short, from .env", ports: ` ["${KEPLOY_ROW_DOTENV}:8080"]`, wantHost: "127.0.0.1", wantPort: "18082", wantOK: true},
		{name: "short, an unset variable is docker's pick", ports: ` ["${KEPLOY_ROW_UNSET}:8080"]`},
		{name: "long, a set variable", ports: long(`        published: "${KEPLOY_ROW_PORT}"`), wantHost: "127.0.0.1", wantPort: "18080", wantOK: true},
		{name: "long, from .env", ports: long(`        published: "${KEPLOY_ROW_DOTENV}"`), wantHost: "127.0.0.1", wantPort: "18082", wantOK: true},
		{name: "long, an unset variable is docker's pick", ports: long(`        published: "${KEPLOY_ROW_UNSET}"`)},
		{name: "long, port 0 is docker's pick", ports: long(`        published: 0`)},
		{name: "long, a host range", ports: long(`        published: "8000-8005"`)},
		{name: "long, no port", ports: long(`        published: abc`), wantErr: true},
		{name: "long, out of range", ports: long(`        published: 70000`), wantErr: true},
		{name: "long, an IPv6 host_ip", ports: long("        published: 8080\n        host_ip: \"::1\""), wantHost: "::1", wantPort: "8080", wantOK: true},
		{name: "long, a bracketed host_ip is no IP address", ports: long("        published: 8080\n        host_ip: \"[::1]\""), wantErr: true},
		{name: "long, UDP in capitals is passed over", ports: long("        published: 5353\n        protocol: UDP") + long("        published: 8081"), wantHost: "127.0.0.1", wantPort: "8081", wantOK: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "docker-compose.yml")
			body := "services:\n  api:\n    container_name: app\n    ports:" + c.ports + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KEPLOY_ROW_DOTENV=18082\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			host, port, ok, err := composePublishedHostPort("docker compose -f "+path+" up", "app")
			if (err != nil) != c.wantErr || ok != c.wantOK || host != c.wantHost || port != c.wantPort {
				t.Fatalf("= (%q,%q,%v,%v), want (%q,%q,%v, error %v)\n%s", host, port, ok, err, c.wantHost, c.wantPort, c.wantOK, c.wantErr, body)
			}
		})
	}
}
