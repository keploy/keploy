//go:build linux

package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// standInSudo starts a stand-in for a sudo that has run its command: a shell
// whose child is a sleep, in a process group of its own, killed whole when the
// test ends.
func standInSudo(t *testing.T) *exec.Cmd {
	t.Helper()
	sudo := exec.Command("sh", "-c", "sleep 60; true")
	sudo.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sudo.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-sudo.Process.Pid, syscall.SIGKILL); _ = sudo.Wait() })
	return sudo
}

// Once the sudo that may prompt has run the agent (its prompt answered), the
// agent's process runs, and its first report is due within bootSilence of
// keploy's runnable time: an agent that hangs before it was waited on for ever.
// A command sudo runs that is not the agent (a helper it runs while the user
// is still at its prompt) is no agent run.
func TestWaitForAgentHoldsAnAgentSudoHasRunToItsFirstReport(t *testing.T) {
	look, silence := bootLook, bootSilence
	bootLook, bootSilence = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { bootLook, bootSilence = look, silence })
	sudo := standInSudo(t)

	r := newAgentRun()
	r.watchBoot(filepath.Join(t.TempDir(), "teardown.json"), true, "/usr/local/bin/keploy")
	r.sudoPid.Store(int64(sudo.Process.Pid))
	for i := 0; i < 50; i++ {
		if r.sudoRanAgent(context.Background()) {
			t.Fatal("a command sudo runs that is not the agent was taken for it")
		}
		time.Sleep(time.Millisecond)
	}

	a := clientFor(newStandIn(t, drops).url)
	a.agentCancel = func() {}
	a.run = newAgentRun()
	a.run.watchBoot(filepath.Join(t.TempDir(), "teardown.json"), true, "/bin/sleep") // the agent, here
	a.run.sudoPid.Store(int64(sudo.Process.Pid))
	err := waitWithin(t, a, context.Background(), nil)
	if !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "without reporting its start") {
		t.Fatalf("waitForAgent = %v; want a stalled start, its first report not made", err)
	}
}

// startNativeAgent sets, before the agent starts, what its wait watches: the
// report next to the teardown progress file its command line names, whether
// the sudo that starts it may prompt, and, once started, that sudo's process:
// started on a PTY (sudo's prompt on a terminal), or not.
func TestStartNativeAgentSetsWhatItsStartsWaitWatches(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root, keploy starts the agent without sudo, never on a PTY")
	}
	for _, onPTY := range []bool{false, true} {
		t.Run(map[bool]string{false: "without a PTY", true: "on a PTY"}[onPTY], func(t *testing.T) {
			startNativeAgentWatched(t, onPTY)
		})
	}
}

func startNativeAgentWatched(t *testing.T, onPTY bool) {
	cmdFn, ptyCmdFn, cachedFn, ptyFn := newNativeAgentCommand, newNativeAgentPTYCommand, sudoCredentialsCached, needsAgentPTY
	t.Cleanup(func() {
		newNativeAgentCommand, newNativeAgentPTYCommand, sudoCredentialsCached, needsAgentPTY = cmdFn, ptyCmdFn, cachedFn, ptyFn
	})
	var started *exec.Cmd
	var argv []string
	standIn := func(args []string, attr *syscall.SysProcAttr) *exec.Cmd {
		argv = args
		started = exec.Command("sh", "-c", "sleep 60; true")
		started.SysProcAttr = attr
		return started
	}
	newNativeAgentCommand = func(_ string, args []string, _ bool) *exec.Cmd {
		if onPTY {
			t.Error("the agent was started without a PTY")
		}
		return standIn(args, &syscall.SysProcAttr{Setpgid: true})
	}
	newNativeAgentPTYCommand = func(_ string, args []string) *exec.Cmd {
		if !onPTY {
			t.Error("the agent was started on a PTY")
		}
		return standIn(args, &syscall.SysProcAttr{Setsid: true})
	}
	sudoCredentialsCached = func() bool { return false }
	needsAgentPTY = func() bool { return onPTY }

	a := clientFor(newStandIn(t, drops).url)
	a.conf.Agent.AgentURI = "" // its stop is the test's
	grp, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(context.WithValue(gctx, models.ErrGroupKey, grp))
	run := newAgentRun()
	exited := make(chan error, 1)
	if err := a.startNativeAgent(ctx, models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797}, exited, run); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-started.Process.Pid, syscall.SIGKILL)
		cancel()
		_ = grp.Wait()
	})
	i := slices.Index(argv, "--teardown-progress-file")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("no teardown progress file in %v", argv)
	}
	if want := filepath.Join(filepath.Dir(argv[i+1]), utils.BootProgressFileName); run.bootReport != want {
		t.Fatalf("the wait reads %q; want %q, next to the teardown progress file", run.bootReport, want)
	}
	if !run.bootPrompt {
		t.Fatal("the wait does not wait for the prompt of a sudo with no cached credentials")
	}
	if got := run.sudoPid.Load(); got != int64(started.Process.Pid) {
		t.Fatalf("the wait watches process %d; want the one started, %d", got, started.Process.Pid)
	}
}

