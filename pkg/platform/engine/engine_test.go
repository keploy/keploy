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
		"podman ps":                             Podman, // the engine it runs, starting nothing
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
		// run's command after its service is the container's, not compose's.
		"sudo -E docker compose run --rm migrate ./migrate -d": "docker",
		"sudo -E docker compose up web -t 30":                  "docker",
		"docker -H tcp://h:2375 run app":                       "docker",
	} {
		if got := Detect(cmd); got != want {
			t.Errorf("Detect(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// A command runs its application in a container when one of its commands
// starts one in the foreground, with the engine as its program. A detached
// start only starts a dependency, and the engine as an argument is no start
// at all.
func TestInvocationIsTheApplications(t *testing.T) {
	for cmd, want := range map[string]string{
		// The application runs in the container.
		"docker build -t app . && docker run --name app app": Docker,
		"cd svc && podman run --name app img":                Podman,
		"podman compose up 2>&1 | tee up.log":                Podman,
		"echo starting; nohup podman run app &":              Podman,
		"podman run --rm app || true":                        Podman,
		"podman run app ; echo done":                         Podman,
		"podman-compose up && echo finished":                 Podman,
		"podman run \\\n  --rm --name app img":               Podman,
		"podman run \\\r\n  --rm app":                        Podman,
		"podman run app | grep ready && ./app":               Podman,
		"timeout -k 5 10m docker run app":                    Docker,
		"nice -n 10 env -u DOCKER_HOST FOO=1 docker run app": Docker,
		"sudo -u docker docker run app":                      Docker,
		"sudo -H docker run app":                             Docker,
		"sudo -Eu alice podman run app":                      Podman,
		"sudo --preserve-env=PATH podman run app":            Podman,
		`C:\Docker\docker.exe run app`:                       Docker,
		`"C:\Program Files\Docker\docker.exe" run app`:       Docker,
		"'podman' run app":                                   Podman,
		"taskset 0x3 podman run app":                         Podman,
		"cd svc&&podman-compose up":                          Podman,
		"(cd svc && podman compose up)":                      Podman,
		"podman run app > run.log 2>&1":                      Podman,
		"FOO='a b' podman run app":                           Podman,
		"podman start -a app":                                Podman,
		"podman container run --rm app":                      Podman,
		"podman kube play app.yaml":                          Podman,
		"docker compose run --rm migrate && go run .":        Docker,
		// Detaching is read before the image or service, not after it.
		"sudo -E docker compose up start":             "docker",
		"sudo -E docker run --rm img ./server -dev":   "docker",
		"docker run -p 8080:80 -e X=-d img":           "docker",
		"docker run -p8080:80 --name app img":         "docker",
		"docker compose -p proj up --scale web=2 web": "docker",
		"taskset -c 0-3 sudo -E docker run img":       "docker",
		"podman pod start app":                        Podman,
		// The application runs on the host; the engine starts a dependency,
		// detached, or is an argument.
		"cd svc && podman compose up -d db && go run .":                                "",
		"make deps && podman run -d -p 5432:5432 postgres && ./app":                    "",
		"podman run -itd postgres; ./app":                                              "",
		"podman run --detach postgres; ./app":                                          "",
		"docker compose up --wait db && ./app":                                         "",
		"docker compose -f deps.yml up --detach && ./app":                              "",
		"podman start pg && go run .":                                                  "",
		"podman container start pg && go run .":                                        "",
		"npm run podman start":                                                         "",
		"yarn docker start":                                                            "",
		"go run ./cmd/podman start":                                                    "",
		"python app.py --backend podman run":                                           "",
		"sudo -E ./manage.sh docker start":                                             "",
		"sudo -E node docker run":                                                      "",
		"cd svc && docker compose down -v && docker compose up -d db && go run .":      "",
		"cd svc && docker compose pull && docker compose up -d db && go run .":         "",
		"make build && docker compose build && docker compose up -d && ./app":          "",
		"cd . && docker compose exec -T db psql -c 'select 1' && ./app":                "",
		"cd . && docker container rm -f pg; docker run -d --name pg postgres && ./app": "",
		"cd . && docker-compose down && docker-compose up -d && ./app":                 "",
		"cd . && podman pod rm -f mypod && ./app":                                      "",
		"podman kube down app.yaml && ./app":                                           "",
		"docker compose -f deps.yml -p deps up -d && ./app":                            "",
		"docker run -dp 5432:5432 postgres && ./app":                                   "",
		"podman compose restart db && ./app":                                           "",
		// compose up takes options after its services; options keep their case.
		"cd svc && docker compose up db -d && go run .":                     "",
		"cd svc && docker compose up db redis --detach && go run .":         "",
		"cd svc && docker compose up db --wait && go run .":                 "",
		"cd svc && docker-compose up db -d && go run .":                     "",
		"cd . && docker run -P -d --name db postgres && ./app":              "",
		"cd . && docker compose up -V -d db && ./app":                       "",
		"cd . && docker compose run -T -d migrate && ./app":                 "",
		"cd . && docker compose up -dV db && ./app":                         "",
		"docker run --use-api-socket -d img && ./app":                       "",
		"podman --log-level=debug run -d db && ./app":                       "",
		"cd . && podman run --tls-verify -d img && ./app":                   "",
		"cd . && podman run --no-hostname --rootfs -d /srv/rootfs && ./app": "",
		"sh -c 'podman run app'":                                            "",
		"echo 'podman run app'":                                             "",
		"cafe podman run app":                                               "",
		"":                                                                  "",
	} {
		got, _ := Invocation(cmd)
		if got != want {
			t.Errorf("Invocation(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// The engine a docker-mode command drives is the first one it runs to launch
// containers, detached or not.
func TestDetectTakesDetachedStartsToo(t *testing.T) {
	for cmd, want := range map[string]string{
		"podman compose up -d && ./app":                 Podman,
		"cd svc && podman compose up -d db && go run .": Podman,
		"podman run app || true":                        Podman,
		"sudo -E podman run img || true":                Podman,
		"podman compose down && ./app":                  Podman, // the engine it runs, starting nothing
		"make up":                                       Docker,
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
