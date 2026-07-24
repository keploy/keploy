package app

import (
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/pkg/platform/docker"
)

// agentSpec is a representative parsed keploy-agent service (standalone
// networking, publishes ports, owns the namespace).
func agentSpec() docker.ServiceSpec {
	return docker.ServiceSpec{
		Key:           "keploy-agent",
		ContainerName: "keploy-v3-abc",
		Image:         "ghcr.io/keploy/keploy-enterprise:v2",
		Env:           []string{"BINARY_TO_DOCKER=true"},
		Ports:         []string{"16789:16789", "26789:26789"},
		Binds:         []string{"keploy-tls-certs:/tmp/keploy-tls", "/sys/fs/cgroup:/sys/fs/cgroup"},
		Tmpfs:         map[string]string{"/sys/fs/bpf": "exec,mode=0755"},
		CapAdd:        []string{"BPF", "SYS_ADMIN"},
		SecurityOpt:   []string{"seccomp:unconfined"},
		Command:       []string{"--port", "16789"},
		DNSSearch:     []string{"default.svc.cluster.local"},
		Healthcheck: &docker.Healthcheck{
			Test:        []string{"CMD", "/app/keploy-enterprise", "health", "--check=agent-ready"},
			Interval:    5 * time.Second,
			Timeout:     5 * time.Second,
			Retries:     60,
			StartPeriod: 10 * time.Second,
		},
	}
}

// appSpec is a representative parsed app service that shares the agent's
// net/pid namespace and (like every cloud-replay app) carries restart:
// unless-stopped in the source YAML.
func appSpec() docker.ServiceSpec {
	return docker.ServiceSpec{
		Key:           "my-app",
		ContainerName: "my-app",
		Image:         "example.com/my-app:1.0",
		Env:           []string{"NODE_EXTRA_CA_CERTS=/tmp/keploy-tls/ca.crt"},
		Binds:         []string{"keploy-tls-certs:/tmp/keploy-tls:ro"},
		WorkingDir:    "/srv",
		Entrypoint:    []string{"/entrypoint.sh"},
		Command:       []string{"serve"},
		PidMode:       "service:keploy-agent",
		NetworkMode:   "service:keploy-agent",
		// These would be rejected by the daemon on a container: netns and must be
		// dropped for the app; modifyAppServiceForKeploy already moves them to the
		// agent, but buildContainerSpec must also not set them.
		DNSSearch: []string{"should.be.ignored"},
		Ports:     []string{"8080:8080"},
	}
}

func TestBuildContainerSpec_Agent(t *testing.T) {
	cfg, hostCfg, err := buildContainerSpec(agentSpec(), "")
	if err != nil {
		t.Fatalf("buildContainerSpec(agent): %v", err)
	}

	// restart is always neutralised to "no".
	if hostCfg.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Errorf("agent RestartPolicy = %q, want %q", hostCfg.RestartPolicy.Name, container.RestartPolicyDisabled)
	}
	// standalone networking -> not sharing a namespace
	if hostCfg.NetworkMode != "" || hostCfg.PidMode != "" {
		t.Errorf("agent should not share a namespace: net=%q pid=%q", hostCfg.NetworkMode, hostCfg.PidMode)
	}
	// ports published on the agent
	if _, ok := hostCfg.PortBindings[nat.Port("16789/tcp")]; !ok {
		t.Errorf("agent PortBindings missing 16789/tcp: %v", hostCfg.PortBindings)
	}
	if _, ok := cfg.ExposedPorts[nat.Port("26789/tcp")]; !ok {
		t.Errorf("agent ExposedPorts missing 26789/tcp: %v", cfg.ExposedPorts)
	}
	// DNS search stays on the agent
	if len(hostCfg.DNSSearch) != 1 || hostCfg.DNSSearch[0] != "default.svc.cluster.local" {
		t.Errorf("agent DNSSearch = %v", hostCfg.DNSSearch)
	}
	// eBPF privileges + tmpfs
	if !contains(hostCfg.CapAdd, "SYS_ADMIN") || !contains(hostCfg.SecurityOpt, "seccomp:unconfined") {
		t.Errorf("agent caps/security_opt = %v / %v", hostCfg.CapAdd, hostCfg.SecurityOpt)
	}
	if hostCfg.Tmpfs["/sys/fs/bpf"] != "exec,mode=0755" {
		t.Errorf("agent tmpfs = %v", hostCfg.Tmpfs)
	}
	// healthcheck carried through
	if cfg.Healthcheck == nil || len(cfg.Healthcheck.Test) != 4 || cfg.Healthcheck.Retries != 60 {
		t.Errorf("agent healthcheck = %+v", cfg.Healthcheck)
	}
	// compose-parity labels for logTargetContainer
	if cfg.Labels["com.docker.compose.service"] != "keploy-agent" {
		t.Errorf("agent labels = %v", cfg.Labels)
	}
}

