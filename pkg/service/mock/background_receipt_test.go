package mock

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A test's background misses (ScopeEvidence.Background: calls of its window
// that are the same request as one of the set's background recordings, with
// none left to answer them) fail nothing, and reach its receipt line all the
// same: a window whose evidence holds only them is kept, each attempt's are
// kept, and the per-test file keeps them as it keeps a miss, each call in
// its persisted form. The line stays no evidence against the test (Empty).
// An attempt kept for its background misses alone gives the test no reason,
// so a later attempt's reason is still the test's.
// Mutation: skip a window whose evidence fails nothing, keep one attempt's,
// drop them from the per-test file or keep their pid and command line there,
// or take the reason from the first kept attempt only.
func TestATestsBackgroundMissesReachItsReceipt(t *testing.T) {
	bg := func(path string) models.UnmatchedCall {
		return models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET " + path, Destination: "127.0.0.1:18094",
			ClosestMock: "mock-0", Background: true, NextSteps: "declare it in keploy.yaml",
			Source: &models.Origin{Comm: "rates.test", Exe: "/tmp/go-build/rates.test", PID: 4242, Cmdline: "rates.test -test.run X", Unnamed: "pid_namespace"}}
	}
	got := withEvidence([]TestReceipt{{Name: "o::A", Outcome: "failed"}, {Name: "o::B", Outcome: "passed"}}, []models.ScopeWindow{
		{Name: "o::A", Evidence: &models.ScopeEvidence{Background: []models.UnmatchedCall{bg("/rates")}}},
		{Name: "o::A", Evidence: &models.ScopeEvidence{Background: []models.UnmatchedCall{bg("/fx")}}},
		{Name: "o::B"},
	})
	require.NotNil(t, got[0].Evidence, "a window whose evidence holds only background misses was dropped")
	require.Equal(t, []models.UnmatchedCall{bg("/rates"), bg("/fx")}, got[0].Evidence.Background, "each attempt's background misses")
	require.True(t, got[0].Evidence.Empty(), "background misses are evidence against the test")
	require.Empty(t, got[0].Evidence.Misses)
	require.Nil(t, got[1].Evidence)

	r := persistedTests(TestsReceipt{Set: "s", Tests: got})
	require.Len(t, r.Tests[0].Evidence.Background, 2, "the per-test file does not keep the background misses")
	for _, c := range r.Tests[0].Evidence.Background {
		require.NotNil(t, c.Source)
		require.Equal(t, models.Origin{Comm: "rates.test", Exe: "/tmp/go-build/rates.test"}, *c.Source, "the per-test file keeps what only travels (pid, command line)")
	}
	require.Equal(t, uint32(4242), got[0].Evidence.Background[0].Source.PID, "the receipt it was made from changed")

	// An attempt kept for its background misses alone gives no reason: a
	// later attempt's reason (a test cut short at exit) is the test's.
	got = withEvidence([]TestReceipt{{Name: "o::A", Outcome: "failed"}}, []models.ScopeWindow{
		{Name: "o::A", Evidence: &models.ScopeEvidence{Background: []models.UnmatchedCall{bg("/rates")}}},
		{Name: "o::A", Evidence: &models.ScopeEvidence{Reason: "keploy: o::A was still open when its process exited"}},
	})
	require.Equal(t, "keploy: o::A was still open when its process exited", got[0].Evidence.Reason, "the later attempt's reason was dropped")
}
