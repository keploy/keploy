//go:build unix

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// noopDockerClient satisfies the dockerClient calls startAgent makes during
// container teardown (RemovalWatch and RemoveRunVolumes) without touching any
// real daemon.
type noopDockerClient struct{ kdocker.Client }

func (*noopDockerClient) VolumeRemove(context.Context, string, bool) error { return nil }
func (*noopDockerClient) ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	ch := make(chan container.WaitResponse, 1)
	ch <- container.WaitResponse{}
	return ch, make(chan error)
}
func (*noopDockerClient) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{}, errors.New("no such container")
}

// fakeDockerForPortTakenRetry puts a fake docker and sudo on PATH for unix
// tests (both Linux and macOS) and points DOCKER_HOST at a minimal local HTTP
// stub that answers Docker volume queries so GetKeployDockerAlias succeeds
// without emitting spurious daemon-unreachable ERROR logs.
func fakeDockerForPortTakenRetry(t *testing.T, dir, dockerScript string) {
	t.Helper()
	// Wrap the caller's script so darwin/windows `docker context ls` returns a
	// valid default context before delegating to dockerScript.
	wrapped := "#!/bin/sh\nif [ \"$1\" = context ]; then printf 'default\\ttrue\\n'; exit 0; fi\n" + strings.TrimPrefix(dockerScript, "#!/bin/sh\n")
	fakes := map[string]string{
		"docker": wrapped,
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
	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.45")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/volumes"):
			_, _ = w.Write([]byte(`{"Volumes":[{"Name":"debugfs","Driver":"local","Options":{"type":"debugfs","device":"debugfs"}}]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(dockerSrv.Close)

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(dockerSrv.URL, "http://"))
	envs := maps.Clone(kdocker.DockerConfig.Envs)
	t.Cleanup(func() { kdocker.DockerConfig.Envs = envs })
}

// When a Docker agent's first start fails because its published host port
// (127.0.0.1:P) is already in use, pkg.RetryAgentSetup immediately starts a
// fresh agent on a newly drawn port. That recovered first attempt must:
//   - NOT emit ERROR logs (which fail CI lanes that reject ERROR output);
//   - NOT return its taken-port error to the caller's errgroup (which poisoned
//     record/replay teardown's setupErrGrp.Wait() and cancelled runner/mock
//     errgroup contexts before the retry could even run);
//   - exclude the refused port on the next draw so the retry never lands back
//     on a port held inside a VM or by another process.
func TestDockerAgentPortTakenRetryDoesNotPoisonErrGroupOrLogError(t *testing.T) {
	ptyFn := needsAgentPTY
	t.Cleanup(func() { needsAgentPTY = ptyFn })
	needsAgentPTY = func() bool { return false }

	const (
		refusedPort uint32 = 61645
		goodPort    uint32 = 61700
	)

	for _, tc := range []struct {
		name              string
		useDerivedGroupCx bool // true: runner/mock pattern (g, gCtx := errgroup.WithContext); false: record/replay pattern (setupErrGrp, _ := errgroup.WithContext)
	}{
		{name: "record/replay setupErrGrp pattern", useDerivedGroupCx: false},
		{name: "runner/mock errgroup.WithContext pattern", useDerivedGroupCx: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			counterFile := filepath.Join(dir, "count")
			portsFile := filepath.Join(dir, "ports")
			pidFile := filepath.Join(dir, "run.pid")
			startedFile := filepath.Join(dir, "attempt2.started")
			t.Cleanup(func() {
				if b, err := os.ReadFile(pidFile); err == nil {
					if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			// Attempt 1 refuses 127.0.0.1:61645/tcp with the exact Docker 29
			// message observed on macOS Docker Desktop; attempt 2 records its
			// start marker (unblocking /health) and stays alive until stopped
			// (docker kill/stop) or cancelled.
			script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = kill ] || [ "$1" = stop ]; then
  if [ -f %q ]; then
    kill -TERM "$(cat %q)" 2>/dev/null || true
  fi
  exit 0
fi
if [ "$1" = container ] && [ "$2" = run ]; then
  n=0
  [ -f %q ] && n=$(cat %q)
  n=$((n + 1))
  printf '%%s' "$n" > %q
  for arg in "$@"; do
    case "$arg" in
      127.0.0.1:*:*) printf '%%s\n' "$arg" >> %q ;;
    esac
  done
  if [ "$n" -eq 1 ]; then
    printf 'docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint keploy-v3-e7b6f2432fb7f2c2 (15a02f2a2b8e21be028305adeaedb8f9977c1e1b04327220585ab0276a4f07a4): failed to bind host port 127.0.0.1:%d/tcp: address already in use\n\nRun '\''docker run --help'\'' for more information\n' >&2
    exit 125
  fi
  printf '%%s' "$$" > %q
  : > %q
  exec sleep 60
fi
exit 0
`, pidFile, pidFile, counterFile, counterFile, counterFile, portsFile, refusedPort, pidFile, startedFile)
			fakeDockerForPortTakenRetry(t, dir, script)

			// Health server for attempt 2 only, once attempt 2's docker run has
			// actually started (matching real container startup ordering).
			var ready atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ready.Load() && strings.HasSuffix(r.URL.Path, "/health") {
					if _, err := os.Stat(startedFile); err == nil {
						w.WriteHeader(http.StatusOK)
						return
					}
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(srv.Close)

			// Stand-in containerPort picker: returns refusedPort unless excluded,
			// then returns goodPort. This verifies that Setup's retry excludes
			// the port Docker just refused.
			wasContainerPort := containerPort
			t.Cleanup(func() { containerPort = wasContainerPort })
			containerPort = func(exclude ...uint32) (uint32, error) {
				for _, p := range []uint32{refusedPort, goodPort} {
					excluded := false
					for _, ex := range exclude {
						if ex == p {
							excluded = true
							break
						}
					}
					if !excluded {
						return p, nil
					}
				}
				return 0, errors.New("no stand-in port left")
			}

			core, logs := observer.New(zapcore.WarnLevel)
			conf := &config.Config{InstallationID: "install-id", ProxyPort: 18992, DNSPort: 18993}
			a := &AgentClient{logger: zap.New(core), conf: conf, dockerClient: &noopDockerClient{}}

			parentCtx, parentCancel := context.WithCancel(context.Background())
			defer parentCancel()

			var grp *errgroup.Group
			var setupCtx context.Context
			var setupCancel context.CancelFunc
			if tc.useDerivedGroupCx {
				var gCtx context.Context
				grp, gCtx = errgroup.WithContext(parentCtx)
				setupCtx, setupCancel = context.WithCancel(context.WithValue(gCtx, models.ErrGroupKey, grp))
			} else {
				grp, _ = errgroup.WithContext(parentCtx)
				setupCtx, setupCancel = context.WithCancel(context.WithValue(context.WithoutCancel(parentCtx), models.ErrGroupKey, grp))
			}
			t.Cleanup(func() {
				setupCancel()
				parentCancel()
				_ = grp.Wait()
			})

			var attempts int
			var pickedPorts []uint32
			err := pkg.RetryAgentSetup(setupCtx, a.logger, func(c context.Context, attempt int) error {
				attempts++
				agentPort, proxyPort, dnsPort, perr := a.agentPorts(true)
				if perr != nil {
					return perr
				}
				pickedPorts = append(pickedPorts, agentPort)
				opts := models.SetupOptions{
					KeployContainer: fmt.Sprintf("keploy-v3-test-%d", attempt),
					Mode:            models.MODE_RECORD,
					AgentPort:       agentPort,
					ProxyPort:       proxyPort,
					DnsPort:         dnsPort,
				}
				if attempt == 2 {
					ready.Store(true)
					a.addressAgent(srv.URL + "/agent")
				} else {
					a.addressAgent(fmt.Sprintf("http://127.0.0.1:%d/agent", agentPort))
				}
				exited, serr := a.startAgent(c, true, opts)
				if serr != nil {
					return serr
				}
				return a.agentReady(c, exited, true, opts)
			})
			if err != nil {
				t.Fatalf("RetryAgentSetup failed after port-taken recovery: %v (attempts=%d, picked=%v)", err, attempts, pickedPorts)
			}
			if attempts != 2 {
				t.Fatalf("attempts = %d, want 2", attempts)
			}
			if len(pickedPorts) != 2 || pickedPorts[0] != refusedPort || pickedPorts[1] != goodPort {
				t.Fatalf("picked ports = %v, want [%d %d] (refused port must be excluded on retry)", pickedPorts, refusedPort, goodPort)
			}

			// Graceful teardown: cancel setupCtx and wait on errgroup.
			setupCancel()
			if gerr := grp.Wait(); gerr != nil {
				t.Fatalf("errgroup.Wait() returned stale error from recovered attempt 1: %v", gerr)
			}
			_ = os.Remove(pidFile)

			if b, err := os.ReadFile(portsFile); err != nil {
				t.Fatalf("failed to read recorded docker publish ports: %v", err)
			} else if got, want := strings.TrimSpace(string(b)), fmt.Sprintf("127.0.0.1:%d:%d\n127.0.0.1:%d:%d", refusedPort, refusedPort, goodPort, goodPort); got != want {
				t.Fatalf("docker run publish args = %q, want %q", got, want)
			}

			if errEntries := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errEntries) != 0 {
				var msgs []string
				for _, e := range errEntries {
					msgs = append(msgs, fmt.Sprintf("%s %v", e.Message, e.ContextMap()))
				}
				t.Fatalf("recovered port-taken retry emitted %d ERROR log(s), want 0:\n%s", len(errEntries), strings.Join(msgs, "\n"))
			}
			if warnCount := logs.FilterLevelExact(zapcore.WarnLevel).Len(); warnCount != 1 {
				t.Fatalf("expected 1 Warn log from RetryAgentSetup, got %d", warnCount)
			}
			// Once the retry succeeded, refusedAgentPorts must be cleared so a
			// subsequent Setup on the same client can pick refusedPort again
			// rather than staying permanently blocked.
			nextAgentPort, _, _, nerr := a.agentPorts(true)
			if nerr != nil || nextAgentPort != refusedPort {
				t.Fatalf("agentPorts(true) after successful agentReady = (%d, %v); want (%d, nil) (refused port must be cleared on success)", nextAgentPort, nerr, refusedPort)
			}
		})
	}
}

