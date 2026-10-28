package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// keploy mock under the switch: a recording the scope table marks as the
// application's background work (models.ScopeTableReq.Background) is served
// to its test in the test's window when the call is made, and the test is
// never failed for not making it: the evidence names it apart from the
// unmade calls. Without the mark, or once a table without marks replaced it,
// the test is failed for not making it, as before.
func TestTheScopeTablesBackgroundMarksAreServedNeverRequired(t *testing.T) {
	mocks := func() []*models.Mock {
		return []*models.Mock{pingMock("mock-1", "/order", "placed", t0), pingMock("mock-2", "/tick", "swept", t0.Add(time.Second))}
	}
	table := map[string][]string{"o::TestOrder": {"mock-1", "mock-2"}}
	for _, tc := range []struct {
		name       string
		marks      []models.BackgroundMarks
		paths      []string
		action     string
		unconsumed []string
		background []string
	}{
		{name: "marked, not called", marks: []models.BackgroundMarks{{"o::TestOrder": {"mock-2": "a timer's call"}}}, paths: []string{"/order"}, action: "", background: []string{"mock-2"}},
		{name: "marked, called", marks: []models.BackgroundMarks{{"o::TestOrder": {"mock-2": "a timer's call"}}}, paths: []string{"/order", "/tick"}, action: ""},
		{name: "unmarked, not called", paths: []string{"/order"}, action: models.ScopeActionFail, unconsumed: []string{"mock-2"}},
		// Another test's entry of the same recording is marked, this test's
		// is not: this test still owes the call.
		{name: "marked on another test only, not called", marks: []models.BackgroundMarks{{"o::TestOther": {"mock-2": "a timer's call"}}}, paths: []string{"/order"}, action: models.ScopeActionFail, unconsumed: []string{"mock-2"}},
		{name: "marks cleared by the next table", marks: []models.BackgroundMarks{{"o::TestOrder": {"mock-2": "a timer's call"}}, nil}, paths: []string{"/order"}, action: models.ScopeActionFail, unconsumed: []string{"mock-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p := exactAgent(t, mocks(), table, map[string][]string{})
			for _, m := range tc.marks {
				require.NoError(t, a.SetScopeBackground(context.Background(), m))
			}
			got, e := runTest(t, a, p, "TestOrder", tc.paths...)
			for _, g := range got {
				require.True(t, strings.HasPrefix(g, "200 "), "the test's own recording answers in its window: %v", got)
			}
			require.Equal(t, tc.action, e.Action, e.Reason)
			ev := windowOf(t, a, "o::TestOrder").Evidence
			if ev == nil {
				// A test with nothing to say keeps no evidence.
				ev = &models.ScopeEvidence{}
			}
			require.Equal(t, tc.unconsumed, ev.Unconsumed)
			require.Equal(t, tc.background, ev.Background)
		})
	}
}

// keploy test under the switch: the set's staging hands the agent the test
// cases' recordings marked as background work (models.ExactServing.Unrequired);
// a test case is served its own in its window and is not left owing it. A
// later staging without marks requires every test mock again: names are
// unique only within a set.
func TestTheStagingsBackgroundMarksAreServedNeverRequired(t *testing.T) {
	ctx := context.Background()
	a, p := keployTestAgent(t, []*models.Mock{pingMock("mock-1", "/order", "placed", t0), pingMock("mock-2", "/tick", "swept", t0.Add(time.Second))}, nil)
	stage := func(marks models.BackgroundMarks) {
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime: models.BaseTime, BeforeTime: time.Now(),
			Exact: &models.ExactServing{Stage: true, Unrequired: marks},
		}))
	}
	stage(models.BackgroundMarks{"test-1": {"mock-2": "a timer's call"}, "test-2": {"mock-2": "a timer's call"}})
	got, misses := keployTestCase(t, a, p, "test-1", []string{"mock-1", "mock-2"}, "/order")
	require.Equal(t, []string{"200 placed"}, got)
	require.Empty(t, misses)
	_, _, unconsumed, ok := p.mm.ExactTestOutcome()
	require.True(t, ok)
	require.Empty(t, unconsumed, "a marked recording the test case did not call is not owed")
	test, unused, _ := p.mm.ExactTestUnusedBackground()
	require.Equal(t, "test-1", test)
	require.Equal(t, []string{"mock-2"}, unused)

	got, _ = keployTestCase(t, a, p, "test-2", []string{"mock-1", "mock-2"}, "/order", "/tick")
	require.Equal(t, []string{"200 placed", "200 swept"}, got, "a marked recording answers in its test case's window")
	_, unused, _ = p.mm.ExactTestUnusedBackground()
	require.Empty(t, unused)

	// test-4 lists the same recording with no mark of its own: it owes it.
	keployTestCase(t, a, p, "test-4", []string{"mock-1", "mock-2"}, "/order")
	_, _, unconsumed, _ = p.mm.ExactTestOutcome()
	require.Equal(t, []string{"mock-2"}, unconsumed, "another test case's mark does not reach this one's entry")

	stage(nil)
	keployTestCase(t, a, p, "test-3", []string{"mock-1", "mock-2"}, "/order")
	_, _, unconsumed, _ = p.mm.ExactTestOutcome()
	require.Equal(t, []string{"mock-2"}, unconsumed, "a staging without marks requires every test mock")
}
