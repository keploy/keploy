package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/pkg/models"
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
func stubDockerCLIOpts(t *testing.T, psJSON string, downCode int, stderrText string, hangOnPS bool) (argvLog string) {
	return stubDockerCLIHang(t, psJSON, downCode, stderrText, hangOnPS, false)
}

// stubDockerCLIHang additionally lets `rm` hang.
func stubDockerCLIHang(t *testing.T, psJSON string, downCode int, stderrText string, hangOnPS, hangOnRM bool) (argvLog string) {
	return stubDockerCLIFull(t, psJSON, downCode, stderrText, hangOnPS, hangOnRM, "")
}

// stubDockerCLIFull additionally fails ANY invocation whose argv mentions
// failDownWhenArgv — which is how a compose file that cannot be loaded really
// behaves: down, ps and config all exit 1 alike. A narrower call that omits the
// file still succeeds.
func stubDockerCLIFull(t *testing.T, psJSON string, downCode int, stderrText string, hangOnPS, hangOnRM bool, failDownWhenArgv string) (argvLog string) {
	return stubDockerCLIConfig(t, psJSON, downCode, stderrText, hangOnPS, hangOnRM, failDownWhenArgv, "")
}

// stubDockerCLIConfig additionally answers `compose config --services`.
func stubDockerCLIConfig(t *testing.T, psJSON string, downCode int, stderrText string, hangOnPS, hangOnRM bool, failDownWhenArgv, configServices string) (argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	psFile := filepath.Join(dir, "ps.json")
	errFile := filepath.Join(dir, "ps.err")
	cfgFile := filepath.Join(dir, "config.txt")
	if err := os.WriteFile(cfgFile, []byte(configServices), 0o600); err != nil {
		t.Fatalf("could not seed the stand-in config output: %v", err)
	}
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
	hangRM := "0"
	if hangOnRM {
		hangRM = "1"
	}
	// Strict POSIX sh: identical under dash and bash. Paths are quoted so a
	// temp dir containing a space cannot break the stub.
	script := `#!/bin/sh
printf 'RUN\n' >> "` + argvLog + `"
for a in "$@"; do printf 'ARG %s\n' "$a" >> "` + argvLog + `"; done
if [ -n "` + failDownWhenArgv + `" ]; then
  for a in "$@"; do
    if [ "$a" = "` + failDownWhenArgv + `" ]; then exit 1; fi
  done
fi
for a in "$@"; do
  if [ "$a" = "-" ]; then
    printf 'STDIN %s\n' "$(cat)" >> "` + argvLog + `"
    break
  fi
done
for a in "$@"; do
  if [ "$a" = "config" ]; then cat "` + cfgFile + `"; exit 0; fi
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
  if [ "$a" = "rm" ] && [ "` + hangRM + `" = "1" ]; then while : ; do sleep 1; done; fi
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

func recordedArgv(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(recordedLog(t, path), "\n") {
		if strings.HasPrefix(l, "ARG ") {
			out = append(out, strings.TrimPrefix(l, "ARG "))
		}
	}
	return out
}

// inspectRecorder answers ContainerInspect as "no such container" so the reap
// barrier returns on its first poll, and records what it was asked about. The
// embedded interface is nil, so any other call panics rather than quietly
// returning a zero value.
type inspectRecorder struct {
	docker.Client
	mu    sync.Mutex
	asked []string
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
		composeFiles:    []string{"/tmp/keploy-compose.yaml"},
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
	a, _, argvLog := keployStack(t, ps, 1)

	ids := a.sweepStragglingProjectContainers()

	for _, id := range ids {
		if id == "id-user-postgres" {
			t.Fatal("the sweep claimed a container the user started outside keploy: `postgres` is not one " +
				"of keploy's compose services, and `down` would have left it alone")
		}
	}
	if !strings.Contains(recordedLog(t, argvLog), "id-app") {
		t.Fatal("the sweep did not remove keploy's own leftover container")
	}
	if strings.Contains(recordedLog(t, argvLog), "id-user-postgres") {
		t.Fatal("the user's orphan was passed to `docker rm -f`")
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
	if !strings.Contains(log, "id-db") {
		t.Fatalf("the leftover container was listed but never removed.\n%s", log)
	}
	// The original two-name removal must survive alongside the sweep: it is the
	// fallback for when `compose ps` itself fails, which is exactly when the
	// daemon is struggling.
	for _, name := range []string{"keploy-v3", "user-app"} {
		if !strings.Contains(log, name) {
			t.Fatalf("the agent/app force-remove was lost (%s missing).\n%s", name, log)
		}
	}
	// And the reap barrier has to cover the swept ids, not just the two names —
	// otherwise the next `up` races the daemon's async reap of exactly the
	// containers this sweep just removed.
	if !rec.askedAbout("id-db") {
		t.Fatalf("the reap barrier did not wait on the swept container; it waited on %v", rec.asked)
	}
}

// TestSweepSendsTheComposeYAMLOnStdinForInMemoryProjects covers the in-memory
// path (the enterprise cloud flow), where the compose document is piped rather
// than written to disk. `docker compose -f -` with nothing on stdin fails with
// "empty compose file", the sweep logs at Debug and returns nothing, and the
// whole fix is silently a no-op on that path.
func TestSweepSendsTheComposeYAMLOnStdinForInMemoryProjects(t *testing.T) {
	ps := `{"ID":"id-app","Service":"app","State":"exited"}` + "\n"
	argvLog := stubDockerCLI(t, ps, 1)
	a := &App{
		logger:          zap.NewNop(),
		docker:          &inspectRecorder{},
		composeContent:  []byte("services:\n  app:\n    image: alpine\n"),
		cmd:             "docker compose -f - up",
		composeServices: []string{"app"},
	}

	a.sweepStragglingProjectContainers()

	log := recordedLog(t, argvLog)
	if !strings.Contains(log, "ARG -\n") {
		t.Fatalf("the in-memory sweep did not pass `-f -`.\n%s", log)
	}
	if !strings.Contains(log, "STDIN services:") {
		t.Fatalf("the compose YAML never reached the child's stdin, so `docker compose -f -` sees an "+
			"empty compose file and the sweep silently does nothing.\n%s", log)
	}
}

// TestSweepWithNoKnownServicesDoesNothing is the safety default: without the
// service list there is no way to tell keploy's containers from the user's, and
// guessing is what the orphan test above forbids.
func TestSweepWithNoKnownServicesDoesNothing(t *testing.T) {
	ps := `{"ID":"id-app","Service":"app","State":"exited"}` + "\n"
	argvLog := stubDockerCLI(t, ps, 1)
	a := &App{logger: zap.NewNop(), composeFiles: []string{"/tmp/c.yaml"}, cmd: "docker compose up"}

	if ids := a.sweepStragglingProjectContainers(); len(ids) != 0 {
		t.Fatalf("swept %v with no known service list", ids)
	}
	if strings.Contains(recordedLog(t, argvLog), "ARG rm") {
		t.Fatal("docker rm was called with no way to know which containers are keploy's")
	}
}

// TestForceRemoveContainersPassesEachIDAsItsOwnArgument guards a mistake that
// looks like it works: hand `docker rm -f` a single space-joined string and it
// reports "No such container: <id1> <id2> ...", exits 0, and removes NOTHING,
// while the caller's logs show a removal was attempted.
func TestForceRemoveContainersPassesEachIDAsItsOwnArgument(t *testing.T) {
	argvLog := stubDockerCLI(t, "", 0)
	a := &App{logger: zap.NewNop()}

	a.forceRemoveContainers([]string{"id-one", "id-two", "id-three"})

	argv := recordedArgv(t, argvLog)
	if len(argv) != 5 { // rm, -f, and one entry per id
		t.Fatalf("docker received %d arguments (%v); want 5 — one per id, not a joined string", len(argv), argv)
	}
	for i, want := range []string{"rm", "-f", "id-one", "id-two", "id-three"} {
		if argv[i] != want {
			t.Fatalf("argv[%d] = %q; want %q (full argv: %v)", i, argv[i], want, argv)
		}
	}
}

// TestForceRemoveContainersWithNothingToRemoveDoesNotCallDocker keeps the
// teardown from issuing a `docker rm -f` with no operands, which errors.
func TestForceRemoveContainersWithNothingToRemoveDoesNotCallDocker(t *testing.T) {
	argvLog := stubDockerCLI(t, "", 0)
	a := &App{logger: zap.NewNop()}

	a.forceRemoveContainers(nil)

	if recordedLog(t, argvLog) != "" {
		t.Fatal("docker was invoked with an empty id list")
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
	argvLog := stubDockerCLIOpts(t, ps, 1, warnings, false)
	a := &App{
		logger:          zap.NewNop(),
		docker:          &inspectRecorder{},
		composeFiles:    []string{"/tmp/keploy-compose.yaml"},
		cmd:             "docker compose up",
		composeServices: []string{"app"},
	}

	ids := a.sweepStragglingProjectContainers()

	if len(ids) != 1 || ids[0] != "id-app" {
		t.Fatalf("the sweep found %v; a compose warning on stderr blinded it, so the teardown silently "+
			"leaves every container behind for anyone whose compose file emits one", ids)
	}
	if !strings.Contains(recordedLog(t, argvLog), "id-app") {
		t.Fatal("the leftover container was never removed")
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
	argvLog := stubDockerCLIOpts(t, ps, 1, "", false)
	a := &App{
		logger:          zap.NewNop(),
		docker:          &inspectRecorder{},
		composeFiles:    []string{"/tmp/keploy-compose.yaml"},
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
	if strings.Contains(recordedLog(t, argvLog), "id-oneoff") {
		t.Fatal("the one-off was passed to `docker rm -f`")
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
		composeFiles:    []string{"/tmp/keploy-compose.yaml"},
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
	stubDockerCLIHang(t, "", 0, "", false, true) // `rm` never returns
	a := &App{logger: zap.NewNop()}

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
	// compose resolves all three: nothing here is profile-gated.
	stubDockerCLIConfig(t, "", 0, "", false, false, "", "app\ndb\nkeploy-agent\n")
	var compose docker.Compose
	src := "services:\n  app:\n    image: a\n  db:\n    image: b\n  keploy-agent:\n    image: c\n"
	if err := yaml.Unmarshal([]byte(src), &compose); err != nil {
		t.Fatalf("could not parse the fixture: %v", err)
	}

	t.Run("file-based", func(t *testing.T) {
		a := &App{logger: zap.NewNop()}
		a.setComposeSource([]string{"/tmp/keploy-compose.yaml"}, "/tmp/keploy-compose.yaml", nil, composeServiceNames(&compose))
		if len(a.composeFiles) == 0 {
			t.Fatal("the compose file list was not recorded")
		}
		if len(a.composeServices) != 3 {
			t.Fatalf("composeServices = %v; the sweep cannot scope to keploy's own containers without it, "+
				"so it would skip every teardown", a.composeServices)
		}
	})

	t.Run("in-memory", func(t *testing.T) {
		a := &App{logger: zap.NewNop()}
		a.setComposeSource(nil, "", []byte(src), composeServiceNames(&compose))
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
	a.setComposeSource([]string{"/tmp/c.yaml"}, "/tmp/c.yaml", nil, composeServiceNames(&compose))
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

// ---------------------------------------------------------------------------
// multi-file compose commands
// ---------------------------------------------------------------------------

// TestComposeCommandArgsPassesEveryFile is the wiring: every compose call the
// teardown makes must carry all of them, or it resolves a narrower project than
// the `up` did and reads the difference as "already gone".
func TestComposeCommandArgsPassesEveryFile(t *testing.T) {
	a := &App{
		logger:       zap.NewNop(),
		composeFiles: []string{"tmp.yaml", "deps.yml"},
		cmd:          "docker compose -f app.yml -f deps.yml -p orderflow up",
	}

	args := a.composeCommandArgs("ps", "-a")

	// Asserted POSITIONALLY, not with Contains. Compose global flags must come
	// before the subcommand: `docker compose -f c.yml ps -a -p proj` fails with
	// "unknown shorthand flag: 'p' in -p" (verified against compose v5.0.2), so
	// moving the project flags after the tail silently breaks every compose call
	// the teardown makes while a Contains-based assertion stays green.
	want := []string{"compose", "-f", "tmp.yaml", "-f", "deps.yml", "-p", "orderflow", "ps", "-a"}
	if len(args) != len(want) {
		t.Fatalf("compose args = %v; want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("compose args = %v; want %v (position %d differs — compose rejects a global "+
				"flag that appears after the subcommand)", args, want, i)
		}
	}
	// The in-memory source wins and uses stdin.
	a.composeContent = []byte("services: {}")
	if got := strings.Join(a.composeCommandArgs("ps"), " "); !strings.Contains(got, "-f -") {
		t.Fatalf("in-memory source did not use stdin: %v", got)
	}
	// No source at all is a no-op, not a bare `docker compose ps`.
	b := &App{logger: zap.NewNop()}
	if got := b.composeCommandArgs("ps"); got != nil {
		t.Fatalf("a run with no compose source built %v", got)
	}
}

// TestComposeDownTearsDownEveryFile is the end-to-end wiring, and the assertion
// the user-visible bug reduces to: `down` must be given deps.yml, or that
// stack's containers are never removed at all.
func TestComposeDownTearsDownEveryFile(t *testing.T) {
	argvLog := stubDockerCLIOpts(t, "", 0, "", false)
	a := &App{
		logger:          zap.NewNop(),
		docker:          &inspectRecorder{},
		composeFiles:    []string{"tmp.yaml", "deps.yml"},
		cmd:             "docker compose -f app.yml -f deps.yml up",
		keployContainer: "keploy-v3",
		container:       "user-app",
		composeServices: []string{"app", "db"},
	}

	a.ComposeDown()

	log := recordedLog(t, argvLog)
	if !strings.Contains(log, "ARG deps.yml") {
		t.Fatalf("`docker compose down` was not given deps.yml, so that file's containers are orphans "+
			"to it and survive every teardown.\n%s", log)
	}
	if !strings.Contains(log, "ARG tmp.yaml") {
		t.Fatalf("`docker compose down` was not given keploy's generated file.\n%s", log)
	}
}

// composeReader answers ReadComposeFile from a fixture map; any other path is
// an error, standing in for a file that cannot be read.
type composeReader struct {
	docker.Client
	files map[string]string
}

func (r composeReader) ReadComposeFile(path string) (*docker.Compose, error) {
	src, ok := r.files[path]
	if !ok {
		return nil, errors.New("no such compose file")
	}
	var c docker.Compose
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// composeSetupDocker drives the real SetupCompose without touching disk or a daemon.
// It parses the fixture files it is given, reports the app's service out of the
// first one, and no-ops the agent injection and the file write.
type composeSetupDocker struct {
	docker.Client
	files map[string]string
}

func (d composeSetupDocker) parse(t *testing.T, path string) *docker.Compose {
	t.Helper()
	var c docker.Compose
	_ = yaml.Unmarshal([]byte(d.files[path]), &c)
	return &c
}

func (d composeSetupDocker) ReadComposeFile(path string) (*docker.Compose, error) {
	src, ok := d.files[path]
	if !ok {
		return nil, errors.New("no such compose file")
	}
	var c docker.Compose
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (d composeSetupDocker) FindContainerInComposeFiles(paths []string, _ string) (*docker.ComposeServiceInfo, error) {
	// The app always lives in the first file, which is what the fixtures set up.
	c, err := d.ReadComposeFile(paths[0])
	if err != nil {
		return nil, err
	}
	return &docker.ComposeServiceInfo{
		AppServiceName: "app",
		ComposePath:    paths[0],
		Compose:        c,
	}, nil
}

func (composeSetupDocker) ModifyComposeForAgent(c *docker.Compose, _ models.SetupOptions, _ string) error {
	// Stand in for the real injection: add the agent service, since capturing
	// composeServices AFTER this is what puts the agent in the swept set.
	var agent docker.Compose
	if err := yaml.Unmarshal([]byte("services:\n  keploy-agent:\n    image: keploy\n"), &agent); err != nil {
		return err
	}
	c.Services.Content = append(c.Services.Content, agent.Services.Content...)
	return nil
}

func (composeSetupDocker) WriteComposeFile(*docker.Compose, string) error { return nil }

// TestSetupComposeRecordsEveryFileAndService drives the real SetupCompose and
// is the guard for the two mutants that survived everything else: narrowing the
// teardown to keploy's generated file, or to the app file's services alone.
// Both leave every helper test green — the helpers are correct, the CALL is
// what would be wrong — and both silently restore the bug for multi-file users.
func TestSetupComposeRecordsEveryFileAndService(t *testing.T) {
	stubDockerCLIConfig(t, "", 0, "", false, false, "", "app\ndb\ncache\nkeploy-agent\n")
	a := &App{
		logger:    zap.NewNop(),
		container: "user-app",
		cmd:       "docker compose -f app.yml -f deps.yml up",
		docker: composeSetupDocker{files: map[string]string{
			"app.yml":  "services:\n  app:\n    image: a\n",
			"deps.yml": "services:\n  db:\n    image: p\n  cache:\n    image: r\n",
		}},
	}

	if err := a.SetupCompose(nil); err != nil {
		t.Fatalf("SetupCompose: %v", err)
	}

	// Every -f the run uses, with the app's file swapped for the generated copy.
	if len(a.composeFiles) != 2 {
		t.Fatalf("composeFiles = %v; want the generated copy plus deps.yml. Anything less and `down` "+
			"treats the missing file's containers as orphans and never removes them", a.composeFiles)
	}
	if a.composeFiles[0] == "app.yml" {
		t.Fatalf("composeFiles = %v; the app's own file must be replaced by keploy's generated copy, "+
			"or the agent is never started", a.composeFiles)
	}
	if a.composeFiles[1] != "deps.yml" {
		t.Fatalf("composeFiles = %v; deps.yml must be carried through in position", a.composeFiles)
	}

	// And the swept service set must span all of them, plus the injected agent.
	want := map[string]bool{"app": true, "db": true, "cache": true, "keploy-agent": true}
	if len(a.composeServices) != len(want) {
		t.Fatalf("composeServices = %v; want %d services across both files plus the agent. A service "+
			"`down` targets but the sweep does not know about survives a cut-short teardown",
			a.composeServices, len(want))
	}
	for _, s := range a.composeServices {
		if !want[s] {
			t.Fatalf("composeServices has an unexpected entry %q (%v)", s, a.composeServices)
		}
	}
}

// TestSetupComposeWrapperCommandTearsDownOnlyTheGeneratedFile covers the other
// launch plan. A wrapper (`make up`, `./start.sh`) has nothing to splice into,
// so keploy points COMPOSE_FILE at its generated copy ALONE — the run then
// drives exactly one document no matter what the wrapper's own command line
// says. The teardown has to match that, not the user's -f list: driving files
// the run never used makes `down` resolve a different project shape than the
// `up` created.
func TestSetupComposeWrapperCommandTearsDownOnlyTheGeneratedFile(t *testing.T) {
	t.Setenv("COMPOSE_FILE", "") // restored by t.Setenv after SetupCompose overwrites it

	// compose is asked about the generated file alone on this path, so it can
	// only ever answer with the app's own services.
	stubDockerCLIConfig(t, "", 0, "", false, false, "", "app\nkeploy-agent\n")
	a := &App{
		logger:    zap.NewNop(),
		container: "user-app",
		// Not a literal compose command, so composeLaunchPlan takes the
		// COMPOSE_FILE branch. The -f flags here belong to the wrapper, not to
		// the compose invocation keploy ends up driving.
		cmd: "./start.sh -f app.yml -f deps.yml",
		docker: composeSetupDocker{files: map[string]string{
			"app.yml":  "services:\n  app:\n    image: a\n",
			"deps.yml": "services:\n  db:\n    image: p\n",
		}},
	}

	if err := a.SetupCompose(nil); err != nil {
		t.Fatalf("SetupCompose: %v", err)
	}

	if len(a.composeFiles) != 1 {
		t.Fatalf("composeFiles = %v; the wrapper run drives only the generated copy (COMPOSE_FILE "+
			"names one file), so the teardown must too", a.composeFiles)
	}
	for _, svc := range a.composeServices {
		if svc == "db" {
			t.Fatalf("composeServices = %v includes a service from a file this run never drove; the "+
				"sweep would remove a container `down` never targeted", a.composeServices)
		}
	}
}

// ---------------------------------------------------------------------------
// composeSourceForRun: the -f list and the swept services, resolved together
// ---------------------------------------------------------------------------

func sourceApp(t *testing.T, files map[string]string) *App {
	t.Helper()
	return &App{logger: zap.NewNop(), docker: composeReader{files: files}}
}

func parseCompose(t *testing.T, src string) *docker.Compose {
	t.Helper()
	var c docker.Compose
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return &c
}

// TestComposeSourceForRunKeepsEveryFileTheRunUses is the fix for the gap that
// made the sweep moot for multi-file users. modifyDockerComposeCommand replaces
// ONLY the -f holding the app and leaves the rest, so
// `docker compose -f app.yml -f deps.yml up` runs as
// `-f docker-compose-tmp.yaml -f deps.yml`. A teardown given just the generated
// file makes deps.yml's containers ORPHANS to `down` — never removed, on any
// run — and the next `up` reuses them with the same rows.
func TestComposeSourceForRunKeepsEveryFileTheRunUses(t *testing.T) {
	a := sourceApp(t, map[string]string{"deps.yml": "services:\n  db:\n    image: p\n  cache:\n    image: r\n"})
	doc := parseCompose(t, "services:\n  app:\n    image: a\n  keploy-agent:\n    image: k\n")

	files, services := a.composeSourceForRun([]string{"app.yml", "deps.yml"}, "app.yml", "tmp.yaml", doc)

	want := []string{"tmp.yaml", "deps.yml"}
	if len(files) != len(want) || files[0] != want[0] || files[1] != want[1] {
		// Order is not cosmetic: compose merges -f left to right, later files
		// override earlier ones, and the FIRST file decides the project
		// directory — so a reordered list can resolve a different project than
		// the `up` created.
		t.Fatalf("files = %v; want %v", files, want)
	}
	wantSvc := map[string]bool{"app": true, "keploy-agent": true, "db": true, "cache": true}
	if len(services) != len(wantSvc) {
		t.Fatalf("services = %v; want all four — anything missing is a container `down` targets that "+
			"the sweep will not clean up", services)
	}
	for _, n := range services {
		if !wantSvc[n] {
			t.Fatalf("unexpected service %q in %v", n, services)
		}
	}
}

// TestComposeSourceForRunResolvesShellTokens is the regression guard for a
// teardown that did nothing at all. The run goes through `sh -c`, the teardown
// does not, so a quoted or variable -f reached docker literally and every
// compose call exited 1.
func TestComposeSourceForRunResolvesShellTokens(t *testing.T) {
	t.Setenv("COMPOSE_DIR", "/srv")
	a := sourceApp(t, map[string]string{
		"deps.yml":      "services:\n  db:\n    image: p\n",
		"/srv/more.yml": "services:\n  cache:\n    image: r\n",
	})
	doc := parseCompose(t, "services:\n  app:\n    image: a\n")

	files, _ := a.composeSourceForRun(
		[]string{"app.yml", `"deps.yml"`, "$COMPOSE_DIR/more.yml"}, "app.yml", "tmp.yaml", doc)

	for _, f := range files {
		if strings.ContainsAny(f, `"'$`) {
			t.Fatalf("files = %v — %q still carries shell syntax; docker gets no shell and cannot "+
				"open it, so the whole `down` fails and removes nothing", files, f)
		}
	}
	joined := strings.Join(files, " ")
	if !strings.Contains(joined, "deps.yml") || !strings.Contains(joined, "/srv/more.yml") {
		t.Fatalf("files = %v; both resolved paths must survive", files)
	}
}