// If every RetryAgentSetup attempt hits a taken port, all 3 attempts run,
// distinct ports are tried, RetryAgentSetup returns utils.ErrPortTaken, and
// the errgroup stays clean (the caller reports Setup's error once).
func TestDockerAgentPortTakenExhaustedRetriesReturnsErrPortTaken(t *testing.T) {
	ptyFn := needsAgentPTY
	t.Cleanup(func() { needsAgentPTY = ptyFn })
	needsAgentPTY = func() bool { return false }

	ports := []uint32{61650, 61651, 61652}
	dir := t.TempDir()
	fakeDockerForPortTakenRetry(t, dir, `#!/bin/sh
if [ "$1" = container ] && [ "$2" = run ]; then
  port=""
  for arg in "$@"; do
    case "$arg" in
      127.0.0.1:*:*) port=$(printf '%s' "$arg" | cut -d: -f2) ;;
    esac
  done
  printf 'docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint keploy-v3-exh (abc): failed to bind host port 127.0.0.1:%s/tcp: address already in use\n' "$port" >&2
  exit 125
fi
exit 0
`)

	wasContainerPort := containerPort
	t.Cleanup(func() { containerPort = wasContainerPort })
	containerPort = func(exclude ...uint32) (uint32, error) {
		for _, p := range ports {
			excluded := false
			for _, ex := range exclude {
				if ex == p {
					excluded = true
					break
				}
			}
			if !excluded {
				return p, nil
			}
		}
		return 0, errors.New("no stand-in port left")
	}

	core, logs := observer.New(zapcore.WarnLevel)
	conf := &config.Config{InstallationID: "install-id", ProxyPort: 18992, DNSPort: 18993}
	a := &AgentClient{logger: zap.New(core), conf: conf, dockerClient: &noopDockerClient{}}

	grp, gCtx := errgroup.WithContext(context.Background())
	setupCtx, setupCancel := context.WithCancel(context.WithValue(gCtx, models.ErrGroupKey, grp))
	t.Cleanup(func() {
		setupCancel()
		_ = grp.Wait()
	})

	var picked []uint32
	err := pkg.RetryAgentSetup(setupCtx, a.logger, func(c context.Context, attempt int) error {
		agentPort, proxyPort, dnsPort, perr := a.agentPorts(true)
		if perr != nil {
			return perr
		}
		picked = append(picked, agentPort)
		opts := models.SetupOptions{
			KeployContainer: fmt.Sprintf("keploy-v3-exh-%d", attempt),
			Mode:            models.MODE_RECORD,
			AgentPort:       agentPort,
			ProxyPort:       proxyPort,
			DnsPort:         dnsPort,
		}
		a.addressAgent(fmt.Sprintf("http://127.0.0.1:%d/agent", agentPort))
		exited, serr := a.startAgent(c, true, opts)
		if serr != nil {
			return serr
		}
		return a.agentReady(c, exited, true, opts)
	})
	if !errors.Is(err, utils.ErrPortTaken) {
		t.Fatalf("RetryAgentSetup = %v; want utils.ErrPortTaken after exhausting retries", err)
	}
	if len(picked) != 3 || picked[0] != 61650 || picked[1] != 61651 || picked[2] != 61652 {
		t.Fatalf("picked ports = %v; want [61650 61651 61652]", picked)
	}
	setupCancel()
	if gerr := grp.Wait(); gerr != nil {
		t.Fatalf("errgroup.Wait() = %v; want nil", gerr)
	}
	if errEntries := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errEntries) != 0 {
		t.Fatalf("exhausted port-taken retries emitted %d ERROR log(s) inside startAgent, want 0", len(errEntries))
	}
}

