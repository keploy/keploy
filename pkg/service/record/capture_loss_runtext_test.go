package record

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// What keploy record's agent lost after the last test case, which no test
// case holds, is said in the run's words (models.CaptureLosses.RunText) on
// every line that says it: the ERROR for a loss that fails the recording,
// and the WARNs for a handshake abandoned, data relayed and runs the kernel
// skipped then. Before, those lines' reasons said "while this test or one
// before it ran" and "so keploy cannot prove this test's calls were all
// captured", of a loss no test case carried.
func TestWhatARecordingLostAfterItsLastTestCaseIsSaidInTheRunsWords(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	late := models.CaptureLosses{
		{Kind: models.CaptureLossPIDMap, Count: 2, Err: -7},
		{Kind: models.CaptureLossTLSAbandoned, Count: 1, What: "to api:443"},
		{Kind: models.CaptureLossServerFirst, Count: 1, What: "to mail:25"},
		{Kind: models.CaptureLossKernelSkip, Count: 3},
	}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), &countingInstr{answers: []models.CaptureLosses{nil, nil, late}})
	w.afterTestCase(context.Background(), "test-1")
	w.afterTestCases(context.Background())
	if !w.finish("test-set-0", false) {
		t.Fatal("a PID map's refusal after the last test case did not fail the recording")
	}
	said := map[string]string{}
	for _, e := range logs.FilterMessageSnippet("after the last test case").All() {
		reason, _ := e.ContextMap()["reason"].(string)
		for _, word := range []string{"this test", "before it", "test's calls"} {
			if strings.Contains(reason, word) {
				t.Errorf("%q says %q of a loss no test case carried: %s", e.Message, word, reason)
			}
		}
		said[e.Level.String()] += reason + "\n"
	}
	for _, want := range []string{
		"the agent's PID map target_namespace_pids refused 2 insert(s) (E2BIG",
		"A map keeps one entry for each live process",
		"(to api:443)",
		"(to mail:25)",
		"list the server-first destination in keploy.yml mock.real",
		"the kernel skipped 3 capture run(s)",
	} {
		if !strings.Contains(said["error"]+said["warn"], want) {
			t.Errorf("no line said %q; said:\n%s%s", want, said["error"], said["warn"])
		}
	}
	if !strings.Contains(said["error"], "target_namespace_pids") {
		t.Errorf("the ERROR does not say the refusal: %s", said["error"])
	}
}
