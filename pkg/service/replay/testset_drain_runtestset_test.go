package replay

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestRunTestSetDrainThatGivesUpLeavesTheWatcherAlone is an application that
// is still stopping when the test set's teardown drain gives up on it. The
// drain gives up with the application's goroutine still running, after the
// app's watcher has sent on the set's exit channel.
//
// The teardown used to close that channel after the drain. With the drain
// timed out nothing orders the close after the watcher's send, so the race
// detector reports the two, as it did in a run of the express-mongoose lane's
// race build, which then exits 66; and a watcher that sent after the close
// would panic. This test fails under -race with the close, and the go-test
// workflow runs it under -race.
//
// The stand-in application does not stop until the test releases it, which it
// does only after RunTestSet has returned. So the drain gives up every time,
// however late the scheduler runs any goroutine here: the case under test
// does not depend on a margin of wall-clock time.
func TestRunTestSetDrainThatGivesUpLeavesTheWatcherAlone(t *testing.T) {
	prev := testSetDrainTimeout
	testSetDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { testSetDrainTimeout = prev })

	h := newPartialRunHarness(t, 2, 0)
	release := make(chan struct{})
	stopped := make(chan struct{})
	h.instr.release = release
	h.instr.stopped = stopped
	// Whatever happens below, the stand-in application is released, and its
	// goroutine ends, inside this test.
	t.Cleanup(func() {
		close(release)
		<-stopped
	})

	status := h.run(t)

	// The precondition, which the release makes hold: RunTestSet returned
	// while the application was still stopping, so the drain did give up.
	// It fails only if the harness lets the application stop on its own.
	select {
	case <-stopped:
		t.Fatalf("the application had stopped before RunTestSet returned, so the drain never timed out; the case under test did not occur")
	default:
	}

	if status != models.TestSetStatusPassed {
		t.Fatalf("a set whose every test passed reported %q after its drain timed out; want PASSED", status)
	}
	if got := len(h.report.results); got != 2 {
		t.Fatalf("recorded %d test results; want 2", got)
	}
}
