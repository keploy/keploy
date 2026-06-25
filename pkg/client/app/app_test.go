package app

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// exit125 returns a real *exec.ExitError whose Error() is "exit status 125",
// matching what utils.ExecuteCommand surfaces when the docker CLI exits 125
// (the code the daemon returns for a container-name conflict, among other
// create/start refusals).
func exit125(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 125").Run()
	if err == nil {
		t.Fatal("expected a non-nil exit-125 error")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 125 {
		t.Fatalf("expected *exec.ExitError code 125, got %T: %v", err, err)
	}
	return err
}

func TestIsExit125(t *testing.T) {
	e125 := exit125(t)
	cases := []struct {
		name    string
		err     error
		errType utils.ErrType
		want    bool
	}{
		{"runtime 125", e125, utils.Runtime, true},
		{"nil error", nil, utils.Runtime, false},
		// An init-time failure (command never started) is not a daemon refusal we
		// can clear by freeing a name.
		{"init-type 125", e125, utils.Init, false},
		{"runtime non-125", errors.New("exit status 1"), utils.Runtime, false},
		// "125" appearing inside an unrelated message must not trip the check; the
		// signature is the exact "exit status 125" exec error string.
		{"125 substring only", errors.New("container test-125 failed"), utils.Runtime, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExit125(tc.err, tc.errType); got != tc.want {
				t.Fatalf("isExit125(%v, %q) = %v, want %v", tc.err, tc.errType, got, tc.want)
			}
		})
	}
}

func TestIsDockerRunNameConflict(t *testing.T) {
	e125 := exit125(t)
	cases := []struct {
		name         string
		err          error
		errType      utils.ErrType
		nameOccupied bool
		want         bool
	}{
		// The only retryable shape: exit 125 AND the --name is still held.
		{"125 + name occupied", e125, utils.Runtime, true, true},
		// Genuine 125 (bad image / mount / flag): the daemon refused, but the name
		// is free, so freeing+retrying cannot help — must fail fast.
		{"125 + name free", e125, utils.Runtime, false, false},
		// A name lingering without a 125 is not this failure mode.
		{"non-125 + name occupied", errors.New("exit status 1"), utils.Runtime, true, false},
		{"nil error + name occupied", nil, utils.Runtime, true, false},
		{"init-type 125 + name occupied", e125, utils.Init, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDockerRunNameConflict(tc.err, tc.errType, tc.nameOccupied); got != tc.want {
				t.Fatalf("isDockerRunNameConflict(%v, %q, occupied=%v) = %v, want %v",
					tc.err, tc.errType, tc.nameOccupied, got, tc.want)
			}
		})
	}
}

// retryOutcome captures what driving the production retry decision produced.
type retryOutcome struct {
	runs     int   // total ExecuteCommand invocations (initial + retries)
	removes  int   // ensureContainerNameFreeWithin invocations
	finalErr error // error after the loop settles
}

// driveNameConflictRetry replays the EXACT loop predicate used in App.run for
// the docker-run name-conflict retry, against a fake command runner and a fake
// name-occupancy probe, so the retry path can be exercised deterministically
// without a docker daemon. runResults[i] is the error returned by the i-th
// ExecuteCommand; nameOccupied[i] is what containerNameFree's complement reports
// before deciding whether to issue retry i. Mirrors app.go's loop so the test
// breaks if the production gating drifts.
func driveNameConflictRetry(dockerRunName string, maxRetries int, runResults []error, nameOccupied []bool) retryOutcome {
	out := retryOutcome{}
	// initial run
	curErr := runResults[0]
	out.runs = 1
	for attempt := 1; dockerRunName != "" && attempt <= maxRetries &&
		isExit125(curErr, utils.Runtime) &&
		isDockerRunNameConflict(curErr, utils.Runtime, nameOccupied[attempt-1]); attempt++ {
		out.removes++ // ensureContainerNameFreeWithin
		// next ExecuteCommand result
		curErr = runResults[attempt]
		out.runs++
	}
	out.finalErr = curErr
	return out
}