// A non-port-taken Docker failure (e.g. missing image) must still log ERROR,
// fail the errgroup, NOT be retried, and clear any previously refused ports
// from an earlier taken-port attempt so the client is not left permanently
// excluding them.
func TestDockerAgentNonPortTakenFailureStillLogsErrorAndFailsErrGroup(t *testing.T) {
	ptyFn := needsAgentPTY
	t.Cleanup(func() { needsAgentPTY = ptyFn })
	needsAgentPTY = func() bool { return false }

	const (
		refusedPort uint32 = 61645
		badImgPort  uint32 = 61646
	)
	dir := t.TempDir()
	counterFile := filepath.Join(dir, "count")
	fakeDockerForPortTakenRetry(t, dir, fmt.Sprintf(`#!/bin/sh
if [ "$1" = container ] && [ "$2" = run ]; then
  n=0
  [ -f %q ] && n=$(cat %q)
  n=$((n + 1))
  printf '%%s' "$n" > %q
  if [ "$n" -eq 1 ]; then
    printf 'docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint keploy-v3-miss (abc): failed to bind host port 127.0.0.1:%d/tcp: address already in use\n' >&2
    exit 125
  fi
  echo "docker: Error response from daemon: No such image: keploy" >&2
  exit 125
fi
exit 0
`, counterFile, counterFile, counterFile, refusedPort))

	wasContainerPort := containerPort
	t.Cleanup(func() { containerPort = wasContainerPort })
	containerPort = func(exclude ...uint32) (uint32, error) {
		for _, p := range []uint32{refusedPort, badImgPort} {
			excluded := false
			for _, ex := range exclude {
				if ex == p {
					excluded = true
					break
				}
			}
			if !excluded {
				return p, nil
			}
		}
		return 0, errors.New("no stand-in port left")
	}

	core, logs := observer.New(zapcore.ErrorLevel)
	conf := &config.Config{InstallationID: "install-id", ProxyPort: 18992, DNSPort: 18993}
	a := &AgentClient{logger: zap.New(core), conf: conf, dockerClient: &noopDockerClient{}}

	grp, _ := errgroup.WithContext(context.Background())
	setupCtx, setupCancel := context.WithCancel(context.WithValue(context.Background(), models.ErrGroupKey, grp))
	t.Cleanup(func() {
		setupCancel()
		_ = grp.Wait()
	})

	attempts := 0
	var picked []uint32
	err := pkg.RetryAgentSetup(setupCtx, a.logger, func(c context.Context, attempt int) error {
		attempts++
		agentPort, proxyPort, dnsPort, perr := a.agentPorts(true)
		if perr != nil {
			return perr
		}
		picked = append(picked, agentPort)
		opts := models.SetupOptions{
			KeployContainer: fmt.Sprintf("keploy-v3-missing-img-%d", attempt),
			Mode:            models.MODE_RECORD,
			AgentPort:       agentPort,
			ProxyPort:       proxyPort,
			DnsPort:         dnsPort,
		}
		a.addressAgent(fmt.Sprintf("http://127.0.0.1:%d/agent", agentPort))
		exited, serr := a.startAgent(c, true, opts)
		if serr != nil {
			return serr
		}
		return a.agentReady(c, exited, true, opts)
	})
	if err == nil || errors.Is(err, utils.ErrPortTaken) {
		t.Fatalf("RetryAgentSetup = %v; want non-ErrPortTaken failure", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d; want 2 (attempt 1 taken-port retried once, attempt 2 non-port-taken failure must not be retried)", attempts)
	}
	if len(picked) != 2 || picked[0] != refusedPort || picked[1] != badImgPort {
		t.Fatalf("picked ports = %v; want [%d %d]", picked, refusedPort, badImgPort)
	}
	setupCancel()
	if gerr := grp.Wait(); gerr == nil {
		t.Fatal("errgroup.Wait() = nil; want non-nil error for non-port-taken Docker failure")
	}
	if logs.FilterLevelExact(zapcore.ErrorLevel).Len() == 0 {
		t.Fatal("expected ERROR logs for non-port-taken Docker failure, got 0")
	}
	// After the non-retryable failure, refusedAgentPorts must be cleared so a
	// later Setup on the same client can pick refusedPort again.
	if nextPort, _, _, perr := a.agentPorts(true); perr != nil || nextPort != refusedPort {
		t.Fatalf("agentPorts(true) after non-retryable failure = (%d, %v); want (%d, nil)", nextPort, perr, refusedPort)
	}
}

// startInDockerWithPTY must also avoid logging ERROR on a taken port while
// still returning utils.ErrPortTaken and recording the refused port.
func TestStartInDockerWithPTYDoesNotLogErrorOnTakenPort(t *testing.T) {
	ptyFn := needsAgentPTY
	t.Cleanup(func() { needsAgentPTY = ptyFn })
	needsAgentPTY = func() bool { return true }

	const (
		port     uint32 = 61647
		nextPort uint32 = 61648
	)
	dir := t.TempDir()
	fakeDockerForPortTakenRetry(t, dir, fmt.Sprintf(`#!/bin/sh
if [ "$1" = container ] && [ "$2" = run ]; then
  printf 'docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint keploy-v3-pty (abc): failed to bind host port 127.0.0.1:%d/tcp: address already in use\n' >&2
  exit 125
fi
exit 0
`, port))

	wasContainerPort := containerPort
	t.Cleanup(func() { containerPort = wasContainerPort })
	containerPort = func(exclude ...uint32) (uint32, error) {
		for _, p := range []uint32{port, nextPort} {
			excluded := false
			for _, ex := range exclude {
				if ex == p {
					excluded = true
					break
				}
			}
			if !excluded {
				return p, nil
			}
		}
		return 0, errors.New("no stand-in port left")
	}

	core, logs := observer.New(zapcore.ErrorLevel)
	a := &AgentClient{logger: zap.New(core), conf: &config.Config{InstallationID: "install-id", ProxyPort: 18992, DNSPort: 18993}}
	opts := models.SetupOptions{KeployContainer: "keploy-v3-pty", Mode: models.MODE_RECORD, AgentPort: port, ProxyPort: 18992, DnsPort: 18993}
	err := a.startInDocker(context.Background(), zap.New(core), opts, newAgentRun(), nil)
	if !errors.Is(err, utils.ErrPortTaken) {
		t.Fatalf("startInDocker(PTY) = %v; want utils.ErrPortTaken", err)
	}
	if errEntries := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errEntries) != 0 {
		var msgs []string
		for _, e := range errEntries {
			msgs = append(msgs, fmt.Sprintf("%s %v", e.Message, e.ContextMap()))
		}
		t.Fatalf("startInDocker(PTY) emitted %d ERROR log(s) on taken port, want 0:\n%s", len(errEntries), strings.Join(msgs, "\n"))
	}
	if got, _, _, perr := a.agentPorts(true); perr != nil || got != nextPort {
		t.Fatalf("agentPorts(true) after PTY taken-port refusal = (%d, %v); want (%d, nil) (refused port %d must be excluded)", got, perr, nextPort, port)
	}
}

