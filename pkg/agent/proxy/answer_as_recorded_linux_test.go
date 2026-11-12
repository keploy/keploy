package proxy

import (
	"os"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
)

// A test worker's variables to answer as recorded are left on its own
// manager, whose next opening of the test takes them: the run's manager is
// left none. A worker no longer served takes none, and says so.
func TestATestWorkersVariablesAnsweredAsRecordedAreItsOwnManagers(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	const slow = "run-appId0-13"
	dest := ipv4Dest(t, "127.0.0.1:50062")
	dest.KernelPid = uint32(os.Getpid())
	p, _ := storeProxy(t, dest)
	run := p.getMockManager()
	w, err := p.ServeInstance(1)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := w.(agent.ExactTestAnswerer)
	if !ok {
		t.Fatal("a test worker's server cannot answer variables as recorded")
	}
	if err := r.AnswerAsRecordedAtOpen("T", []string{slow}); err != nil {
		t.Fatal(err)
	}
	run.OpenExactTest("T", nil, nil)
	if run.vars.AnsweredAsRecorded(slow) {
		t.Fatal("the run's manager took a test worker's variables")
	}
	m := instanceMgrOf(t, p, 1)
	m.OpenExactTest("T", nil, nil)
	if !m.vars.AnsweredAsRecorded(slow) {
		t.Fatal("the worker's own manager did not take its variables")
	}
	if err := p.StopServingInstance(1); err != nil {
		t.Fatal(err)
	}
	if err := r.AnswerAsRecordedAtOpen("T", []string{slow}); err == nil {
		t.Fatal("a worker no longer served took variables to answer as recorded")
	}
}