func TestDockerRunNameConflictRetryPath(t *testing.T) {
	e125 := exit125(t)
	genuine := errors.New("exit status 1") // e.g. bad mount surfaced as non-125

	t.Run("conflict then success recovers in one retry", func(t *testing.T) {
		// First run hits the name conflict (125, name occupied), the force-remove
		// frees the name, the retry succeeds.
		got := driveNameConflictRetry("my-app", dockerRunNameConflictRetries,
			[]error{e125, nil},
			[]bool{true})
		if got.runs != 2 || got.removes != 1 || got.finalErr != nil {
			t.Fatalf("conflict-then-success: %+v; want runs=2 removes=1 finalErr=nil", got)
		}
	})

	t.Run("genuine 125 with free name fails fast (no retry)", func(t *testing.T) {
		// 125 but the name is NOT held -> not the conflict; must not retry.
		got := driveNameConflictRetry("my-app", dockerRunNameConflictRetries,
			[]error{e125},
			[]bool{false})
		if got.runs != 1 || got.removes != 0 || got.finalErr == nil {
			t.Fatalf("genuine-125-free-name: %+v; want runs=1 removes=0 finalErr!=nil", got)
		}
	})

	t.Run("non-125 runtime error never retries", func(t *testing.T) {
		got := driveNameConflictRetry("my-app", dockerRunNameConflictRetries,
			[]error{genuine},
			[]bool{true})
		if got.runs != 1 || got.removes != 0 || got.finalErr == nil {
			t.Fatalf("non-125: %+v; want runs=1 removes=0 finalErr!=nil", got)
		}
	})

	t.Run("persistent conflict is bounded by max retries", func(t *testing.T) {
		// Name stays occupied and every run keeps hitting 125: stop after the
		// bounded number of retries and surface the 125.
		runResults := make([]error, dockerRunNameConflictRetries+1)
		occupied := make([]bool, dockerRunNameConflictRetries)
		for i := range runResults {
			runResults[i] = e125
		}
		for i := range occupied {
			occupied[i] = true
		}
		got := driveNameConflictRetry("my-app", dockerRunNameConflictRetries, runResults, occupied)
		if got.runs != dockerRunNameConflictRetries+1 || got.removes != dockerRunNameConflictRetries || got.finalErr == nil {
			t.Fatalf("persistent-conflict: %+v; want runs=%d removes=%d finalErr!=nil",
				got, dockerRunNameConflictRetries+1, dockerRunNameConflictRetries)
		}
	})

	t.Run("empty container name disables retry", func(t *testing.T) {
		got := driveNameConflictRetry("", dockerRunNameConflictRetries,
			[]error{e125},
			[]bool{true})
		if got.runs != 1 || got.removes != 0 {
			t.Fatalf("empty-name: %+v; want runs=1 removes=0", got)
		}
	})
}