// A container runs once its status is running: not while it is created, nor
// paused (its processes frozen: docker says Running of a paused container
// too), restarting or exited.
func TestAPausedContainerIsNotRunning(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	script := "#!/bin/sh\n[ \"$1\" = inspect ] && cat \"" + state + "\" && exit 0\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	running := containerRunning(newEngineCLI(zap.NewNop()), "keploy-v3")
	for _, tc := range []struct {
		status string
		want   bool
	}{{"created", false}, {"running", true}, {"paused", false}, {"restarting", false}, {"exited", false}} {
		if err := os.WriteFile(state, []byte(tc.status+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := running(context.Background()); got != tc.want {
			t.Errorf("a container whose status is %s runs: %v, want %v", tc.status, got, tc.want)
		}
	}
}

// standInEngine puts a stand-in for the container engine's CLI first on PATH:
// it answers inspect of one container from the state the test last set
// ("absent" while the engine does not know it, as before docker compose has
// created it, and once it is removed; else its status), with what docker says
// of a container it does not know, on stderr, and notes the state each inspect
// found. Nothing else it is asked answers.
type standInEngine struct {
	state, looks string
}

func newStandInEngine(t *testing.T) *standInEngine {
	t.Helper()
	dir := t.TempDir()
	e := &standInEngine{state: filepath.Join(dir, "state"), looks: filepath.Join(dir, "looks")}
	e.set(t, "absent")
	script := `#!/bin/sh
s=$(cat "` + e.state + `")
[ "$1" = inspect ] || exit 1
for last; do :; done
echo "$s" >> "` + e.looks + `"
if [ "$s" = absent ]; then
  echo "Error: No such object: $last" >&2
  exit 1
fi
case "$3" in
  *ExitCode*) echo "$s 0" ;;
  *) echo "$s" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return e
}

func (e *standInEngine) set(t *testing.T, state string) {
	t.Helper()
	if err := os.WriteFile(e.state+".tmp", []byte(state+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(e.state+".tmp", e.state); err != nil {
		t.Fatal(err)
	}
}

func (e *standInEngine) is(state string) bool {
	b, err := os.ReadFile(e.state)
	return err == nil && strings.TrimSpace(string(b)) == state
}

// awaitLooks waits until the engine has answered n inspects that found the
// container in state, failing the test if done says the wait it serves ended
// first.
func (e *standInEngine) awaitLooks(t *testing.T, state string, n int, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, _ := os.ReadFile(e.looks)
		got := 0
		for _, l := range strings.Split(string(b), "\n") {
			if l == state {
				got++
			}
		}
		if got >= n {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("the release ended with %v after %d looks found the container %s", err, got, state)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d looks found the container %s, want %d", got, state, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// standInComposeAgent is the agent the engine e runs: it answers only while
// its container runs (else the connection is dropped, unanswered, as nothing
// serves the agent's address then), with answer's status (200 for a nil
// answer; 0 drops the connection all the same).
func standInComposeAgent(t *testing.T, e *standInEngine, answer func() int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusOK
		if answer != nil {
			code = answer()
		}
		if !e.is("running") || code == 0 {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fastLooks has the watches look at the agent once a millisecond.
func fastLooks(t *testing.T) {
	t.Helper()
	look := bootLook
	bootLook = time.Millisecond
	t.Cleanup(func() { bootLook = look })
}

// The release of the app waits for the agent's container docker compose has
// not created yet: a container the engine does not know, which keploy has never
// known to be there, is one still to come, not one that is gone. Enterprise's
// headless test and replay stack ask for the release as soon as they have
// started the project, and on a loaded host compose takes more than a look to
// create the agent's container (atg-with-mocks, pipeline 12070: "Container
// keploy-v3-4bc1 Creating", and the release ended at its first look, with "the
// keploy agent's container is gone (removed)").
func TestTheReleaseWaitsForAContainerComposeHasNotCreatedYet(t *testing.T) {
	fastLooks(t)
	e := newStandInEngine(t)
	srv := standInComposeAgent(t, e, nil)
	t.Setenv("KEPLOY_PROVEN", "")
	a := clientFor(srv.URL)
	a.nameAgentContainer(fmt.Sprintf("keploy-v3-not-yet-%d", os.Getpid()))
	done := make(chan error, 1)
	go func() { done <- a.MakeAgentReadyForDockerCompose(context.Background()) }()
	e.awaitLooks(t, "absent", 20, done)
	e.set(t, "created")
	e.awaitLooks(t, "created", 20, done)
	e.set(t, "running")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the release ended with %v; want the agent's release, once the container docker compose created runs", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the release did not end once the agent took it")
	}
}

// Once keploy has known the agent's container to be there, the engine not
// knowing it is its end: the release ends, saying it is gone, whether keploy
// knew it from a look at it (the engine said it runs) or from the agent's
// answer alone (its container removed before any look got to it).
func TestTheReleaseEndsOnTheRemovalOfAContainerKeployKnew(t *testing.T) {
	for _, knownBy := range []string{"a look", "the agent's answer"} {
		t.Run(knownBy, func(t *testing.T) {
			fastLooks(t)
			e := newStandInEngine(t)
			// The agent takes no release: known by a look, it answers
			// nothing, so that only the look makes it known; else it
			// answers that it is not ready.
			answer := http.StatusServiceUnavailable
			if knownBy == "a look" {
				answer = 0
			}
			srv := standInComposeAgent(t, e, func() int { return answer })
			t.Setenv("KEPLOY_PROVEN", "")
			a := clientFor(srv.URL)
			a.nameAgentContainer(fmt.Sprintf("keploy-v3-known-%d", os.Getpid()))
			done := make(chan error, 1)
			if knownBy == "a look" {
				e.set(t, "running")
				go func() { done <- a.MakeAgentReadyForDockerCompose(context.Background()) }()
				e.awaitLooks(t, "running", 5, done)
			} else {
				e.set(t, "running")
				res, err := a.client.Get(srv.URL + "/agent/health")
				if err != nil {
					t.Fatal(err)
				}
				_ = res.Body.Close()
				e.set(t, "absent")
				go func() { done <- a.MakeAgentReadyForDockerCompose(context.Background()) }()
			}
			e.set(t, "absent")
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "the keploy agent's container is gone (removed)") {
					t.Fatalf("the release ended with %v; want its container gone", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the release did not end once the container keploy knew was removed")
			}
		})
	}
}

// What was known of a container name before a Setup names its own agent's
// container so is forgotten: names are short and random, and a later Setup's
// container that compose has not created yet is not an earlier one's that was
// removed.
func TestNamingTheAgentsContainerForgetsWhatWasKnownOfTheName(t *testing.T) {
	name := fmt.Sprintf("keploy-v3-renamed-%d", os.Getpid())
	knowContainer(name)
	a := clientFor("http://127.0.0.1:1")
	a.nameAgentContainer(name)
	if containerKnown(name) {
		t.Fatal("a name a Setup took is still known from before it")
	}
	answeredAt("127.0.0.1:1")
	if !containerKnown(name) {
		t.Fatal("an answer at the agent's address does not make its container known")
	}
}
