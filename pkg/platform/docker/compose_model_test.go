package docker

import (
	"testing"
	"time"
)

// goldenCloudReplayCompose mirrors the FINAL (fully hook-modified) compose
// document `keploy cloud replay` produces: the injected keploy-agent service
// (with the enterprise shell-free healthcheck rewrite, low-latency cap_add /
// security_opt / tmpfs, and the bare-key host-inherit env) plus the app service
// (namespace-shared, TLS env, depends_on: service_healthy, restart:
// unless-stopped). It exercises every field the SDK orchestrator relies on.
const goldenCloudReplayCompose = `
services:
  keploy-agent:
    image: ghcr.io/keploy/keploy-enterprise:v2.0.0
    container_name: keploy-v3-abc123
    cap_add:
      - BPF
      - PERFMON
      - NET_ADMIN
      - SYS_ADMIN
    security_opt:
      - seccomp:unconfined
      - apparmor:unconfined
    environment:
      - BINARY_TO_DOCKER=true
      - CERT_EXPORT_PATH=/tmp/keploy-tls
      - KEPLOY_COUCHBASE_PASSWORD
    ports:
      - "16789:16789"
      - "26789:26789"
    volumes:
      - /sys/fs/cgroup:/sys/fs/cgroup
      - keploy-tls-certs:/tmp/keploy-tls
      - keploy-sockets-vol:/tmp
    tmpfs:
      - /sys/fs/bpf:exec,mode=0755
    command:
      - --port
      - "16789"
      - --mode
      - test
    dns_search:
      - default.svc.cluster.local
      - svc.cluster.local
    healthcheck:
      test:
        - CMD
        - /app/keploy-enterprise
        - health
        - --check=agent-ready
      interval: 5s
      timeout: 5s
      retries: 60
      start_period: 10s
    networks:
      default:
        aliases:
          - my-app
  my-app:
    image: my-registry.example.com/team/my-app:1.4.2
    container_name: my-app
    restart: unless-stopped
    working_dir: /srv
    environment:
      PGCHANNELBINDING: disable
      NODE_EXTRA_CA_CERTS: /tmp/keploy-tls/ca.crt
      UPSTREAM_URL: http://${BACKEND_HOST}:9000
      INHERITED_TOKEN:
    volumes:
      - keploy-tls-certs:/tmp/keploy-tls:ro
      - /tmp/keploy-cloud-replay-x/cfg:/etc/app:ro
    depends_on:
      keploy-agent:
        condition: service_healthy
    pid: service:keploy-agent
    network_mode: service:keploy-agent
volumes:
  keploy-tls-certs:
  keploy-sockets-vol:
`