// TestComposeSourceForRunSingleQuotesSuppressExpansion follows sh's own rules
// rather than stripping everything: inside single quotes $VAR is literal.
// Over-expanding invents a path the run never used.
func TestComposeSourceForRunSingleQuotesSuppressExpansion(t *testing.T) {
	t.Setenv("NOPE", "/expanded")
	if got := resolveShellToken(`'$NOPE/x.yml'`); got != "$NOPE/x.yml" {
		t.Fatalf("resolveShellToken single-quoted = %q; sh does not expand there", got)
	}
	if got := resolveShellToken(`"$NOPE/x.yml"`); got != "/expanded/x.yml" {
		t.Fatalf("resolveShellToken double-quoted = %q; sh DOES expand there", got)
	}
}

// TestComposeSourceForRunDropsAnUnreadableFileFromBOTH is the consistency the
// one-pass design exists for. Skipping a file when building the service set but
// still handing it to docker as -f makes `down` fail outright and tear down
// NOTHING — strictly worse than the under-coverage the skip was claimed to be.
func TestComposeSourceForRunDropsAnUnreadableFileFromBOTH(t *testing.T) {
	a := sourceApp(t, map[string]string{}) // every extra file is unreadable
	doc := parseCompose(t, "services:\n  app:\n    image: a\n")

	files, services := a.composeSourceForRun([]string{"app.yml", "gone.yml"}, "app.yml", "tmp.yaml", doc)

	for _, f := range files {
		if strings.Contains(f, "gone.yml") {
			t.Fatalf("files = %v still contains a file that cannot be read; every compose call then "+
				"exits 1 and the teardown removes nothing", files)
		}
	}
	if len(services) != 1 || services[0] != "app" {
		t.Fatalf("services = %v; the app's own services must survive an unreadable extra file", services)
	}
}

