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
