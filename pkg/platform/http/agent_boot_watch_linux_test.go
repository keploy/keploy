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
	"go.keploy.io/server/v3/pkg/client/app"
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
	running := containerRunning(newEngineCLI(zap.NewNop()), &namedContainer{name: "keploy-v3"})
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

// looksFound is how many inspects the engine has answered that found the
// container in state; "" counts them all.
func (e *standInEngine) looksFound(state string) int {
	b, _ := os.ReadFile(e.looks)
	got := 0
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" && (state == "" || l == state) {
			got++
		}
	}
	return got
}

// awaitLooks waits until the engine has answered n inspects that found the
// container in state, failing the test if done says the wait it serves ended
// first. It counts looks, not time: a loaded host only makes them come later.
func (e *standInEngine) awaitLooks(t *testing.T, state string, n int, done <-chan error) {
	t.Helper()
	for {
		got := e.looksFound(state)
		if got >= n {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("the release ended with %v after %d looks found the container %s", err, got, state)
		default:
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitRelease waits for the release done serves to end, and returns what it
// ended with. An end that is due at the watch's next look at the container
// (withinLooks > 0) fails the test once the engine has answered that many
// more inspects without it: the watch looks on while the release waits, so it
// is the count, not a clock, that says the release waited past the look that
// was to end it. With withinLooks 0 it waits however long the end takes: one
// the agent's answer gives, at the release's next ask, not at a look.
func (e *standInEngine) awaitRelease(t *testing.T, done <-chan error, withinLooks int) error {
	t.Helper()
	if withinLooks <= 0 {
		return <-done
	}
	from := e.looksFound("")
	for {
		select {
		case err := <-done:
			return err
		default:
		}
		if n := e.looksFound("") - from; n >= withinLooks {
			t.Fatalf("the release still waits, %d looks at the container after the one that was to end it", n)
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

// endLooks is how many more inspects a release whose end is due at the watch's
// next look may see answered before awaitRelease takes it to wait on: a look
// asks the engine twice (whether the container runs, whether it has ended),
// and one can be out as the end comes, so a few would do; the rest is margin,
// counted in looks.
const endLooks = 50

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
	t.Cleanup(a.forgetAgentContainer)
	done := make(chan error, 1)
	go func() { done <- a.MakeAgentReadyForDockerCompose(context.Background()) }()
	e.awaitLooks(t, "absent", 20, done)
	e.set(t, "created")
	e.awaitLooks(t, "created", 20, done)
	e.set(t, "running")
	if err := e.awaitRelease(t, done, 0); err != nil {
		t.Fatalf("the release ended with %v; want the agent's release, once the container docker compose created runs", err)
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
			t.Cleanup(a.forgetAgentContainer)
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
			if err := e.awaitRelease(t, done, endLooks); err == nil || !strings.Contains(err.Error(), "the keploy agent's container is gone (removed)") {
				t.Fatalf("the release ended with %v; want its container gone", err)
			}
		})
	}
}

// A Setup's naming of its agent's container starts what is known of it
// afresh: names are short and random, and a later Setup's container that
// compose has not created yet is not an earlier one's that was removed, nor
// one an earlier run ended without. The client keeps the entry its naming
// made, and forgets the container it named before, and its address: what
// keploy keeps of agent containers outside its clients is an address for
// each client, not one for each Setup a process ever made. A look at a name
// the client did not name notes nothing on the one it did.
func TestNamingTheAgentsContainerForgetsWhatWasKnownOfTheName(t *testing.T) {
	name := fmt.Sprintf("keploy-v3-renamed-%d", os.Getpid())
	a := clientFor("http://127.0.0.1:1")
	t.Cleanup(a.forgetAgentContainer)
	a.nameAgentContainer(name)
	first := a.containerNamed(name)
	answeredAt("127.0.0.1:1")
	if !first.isKnown() {
		t.Fatal("an answer at the agent's address does not make its container known")
	}
	first.noteRun(true)
	a.nameAgentContainer(name)
	if c := a.containerNamed(name); c == first || c.isKnown() || c.runOver() {
		t.Fatal("a name a Setup took is still known, or its run over, from before it")
	}

	next := name + "-next"
	a.conf.Agent.AgentURI = "http://127.0.0.1:2"
	a.nameAgentContainer(next)
	if _, ok := containerAt.Load("127.0.0.1:1"); ok {
		t.Fatal("the client named another container, and its address before still names the one before")
	}
	answeredAt("127.0.0.1:2")
	if !a.containerNamed(next).isKnown() {
		t.Fatal("an answer at the agent's new address does not make its new container known")
	}

	// Another client's naming of the same name starts it afresh too: a name
	// is two random bytes, and a later client's container that compose has
	// not created yet is not the earlier client's, which keploy knew.
	b := clientFor("http://127.0.0.1:3")
	t.Cleanup(b.forgetAgentContainer)
	b.nameAgentContainer(next)
	later := b.containerNamed(next)
	if later.isKnown() {
		t.Fatal("another client named the same container, and it is known from the first client's")
	}
	// What a client forgets is only what it kept itself: the earlier client
	// moving on to another container leaves the later client's address as it
	// is. And an answer at the earlier client's address, from its agent, says
	// nothing of the later client's container.
	answeredAt("127.0.0.1:2")
	if later.isKnown() {
		t.Fatal("an answer at the earlier client's address made the later client's container known")
	}
	a.nameAgentContainer(name)
	answeredAt("127.0.0.1:3")
	if !later.isKnown() {
		t.Fatal("an answer at the later client's address does not make its container known, once the earlier client moved on")
	}
	// Nor does a client moving on forget the address a later client's agent
	// now serves from, the same as its own was.
	c := clientFor("http://127.0.0.1:3")
	t.Cleanup(c.forgetAgentContainer)
	third := name + "-third"
	c.nameAgentContainer(third)
	b.forgetAgentContainer()
	answeredAt("127.0.0.1:3")
	if !c.containerNamed(third).isKnown() {
		t.Fatal("a client moved on, and forgot the address a later client's agent serves from")
	}

	other := a.containerNamed(name + "-not-named")
	other.know()
	other.noteRun(true)
	if own := a.containerNamed(name); own == other || own.isKnown() || own.runOver() {
		t.Fatal("a look at a container the client did not name noted on the one it did")
	}

	// A client that sets up an agent with no container (a native Setup)
	// forgets the one it named, and its address: its watches watch no
	// container, and its run is no container's.
	a.forgetAgentContainer()
	a.mu.Lock()
	kept := a.agentContainer
	a.mu.Unlock()
	if kept != nil {
		t.Fatal("the client forgot its container, and still has it")
	}
	if _, ok := containerAt.Load("127.0.0.1:2"); ok {
		t.Fatal("the client forgot its container, and its address still names it")
	}
}

// Two clients whose Setups drew the same name each have their own container:
// a name is two random bytes, and a process can make one client after
// another (enterprise's sandbox makes one for each command it runs), or have
// two at once. What one client's run and one client's watch see is never
// taken for the other's: the earlier client's run's end does not end the
// later client's release, asked before the later client's run began, with
// its container still to come; the earlier client's run's begin does not
// undo the later client's run's end; and the earlier client's look at its own
// container, while it is there, does not make the later client's known, so
// that the later one, not created yet, is waited for, not taken for gone
// once the earlier one is removed. The release goes through Run and
// MakeAgentReadyForDockerCompose, as enterprise's callers drive it.
func TestAnotherClientsContainerOfTheSameNameIsNotThisClients(t *testing.T) {
	failingApp := func() *app.App {
		return app.NewApp(zap.NewNop(), "exit 3", nil, models.SetupOptions{CommandType: string(utils.Native)})
	}
	release := func(c *AgentClient) <-chan error {
		done := make(chan error, 1)
		go func() { done <- c.MakeAgentReadyForDockerCompose(context.Background()) }()
		return done
	}
	t.Run("the earlier client's run's end", func(t *testing.T) {
		fastLooks(t)
		e := newStandInEngine(t)
		t.Setenv("KEPLOY_PROVEN", "")
		name := fmt.Sprintf("keploy-v3-same-end-%d", os.Getpid())
		earlier := clientFor(standInComposeAgent(t, e, nil).URL)
		t.Cleanup(earlier.forgetAgentContainer)
		earlier.nameAgentContainer(name)
		later := clientFor(standInComposeAgent(t, e, nil).URL)
		t.Cleanup(later.forgetAgentContainer)
		later.nameAgentContainer(name)
		earlier.apps.Store(uint64(0), failingApp())
		if appErr := earlier.Run(context.Background(), models.RunOptions{}); appErr.AppErrorType != models.ErrUnExpected {
			t.Fatalf("the earlier client's run ended %+v; want its failure, %s", appErr, models.ErrUnExpected)
		}
		done := release(later)
		e.awaitLooks(t, "absent", 20, done)
		e.set(t, "running")
		if err := e.awaitRelease(t, done, 0); err != nil {
			t.Fatalf("the later client's release ended with %v; want the agent's release, once its container runs", err)
		}
	})
	t.Run("the earlier client's run's begin", func(t *testing.T) {
		fastLooks(t)
		e := newStandInEngine(t)
		t.Setenv("KEPLOY_PROVEN", "")
		name := fmt.Sprintf("keploy-v3-same-begin-%d", os.Getpid())
		earlier := clientFor(standInComposeAgent(t, e, nil).URL)
		t.Cleanup(earlier.forgetAgentContainer)
		earlier.nameAgentContainer(name)
		later := clientFor(standInComposeAgent(t, e, nil).URL)
		t.Cleanup(later.forgetAgentContainer)
		later.nameAgentContainer(name)
		later.apps.Store(uint64(0), failingApp())
		if appErr := later.Run(context.Background(), models.RunOptions{}); appErr.AppErrorType != models.ErrUnExpected {
			t.Fatalf("the later client's run ended %+v; want its failure, %s", appErr, models.ErrUnExpected)
		}
		// The earlier client's run begins, and goes on until the test ends
		// it. Its app says it has begun through a FIFO, whose read ends once
		// the app has opened it and written: Run notes its run's begin
		// before it starts the app.
		began := filepath.Join(t.TempDir(), "began")
		if err := syscall.Mkfifo(began, 0o600); err != nil {
			t.Fatal(err)
		}
		earlier.apps.Store(uint64(0), app.NewApp(zap.NewNop(), "echo begun > '"+began+"'; exec sleep 600", nil, models.SetupOptions{CommandType: string(utils.Native)}))
		ctx, cancel := context.WithCancel(context.Background())
		ran := make(chan models.AppError, 1)
		go func() { ran <- earlier.Run(ctx, models.RunOptions{}) }()
		t.Cleanup(func() {
			cancel()
			<-ran
		})
		if _, err := os.ReadFile(began); err != nil {
			t.Fatal(err)
		}
		if err := e.awaitRelease(t, release(later), endLooks); err == nil || !strings.Contains(err.Error(), "ended without the keploy agent's container") {
			t.Fatalf("the later client's release ended with %v; want its run ended without the agent's container", err)
		}
	})
	t.Run("the earlier client's look at its own container", func(t *testing.T) {
		fastLooks(t)
		e := newStandInEngine(t)
		t.Setenv("KEPLOY_PROVEN", "")
		name := fmt.Sprintf("keploy-v3-same-look-%d", os.Getpid())
		// The earlier client's agent answers nothing: only its watch's look
		// makes its container known.
		earlier := clientFor(standInComposeAgent(t, e, func() int { return 0 }).URL)
		t.Cleanup(earlier.forgetAgentContainer)
		earlier.nameAgentContainer(name)
		later := clientFor(standInComposeAgent(t, e, nil).URL)
		t.Cleanup(later.forgetAgentContainer)
		later.nameAgentContainer(name)
		e.set(t, "running")
		earlierDone := release(earlier)
		e.awaitLooks(t, "running", 5, earlierDone)
		e.set(t, "absent")
		if err := e.awaitRelease(t, earlierDone, endLooks); err == nil || !strings.Contains(err.Error(), "the keploy agent's container is gone (removed)") {
			t.Fatalf("the earlier client's release ended with %v; want its container gone", err)
		}
		// The later client's container is not created yet: its release
		// waits for it.
		done := release(later)
		e.awaitLooks(t, "absent", 20, done)
		e.set(t, "running")
		if err := e.awaitRelease(t, done, 0); err != nil {
			t.Fatalf("the later client's release ended with %v; want the agent's release, once its container runs", err)
		}
	})
}

// The release of the app ends once the run of the application that was to
// create the agent's container is over, and the engine has no such container,
// which keploy never knew to be there: docker compose ended without creating
// it (the agent's image did not pull, a hook wrote a file compose rejects, a
// service created before it hit a name conflict). Enterprise's headless test
// and replay stack wait on nothing else that ends then: their ctx ends with a
// signal only. It ends so whether the run ends while the release waits, or
// before the release began (the headless test fetches its suites between the
// two), and whether the run's app ran or the run ended before it could (no
// app to run); at the watch's first look after the run's end. The project's
// run here is an `up` that fails before compose creates the agent's
// container: the stand-in engine never has it.
func TestTheReleaseEndsOnceTheRunEndedWithoutTheAgentsContainer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		releaseFirst  bool
		appToRun      bool
		wantRunFailed models.AppErrorType
	}{
		{"while the release waits", true, true, models.ErrUnExpected},
		{"before the release began", false, true, models.ErrUnExpected},
		{"a run that ended before its app's run began", true, false, models.ErrInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastLooks(t)
			e := newStandInEngine(t)
			srv := standInComposeAgent(t, e, nil)
			t.Setenv("KEPLOY_PROVEN", "")
			a := clientFor(srv.URL)
			a.nameAgentContainer(fmt.Sprintf("keploy-v3-never-%d", os.Getpid()))
			t.Cleanup(a.forgetAgentContainer)
			if tc.appToRun {
				a.apps.Store(uint64(0), app.NewApp(zap.NewNop(), "exit 3", nil, models.SetupOptions{CommandType: string(utils.Native)}))
			}
			done := make(chan error, 1)
			release := func() { go func() { done <- a.MakeAgentReadyForDockerCompose(context.Background()) }() }
			// Run's end is noted as it returns, so the release cannot end
			// before Run has returned: the run is waited for in full.
			run := func() {
				if appErr := a.Run(context.Background(), models.RunOptions{}); appErr.AppErrorType != tc.wantRunFailed {
					t.Fatalf("the project's run ended %+v; want its failure, %s", appErr, tc.wantRunFailed)
				}
			}
			if tc.releaseFirst {
				release()
				// It waits, while the run that is to create the container
				// has not ended.
				e.awaitLooks(t, "absent", 20, done)
				run()
			} else {
				run()
				release()
			}
			if err := e.awaitRelease(t, done, endLooks); err == nil || !strings.Contains(err.Error(), "ended without the keploy agent's container") {
				t.Fatalf("the release ended with %v; want the run ended without the agent's container", err)
			}
		})
	}
}