// TestComposeSourceForRunSwapsOnlyTheFirstAppFileOccurrence mirrors
// modifyDockerComposeCommand, which uses strings.Replace with a count of 1.
func TestComposeSourceForRunSwapsOnlyTheFirstAppFileOccurrence(t *testing.T) {
	a := sourceApp(t, map[string]string{"app.yml": "services:\n  app:\n    image: a\n"})
	doc := parseCompose(t, "services:\n  app:\n    image: a\n")

	files, _ := a.composeSourceForRun([]string{"app.yml", "app.yml"}, "app.yml", "tmp.yaml", doc)

	if len(files) != 2 || files[0] != "tmp.yaml" || files[1] != "app.yml" {
		t.Fatalf("files = %v; the run swaps only the first occurrence, so anything else makes the "+
			"teardown drive a different list than the up", files)
	}
}

// TestComposeSourceForRunWithNoExplicitFlags covers the discovery path, where
// the app's file came from a default filename rather than a -f.
func TestComposeSourceForRunWithNoExplicitFlags(t *testing.T) {
	a := sourceApp(t, map[string]string{})
	doc := parseCompose(t, "services:\n  app:\n    image: a\n")

	for _, paths := range [][]string{nil, {"other.yml"}} {
		files, services := a.composeSourceForRun(paths, "docker-compose.yml", "tmp.yaml", doc)
		if len(files) != 1 || files[0] != "tmp.yaml" {
			t.Fatalf("files = %v for paths %v; want just the generated copy", files, paths)
		}
		if len(services) != 1 || services[0] != "app" {
			t.Fatalf("services = %v for paths %v", services, paths)
		}
	}
}

