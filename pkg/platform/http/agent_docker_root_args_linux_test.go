//go:build linux

package http

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/agentargs"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// `keploy record --disable-tele` in docker mode: the agent container is a
// keploy process of its own, which sees no keploy.yml and none of the CLI's
// flags but those its `docker run` hands it. startInDocker builds that command
// from the CLI's own config (a.conf). Built from a fresh config holding only
// the installation id, the container got --disable-tele=false, and the agent
// resolved and pinged telemetry.keploy.io while the CLI had been told not to.
//
// The row drives the AgentClient path itself, startInDocker up to the `docker
// run` it executes (fakeDockerForStartInDocker).
func TestStartInDocker_HandsTheAgentContainerTheCLIsRootSettings(t *testing.T) {
	calls := fakeDockerForStartInDocker(t)

	// Every root setting set to the value a fresh config does not hold, so
	// a command built from anything but the CLI's config shows it.
	conf := &config.Config{
		InstallationID: "install-id",
		Debug:          true,
		DisableTele:    true,
		DisableANSI:    true,
		DisableMapping: true,
	}
	a := &AgentClient{logger: zap.NewNop(), conf: conf}
	opts := models.SetupOptions{
		KeployContainer: "keploy-root-args-test",
		Mode:            models.MODE_RECORD,
		AgentPort:       43900,
		ProxyPort:       16789,
		DnsPort:         26789,
	}
	if err := a.startInDocker(context.Background(), zap.NewNop(), opts, newAgentRun()); err != nil {
		t.Fatalf("startInDocker: %v", err)
	}

	run := agentContainerRun(t, calls)
	words := strings.Fields(run)
	for _, want := range agentargs.Root(conf) {
		n := 0
		for _, w := range words {
			if w == want {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the agent container's command carries %q %d times, want once:\n%s", want, n, run)
		}
	}
}

// `keploy record -c "docker run --dns ..."`: the app joins the agent
// container's network namespace, where docker refuses its own --dns and
// ignores its --dns-search and --dns-option, so Setup takes them (and the
// app's ports, networks and host entries) out of its command and hands them
// to the agent container's `docker run`. The row runs Setup's own steps for a
// docker run command, runFlagsOf and withAppRunSettings, into startInDocker,
// and reads the `docker run` it executes: each of the app's settings is there.
func TestSetup_HandsTheAgentContainerTheAppsRunSettings(t *testing.T) {
	calls := fakeDockerForStartInDocker(t)
	a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{InstallationID: "install-id"}}
	appCmd := "docker run --dns 192.0.2.53 --dns-search corp.example --dns-option ndots:3 " +
		"--add-host db.internal:10.0.0.9 -p 8080:80 --network shop --name app httpd:2.4"
	cleaned, ports, networks, hosts, dns := runFlagsOf(appCmd, utils.DockerRun)
	if strings.Join(strings.Fields(cleaned), " ") != "docker run --name app httpd:2.4" {
		t.Fatalf("the app's command became %q", cleaned)
	}
	opts := withAppRunSettings(models.SetupOptions{
		KeployContainer: "keploy-app-run-test",
		Mode:            models.MODE_RECORD,
		AgentPort:       43901,
		ProxyPort:       16790,
		DnsPort:         26790,
	}, ports, networks, hosts, dns)
	if err := a.startInDocker(context.Background(), zap.NewNop(), opts, newAgentRun()); err != nil {
		t.Fatalf("startInDocker: %v", err)
	}
	run := " " + strings.Join(strings.Fields(agentContainerRun(t, calls)), " ") + " "
	for _, want := range []string{
		"--dns 192.0.2.53",
		"--dns-search corp.example",
		"--dns-option ndots:3",
		"--add-host db.internal:10.0.0.9",
		"-p 8080:80",
		"--network shop",
	} {
		if strings.Count(run, " "+want+" ") != 1 {
			t.Errorf("the agent container's command carries %q %d times, want once:\n%s", want, strings.Count(run, " "+want+" "), run)
		}
	}
}

// fakeDockerForStartInDocker puts a `docker` first on PATH that logs its
// arguments and exits, and a `sudo` before it that runs what follows its
// options (a non-root keploy runs docker through sudo, which would reset PATH
// to secure_path and skip the fake), and points the engine API at a socket
// nobody serves, so the debugfs volume keploy creates first fails at once, as
// it is allowed to. It returns the file the fake logs to.
func fakeDockerForStartInDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	fakes := map[string]string{
		"docker": "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + calls + "'\nexit 0\n",
		"sudo": `#!/bin/sh
if [ "$1" = --version ]; then echo "Sudo version 1.9.15"; exit 0; fi
while [ $# -gt 0 ]; do
  case "$1" in -*) shift ;; *) break ;; esac
done
exec "$@"
`,
	}
	for name, script := range fakes {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(dir, "no-daemon.sock"))
	// GetKeployDockerAlias writes the installation id into the package's
	// DockerConfig; leave it as other tests found it.
	envs := maps.Clone(kdocker.DockerConfig.Envs)
	t.Cleanup(func() { kdocker.DockerConfig.Envs = envs })
	return calls
}

// agentContainerRun returns the `docker container run` the fake logged.
func agentContainerRun(t *testing.T, calls string) string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("startInDocker ran no docker: %v", err)
	}
	var run string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, "container run ") {
			run = line
		}
	}
	if run == "" {
		t.Fatalf("no `docker container run` among the calls:\n%s", b)
	}
	return run
}