// The release of the app ends, too, once the run of the application is over
// with the agent's container still there but not running: compose created it
// and never started it, or it is paused or restarting, and the down that was
// to remove it failed, or was cut by its bound. Nothing will start it once
// the run is over, so whatever the engine says of its state, the release ends
// at the watch's first look after the run's end, whether the run ends while
// the release waits or before it began. A container that runs is not ended by
// the run's end: its agent can still answer. One being removed is gone.
func TestTheReleaseEndsOnceTheRunEndedLeavingTheAgentsContainerNotRunning(t *testing.T) {
	for _, tc := range []struct {
		state        string
		releaseFirst bool
	}{
		{"created", true},
		{"created", false},
		{"paused", true},
		{"restarting", true},
	} {
		order := "while the release waits"
		if !tc.releaseFirst {
			order = "before the release began"
		}
		t.Run(tc.state+", "+order, func(t *testing.T) {
			fastLooks(t)
			e := newStandInEngine(t)
			e.set(t, tc.state)
			srv := standInComposeAgent(t, e, nil)
			t.Setenv("KEPLOY_PROVEN", "")
			a := clientFor(srv.URL)
			name := fmt.Sprintf("keploy-v3-left-%d", os.Getpid())
			a.nameAgentContainer(name)
			t.Cleanup(a.forgetAgentContainer)
			a.apps.Store(uint64(0), app.NewApp(zap.NewNop(), "exit 3", nil, models.SetupOptions{CommandType: string(utils.Native)}))
			done := make(chan error, 1)
			release := func() { go func() { done <- a.MakeAgentReadyForDockerCompose(context.Background()) }() }
			run := func() {
				if appErr := a.Run(context.Background(), models.RunOptions{}); appErr.AppErrorType != models.ErrUnExpected {
					t.Fatalf("the project's run ended %+v; want its failure, %s", appErr, models.ErrUnExpected)
				}
			}
			if tc.releaseFirst {
				release()
				// It waits, while the run that is to start the container
				// has not ended.
				e.awaitLooks(t, tc.state, 20, done)
				run()
			} else {
				run()
				release()
			}
			if err := e.awaitRelease(t, done, endLooks); err == nil || !strings.Contains(err.Error(), "ended with the keploy agent's container not running") {
				t.Fatalf("the release ended with %v; want the run ended with the agent's container not running", err)
			}

			exited := containerExited(newEngineCLI(zap.NewNop()), a.containerNamed(name))
			e.set(t, "running")
			if code, ended := exited(context.Background()); ended {
				t.Fatalf("containerExited = %d, %v of a container that runs, once its run is over; want not ended", code, ended)
			}
			e.set(t, "removing")
			if code, ended := exited(context.Background()); !ended || code != containerGone {
				t.Fatalf("containerExited = %d, %v of a container being removed; want containerGone, ended", code, ended)
			}
		})
	}
}