// TestFailedFullListDownStillSweeps is the guard for a fix that would have been
// a regression against the bug this file exists to close.
//
// When the full -f list cannot be loaded, the teardown retries with keploy's
// own generated file so the agent and app names are freed. That narrow retry
// targets a SUBSET on purpose — so it must NOT count as "the down succeeded".
// Letting it set that flag skips the sweep and abandons precisely the
// dependency stragglers the next `up` would then reuse (#4614).
func TestFailedFullListDownStillSweeps(t *testing.T) {
	ps := `{"ID":"id-db","Service":"db","State":"exited","Labels":"com.docker.compose.oneoff=False"}` + "\n"
	// `down` fails whenever deps.yml is in argv; the narrow retry (generated
	// file only) therefore succeeds.
	argvLog := stubDockerCLIFull(t, ps, 0, "", false, false, "deps.yml")
	a := &App{
		logger:           zap.NewNop(),
		docker:           &inspectRecorder{},
		composeFiles:     []string{"tmp.yaml", "deps.yml"},
		composeGenerated: "tmp.yaml",
		cmd:              "docker compose -f app.yml -f deps.yml up",
		keployContainer:  "keploy-v3",
		container:        "user-app",
		composeServices:  []string{"app", "db"},
	}

	a.ComposeDown()

	log := recordedLog(t, argvLog)
	if !strings.Contains(log, "ARG ps") {
		t.Fatalf("the full-list down failed and no sweep followed; the narrow retry was treated as a "+
			"successful teardown, so the dependency containers it never targeted are left for the "+
			"next `up` to reuse.\n%s", log)
	}
	if !strings.Contains(log, "id-db") {
		t.Fatalf("the straggler was listed but never removed.\n%s", log)
	}
}

