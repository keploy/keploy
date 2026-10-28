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
// run` it executes: a `docker` first on PATH logs its arguments and exits, a
// `sudo` before it runs what follows its options (a non-root keploy runs
// docker through sudo, which would reset PATH to secure_path and skip the
// fake), and the engine API points at a socket nobody serves, so the debugfs
// volume keploy creates first fails at once, as it is allowed to.
func TestStartInDocker_HandsTheAgentContainerTheCLIsRootSettings(t *testing.T) {
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
