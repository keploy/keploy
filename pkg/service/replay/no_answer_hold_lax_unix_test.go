//go:build unix

package replay

import (
	"bytes"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// strictWindowOffEnv names, in the test binary run again, the row it runs with
// KEPLOY_STRICT_MOCK_WINDOW=0, which the process reads once as it starts.
const strictWindowOffEnv = "KEPLOY_NO_ANSWER_HOLD_STRICT_WINDOW_OFF"

// withStrictWindowOff runs the calling test again, alone, with
// KEPLOY_STRICT_MOCK_WINDOW=0, and fails it if it did not pass there: it
// returns true in that run, which does the test's work, and false in the first.
func withStrictWindowOff(t *testing.T) bool {
	t.Helper()
	if os.Getenv(strictWindowOffEnv) == t.Name() {
		return true
	}
	var out bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), strictWindowOffEnv+"="+t.Name(), "KEPLOY_STRICT_MOCK_WINDOW=0")
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("run the row with the strict window switched off: %v", err)
	}
	// A row that dies early leaves nothing behind.
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	err := cmd.Wait()
	t.Logf("with KEPLOY_STRICT_MOCK_WINDOW=0:\n%s", out.String())
	if err != nil || !bytes.Contains(out.Bytes(), []byte("--- PASS: "+t.Name())) {
		t.Fatalf("the row did not pass with the strict window switched off: %v", err)
	}
	return false
}

// With KEPLOY_STRICT_MOCK_WINDOW=0 the agent's window filter and its mock
// manager are lax whatever the config says: an unstated per-test mock out of
// the window, or in flight when it began, is promoted to the session pool, and
// the mock manager handed one past the filter (the lookahead's door) files it
// in its session tree as the filter promotes it (setMocksWithWindowLocked's
// sessionIn), by the same serve decision (pkg.StageInWindow). A stated mock
// keeps its strict rules. The hold follows the agent: it keeps what the agent
// serves the unanswered test's window as its own, and of the promoted ones,
// those recorded in the window (the stated limit of lax mode).
func TestHoldNoAnswerMocksFollowsTheAgentWithTheStrictWindowSwitchedOff(t *testing.T) {
	if !withStrictWindowOff(t) {
		return
	}
	if pkg.IsStrictMockWindow(true) {
		t.Fatal("precondition: KEPLOY_STRICT_MOCK_WINDOW=0 switches the strict window off")
	}
	registerNoAnswerCarryOver(t)
	cases, at := holdTableSet()
	grpc := func(op string, s float64) *models.Mock { return noAnswerHoldMock(models.GRPC_EXPORT, op, at(s)) }
	http := func(op string, s float64) *models.Mock { return noAnswerHoldMock(models.HTTP, op, at(s)) }
	answered := func(m *models.Mock, s float64) *models.Mock {
		m.Spec.ResTimestampMock = at(s)
		return m
	}
	// The config asks for the strict window; the switch overrides it.
	const (
		strictMode = true
		perTest    = false
		session    = true
	)
	for _, tc := range []holdRow{
		{"placed in the window", grpc("Window", 71), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"an unstated call in the gap after the window (promoted)", grpc("After", 75), perTest, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, false},
		{"an unstated call in flight when the window began (promoted)", answered(grpc("InFlight", 69.5), 70.5), perTest, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, false},
		{"an unstated carry-over call in the gap after the window (promoted)", grpc(noAnswerCarryOverOp, 73), perTest, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, false},
		{"a stated call in the gap after the window", statedPerTest(http("After", 75)), perTest, strictMode, pkg.NotServed, pkg.NotServed, pkg.NotServed, false},
		{"a stated call in flight when the window began", answered(statedPerTest(http("InFlight", 69.5)), 70.5), perTest, strictMode, pkg.ServedPerTest, pkg.ServedPerTest, pkg.ServedPerTest, true},
		{"a server push in the gap after the window", grpc(noAnswerPushOp, 73), perTest, strictMode, pkg.ServedReleased, pkg.ServedReleased, pkg.ServedReleased, true},
		{"a session-pool mock in the window", http("Shared", 71), session, strictMode, pkg.ServedSession, pkg.ServedSession, pkg.ServedSession, true},
	} {
		t.Run(tc.name, func(t *testing.T) { checkHoldRow(t, cases, tc, byWindow) })
	}
}

// KEPLOY_STRICT_MOCK_WINDOW=0 is set for the replay's process only: sudo drops
// it on the way to a native agent, a docker agent's container is not given
// it, and a k8s-proxy agent runs in its own pod. The agent runs the strict
// window and says so (models.MockWindowHeader): the hold keeps what that
// agent serves the unanswered test-7, the unstated carry-over call its
// lookahead releases first at test-7's window and the unstated call in flight
// when the window began, and not what the replay's own env would say. An agent
// that has not said is taken for the mode that holds the most.
func TestRunTestSetHoldsByTheAgentsWindowModeNotTheReplaysEnv(t *testing.T) {
	if !withStrictWindowOff(t) {
		return
	}
	if pkg.IsStrictMockWindow(true) {
		t.Fatal("precondition: KEPLOY_STRICT_MOCK_WINDOW=0 switches the strict window off in this process")
	}
	for _, row := range []struct {
		name string
		said models.MockWindowsSaid
		ok   bool
	}{
		{"the agent says it ran the strict window", models.MockWindowsSaid{Strict: true, Lookahead: true}, true},
		{"the agent has not said", models.MockWindowsSaid{}, false},
	} {
		for _, format := range []string{"yaml", "gob"} {
			t.Run(row.name+"/"+format, func(t *testing.T) {
				left, set, carry := holdByWhatTheAgentSaid(t, format, true, row.said, row.ok)
				if !left[carry.Name] {
					t.Errorf("the unstated carry-over call the agent's lookahead releases at test-7's window (%s) was pruned; keeps %v", carry.Name, sortedNames(left))
				}
				if !left[set.inFlight.Name] {
					t.Errorf("the unstated call in flight when test-7's window began (%s), which the strict agent serves it, was pruned; keeps %v", set.inFlight.Name, sortedNames(left))
				}
			})
		}
	}
}