// TestNarrowRetryFreesTheAgentAndAppNames is the other half: the retry is worth
// keeping. With the full list unusable, keploy's own file can still be torn
// down, which is what stops the next `up` hitting "container name already in
// use" on the agent.
func TestNarrowRetryFreesTheAgentAndAppNames(t *testing.T) {
	argvLog := stubDockerCLIFull(t, "", 0, "", false, false, "deps.yml")
	a := &App{
		logger:           zap.NewNop(),
		docker:           &inspectRecorder{},
		composeFiles:     []string{"tmp.yaml", "deps.yml"},
		composeGenerated: "tmp.yaml",
		cmd:              "docker compose -f app.yml -f deps.yml up",
		keployContainer:  "keploy-v3",
		container:        "user-app",
		composeServices:  []string{"app"},
	}

	a.ComposeDown()

	// Asserted as a DISTINCT invocation: a `down` whose argv never mentions
	// deps.yml. Merely checking that tmp.yaml appears somewhere is satisfied by
	// the full-list down, so deleting the whole retry block would pass.
	var sawNarrowDown bool
	for _, inv := range recordedInvocations(t, argvLog) {
		isDown, mentionsDeps := false, false
		for _, tok := range inv {
			if tok == "down" {
				isDown = true
			}
			if tok == "deps.yml" {
				mentionsDeps = true
			}
		}
		if isDown && !mentionsDeps {
			sawNarrowDown = true
		}
	}
	if !sawNarrowDown {
		t.Fatal("no fallback `down` on keploy's own file alone; with the full list unusable the agent " +
			"and app container names are never freed, and the next `up` fails on a name conflict")
	}
}