func TestParseComposeForSDK_Golden(t *testing.T) {
	t.Setenv("KEPLOY_COUCHBASE_PASSWORD", "s3cr3t")
	t.Setenv("BACKEND_HOST", "backend.internal")
	t.Setenv("INHERITED_TOKEN", "tok-42")

	model, err := ParseComposeForSDK([]byte(goldenCloudReplayCompose))
	if err != nil {
		t.Fatalf("ParseComposeForSDK: %v", err)
	}
	if len(model.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(model.Services))
	}

	var agent, app *ServiceSpec
	for i := range model.Services {
		switch model.Services[i].Key {
		case "keploy-agent":
			agent = &model.Services[i]
		case "my-app":
			app = &model.Services[i]
		}
	}
	if agent == nil || app == nil {
		t.Fatalf("missing service(s): agent=%v app=%v", agent, app)
	}

	// --- agent ---
	if agent.ContainerName != "keploy-v3-abc123" {
		t.Errorf("agent container_name = %q", agent.ContainerName)
	}
	wantCaps := []string{"BPF", "PERFMON", "NET_ADMIN", "SYS_ADMIN"}
	if !eqStrs(agent.CapAdd, wantCaps) {
		t.Errorf("agent cap_add = %v, want %v", agent.CapAdd, wantCaps)
	}
	if !eqStrs(agent.SecurityOpt, []string{"seccomp:unconfined", "apparmor:unconfined"}) {
		t.Errorf("agent security_opt = %v", agent.SecurityOpt)
	}
	if got := agent.Tmpfs["/sys/fs/bpf"]; got != "exec,mode=0755" {
		t.Errorf("agent tmpfs[/sys/fs/bpf] = %q", got)
	}
	if !eqStrs(agent.Ports, []string{"16789:16789", "26789:26789"}) {
		t.Errorf("agent ports = %v", agent.Ports)
	}
	if !eqStrs(agent.DNSSearch, []string{"default.svc.cluster.local", "svc.cluster.local"}) {
		t.Errorf("agent dns_search = %v", agent.DNSSearch)
	}
	// bare-key host inheritance (the couchbase password mechanism)
	if !hasEnv(agent.Env, "KEPLOY_COUCHBASE_PASSWORD=s3cr3t") {
		t.Errorf("agent env missing inherited KEPLOY_COUCHBASE_PASSWORD=s3cr3t: %v", agent.Env)
	}
	if !hasEnv(agent.Env, "BINARY_TO_DOCKER=true") {
		t.Errorf("agent env missing BINARY_TO_DOCKER: %v", agent.Env)
	}
	// agent owns its namespace (no service: sharing)
	if agent.NetworkMode != "" || agent.PidMode != "" {
		t.Errorf("agent should not share a namespace: net=%q pid=%q", agent.NetworkMode, agent.PidMode)
	}
	// healthcheck: the enterprise shell-free CMD form, durations parsed
	if hc := agent.Healthcheck; hc == nil {
		t.Error("agent healthcheck not parsed")
	} else {
		wantTest := []string{"CMD", "/app/keploy-enterprise", "health", "--check=agent-ready"}
		if !eqStrs(hc.Test, wantTest) {
			t.Errorf("agent healthcheck test = %v, want %v", hc.Test, wantTest)
		}
		if hc.Interval != 5*time.Second || hc.Timeout != 5*time.Second || hc.StartPeriod != 10*time.Second {
			t.Errorf("agent healthcheck durations: interval=%v timeout=%v start=%v", hc.Interval, hc.Timeout, hc.StartPeriod)
		}
		if hc.Retries != 60 {
			t.Errorf("agent healthcheck retries = %d", hc.Retries)
		}
	}

	// --- app ---
	if app.WorkingDir != "/srv" {
		t.Errorf("app working_dir = %q", app.WorkingDir)
	}
	// namespace sharing translated later to container:<id>
	svcName, shared := IsShareServiceNamespace(app.NetworkMode)
	if !shared || svcName != "keploy-agent" {
		t.Errorf("app network_mode = %q (shared=%v svc=%q)", app.NetworkMode, shared, svcName)
	}
	if _, shared := IsShareServiceNamespace(app.PidMode); !shared {
		t.Errorf("app pid = %q, want service:keploy-agent", app.PidMode)
	}
	// app carries no ports/networks (removed by modifyAppServiceForKeploy)
	if len(app.Ports) != 0 {
		t.Errorf("app should have no ports, got %v", app.Ports)
	}
	// ${VAR} expansion in a map-form value
	if !hasEnv(app.Env, "UPSTREAM_URL=http://backend.internal:9000") {
		t.Errorf("app env UPSTREAM_URL not expanded: %v", app.Env)
	}
	// map-form null value inherits from host
	if !hasEnv(app.Env, "INHERITED_TOKEN=tok-42") {
		t.Errorf("app env INHERITED_TOKEN not inherited: %v", app.Env)
	}
	if !hasEnv(app.Env, "NODE_EXTRA_CA_CERTS=/tmp/keploy-tls/ca.crt") {
		t.Errorf("app env missing TLS cert var: %v", app.Env)
	}
	// depends_on: service_healthy
	if len(app.DependsOn) != 1 || app.DependsOn[0].Service != "keploy-agent" || app.DependsOn[0].Condition != ServiceCondHealthy {
		t.Errorf("app depends_on = %+v", app.DependsOn)
	}
	// bind (:ro) + named volume both captured verbatim for HostConfig.Binds
	if !eqStrs(app.Binds, []string{"keploy-tls-certs:/tmp/keploy-tls:ro", "/tmp/keploy-cloud-replay-x/cfg:/etc/app:ro"}) {
		t.Errorf("app binds = %v", app.Binds)
	}

	// --- top-level named volumes ---
	if _, ok := model.Volumes["keploy-tls-certs"]; !ok {
		t.Errorf("top-level volume keploy-tls-certs missing: %v", model.Volumes)
	}
	if _, ok := model.Volumes["keploy-sockets-vol"]; !ok {
		t.Errorf("top-level volume keploy-sockets-vol missing: %v", model.Volumes)
	}
}

func TestExpandComposeValue(t *testing.T) {
	t.Setenv("FOO", "bar")
	t.Setenv("EMPTY", "")
	cases := map[string]string{
		"plain":                "plain",
		"$FOO":                 "bar",
		"${FOO}":               "bar",
		"a-${FOO}-b":           "a-bar-b",
		"$$FOO":                "$FOO", // $$ escapes to a literal $
		"${MISSING:-fallback}": "fallback",
		"${EMPTY:-fallback}":   "fallback", // :- uses default when empty
		"${FOO:-fallback}":     "bar",
		"pre$$post":            "pre$post",
		// $2 / $10 are not valid variable names so they are kept verbatim, while
		// $abc IS a (here unset) variable and expands to empty — documents how an
		// unescaped literal '$' is handled (compose users escape with $$).
		"$2y$10$abc": "$2y$10",
	}
	for in, want := range cases {
		if got := expandComposeValue(in); got != want {
			t.Errorf("expandComposeValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseComposeForSDK_NoServices(t *testing.T) {
	if _, err := ParseComposeForSDK([]byte("volumes:\n  v:\n")); err == nil {
		t.Error("expected error for a compose document with no services")
	}
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