// A native agent (both non-PTY and PTY) whose first start exits with
// BootProgress.PortTaken=true must not poison the caller's errgroup, must not
// log ERROR before RetryAgentSetup retries and succeeds, and its already-gone
// first attempt must not issue a second /stop request against the retry's
// running agent during teardown.
func TestNativeAgentPortTakenRetryDoesNotPoisonErrGroupOrLogError(t *testing.T) {
	for _, onPTY := range []bool{false, true} {
		name := "without_PTY"
		if onPTY {
			name = "with_PTY"
		}
		t.Run(name, func(t *testing.T) {
			cmdFn, ptyCmdFn, cachedFn, ptyFn, look := newNativeAgentCommand, newNativeAgentPTYCommand, sudoCredentialsCached, needsAgentPTY, bootLook
			t.Cleanup(func() {
				newNativeAgentCommand, newNativeAgentPTYCommand, sudoCredentialsCached, needsAgentPTY, bootLook = cmdFn, ptyCmdFn, cachedFn, ptyFn, look
			})
			bootLook = time.Hour
			sudoCredentialsCached = func() bool { return !onPTY }
			needsAgentPTY = func() bool { return onPTY }

			const failure = "the proxy's port 16789 is taken by pid 7 (x): bind: address already in use"
			report, err := json.Marshal(utils.BootProgress{Failed: true, ExitCode: 1, Failure: failure, PortTaken: true})
			if err != nil {
				t.Fatal(err)
			}

			var stopCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/health") {
					w.WriteHeader(http.StatusOK)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/stop") {
					stopCalls.Add(1)
				}
				// Reject /stop so endAgent falls back to SIGTERM against the stand-in
				// process instead of waiting indefinitely on a fake server that does
				// not exit on HTTP stop.
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(srv.Close)

			var attemptNum atomic.Int32
			var procsMu sync.Mutex
			var procs []*exec.Cmd
			makeStandIn := func(args []string, attr *syscall.SysProcAttr) *exec.Cmd {
				n := attemptNum.Add(1)
				var progressPath string
				for i := 0; i < len(args)-1; i++ {
					if args[i] == "--teardown-progress-file" {
						progressPath = args[i+1]
						break
					}
				}
				bootReport := filepath.Join(filepath.Dir(progressPath), utils.BootProgressFileName)
				var cmd *exec.Cmd
				if n == 1 {
					cmd = exec.Command("sh", "-c", `printf '%s' "$1" > "$2"; exit 1`, "sh", string(report), bootReport)
				} else {
					cmd = exec.Command("sh", "-c", `exec sleep 60`)
				}
				cmd.SysProcAttr = attr
				procsMu.Lock()
				procs = append(procs, cmd)
				procsMu.Unlock()
				return cmd
			}
			newNativeAgentCommand = func(_ string, args []string, _ bool) *exec.Cmd {
				return makeStandIn(args, &syscall.SysProcAttr{Setpgid: true})
			}
			newNativeAgentPTYCommand = func(_ string, args []string) *exec.Cmd {
				return makeStandIn(args, &syscall.SysProcAttr{Setsid: true})
			}

			core, logs := observer.New(zapcore.WarnLevel)
			conf := &config.Config{InstallationID: "install-id"}
			a := &AgentClient{logger: zap.New(core), conf: conf}

			grp, gCtx := errgroup.WithContext(context.Background())
			setupCtx, setupCancel := context.WithCancel(context.WithValue(gCtx, models.ErrGroupKey, grp))
			var grpDone atomic.Bool
			t.Cleanup(func() {
				setupCancel()
				if !grpDone.Load() {
					procsMu.Lock()
					for _, p := range procs {
						if p != nil && p.Process != nil {
							_ = syscall.Kill(-p.Process.Pid, syscall.SIGKILL)
						}
					}
					procsMu.Unlock()
				}
				_ = grp.Wait()
			})

			attempts := 0
			err = pkg.RetryAgentSetup(setupCtx, a.logger, func(c context.Context, attempt int) error {
				attempts++
				if attempt == 2 {
					a.addressAgent(srv.URL + "/agent")
				} else {
					a.addressAgent("http://127.0.0.1:1/agent")
				}
				opts := models.SetupOptions{
					AgentPort: 26795 + uint32(attempt),
					ProxyPort: 16789 + uint32(attempt),
					DnsPort:   26797 + uint32(attempt),
				}
				exited, serr := a.startAgent(c, false, opts)
				if serr != nil {
					return serr
				}
				return a.agentReady(c, exited, false, opts)
			})
			if err != nil {
				t.Fatalf("RetryAgentSetup(native, PTY=%v) failed after port-taken recovery: %v (attempts=%d)", onPTY, err, attempts)
			}
			if attempts != 2 {
				t.Fatalf("native attempts (PTY=%v) = %d, want 2", onPTY, attempts)
			}
			setupCancel()
			gerr := grp.Wait()
			grpDone.Store(true)
			if gerr != nil {
				t.Fatalf("native errgroup.Wait() (PTY=%v) returned stale error from recovered attempt 1: %v", onPTY, gerr)
			}
			if got := stopCalls.Load(); got != 1 {
				t.Fatalf("native /stop calls during teardown (PTY=%v) = %d, want 1 (dead attempt 1 must not stop attempt 2)", onPTY, got)
			}
			if errEntries := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errEntries) != 0 {
				var msgs []string
				for _, e := range errEntries {
					msgs = append(msgs, fmt.Sprintf("%s %v", e.Message, e.ContextMap()))
				}
				t.Fatalf("recovered native port-taken retry (PTY=%v) emitted %d ERROR log(s), want 0:\n%s", onPTY, len(errEntries), strings.Join(msgs, "\n"))
			}
		})
	}
}