// TestSweepNeverTouchesAServiceComposeDownLeavesAlone is the guard against the
// only failure direction that destroys a user's data.
//
// `docker compose down` leaves a service behind an unenabled `profiles:`
// standing. Keploy's own YAML walk lists it anyway — and cannot even see the
// cases where the profile arrives via a merge key or `extends:`, because
// yaml.v3 does not expand `<<` into a yaml.Node and never resolves `extends:`.
// `ps` carries no profile label either. So the swept set is intersected with
// compose's own profile-resolved answer.
func TestSweepNeverTouchesAServiceComposeDownLeavesAlone(t *testing.T) {
	ps := `{"ID":"id-app","Service":"app","State":"exited","Labels":"com.docker.compose.oneoff=False"}
{"ID":"id-db","Service":"db","State":"exited","Labels":"com.docker.compose.oneoff=False"}
{"ID":"id-debugtool","Service":"debugtool","State":"running","Labels":"com.docker.compose.oneoff=False"}
`
	// compose resolves the project WITHOUT the debug profile, so `down` leaves
	// debugtool alone — even though the raw YAML keys include it.
	argvLog := stubDockerCLIConfig(t, ps, 1, "", false, false, "", "app\ndb\nkeploy-agent\n")
	a := &App{logger: zap.NewNop(), docker: &inspectRecorder{},
		cmd: "docker compose up", keployContainer: "keploy-v3", container: "user-app"}
	a.setComposeSource([]string{"tmp.yaml"}, "tmp.yaml", nil,
		[]string{"app", "db", "debugtool", "keploy-agent"})

	a.ComposeDown()

	log := recordedLog(t, argvLog)
	if strings.Contains(log, "id-debugtool") {
		t.Fatalf("the sweep removed a profile-gated container that `down` deliberately left running — "+
			"the user started it with --profile and keploy destroyed it.\n%s", log)
	}
	if !strings.Contains(log, "id-app") || !strings.Contains(log, "id-db") {
		t.Fatalf("the sweep stopped removing keploy's own leftovers.\n%s", log)
	}
}