func TestBuildContainerSpec_App(t *testing.T) {
	cfg, hostCfg, err := buildContainerSpec(appSpec(), "agent123")
	if err != nil {
		t.Fatalf("buildContainerSpec(app): %v", err)
	}

	if hostCfg.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Errorf("app RestartPolicy = %q, want disabled", hostCfg.RestartPolicy.Name)
	}
	// shares the agent's net + pid namespace
	if hostCfg.NetworkMode != container.NetworkMode("container:agent123") {
		t.Errorf("app NetworkMode = %q, want container:agent123", hostCfg.NetworkMode)
	}
	if hostCfg.PidMode != container.PidMode("container:agent123") {
		t.Errorf("app PidMode = %q, want container:agent123", hostCfg.PidMode)
	}
	// no ports / DNS on the app (daemon rejects them with a container: netns)
	if len(hostCfg.PortBindings) != 0 || len(cfg.ExposedPorts) != 0 {
		t.Errorf("app must not publish ports: bindings=%v exposed=%v", hostCfg.PortBindings, cfg.ExposedPorts)
	}
	if len(hostCfg.DNSSearch) != 0 {
		t.Errorf("app must not set DNSSearch on a shared netns, got %v", hostCfg.DNSSearch)
	}
	// entrypoint/command/workdir/env/binds carried through
	if cfg.WorkingDir != "/srv" || len(cfg.Entrypoint) != 1 || len(cfg.Cmd) != 1 {
		t.Errorf("app cfg entrypoint/cmd/workdir wrong: %+v", cfg)
	}
	if len(hostCfg.Binds) != 1 || hostCfg.Binds[0] != "keploy-tls-certs:/tmp/keploy-tls:ro" {
		t.Errorf("app binds = %v", hostCfg.Binds)
	}
}

// TestSDKStack_ConcurrentTrackAndDrain exercises the sdkStack mutex under the
// exact concurrency the setup-failure path creates: runComposeSDK (app-run
// goroutine) appending via sdkTrack while ComposeDownOnSetupFailure (replay
// orchestrator goroutine) snapshots+clears via sdkDrainStack. Run with -race.
func TestSDKStack_ConcurrentTrackAndDrain(t *testing.T) {
	a := &App{}
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			a.sdkTrack(sdkContainerRef{id: "c", name: "n", service: "s", isApp: i%2 == 0})
		}
	}()

	drained := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			drained += len(a.sdkDrainStack())
			_ = a.sdkSnapshotStack()
		}
	}()

	wg.Wait()
	// Whatever wasn't drained by the racing goroutine remains tracked; the two
	// together must account for every tracked container (none lost or double
	// counted).
	if total := drained + len(a.sdkDrainStack()); total != 500 {
		t.Fatalf("tracked/drained mismatch: got %d, want 500", total)
	}
}

func TestBuildContainerSpec_AgentDNSOpt(t *testing.T) {
	spec := agentSpec()
	spec.DNSOpt = []string{"ndots:2"}
	_, hostCfg, err := buildContainerSpec(spec, "")
	if err != nil {
		t.Fatalf("buildContainerSpec: %v", err)
	}
	if len(hostCfg.DNSOptions) != 1 || hostCfg.DNSOptions[0] != "ndots:2" {
		t.Errorf("agent DNSOptions = %v, want [ndots:2]", hostCfg.DNSOptions)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