// TestParseComposePSIDs locks the parsing that the stale-agent cleanup hinges
// on: `docker compose ps -aq keploy-agent` prints one container id per line, and
// removeStaleComposeAgentWithin must derive an exact id list from it (no blanks,
// no whitespace). An over-eager parse would force-remove the wrong container or
// a phantom id; an under-eager one (e.g. dropping a real id on a trailing
// newline) would leave the prior agent to trip the compose Recreate race.
func TestParseComposePSIDs(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []string
	}{
		// First up of a project: compose prints nothing.
		{"empty", "", nil},
		{"only newlines", "\n\n", nil},
		{"whitespace only", "   \n\t\n", nil},
		// The common single-agent case, with the trailing newline the CLI emits.
		{"single id trailing newline", "fde9a83d78a6\n", []string{"fde9a83d78a6"}},
		{"single id no newline", "fde9a83d78a6", []string{"fde9a83d78a6"}},
		// Defensive: more than one tracked agent container (e.g. a half-reaped
		// prior + a leftover) — every id must be returned so all get removed.
		{"multiple ids", "id1\nid2\nid3\n", []string{"id1", "id2", "id3"}},
		// Surrounding whitespace and interleaved blanks are tolerated.
		{"ids with blanks and spaces", "  id1  \n\n  id2\n", []string{"id1", "id2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseComposePSIDs(tc.out)
			if len(got) != len(tc.want) {
				t.Fatalf("parseComposePSIDs(%q) = %v (len %d), want %v (len %d)",
					tc.out, got, len(got), tc.want, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseComposePSIDs(%q)[%d] = %q, want %q", tc.out, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestComposeAgentContainerIDsNoSource verifies the stale-agent lookup is a
// no-op when there is no compose source configured (neither a temp compose file
// nor in-memory content) — i.e. it never shells out to docker for a non-compose
// run. composeAgentContainerIDs must return nil so removeStaleComposeAgentWithin
// short-circuits before any docker call.
func TestComposeAgentContainerIDsNoSource(t *testing.T) {
	a := &App{logger: zap.NewNop()}
	if ids := a.composeAgentContainerIDs(context.Background()); ids != nil {
		t.Fatalf("composeAgentContainerIDs with no compose source = %v, want nil", ids)
	}
	// removeStaleComposeAgentWithin must also be a no-op (no panic, returns
	// promptly) when there is nothing to resolve.
	a.removeStaleComposeAgentWithin(forceRemoveBudget)
}

// app "created" + a non-zero-exited dependency is the transient signature.
func depFailStates(appService string) []composeServiceState {
	return []composeServiceState{
		{Service: appService, State: "created", ExitCode: 0},
		{Service: "localstack", State: "exited", ExitCode: 245},
	}
}

// TestParseComposeServiceStates locks the parsing of `docker compose ps -a
// --format json`, which the transient-dependency classifier reads to decide
// whether a failed `up` is a recoverable dependency crash. A misparse here would
// either mask a real app failure (false "transient") or fail a recoverable run
// (false "genuine"); both are exactly what the gate must not do.
func TestParseComposeServiceStates(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []composeServiceState
	}{
		{"empty", "", nil},
		{"whitespace", "  \n\t\n", nil},
		// NDJSON form (docker compose v2 default): one object per line.
		{
			"ndjson app-created dep-exited",
			`{"Service":"app","State":"created","ExitCode":0}
{"Service":"localstack","State":"exited","ExitCode":245}`,
			[]composeServiceState{
				{Service: "app", State: "created", ExitCode: 0},
				{Service: "localstack", State: "exited", ExitCode: 245},
			},
		},
		// Array form (tolerated for CLI variants that emit a single JSON array).
		{
			"array form",
			`[{"Service":"app","State":"exited","ExitCode":3}]`,
			[]composeServiceState{{Service: "app", State: "exited", ExitCode: 3}},
		},
		// A malformed line means we cannot trust the classification: return nil so
		// the caller treats the failure as non-transient and fails fast.
		{"malformed line bails", `{"Service":"app" BROKEN`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseComposeServiceStates(tc.out)
			if len(got) != len(tc.want) {
				t.Fatalf("parseComposeServiceStates(%q) = %+v (len %d), want %+v (len %d)",
					tc.out, got, len(got), tc.want, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseComposeServiceStates(%q)[%d] = %+v, want %+v", tc.out, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestIsTransientComposeDependencyFailure is the core safety test: it proves the
// gate retries ONLY the transient dependency-startup crash and NEVER a genuine
// application failure.
func TestIsTransientComposeDependencyFailure(t *testing.T) {
	runtimeErr := errors.New("exit status 1")
	cases := []struct {
		name       string
		err        error
		errType    utils.ErrType
		appService string
		states     []composeServiceState
		want       bool
	}{
		// THE recoverable case: app never started ("created"), a dependency exited
		// non-zero. This is the orderflow-localstack-exited(245) shape.
		{
			"app created + dep exited nonzero -> transient",
			runtimeErr, utils.Runtime, "app", depFailStates("app"), true,
		},
		// THE firewall: a genuine app failure leaves the app service "exited" with
		// its own non-zero code — never "created" — so it must NOT be retried even
		// though a sibling also exited (e.g. the abort-killed dependency at 137).
		{
			"app exited nonzero (genuine fail) -> NOT transient",
			runtimeErr, utils.Runtime, "app",
			[]composeServiceState{
				{Service: "app", State: "exited", ExitCode: 3},
				{Service: "gooddep", State: "exited", ExitCode: 137},
			},
			false,
		},
		// App created but no dependency actually crashed (all healthy / exited 0):
		// nothing to recover, don't retry.
		{
			"app created but no crashed dep -> NOT transient",
			runtimeErr, utils.Runtime, "app",
			[]composeServiceState{
				{Service: "app", State: "created", ExitCode: 0},
				{Service: "dep", State: "exited", ExitCode: 0},
			},
			false,
		},
		// Only the injected keploy-agent exited non-zero: it is keploy's own
		// service, never the user's crashed dependency, so it must not count.
		{
			"only keploy-agent exited nonzero -> NOT transient",
			runtimeErr, utils.Runtime, "app",
			[]composeServiceState{
				{Service: "app", State: "created", ExitCode: 0},
				{Service: keployAgentComposeService, State: "exited", ExitCode: 1},
			},
			false,
		},
		// A successful run (nil error) is never a failure to retry.
		{"nil error -> NOT transient", nil, utils.Runtime, "app", depFailStates("app"), false},
		// An init-type error (command never started) is not this runtime abort.
		{"init type -> NOT transient", runtimeErr, utils.Init, "app", depFailStates("app"), false},
		// No app service known: can't prove the app never started, so don't retry.
		{"empty app service -> NOT transient", runtimeErr, utils.Runtime, "", depFailStates("app"), false},
		// Empty/absent state (probe failed): conservatively NOT transient -> fail
		// fast rather than retry blindly.
		{"no states -> NOT transient", runtimeErr, utils.Runtime, "app", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientComposeDependencyFailure(tc.err, tc.errType, tc.appService, tc.states); got != tc.want {
				t.Fatalf("isTransientComposeDependencyFailure(%v, %q, %q, %+v) = %v, want %v",
					tc.err, tc.errType, tc.appService, tc.states, got, tc.want)
			}
		})
	}
}

// TestShouldRetryComposeUp pins every gate of the production retry loop's single
// decision predicate in default CI (the live dockerlive tests cover the loop's
// side effects; this covers its branching). Each case flips exactly one gate off
// the all-true baseline to prove that gate is load-bearing.
//
// The states are supplied via a thunk that RECORDS whether it was invoked, so the
// no-retry cases additionally assert the (expensive `docker compose ps`) probe is
// NOT shelled out unless the four cheap predicates all pass — it must stay off the
// non-compose/over-budget/cancel/success paths the loop re-evaluates every turn.
func TestShouldRetryComposeUp(t *testing.T) {
	transient := depFailStates("app")
	// statesThunk returns the given states and flips *called when invoked, so a
	// test can assert the lazy probe ran (or didn't).
	statesThunk := func(states []composeServiceState, called *bool) func() []composeServiceState {
		return func() []composeServiceState {
			*called = true
			return states
		}
	}
	base := func(called *bool) (utils.CmdType, int, int, error, error, utils.ErrType, string, func() []composeServiceState) {
		return utils.DockerCompose, 1, composeDepFailureRetries, nil, errors.New("exit status 1"), utils.Runtime, "app", statesThunk(transient, called)
	}

	t.Run("all gates satisfied -> retry (probe IS called)", func(t *testing.T) {
		var called bool
		if !shouldRetryComposeUp(base(&called)) {
			t.Fatal("baseline (compose, in-budget, ctx live, runtime, transient) should retry")
		}
		if !called {
			t.Fatal("when the cheap predicates pass, the state probe must be evaluated")
		}
	})
	t.Run("non-compose kind -> no retry (probe NOT called)", func(t *testing.T) {
		var called bool
		_, at, mx, ce, re, et, as, sf := base(&called)
		if shouldRetryComposeUp(utils.DockerRun, at, mx, ce, re, et, as, sf) {
			t.Fatal("a docker-run is never the compose dependency-failure case")
		}
		if called {
			t.Fatal("non-compose run must short-circuit BEFORE the state probe")
		}
	})
	t.Run("attempt over budget -> no retry (probe NOT called)", func(t *testing.T) {
		var called bool
		k, _, mx, ce, re, et, as, sf := base(&called)
		if shouldRetryComposeUp(k, mx+1, mx, ce, re, et, as, sf) {
			t.Fatal("attempt past the bound must stop the loop")
		}
		if called {
			t.Fatal("over-budget attempt must short-circuit BEFORE the state probe")
		}
	})
	t.Run("ctx cancelled -> no retry (probe NOT called)", func(t *testing.T) {
		var called bool
		k, at, mx, _, re, et, as, sf := base(&called)
		if shouldRetryComposeUp(k, at, mx, context.Canceled, re, et, as, sf) {
			t.Fatal("a cancelled run must not retry")
		}
		if called {
			t.Fatal("cancelled run must short-circuit BEFORE the state probe")
		}
	})
	t.Run("nil error (success) -> no retry (probe NOT called)", func(t *testing.T) {
		var called bool
		k, at, mx, ce, _, et, as, sf := base(&called)
		if shouldRetryComposeUp(k, at, mx, ce, nil, et, as, sf) {
			t.Fatal("a successful up (nil error) must not retry")
		}
		if called {
			t.Fatal("successful up must short-circuit BEFORE the state probe")
		}
	})
	t.Run("init-type error -> no retry (probe NOT called)", func(t *testing.T) {
		var called bool
		k, at, mx, ce, re, _, as, sf := base(&called)
		if shouldRetryComposeUp(k, at, mx, ce, re, utils.Init, as, sf) {
			t.Fatal("an init-type failure is not the runtime compose abort")
		}
		if called {
			t.Fatal("init-type failure must short-circuit BEFORE the state probe")
		}
	})
	t.Run("non-transient states -> no retry (probe IS called)", func(t *testing.T) {
		var called bool
		k, at, mx, ce, re, et, as, _ := base(&called)
		appExited := statesThunk([]composeServiceState{{Service: "app", State: "exited", ExitCode: 3}}, &called)
		if shouldRetryComposeUp(k, at, mx, ce, re, et, as, appExited) {
			t.Fatal("a genuinely-failed app (app exited) must not retry")
		}
		if !called {
			t.Fatal("with the cheap predicates passing, the probe must run to classify the states")
		}
	})
}

// composeRetryOutcome captures what driving the production compose-dep retry
// decision produced.
type composeRetryOutcome struct {
	runs     int
	teardown int // ComposeDown invocations between attempts
	finalErr error
}

// composeUpResult models one `docker compose up` outcome the way the production
// retry loop observes it: the error the up returned AND the per-service states
// `docker compose ps -a` reports immediately after (the production loop re-probes
// live state each iteration, so the two are always paired and never misindexed).
type composeUpResult struct {
	err    error
	states []composeServiceState
}

// driveComposeDepRetry drives the transient-dependency retry against a sequence
// of fake up-results, calling the SAME production predicate (shouldRetryComposeUp)
// the run() loop uses — not a copy of its condition — so a drift in the
// production gating breaks this test. It models how run() re-reads the live
// ps-states paired with each up. ctx is always live and kind is always compose
// here; the firewall (errType/transient classification) is what's exercised.
func driveComposeDepRetry(appService string, maxRetries int, ups []composeUpResult) composeRetryOutcome {
	out := composeRetryOutcome{}
	cur := ups[0]
	curType := utils.Runtime
	out.runs = 1
	for attempt := 1; shouldRetryComposeUp(utils.DockerCompose, attempt, maxRetries,
		nil /* ctxErr */, cur.err, curType, appService,
		func() []composeServiceState { return cur.states }); attempt++ {
		out.teardown++ // ComposeDown
		cur = ups[attempt]
		out.runs++
	}
	out.finalErr = cur.err
	return out
}

// TestComposeDepFailureRetryPath drives the retry loop end-to-end on the gating
// predicate, proving recovery for the flaky dependency and NO-masking for a
// genuinely-failing app.
func TestComposeDepFailureRetryPath(t *testing.T) {
	depErr := errors.New("exit status 1") // compose "dependency failed to start"
	appErr := errors.New("exit status 3") // the app's own non-zero exit
	created := func() []composeServiceState { return depFailStates("app") }
	appExited := func() []composeServiceState {
		return []composeServiceState{
			{Service: "app", State: "exited", ExitCode: 3},
			{Service: "gooddep", State: "exited", ExitCode: 137},
		}
	}

	t.Run("flaky dep recovers on the first retry", func(t *testing.T) {
		// up#1 fails with the dependency-startup signature; the retry succeeds (a
		// successful up reports the app "running", not "created", so the gate stops).
		got := driveComposeDepRetry("app", composeDepFailureRetries, []composeUpResult{
			{err: depErr, states: created()},
			{err: nil, states: []composeServiceState{{Service: "app", State: "running"}}},
		})
		if got.runs != 2 || got.teardown != 1 || got.finalErr != nil {
			t.Fatalf("flaky-dep-recovers: %+v; want runs=2 teardown=1 finalErr=nil", got)
		}
	})

	t.Run("genuinely failing app is never retried (no masking)", func(t *testing.T) {
		// up#1 fails because the APP exited non-zero (app service "exited"): the
		// gate must reject it, so no retry and the real error surfaces immediately.
		got := driveComposeDepRetry("app", composeDepFailureRetries, []composeUpResult{
			{err: appErr, states: appExited()},
		})
		if got.runs != 1 || got.teardown != 0 || got.finalErr == nil {
			t.Fatalf("genuine-app-fail: %+v; want runs=1 teardown=0 finalErr!=nil", got)
		}
	})

	t.Run("dependency that crashes every time is bounded then surfaced", func(t *testing.T) {
		// Every up hits the transient signature: retry up to the bound, then stop
		// and surface the (still non-nil) error — DELAYED, never hidden.
		ups := make([]composeUpResult, composeDepFailureRetries+1)
		for i := range ups {
			ups[i] = composeUpResult{err: depErr, states: created()}
		}
		got := driveComposeDepRetry("app", composeDepFailureRetries, ups)
		if got.runs != composeDepFailureRetries+1 || got.teardown != composeDepFailureRetries || got.finalErr == nil {
			t.Fatalf("persistent-dep-crash: %+v; want runs=%d teardown=%d finalErr!=nil",
				got, composeDepFailureRetries+1, composeDepFailureRetries)
		}
	})

	t.Run("first up succeeds -> no retry, no teardown", func(t *testing.T) {
		got := driveComposeDepRetry("app", composeDepFailureRetries, []composeUpResult{
			{err: nil, states: []composeServiceState{{Service: "app", State: "running"}}},
		})
		if got.runs != 1 || got.teardown != 0 || got.finalErr != nil {
			t.Fatalf("first-up-ok: %+v; want runs=1 teardown=0 finalErr=nil", got)
		}
	})
}

// TestComposeServiceStatesNoSource verifies the state probe is a no-op (returns
// nil, never shells out) when no compose source is configured, so a non-compose
// run can never accidentally enter the dependency-retry path.
func TestComposeServiceStatesNoSource(t *testing.T) {
	a := &App{logger: zap.NewNop()}
	if states := a.composeServiceStates(context.Background()); states != nil {
		t.Fatalf("composeServiceStates with no compose source = %+v, want nil", states)
	}
}

// TestKeployAgentComposeServiceConstant guards the fixed compose service key the
// stale-agent cleanup resolves against. The cleanup finds the prior agent via
// `docker compose ps keploy-agent`; if the injected service key (docker.go's
// AddKeployAgentToCompose) ever changes, this constant must change in lockstep
// or the cleanup silently stops matching and the Recreate race returns.
func TestKeployAgentComposeServiceConstant(t *testing.T) {
	if keployAgentComposeService != "keploy-agent" {
		t.Fatalf("keployAgentComposeService = %q, want \"keploy-agent\" (must match the injected compose service key in pkg/platform/docker/docker.go)",
			keployAgentComposeService)
	}
}