// TestComposeServicesEmptyWhenComposeCannotAnswer pins the degradation
// direction: no answer means no sweep, which is the behaviour from before the
// sweep existed — never a guess.
func TestComposeServicesEmptyWhenComposeCannotAnswer(t *testing.T) {
	ps := `{"ID":"id-app","Service":"app","State":"exited","Labels":"com.docker.compose.oneoff=False"}` + "\n"
	// config exits non-zero (empty output + the stub's `config` branch returns
	// it verbatim, so an empty list stands in for "could not answer").
	argvLog := stubDockerCLIConfig(t, ps, 1, "", false, false, "", "")
	a := &App{logger: zap.NewNop(), docker: &inspectRecorder{},
		cmd: "docker compose up", keployContainer: "keploy-v3", container: "user-app"}
	a.setComposeSource([]string{"tmp.yaml"}, "tmp.yaml", nil, []string{"app"})

	if len(a.composeServices) != 0 {
		t.Fatalf("composeServices = %v; with no answer from compose there is no way to tell keploy's "+
			"containers from the user's, and guessing is what destroys data", a.composeServices)
	}
	a.ComposeDown()
	if strings.Contains(recordedLog(t, argvLog), "id-app") {
		t.Fatal("the sweep ran without a resolved service list")
	}
}

// TestIntersectServicesDropsWhatComposeDoesNotList is the pure unit, including
// the stale-generated-file shape: if compose's answer is disjoint from what
// keploy parsed, the result is empty rather than compose's list.
func TestIntersectServicesDropsWhatComposeDoesNotList(t *testing.T) {
	for _, tc := range []struct {
		name             string
		static, resolved []string
		want             int
	}{
		{"profile-gated service excluded", []string{"app", "debugtool"}, []string{"app"}, 1},
		{"compose could not answer", []string{"app"}, nil, 0},
		{"keploy parsed nothing", nil, []string{"app"}, 0},
		{"stale generated file: disjoint", []string{"old"}, []string{"app"}, 0},
		{"empty profiles key is ungated and kept", []string{"app", "blank"}, []string{"app", "blank"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := intersectServices(tc.static, tc.resolved); len(got) != tc.want {
				t.Fatalf("intersectServices(%v, %v) = %v; want %d entries", tc.static, tc.resolved, got, tc.want)
			}
		})
	}
}

// TestSweepNeverTouchesAServiceKeployNeverParsed is the other half of the
// intersection, and the reason it is an intersection rather than "prefer
// compose's answer".
//
// Compose reads whatever is on disk NOW. If its answer contains a service
// keploy never parsed — a stale docker-compose-tmp.yaml left by a killed run,
// something pulled in via `include:` — taking compose's list wholesale would
// sweep a container keploy has no claim to. Bounding by keploy's own keys as
// well makes under-collection the failure direction in every branch.
func TestSweepNeverTouchesAServiceKeployNeverParsed(t *testing.T) {
	ps := `{"ID":"id-app","Service":"app","State":"exited","Labels":"com.docker.compose.oneoff=False"}
{"ID":"id-ghost","Service":"ghost","State":"running","Labels":"com.docker.compose.oneoff=False"}
`
	// compose lists a service keploy's own document does not define.
	argvLog := stubDockerCLIConfig(t, ps, 1, "", false, false, "", "app\nghost\n")
	a := &App{logger: zap.NewNop(), docker: &inspectRecorder{},
		cmd: "docker compose up", keployContainer: "keploy-v3", container: "user-app"}
	a.setComposeSource([]string{"tmp.yaml"}, "tmp.yaml", nil, []string{"app"})

	a.ComposeDown()

	log := recordedLog(t, argvLog)
	if strings.Contains(log, "id-ghost") {
		t.Fatalf("the sweep removed a container for a service keploy never parsed — compose's answer "+
			"was taken wholesale instead of being bounded by keploy's own files.\n%s", log)
	}
	if !strings.Contains(log, "id-app") {
		t.Fatalf("the sweep stopped removing keploy's own leftover.\n%s", log)
	}
}

// recordedInvocations splits the stand-in docker's argv log into one slice per
// invocation. The stub writes a RUN marker before each call's arguments.
func recordedInvocations(t *testing.T, path string) [][]string {
	t.Helper()
	var out [][]string
	var cur []string
	started := false
	for _, l := range strings.Split(recordedLog(t, path), "\n") {
		switch {
		case l == "RUN":
			if started {
				out = append(out, cur)
			}
			cur, started = nil, true
		case strings.HasPrefix(l, "ARG "):
			cur = append(cur, strings.TrimPrefix(l, "ARG "))
		}
	}
	if started {
		out = append(out, cur)
	}
	return out
}

