package record

import (
	"context"
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// watchThenFinal runs the watch over the reads in order: the base, then one
// per step, where a step is a test case's check (its name) or the final read
// (""), each made before the next. It returns whether the recording fails,
// and its ERRORs.
func watchThenFinal(t *testing.T, instr *countingInstr, steps ...string) (bool, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	for _, s := range steps {
		if s == "" {
			w.afterTestCases(context.Background())
		} else {
			w.afterTestCase(context.Background(), s)
		}
		settle(w) // each read made before the next step
	}
	return w.finish("test-set-0", false), logs
}

const afterLast = "after the last test case was recorded, and no test case holds"

// Under the proven switch what the agent lost after the last test case was
// checked is charged to no test case: no test case's mocks miss it, so the
// final read, once the app has stopped, is what says it, and the recording
// fails (design v14 §7.5: evidence charged to no test). Before, nothing read
// the count after the last test case, and such a recording passed.
func TestARecordingThatLostACaptureAfterItsLastTestCaseFails(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")

	// Nothing lost while the test cases ran; 2 after.
	failed, logs := watchThenFinal(t, &countingInstr{answers: []models.CaptureLosses{pid(1), pid(1), pid(1), pid(3)}}, "test-1", "test-2", "")
	if !failed || logs.FilterMessageSnippet(afterLast).Len() != 1 {
		t.Fatalf("failed %v, %d ERRORs naming a loss after the last test case; want the recording failed with one", failed, logs.FilterMessageSnippet(afterLast).Len())
	}

	// A test case already charged with an earlier loss, and another after.
	failed, logs = watchThenFinal(t, &countingInstr{answers: []models.CaptureLosses{pid(1), pid(1), pid(3), pid(5)}}, "test-1", "test-2", "")
	if !failed || logs.FilterMessageSnippet("while these test cases were recorded").Len() != 1 || logs.FilterMessageSnippet(afterLast).Len() != 1 {
		t.Fatalf("failed %v; want test-2 tainted and the later loss said as no test case's", failed)
	}

	// Nothing lost after the last test case: only the test cases' own.
	failed, logs = watchThenFinal(t, &countingInstr{answers: []models.CaptureLosses{pid(1), pid(1), pid(3), pid(3)}}, "test-1", "test-2", "")
	if !failed || logs.FilterMessageSnippet(afterLast).Len() != 0 {
		t.Fatalf("failed %v, %d ERRORs naming a loss after the last test case; want none", failed, logs.FilterMessageSnippet(afterLast).Len())
	}

	// A test case the drain saves after the final read is charged with what
	// it read: the final read's excess over it is no one's.
	failed, logs = watchThenFinal(t, &countingInstr{answers: []models.CaptureLosses{pid(1), pid(1), pid(3), pid(3)}}, "test-1", "", "test-2")
	if !failed || logs.FilterMessageSnippet(afterLast).Len() != 0 {
		t.Fatalf("failed %v, %d ERRORs naming a loss after the last test case; want test-2's loss said only as test-2's", failed, logs.FilterMessageSnippet(afterLast).Len())
	}

	// Nothing lost at all: the final read changes nothing.
	failed, logs = watchThenFinal(t, &countingInstr{answers: []models.CaptureLosses{pid(1)}}, "test-1", "")
	if failed || logs.Len() != 0 {
		t.Fatalf("failed %v, %d ERRORs; want a clean recording", failed, logs.Len())
	}
}

// A final read that fails leaves what was lost after the last test case
// unknown: the recording is not verified, and fails.
func TestARecordingWhoseFinalCountCannotBeReadIsNotVerified(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	failed, logs := watchThenFinal(t, &countingInstr{answers: []models.CaptureLosses{pid(1)}, errAt: map[int]error{3: errors.New("connection reset")}}, "test-1", "")
	if !failed || logs.FilterMessageSnippet("after the last test case was recorded").Len() != 1 {
		t.Fatalf("failed %v; want the recording not verified for its final read", failed)
	}
}
