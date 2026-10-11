package replay

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// misses answers the harness's per-test GetMockErrors calls in test order:
// reads[i] is what the agent hands back for the (i+1)th test.
func misses(reads ...func() ([]models.UnmatchedCall, error)) func() ([]models.UnmatchedCall, error) {
	var mu sync.Mutex
	n := 0
	return func() ([]models.UnmatchedCall, error) {
		mu.Lock()
		defer mu.Unlock()
		i := n
		n++
		if i < len(reads) && reads[i] != nil {
			return reads[i]()
		}
		return nil, nil
	}
}

func incompleteRead() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /read"}},
		fmt.Errorf("%w (agent: keploy's error drain stopped)", models.ErrUnmatchedCallsUnread)
}

func failedRead() ([]models.UnmatchedCall, error) {
	return nil, errors.New("get mock errors returned status 500")
}

// A test whose misses could not all be read has no verdict under the switch,
// and neither has its set: INTERNAL_ERR, with the tests named. With the switch
// off the run is what it was (the failure is a Debug line, the set PASSED).
func TestRunTestSet_UnreadMissesLeaveTheSetUnjudged(t *testing.T) {
	for _, read := range []struct {
		name string
		fn   func() ([]models.UnmatchedCall, error)
	}{{"incomplete read", incompleteRead}, {"failed read", failedRead}} {
		for _, on := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/switch=%v", read.name, on), func(t *testing.T) {
				if on {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "")
				}
				h := newPartialRunHarness(t, 3, 0)
				h.instr.mockErrorsFor = misses(nil, read.fn, nil)

				status := h.run(t)

				var unread []string
				for _, r := range h.report.report.Tests {
					if r.FailureInfo.UnmatchedCallsUnread {
						unread = append(unread, r.TestCaseID)
					}
				}
				if !on {
					if status != models.TestSetStatusPassed || len(unread) != 0 {
						t.Fatalf("switch off: status %s, unread %v; want PASSED and none, as before", status, unread)
					}
					return
				}
				if status != models.TestSetStatusInternalErr {
					t.Fatalf("status %s; a set with a test whose misses were not all read must be neither PASSED nor FAILED", status)
				}
				if len(unread) != 1 || unread[0] != "test-2" {
					t.Fatalf("unread %v, want [test-2]", unread)
				}
				if reason := h.report.report.FailureReason; !strings.Contains(reason, "not judged") || !strings.Contains(reason, "test-2") {
					t.Fatalf("failure reason %q does not say the set is not judged, and for which test", reason)
				}
			})
		}
	}
}

// What an incomplete read did return is still attached to its test.
func TestAttachMockErrors_KeepsWhatAnIncompleteReadReturned(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 1, 0)
	h.instr.mockErrorsFor = misses(incompleteRead)
	result := &models.TestResult{}
	h.replayer.attachMockErrors(t.Context(), "test-set-0", "test-1", result)
	if !result.FailureInfo.UnmatchedCallsUnread {
		t.Fatal("the result is not marked unread")
	}
	if len(result.FailureInfo.UnmatchedCalls) != 1 || result.FailureInfo.UnmatchedCalls[0].ActualSummary != "GET /read" {
		t.Fatalf("unmatched calls %+v; the calls the read did return must be kept", result.FailureInfo.UnmatchedCalls)
	}
}