// TestProjectQueriesSurviveAnUnloadableUserFile is the guard for a regression
// that took out three separate protections at once.
//
// Passing the user's full -f list to the `ps` queries means any file compose
// cannot load — an unset ${VAR} in an interpolation, a file the app command
// writes later, a schema stricter than keploy's parser — makes them exit 1. On
// main those queries ran on keploy's generated file alone, which always loads.
// The narrow retry covers only `down`, so the fallout was: the sweep blind, the
// stale-agent guard a no-op before every `up`, and the transient-dependency
// classifier reading nil as "not transient" and disabling the bring-up retry.
//
// The extra files buy these queries nothing anyway: `compose ps` is
// PROJECT-scoped, so keploy's own file alone still lists every container in the
// project, including services defined only in the user's other files (measured
// on compose v5.0.2).
func TestProjectQueriesSurviveAnUnloadableUserFile(t *testing.T) {
	ps := `{"ID":"id-db","Service":"db","State":"exited","Labels":"com.docker.compose.oneoff=False"}` + "\n"
	// The stub fails ANY invocation whose argv mentions the broken file —
	// down, ps and config alike, which is what a real compose does.
	argvLog := stubDockerCLIFull(t, ps, 0, "", false, false, "broken.yml")
	a := &App{
		logger:           zap.NewNop(),
		docker:           &inspectRecorder{},
		composeFiles:     []string{"tmp.yaml", "broken.yml"},
		composeGenerated: "tmp.yaml",
		cmd:              "docker compose -f app.yml -f broken.yml up",
		keployContainer:  "keploy-v3",
		container:        "user-app",
		composeServices:  []string{"app", "db"},
	}

	a.ComposeDown()

	for _, inv := range recordedInvocations(t, argvLog) {
		isPS, mentionsBroken := false, false
		for _, tok := range inv {
			if tok == "ps" {
				isPS = true
			}
			if tok == "broken.yml" {
				mentionsBroken = true
			}
		}
		if isPS && mentionsBroken {
			t.Fatalf("a `ps` query was given the unloadable user file: %v — it exits 1, and with it go "+
				"the sweep, the stale-agent guard and the dependency-failure classifier", inv)
		}
	}
	if !strings.Contains(recordedLog(t, argvLog), "id-db") {
		t.Fatal("the sweep found nothing although the project query should have succeeded on keploy's " +
			"own file; the straggler is left for the next `up` to reuse")
	}
}

// TestComposeServicesFallBackToKeploysOwnFile covers the state between "all
// good" and "no sweep at all". If the FULL -f list cannot be resolved — an
// unset ${VAR}, an --env-file the command does not carry, a file written later
// — asking about keploy's own file alone still yields a usable set. It covers
// fewer services, so the sweep under-collects, which is the safe direction and
// much better than disabling it for the whole run.
func TestComposeServicesFallBackToKeploysOwnFile(t *testing.T) {
	// Any call naming deps.yml fails; the narrow one succeeds.
	stubDockerCLIConfig(t, "", 0, "", false, false, "deps.yml", "app\nkeploy-agent\n")
	a := &App{logger: zap.NewNop(), docker: &inspectRecorder{},
		cmd: "docker compose -f app.yml -f deps.yml up"}

	a.setComposeSource([]string{"tmp.yaml", "deps.yml"}, "tmp.yaml", nil,
		[]string{"app", "db", "keploy-agent"})

	if len(a.composeServices) == 0 {
		t.Fatal("the sweep was disabled for the whole run because ONE of the user's files could not " +
			"be resolved; keploy's own file could still have answered")
	}
	for _, svc := range a.composeServices {
		if svc == "db" {
			t.Fatalf("composeServices = %v includes a service only the unresolvable file defines; the "+
				"fallback must under-collect, never guess", a.composeServices)
		}
	}
}

// TestComposeCommandArgsCarriesEnvFile pins --env-file. It is pure
// model-loading: without it a compose file using ${VAR} interpolation loads for
// the `up` and fails for every teardown call, which takes out the sweep, the
// stale-agent guard and the dependency-failure classifier together.
//
// --profile must NOT be carried: it would widen what `down` targets, and the
// teardown must never remove a container `down` would have left alone.
func TestComposeCommandArgsCarriesEnvFile(t *testing.T) {
	t.Setenv("ENVDIR", "/cfg")
	a := &App{
		logger:       zap.NewNop(),
		composeFiles: []string{"tmp.yaml"},
		cmd:          `docker compose --env-file "$ENVDIR/prod.env" --profile debug -f app.yml up`,
	}

	joined := strings.Join(a.composeCommandArgs("config", "--services"), " ")

	if !strings.Contains(joined, "--env-file /cfg/prod.env") {
		t.Fatalf("args = %q; --env-file must be carried AND shell-resolved, or every teardown call "+
			"fails to interpolate and three separate guards go blind at once", joined)
	}
	if strings.Contains(joined, "--profile") {
		t.Fatalf("args = %q; --profile must NOT be carried — it widens what `down` targets, and the "+
			"sweep may never remove a container `down` would have spared", joined)
	}
}
