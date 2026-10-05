package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// withRegistry runs the test against an empty registry and Docker active, and
// puts back whatever was there.
func withRegistry(t *testing.T) {
	t.Helper()
	mu.Lock()
	saved, savedActive := preparers, active
	preparers, active = map[string]Preparer{}, dockerRuntime
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		preparers, active = saved, savedActive
		mu.Unlock()
	})
}

func TestDetect(t *testing.T) {
	for cmd, want := range map[string]string{
		"docker run --name app img":             Docker,
		"docker compose up":                     Docker,
		"podman run --name app img":             Podman,
		"PODMAN run app":                        Podman,
		"sudo podman start -a app":              Podman,
		"sudo -E podman compose up":             Podman,
		"env FOO=bar /usr/bin/podman run img":   Podman,
		"podman-compose -f c.yml up":            Podman,
		"/usr/local/bin/podman-compose up":      Podman,
		"make up":                               Docker,
		"./run-podman.sh":                       Docker,
		"npm run podman":                        Docker,
		"podman ps":                             Docker,
		"":                                      Docker,
		"sudo docker run --pid=host podman-img": Docker,
		"sudo -u alice podman run img":          Podman,
		"timeout 600 podman compose up":         Podman,
		"/usr/bin/sudo podman start -a app":     Podman,
		"podman --remote run img":               Podman,
		"docker --context remote compose up":    Docker,
		"podman -c machine run img":             Podman,
		"podman --connection machine run img":   Podman,
		"podman --log-level debug run img":      Podman,
		"podman kube play app.yaml":             Podman,
		"podman pod start app":                  Podman,
	} {
		if got := Detect(cmd); got != want {
			t.Errorf("Detect(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestDockerIsAlwaysSupportedAndPodmanOnlyWhenRegistered(t *testing.T) {
	withRegistry(t)
	if !Supported(Docker) {
		t.Fatal("Docker must be supported without registration")
	}
	if Supported(Podman) {
		t.Fatal("Podman must not be supported until a build registers it")
	}
	Register(Podman, func(context.Context, *zap.Logger) (Runtime, error) { return Runtime{Name: Podman}, nil })
	if !Supported(Podman) {
		t.Fatal("a registered engine must be supported")
	}
}

func TestPrepareRefusesAnUnregisteredEngine(t *testing.T) {
	withRegistry(t)
	err := Prepare(context.Background(), zap.NewNop(), Podman)
	if err == nil {
		t.Fatal("preparing an unregistered engine must fail")
	}
	if !strings.Contains(err.Error(), "Podman") || !strings.Contains(err.Error(), "https://keploy.io/install.sh") {
		t.Fatalf("the refusal must name Podman and say where to get a keploy that supports it, got %q", err)
	}
	if got := Active(); got.Name != Docker {
		t.Fatalf("a failed Prepare must leave Docker active, got %q", got.Name)
	}
}

func TestPrepareMakesARegisteredEngineActiveAndExportsItsEndpoint(t *testing.T) {
	withRegistry(t)
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	want := Runtime{
		Name:              Podman,
		CLI:               "/usr/bin/podman",
		Compose:           []string{"/usr/bin/podman", "compose"},
		Host:              "unix:///run/podman/podman.sock",
		AgentSecurityOpts: []string{"label=disable", "seccomp=unconfined"},
	}
	Register(Podman, func(context.Context, *zap.Logger) (Runtime, error) { return want, nil })

	if err := Prepare(context.Background(), zap.NewNop(), Podman); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got := Active()
	if got.Name != want.Name || got.CLI != want.CLI || strings.Join(got.Compose, " ") != strings.Join(want.Compose, " ") ||
		strings.Join(got.AgentSecurityOpts, ",") != strings.Join(want.AgentSecurityOpts, ",") {
		t.Fatalf("Active() = %+v, want %+v", got, want)
	}
	if h := os.Getenv("DOCKER_HOST"); h != want.Host {
		t.Fatalf("DOCKER_HOST = %q, want the engine's endpoint %q", h, want.Host)
	}

	// Back to Docker: its runtime, and the environment's endpoint is left as
	// it is (Docker's Host is empty).
	if err := Prepare(context.Background(), zap.NewNop(), Docker); err != nil {
		t.Fatalf("Prepare(Docker): %v", err)
	}
	if got := Active(); got.Name != Docker || got.CLI != "docker" {
		t.Fatalf("after Prepare(Docker), Active() = %+v", got)
	}
}

func TestPrepareReportsAPreparerFailure(t *testing.T) {
	withRegistry(t)
	boom := errors.New("no podman binary")
	Register(Podman, func(context.Context, *zap.Logger) (Runtime, error) { return Runtime{}, boom })
	err := Prepare(context.Background(), zap.NewNop(), Podman)
	if !errors.Is(err, boom) {
		t.Fatalf("Prepare must wrap the preparer's error, got %v", err)
	}
	if got := Active(); got.Name != Docker {
		t.Fatalf("a failed Prepare must leave Docker active, got %q", got.Name)
	}
}

// Docker's agent needs label=disable: on an SELinux host the agent container
// is otherwise denied bpf(2) and never starts.
func TestDockerAgentRunsWithoutSELinuxConfinement(t *testing.T) {
	withRegistry(t)
	opts := Active().AgentSecurityOpts
	if len(opts) != 1 || opts[0] != "label=disable" {
		t.Fatalf("Docker's agent security options = %v, want [label=disable]", opts)
	}
}

// What keploy runs its own commands with is never empty, whatever a preparer
// returns: an empty compose argv would make every compose teardown panic.
func TestPrepareFillsWhatAPreparerLeftEmpty(t *testing.T) {
	withRegistry(t)
	t.Setenv("DOCKER_HOST", "")
	Register(Podman, func(context.Context, *zap.Logger) (Runtime, error) { return Runtime{}, nil })
	if err := Prepare(context.Background(), zap.NewNop(), Podman); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got := Active()
	if got.Name != Podman || got.CLI != Podman || strings.Join(got.Compose, " ") != "podman compose" {
		t.Fatalf("Active() = %+v, want name, CLI and compose filled from the engine's name", got)
	}
}