// A run of the application begun again after one that ended without the
// agent's container is the one to create it now: the release waits for it,
// as for any container compose has not created yet, and takes the agent's
// release once it runs. The run before it being over is no end of this one.
func TestTheReleaseWaitsForTheContainerOfARunBegunAgain(t *testing.T) {
	fastLooks(t)
	e := newStandInEngine(t)
	srv := standInComposeAgent(t, e, nil)
	t.Setenv("KEPLOY_PROVEN", "")
	a := clientFor(srv.URL)
	name := fmt.Sprintf("keploy-v3-again-%d", os.Getpid())
	a.nameAgentContainer(name)
	t.Cleanup(a.forgetAgentContainer)
	a.apps.Store(uint64(0), app.NewApp(zap.NewNop(), "exit 3", nil, models.SetupOptions{CommandType: string(utils.Native)}))
	if appErr := a.Run(context.Background(), models.RunOptions{}); appErr.AppErrorType == "" {
		t.Fatalf("the first run ended %+v; want its failure", appErr)
	}
	if !a.containerNamed(name).runOver() {
		t.Fatal("setup: the first run is over, and is not noted so")
	}

	// The second run goes on until the test ends it. Its app says it has
	// begun through a FIFO, whose read ends once the app has opened it and
	// written: Run notes its run's begin before it starts the app.
	began := filepath.Join(t.TempDir(), "began")
	if err := syscall.Mkfifo(began, 0o600); err != nil {
		t.Fatal(err)
	}
	a.apps.Store(uint64(0), app.NewApp(zap.NewNop(), "echo begun > '"+began+"'; exec sleep 600", nil, models.SetupOptions{CommandType: string(utils.Native)}))
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan models.AppError, 1)
	go func() { ran <- a.Run(ctx, models.RunOptions{}) }()
	t.Cleanup(func() {
		cancel()
		<-ran
	})
	if _, err := os.ReadFile(began); err != nil {
		t.Fatal(err)
	}
	if a.containerNamed(name).runOver() {
		t.Fatal("the second run began, and the first one's end is still noted")
	}

	done := make(chan error, 1)
	go func() { done <- a.MakeAgentReadyForDockerCompose(context.Background()) }()
	e.awaitLooks(t, "absent", 20, done)
	e.set(t, "running")
	if err := e.awaitRelease(t, done, 0); err != nil {
		t.Fatalf("the release ended with %v; want the agent's release, once the container the second run created runs", err)
	}
}
