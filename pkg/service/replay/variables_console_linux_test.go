//go:build linux

package replay

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A create that answers another id than it sent its dependency is failed on
// the console as in the report: the comparison of its answer, which passes,
// prints no pass line.
func TestRunTestSet_ACaptureMismatchPrintsNoPassLine(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h, instr := createThenRead(t, "u_live_0002")
	h.r.instrumentation = &varInstr{prInstr: instr, bound: func() ([]models.VariableBinding, error) {
		return []models.VariableBinding{{Name: "id", Value: "u_other_0003", Recorded: "u_rec_0001", Mock: "mock-3", Place: "body.id"}}, nil
	}}
	out := captureConsole(t, func() { h.run(t) })
	if strings.Contains(out, `Testrun passed for testcase with id: "test-1"`) {
		t.Fatalf("the console says the failed create passed:\n%s", out)
	}
	if r := h.result(t, "test-1"); r.Status != models.TestStatusFailed {
		t.Fatalf("the create is %s, want FAILED", r.Status)
	}
}
