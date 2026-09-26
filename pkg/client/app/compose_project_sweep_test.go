package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// These cover the teardown sweep that finishes what a cut-short
// `docker compose down` left behind. Both halves shell out to the docker CLI,
// so the tests put a recording stand-in for `docker` at the front of PATH:
// that exercises the real argument construction, the real stdin plumbing and
// the real output parsing, which is where this breaks silently.

// stubDockerCLI installs an executable named `docker` that logs its argv (one
// argument per line) plus anything it was handed on stdin, and answers `ps`
// with psJSON. `down` exits downCode, so a test can choose the cut-short path.
func stubDockerCLI(t *testing.T, psJSON string, downCode int) (argvLog string) {
	return stubDockerCLIOpts(t, psJSON, downCode, "", false)
}

// stubDockerCLIOpts additionally lets a test put text on the stub's STDERR (the
// shape compose warnings take) or make `ps` hang forever.
//
// There is no `rm` hang knob any more: force-removal moved to the Engine API,
// so a hanging daemon is simulated with inspectRecorder.hangRemove instead.
func stubDockerCLIOpts(t *testing.T, psJSON string, downCode int, stderrText string, hangOnPS bool) (argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	psFile := filepath.Join(dir, "ps.json")
	errFile := filepath.Join(dir, "ps.err")
	if err := os.WriteFile(psFile, []byte(psJSON), 0o600); err != nil {
		t.Fatalf("could not seed the stand-in ps output: %v", err)
	}
	if err := os.WriteFile(errFile, []byte(stderrText), 0o600); err != nil {
		t.Fatalf("could not seed the stand-in ps stderr: %v", err)
	}
	hang := "0"
	if hangOnPS {
		hang = "1"
	}
	// Strict POSIX sh: identical under dash and bash. Paths are quoted so a
	// temp dir containing a space cannot break the stub.
	script := `#!/bin/sh
for a in "$@"; do printf 'ARG %s\n' "$a" >> "` + argvLog + `"; done
for a in "$@"; do
  if [ "$a" = "-" ]; then
    printf 'STDIN %s\n' "$(cat)" >> "` + argvLog + `"
    break
  fi
done
for a in "$@"; do
  if [ "$a" = "ps" ]; then
    if [ "` + hang + `" = "1" ]; then while : ; do sleep 1; done; fi
    cat "` + errFile + `" >&2
    cat "` + psFile + `"
    exit 0
  fi
done
for a in "$@"; do
  if [ "$a" = "down" ]; then exit ` + itoa(downCode) + `; fi
done
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("could not write the stand-in docker: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvLog
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

func recordedLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// inspectRecorder answers ContainerInspect as "no such container" so the reap
// barrier returns on its first poll, and records what it was asked about. The
// embedded interface is nil, so any other call panics rather than quietly
// returning a zero value.
type inspectRecorder struct {
	docker.Client
	mu    sync.Mutex
	asked []string
	// removed records every ContainerRemove. Removal moved from `docker rm -f`
	// to the Engine API, so the argv log no longer sees it.
	removed []string
	// listed records the name filters ContainerList was asked about.
	listed []string
	// hangRemove makes ContainerRemove block until its context expires, which
	// is how a saturated daemon presents.
	hangRemove bool
}

func (r *inspectRecorder) ContainerRemove(ctx context.Context, name string, _ container.RemoveOptions) error {
	if r.hangRemove {
		<-ctx.Done()
		return ctx.Err()
	}
	r.mu.Lock()
	r.removed = append(r.removed, name)
	r.mu.Unlock()
	return nil
}

func (r *inspectRecorder) ContainerList(_ context.Context, options container.ListOptions) ([]container.Summary, error) {
	r.mu.Lock()
	r.listed = append(r.listed, options.Filters.Get("name")...)
	r.mu.Unlock()
	return nil, nil
}

// didRemove reports whether the given container was force-removed.
func (r *inspectRecorder) didRemove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.removed {
		if n == name {
			return true
		}
	}
	return false
}

func (r *inspectRecorder) removedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.removed...)
}

func (r *inspectRecorder) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	r.mu.Lock()
	r.asked = append(r.asked, name)
	r.mu.Unlock()
	return container.InspectResponse{}, errors.New("no such container")
}

func (r *inspectRecorder) askedAbout(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.asked {
		if n == name {
			return true
		}
	}
	return false
}

// keployStack is a compose project of keploy's own making: the user's app and
// db services plus the injected agent.
func keployStack(t *testing.T, psJSON string, downCode int) (*App, *inspectRecorder, string) {
	t.Helper()
	argvLog := stubDockerCLI(t, psJSON, downCode)
	rec := &inspectRecorder{}
	a := &App{
		logger:          zap.NewNop(),
		docker:          rec,
		composeFile:     "/tmp/keploy-compose.yaml",
		cmd:             "docker compose -p orderflow up",
		keployContainer: "keploy-v3",
		container:       "user-app",
		composeServices: []string{"app", "db", "keploy-agent"},
	}
	return a, rec, argvLog
}

// TestSweepLeavesOrphansAlone is the guard for a regression that would have
// been worse than the bug it came with.
//
// `docker compose down` removes the services in the compose file it was given
// and deliberately LEAVES orphans — containers in the same project whose
// service is not in that file. The default project name is the working
// directory's basename, so a user who runs
//
//	docker compose -f docker-compose.deps.yml up -d
//	keploy test -c "docker compose up"
//
// has their postgres in the same project as keploy's stack. A sweep that took
// everything compose reports for the project would force-remove it; it is not
// in keploy's compose file, so the next `up` never recreates it and every
// later test-set fails to reach a database.
func TestSweepLeavesOrphansAlone(t *testing.T) {
	ps := `{"ID":"id-app","Service":"app","State":"exited"}
{"ID":"id-db","Service":"db","State":"exited"}
{"ID":"id-user-postgres","Service":"postgres","State":"running"}
`
	a, rec, _ := keployStack(t, ps, 1)

	ids := a.sweepStragglingProjectContainers()

	for _, id := range ids {
		if id == "id-user-postgres" {
			t.Fatal("the sweep claimed a container the user started outside keploy: `postgres` is not one " +
				"of keploy's compose services, and `down` would have left it alone")
		}
	}
	if !rec.didRemove("id-app") {
		t.Fatalf("the sweep did not remove keploy's own leftover container; removed %v", rec.removedIDs())
	}
	if rec.didRemove("id-user-postgres") {
		t.Fatalf("the user's orphan was force-removed; removed %v", rec.removedIDs())
	}
}

// TestSweepIsSkippedWhenTheDownSucceeded keeps the extra docker calls, and the
// risk above, off the healthy path — which is nearly every teardown.
func TestSweepIsSkippedWhenTheDownSucceeded(t *testing.T) {
	ps := `{"ID":"id-app","Service":"app","State":"exited"}` + "\n"
	a, _, argvLog := keployStack(t, ps, 0) // down exits 0

	a.ComposeDown()

	if strings.Contains(recordedLog(t, argvLog), "ARG ps") {
		t.Fatal("ComposeDown swept the project after a down that exited 0; there was nothing left to " +
			"finish, and the sweep is the one step that can touch a user's container")
	}
}

// TestComposeDownSweepsAfterAFailedDown pins the WIRING. Without it the sweep
// can be deleted from ComposeDown and every other test here still passes,
// because they call the helper directly.
func TestComposeDownSweepsAfterAFailedDown(t *testing.T) {
	ps := `{"ID":"id-db","Service":"db","State":"exited"}` + "\n"
	a, rec, argvLog := keployStack(t, ps, 1) // down cut short

	a.ComposeDown()

	log := recordedLog(t, argvLog)
	if !strings.Contains(log, "ARG ps") {
		t.Fatalf("ComposeDown never asked compose what was left standing after a failed down, so the "+
			"next `up` still reuses it.\n%s", log)
	}
	if !rec.didRemove("id-db") {
		t.Fatalf("the leftover container was listed but never removed; removed %v", rec.removedIDs())
	}
	// The original two-name removal must survive alongside the sweep: it is the
	// fallback for when `compose ps` itself fails, which is exactly when the
	// daemon is struggling.
	for _, name := range []string{"keploy-v3", "user-app"} {
		if !rec.didRemove(name) {
			t.Fatalf("the agent/app force-remove was lost (%s missing); removed %v", name, rec.removedIDs())
		}
	}
	// And the reap barrier has to cover the swept ids, not just the two names —
	// otherwise the next `up` races the daemon's async reap of exactly the
	// containers this sweep just removed.
	if !rec.askedAbout("id-db") {
		t.Fatalf("the reap barrier did not wait on the swept container; it waited on %v", rec.asked)
	}
}

// TestSweepUsesTheComposeLibraryForInMemoryProjects covers the in-memory path
// (the enterprise cloud flow), where the compose document is never written to
// disk. That path no longer shells out at all — it drives the compose library —
// so the failure this guards is that the sweep enumerates NOTHING and silently
// becomes a no-op on the very path it was written for.
//
// It also pins that no `docker` binary is invoked: the whole point of the
// library path is that the runner image needs no docker CLI.
func TestSweepUsesTheComposeLibraryForInMemoryProjects(t *testing.T) {
	argvLog := stubDockerCLI(t, "", 0)
	stack := &fakeComposeStack{
		ps: []docker.ServiceState{{ID: "id-app", Service: "app", State: "exited"}},
	}
	a := &App{
		logger:          zap.NewNop(),
		docker:          &inspectRecorder{},
		composeContent:  []byte("services:\n  app:\n    image: alpine\n"),
		cmd:             "docker compose -f - up",
		composeServices: []string{"app"},
		newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
	}

	ids := a.sweepStragglingProjectContainers()

	if stack.psCalls == 0 {
		t.Fatal("the in-memory sweep never asked the compose library what was left standing, " +
			"so it silently removes nothing on the path it exists for")
	}
	if len(ids) != 1 || ids[0] != "id-app" {
		t.Fatalf("sweep returned %v, want [id-app]", ids)
	}
	if log := recordedLog(t, argvLog); strings.Contains(log, "ARG") {
		t.Fatalf("the in-memory sweep shelled out to a `docker` binary; the library path must not.\n%s", log)
	}
}

// TestSweepSkipsComposeRunOneOffsOnTheLibraryPath pins that the label carried
// through the library is still read. A `compose run` container shares the
// service label with a `compose up` one, so without the oneoff label the sweep
// would remove a container that is the user's, not keploy's.
func TestSweepSkipsComposeRunOneOffsOnTheLibraryPath(t *testing.T) {
	stubDockerCLI(t, "", 0)
	stack := &fakeComposeStack{ps: []docker.ServiceState{
		{ID: "id-oneoff", Service: "app", State: "exited",
			Labels: map[string]string{"com.docker.compose.oneoff": "True"}},
		{ID: "id-real", Service: "app", State: "exited"},
	}}
	a := &App{
		logger:          zap.NewNop(),
		docker:          &inspectRecorder{},
		composeContent:  []byte("services:\n  app:\n    image: alpine\n"),
		cmd:             "docker compose -f - up",
		composeServices: []string{"app"},
		newComposeStack: func(context.Context) (composeStack, error) { return stack, nil },
	}

	ids := a.sweepStragglingProjectContainers()

	for _, id := range ids {
		if id == "id-oneoff" {
			t.Fatalf("the sweep removed a `compose run` one-off, which is the user's container: %v", ids)
		}
	}
	if len(ids) != 1 || ids[0] != "id-real" {
		t.Fatalf("sweep returned %v, want [id-real]", ids)
	}
}

// TestSweepWithNoKnownServicesDoesNothing is the safety default: without the
// service list there is no way to tell keploy's containers from the user's, and
// guessing is what the orphan test above forbids.
func TestSweepWithNoKnownServicesDoesNothing(t *testing.T) {
	ps := `{"ID":"id-app","Service":"app","State":"exited"}` + "\n"
	stubDockerCLI(t, ps, 1)
	rec := &inspectRecorder{}
	a := &App{logger: zap.NewNop(), docker: rec, composeFile: "/tmp/c.yaml", cmd: "docker compose up"}

	if ids := a.sweepStragglingProjectContainers(); len(ids) != 0 {
		t.Fatalf("swept %v with no known service list", ids)
	}
	if got := rec.removedIDs(); len(got) != 0 {
		t.Fatalf("containers were force-removed with no way to know which are keploy's: %v", got)
	}
}

// TestForceRemoveContainersPassesEachIDAsItsOwnArgument guards a mistake that
// looks like it works: hand `docker rm -f` a single space-joined string and it
// reports "No such container: <id1> <id2> ...", exits 0, and removes NOTHING,
// while the caller's logs show a removal was attempted.
func TestForceRemoveContainersRemovesEachIDIndividually(t *testing.T) {
	rec := &inspectRecorder{}
	a := &App{logger: zap.NewNop(), docker: rec}

	a.forceRemoveContainers([]string{"id-one", "id-two", "id-three"})

	got := rec.removedIDs()
	sort.Strings(got)
	want := []string{"id-one", "id-three", "id-two"}
	if len(got) != len(want) {
		t.Fatalf("removed %v; want one call per id (%v), not a single joined operand", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("removed %v; want %v", got, want)
		}
	}
}

// TestForceRemoveContainersWithNothingToRemoveDoesNotCallDocker keeps the
// teardown from issuing a `docker rm -f` with no operands, which errors.
func TestForceRemoveContainersWithNothingToRemoveDoesNotCallDocker(t *testing.T) {
	rec := &inspectRecorder{}
	a := &App{logger: zap.NewNop(), docker: rec}

	a.forceRemoveContainers(nil)

	if got := rec.removedIDs(); len(got) != 0 {
		t.Fatalf("the daemon was called with an empty id list: %v", got)
	}
}

// TestComposeServiceNamesReadsEveryService — the sweep's entire scoping depends
// on this list being complete. A service missing from it is a container the
// sweep will not clean up; a service wrongly in it is a user container at risk.
func TestComposeServiceNamesReadsEveryService(t *testing.T) {
	var compose docker.Compose
	src := "services:\n  app:\n    image: a\n  db:\n    image: b\n  keploy-agent:\n    image: c\n"
	if err := yaml.Unmarshal([]byte(src), &compose); err != nil {
		t.Fatalf("could not parse the fixture: %v", err)
	}

	got := composeServiceNames(&compose)
	want := map[string]bool{"app": true, "db": true, "keploy-agent": true}
	if len(got) != len(want) {
		t.Fatalf("composeServiceNames = %v; want the three services", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("composeServiceNames returned an unexpected service %q (%v)", n, got)
		}
	}
}

func TestComposeServiceNamesOnNilIsEmpty(t *testing.T) {
	if got := composeServiceNames(nil); got != nil {
		t.Fatalf("composeServiceNames(nil) = %v; want nil", got)
	}
}

// TestSweepSurvivesComposeWarningsOnStderr is the regression guard for a defect
// that made this entire fix a no-op for a large share of real users, while
// every other test here stayed green.
//
// `docker compose ps` writes warnings to STDERR:
//
//	level=warning msg="The \"TAG\" variable is not set. Defaulting to a blank string."
//	level=warning msg="... the attribute `version` is obsolete ..."
//
// The reader used CombinedOutput, so those lines entered the JSON stream, and
// parseComposeServiceStates bails to nil on its first unparseable line. An
// obsolete `version:` key or one unset ${VAR} was enough — and both survive
// into keploy's generated compose file. The sweep found nothing, every time.
//
// Every other test in this file feeds the stub's stdout clean JSON, which is
// exactly why none of them saw it.
func TestSweepSurvivesComposeWarningsOnStderr(t *testing.T) {
	ps := `{"ID":"id-app","Service":"app","State":"exited"}` + "\n"
	warnings := "level=warning msg=\"The \\\"TAG\\\" variable is not set. Defaulting to a blank string.\"\n" +
		"level=warning msg=\"the attribute `version` is obsolete, it will be ignored\"\n"
	stubDockerCLIOpts(t, ps, 1, warnings, false)
	rec := &inspectRecorder{}
	a := &App{
		logger:          zap.NewNop(),
		docker:          rec,
		composeFile:     "/tmp/keploy-compose.yaml",
		cmd:             "docker compose up",
		composeServices: []string{"app"},
	}

	ids := a.sweepStragglingProjectContainers()

	if len(ids) != 1 || ids[0] != "id-app" {
		t.Fatalf("the sweep found %v; a compose warning on stderr blinded it, so the teardown silently "+
			"leaves every container behind for anyone whose compose file emits one", ids)
	}
	if !rec.didRemove("id-app") {
		t.Fatalf("the leftover container was never removed; removed %v", rec.removedIDs())
	}
}

// TestSweepLeavesComposeRunOneOffsAlone is the same class of mistake as the
// orphan case, one level down. `down` does not remove `compose run` one-offs,
// so neither may the sweep — and the service label cannot tell them apart,
// because keploy's service names ARE the user's service names. A user running
//
//	docker compose run --rm app rake db:migrate
//
// gets a container labelled service=app that is in keploy's own service set.
// The oneoff label is the exact signal; nothing here is a name heuristic.
func TestSweepLeavesComposeRunOneOffsAlone(t *testing.T) {
	ps := `{"ID":"id-app","Service":"app","State":"exited","Labels":"com.docker.compose.oneoff=False,com.docker.compose.service=app"}
{"ID":"id-oneoff","Service":"app","State":"running","Labels":"com.docker.compose.oneoff=True,com.docker.compose.service=app"}
`
	stubDockerCLIOpts(t, ps, 1, "", false)
	rec := &inspectRecorder{}
	a := &App{
		logger:          zap.NewNop(),
		docker:          rec,
		composeFile:     "/tmp/keploy-compose.yaml",
		cmd:             "docker compose up",
		composeServices: []string{"app"},
	}

	ids := a.sweepStragglingProjectContainers()

	for _, id := range ids {
		if id == "id-oneoff" {
			t.Fatal("the sweep claimed a `docker compose run` one-off; `down` leaves those alone, and it " +
				"is the user's container even though it carries one of keploy's service names")
		}
	}
	if len(ids) != 1 || ids[0] != "id-app" {
		t.Fatalf("the sweep should still remove keploy's own leftover; got %v", ids)
	}
	if rec.didRemove("id-oneoff") {
		t.Fatalf("the one-off was force-removed; removed %v", rec.removedIDs())
	}
}

// TestSweepIsBoundedWhenComposeHangs pins the invariant the whole budget const
// block exists for: every docker call on the teardown path is bounded, so the
// app-runner goroutine returns inside DrainErrGroup's 30s budget. An unbounded
// `compose ps` here reintroduces exactly the "a goroutine is ignoring context
// cancellation" failure those budgets were written to remove.
func TestSweepIsBoundedWhenComposeHangs(t *testing.T) {
	stubDockerCLIOpts(t, "", 1, "", true) // `ps` never returns
	a := &App{
		logger:          zap.NewNop(),
		docker:          &inspectRecorder{},
		composeFile:     "/tmp/keploy-compose.yaml",
		cmd:             "docker compose up",
		composeServices: []string{"app"},
	}

	done := make(chan struct{})
	go func() {
		a.sweepStragglingProjectContainers()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(projectSweepBudget + 8*time.Second):
		t.Fatalf("the sweep did not return within its %s budget against a hanging compose; it is "+
			"unbounded on the SIGINT drain path", projectSweepBudget)
	}
}

// TestParseComposeServiceStatesReadsTheOneOffLabel keeps the label plumbing
// honest: if the JSON tag were wrong, every row would read as a non-one-off and
// the guard above would pass while protecting nothing.
func TestParseComposeServiceStatesReadsTheOneOffLabel(t *testing.T) {
	states := parseComposeServiceStates(`{"ID":"x","Service":"app","Labels":"com.docker.compose.oneoff=True"}`)
	if len(states) != 1 {
		t.Fatalf("parsed %d rows; want 1", len(states))
	}
	if !isComposeOneOff(states[0].Labels) {
		t.Fatalf("the oneoff label did not survive parsing: %q", states[0].Labels)
	}
	if isComposeOneOff("com.docker.compose.oneoff=False,com.docker.compose.service=app") {
		t.Fatal("a normal `compose up` container was classified as a one-off")
	}
	if isComposeOneOff("") {
		t.Fatal("a row with no labels was classified as a one-off")
	}
}

// TestForceRemoveContainersIsBoundedWhenDockerHangs is the other half of the
// budget invariant. The const block's comment names BOTH sweep calls — the
// `compose ps` that lists the containers and the single `docker rm -f` that
// removes them — but only the first was pinned. A `docker rm -f` of two dozen
// containers against a wedged daemon is the original unbounded-teardown bug,
// the one the whole budget block was written to remove.
func TestForceRemoveContainersIsBoundedWhenDockerHangs(t *testing.T) {
	a := &App{logger: zap.NewNop(), docker: &inspectRecorder{hangRemove: true}} // removal never returns

	done := make(chan struct{})
	go func() {
		a.forceRemoveContainers([]string{"id-one", "id-two"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(projectSweepBudget + 8*time.Second):
		t.Fatalf("forceRemoveContainers did not return within its %s budget against a hanging docker; "+
			"it is unbounded on the SIGINT drain path", projectSweepBudget)
	}
}

// TestSetComposeSourceKeepsTheServiceListWithTheSource pins the coupling that
// the whole scoping rests on. Both compose setup paths go through this, and
// without the service list the sweep has no way to tell keploy's containers
// from the user's, so it skips — silently, and indistinguishably from a clean
// teardown. That is the failure shape this change has already hit twice.
func TestSetComposeSourceKeepsTheServiceListWithTheSource(t *testing.T) {
	var compose docker.Compose
	src := "services:\n  app:\n    image: a\n  db:\n    image: b\n  keploy-agent:\n    image: c\n"
	if err := yaml.Unmarshal([]byte(src), &compose); err != nil {
		t.Fatalf("could not parse the fixture: %v", err)
	}

	t.Run("file-based", func(t *testing.T) {
		a := &App{logger: zap.NewNop()}
		a.setComposeSource("/tmp/keploy-compose.yaml", nil, &compose)
		if a.composeFile == "" {
			t.Fatal("the compose file was not recorded")
		}
		if len(a.composeServices) != 3 {
			t.Fatalf("composeServices = %v; the sweep cannot scope to keploy's own containers without it, "+
				"so it would skip every teardown", a.composeServices)
		}
	})

	t.Run("in-memory", func(t *testing.T) {
		a := &App{logger: zap.NewNop()}
		a.setComposeSource("", []byte(src), &compose)
		if len(a.composeContent) == 0 {
			t.Fatal("the compose content was not recorded")
		}
		if len(a.composeServices) != 3 {
			t.Fatalf("composeServices = %v on the in-memory path", a.composeServices)
		}
	})

	// The injected agent must be in the set: the sweep is allowed to remove it,
	// which is what pins the "called AFTER ModifyComposeForAgent" ordering.
	a := &App{logger: zap.NewNop()}
	a.setComposeSource("/tmp/c.yaml", nil, &compose)
	var sawAgent bool
	for _, svc := range a.composeServices {
		if svc == "keploy-agent" {
			sawAgent = true
		}
	}
	if !sawAgent {
		t.Fatal("the injected keploy-agent service is not in the set; the service list was captured " +
			"before ModifyComposeForAgent")
	}
}

// fakeComposeStack is a composeStack that answers from canned data, so the
// in-memory compose path keeps unit coverage without a docker daemon.
type fakeComposeStack struct {
	ps       []docker.ServiceState
	psCalls  int
	downCall int
	upErr    error
	// upBlocksUntil, when non-nil, makes Up block until it is closed or the
	// context ends — the shape a foreground `up` has.
	upBlocksUntil chan struct{}
}

func (f *fakeComposeStack) ProjectName() string { return "fake" }

func (f *fakeComposeStack) Up(ctx context.Context, _ docker.ComposeUpOptions, _, _ io.Writer) error {
	if f.upBlocksUntil != nil {
		select {
		case <-f.upBlocksUntil:
		case <-ctx.Done():
		}
	}
	return f.upErr
}

func (f *fakeComposeStack) Down(context.Context, time.Duration) error {
	f.downCall++
	return nil
}

func (f *fakeComposeStack) Ps(context.Context) ([]docker.ServiceState, error) {
	f.psCalls++
	return f.ps, nil
}

func (f *fakeComposeStack) ContainerIDsForService(_ context.Context, service string) ([]string, error) {
	var ids []string
	for _, s := range f.ps {
		if s.Service == service {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
}
