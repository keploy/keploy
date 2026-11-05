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
	if err := a.startInDocker(context.Background(), zap.NewNop(), opts, newAgentRun(), nil); err != nil {
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
	if err := a.startInDocker(context.Background(), zap.NewNop(), opts, newAgentRun(), nil); err != nil {
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
	fakeEngineForStartInDocker(t, dir, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+calls+"'\nexit 0\n")
	return calls
}

// noDaemonHost is the engine API's socket in fakeEngineForStartInDocker: a
// socket nobody can serve, as nothing can be made under /dev/null, which is
// not a directory. It is not one in the test's temporary directory: a unix
// socket's path holds at most 107 bytes, and that directory's grows with
// GOTMPDIR (or TMPDIR) and the test's name. Past that bound the engine API's
// client refused the path itself, startInDocker returned before its `docker
// run`, and a row waiting for that `docker run` waited out its bound.
const noDaemonHost = "unix:///dev/null/no-daemon.sock"

// fakeEngineForStartInDocker is fakeDockerForStartInDocker's engine, with
// docker the given script, in dir.
func fakeEngineForStartInDocker(t *testing.T, dir, docker string) {
	t.Helper()
	fakes := map[string]string{
		"docker": docker,
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
	t.Setenv("DOCKER_HOST", noDaemonHost)
	// GetKeployDockerAlias writes the installation id into the package's
	// DockerConfig; leave it as other tests found it.
	envs := maps.Clone(kdocker.DockerConfig.Envs)
	t.Cleanup(func() { kdocker.DockerConfig.Envs = envs })
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

// The fake engine takes startInDocker to its `docker run` however long the
// test's temporary directory is. With its engine API's socket in that
// directory, a GOTMPDIR (t.TempDir's root) of a few dozen bytes made the
// socket's path longer than a unix socket's can be: the engine API's client
// refused it, startInDocker ended before its `docker run`, and
// TestADockerRunsEndIsNotHeldByADaemonThatGoesSilent waited its whole bound
// for a `docker run` that never came.
func TestTheFakeEngineRunsTheAgentUnderALongTempDir(t *testing.T) {
	root, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	long := filepath.Join(root, strings.Repeat("d", 100))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	// Before this test's first TempDir, which roots them all.
	t.Setenv("GOTMPDIR", long)
	t.Setenv("TMPDIR", long)
	calls := fakeDockerForStartInDocker(t)
	if !strings.HasPrefix(calls, long) {
		t.Fatalf("the fake engine is in %s, not under the long directory %s", calls, long)
	}
	a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{InstallationID: "install-id"}}
	opts := models.SetupOptions{KeployContainer: "keploy-v3-0a1b2c3d4e5f6071", Mode: models.MODE_RECORD, AgentPort: 43902, ProxyPort: 16791, DnsPort: 26791}
	if err := a.startInDocker(context.Background(), zap.NewNop(), opts, newAgentRun(), nil); err != nil {
		t.Fatalf("startInDocker under a %d-byte temporary directory: %v", len(long), err)
	}
	agentContainerRun(t, calls)
}

// Two docker-run runs on one host: each agent container writes its CA to its
// own run's volume. With one name for every run, the second run's agent
// overwrote the first's ca.crt while the first ran, and the first's
// application then refused its own agent's certificates. The row reads the
// CA mount of the `docker run` startInDocker executes for each run.
func TestStartInDocker_MountsEachRunsOwnCAVolume(t *testing.T) {
	seen := map[string]string{}
	for _, agent := range []string{"keploy-v3-0a1b2c3d4e5f6071", "keploy-v3-8899aabbccddeeff"} {
		calls := fakeDockerForStartInDocker(t)
		a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{InstallationID: "install-id"}}
		opts := models.SetupOptions{KeployContainer: agent, Mode: models.MODE_RECORD, AgentPort: 43902, ProxyPort: 16791, DnsPort: 26791}
		if err := a.startInDocker(context.Background(), zap.NewNop(), opts, newAgentRun(), nil); err != nil {
			t.Fatalf("startInDocker: %v", err)
		}
		words := strings.Fields(agentContainerRun(t, calls))
		var mounts []string
		for i, w := range words {
			if w == "-v" && i+1 < len(words) && strings.HasSuffix(words[i+1], ":/tmp/keploy-tls") {
				mounts = append(mounts, strings.TrimSuffix(words[i+1], ":/tmp/keploy-tls"))
			}
		}
		if len(mounts) != 1 {
			t.Fatalf("agent %s: CA mounts %v, want one", agent, mounts)
		}
		seen[agent] = mounts[0]
	}
	if seen["keploy-v3-0a1b2c3d4e5f6071"] != "keploy-tls-certs-0a1b2c3d4e5f6071" ||
		seen["keploy-v3-8899aabbccddeeff"] != "keploy-tls-certs-8899aabbccddeeff" {
		t.Fatalf("the two runs' agents mount %v, want each its own run's volume", seen)
	}
}

// An embedder's run volume (kdocker.RunVolume) is each run's own in its agent
// container: two runs' agents mount two volumes at its target, whatever the
// target.
func TestStartInDocker_MountsEachRunsOwnEmbedderVolume(t *testing.T) {
	prev := kdocker.DockerConfig.RunVolumes
	t.Cleanup(func() { kdocker.DockerConfig.RunVolumes = prev })
	for _, v := range []kdocker.RunVolume{{Base: "keploy-sockets-vol", Target: "/tmp"}, {Base: "keploy-embed-vol", Target: "/srv/keploy-embed"}} {
		kdocker.DockerConfig.RunVolumes = []kdocker.RunVolume{v}
		for _, agent := range []string{"keploy-v3-0a1b2c3d4e5f6071", "keploy-v3-8899aabbccddeeff"} {
			calls := fakeDockerForStartInDocker(t)
			a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{InstallationID: "install-id"}}
			opts := models.SetupOptions{KeployContainer: agent, Mode: models.MODE_RECORD, AgentPort: 43902, ProxyPort: 16791, DnsPort: 26791}
			if err := a.startInDocker(context.Background(), zap.NewNop(), opts, newAgentRun(), nil); err != nil {
				t.Fatalf("startInDocker: %v", err)
			}
			words := strings.Fields(agentContainerRun(t, calls))
			var mounts []string
			for i, w := range words {
				if w == "-v" && i+1 < len(words) && strings.HasSuffix(words[i+1], ":"+v.Target) {
					mounts = append(mounts, words[i+1])
				}
			}
			want := v.Base + "-" + strings.TrimPrefix(agent, "keploy-v3-") + ":" + v.Target
			if len(mounts) != 1 || mounts[0] != want {
				t.Errorf("agent %s: %s mounts %v, want only %s", agent, v.Target, mounts, want)
			}
		}
	}
}
