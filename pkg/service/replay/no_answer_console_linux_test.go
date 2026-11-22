//go:build linux

package replay

import (
	"errors"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A test case that got no answer is FAILED on its own, and its row is built
// by comparing a stand-in answer with the recording (CreateFailedTestResult).
// Assertions that judge the test case in place of that comparison may pass
// the stand-in (an assertion keploy does not know passes any answer): the
// console then printed "Testrun passed" for the FAILED test.
func TestRunTestSet_ATestWithNoAnswerPrintsNoPassLine(t *testing.T) {
	setSwitch(t, false)
	h := newPartialRunHarness(t, 1, 0)
	h.cases[0].Assertions = map[models.AssertionType]interface{}{"json_path": "$.id"}
	h.replayer.hookImpl = prHooks{simErr: map[string]error{"test-1": errors.New("connection refused")}}

	console := captureConsole(t, func() { h.run(t) })
	if strings.Contains(console, "Testrun passed") {
		t.Fatalf("the console printed the pass line for a test that got no answer:\n%s", console)
	}
	if r := resultOf(t, h, "test-1"); r.Status != models.TestStatusFailed {
		t.Fatalf("test-1 is %s, want FAILED: it got no answer", r.Status)
	}
}
