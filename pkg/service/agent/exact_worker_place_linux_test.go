//go:build linux

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// workerPlaceAgent is keploy test's agent on a real mock manager, its set
// staged with a worker scope: w-start at the set's start, w-quarantined
// placed after test-1, w-filtered after test-2 and w-later after test-4,
// each answering its own GET path. order is the run's order the staging
// hands it (models.ExactServing.Order), nil for none. Then each of steps is
// served: a test case's name opens it, "" serves the pool between test cases.
func workerPlaceAgent(t *testing.T, order, steps []string) (*Agent, *servingProxy) {
	t.Helper()
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	t.Cleanup(p.mm.Close)
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{
		pingMock("w-start", "/start", "start", t0),
		pingMock("w-quarantined", "/token", "token", t0.Add(time.Second)),
		pingMock("w-filtered", "/refresh", "refresh", t0.Add(2*time.Second)),
		pingMock("w-later", "/later", "later", t0.Add(3*time.Second)),
	}))
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(),
		Exact: &models.ExactServing{Stage: true, Order: order, Worker: []models.MockEntry{
			{Name: "w-start"},
			{Name: "w-quarantined", Place: "test-1"},
			{Name: "w-filtered", Place: "test-2"},
			{Name: "w-later", Place: "test-4"},
		}},
	}))
	for _, step := range steps {
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: step}}), "serve %q", step)
	}
	return a, p
}

// A worker-scope recording placed after a test case answers once keploy
// test's run reaches that place in its own order (models.ExactServing.Order):
// when a test case after it opens, whether the place's test case ran or the
// run skipped it (quarantined, or left out by a --testcase selection), in a
// test case and between test cases. One placed after a test case the run has
// not passed yet (later in the order than the open one) does not answer, and
// one with no place answers from the set's start. A staging that names no
// order reaches a place only as its test case opens, as before; a test case
// the order does not hold passes none; a retry cycle that opens an earlier
// test case again passes nothing more; and a name the order holds twice is
// passed from its first place.
func TestAWorkerPlaceIsReachedOnceTheRunPassesItsTestCase(t *testing.T) {
	order := []string{"test-1", "test-2", "test-3", "test-4"}
	for _, tc := range []struct {
		name  string
		order []string
		steps []string
		want  []string
	}{
		{"a place at a quarantined and at a filtered test case before the open one", order,
			[]string{"test-3"}, []string{"/start", "/token", "/refresh"}},
		{"between test cases, after the open one", order,
			[]string{"test-3", ""}, []string{"/start", "/token", "/refresh"}},
		{"a place at the open test case, the later ones not passed", order,
			[]string{"test-1"}, []string{"/start", "/token"}},
		{"a place at a test case after the open one, reached once it opens", order,
			[]string{"test-3", "", "test-4"}, []string{"/start", "/token", "/refresh", "/later"}},
		{"a retry cycle that opens an earlier test case again", order,
			[]string{"test-3", "", "test-1"}, []string{"/start", "/token", "/refresh"}},
		{"no order: only an opened test case's place", nil,
			[]string{"test-3"}, []string{"/start"}},
		{"a test case the order does not hold", order,
			[]string{"test-x"}, []string{"/start"}},
		{"a name the order holds twice", []string{"test-1", "test-3", "test-2", "test-3", "test-4"},
			[]string{"test-3"}, []string{"/start", "/token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p := workerPlaceAgent(t, tc.order, tc.steps)
			var answered []string
			for _, path := range []string{"/start", "/token", "/refresh", "/later"} {
				if code, _ := p.call(t, path); code == 200 {
					answered = append(answered, path)
				}
			}
			require.Equal(t, tc.want, answered, "after %v the worker scope answered", tc.steps)
		})
	}
}

// As a pass of keploy test's run ends, the replay tells the agent how many
// test cases of the run's order it has passed (models.ExactServing.Passed),
// on a pool call: the test cases it skipped after the last one it opened
// then reach the worker-scope recordings placed after them, and a call the
// application makes after its last test case answers. Here test-4 is
// quarantined (or left out by a selection with test-2 and test-3), or it is
// the streaming test case whose pass has not run yet while test-2 and test-3
// were quarantined: the main loop's end then passes the three before it
// only. A pass end that names more than the order holds passes all of it, one
// with no order reaches nothing, and one under what the run has passed
// already takes back nothing.
func TestTheEndOfAPassReachesThePlacesOfTheTestCasesItSkipped(t *testing.T) {
	order := []string{"test-1", "test-2", "test-3", "test-4"}
	all := []string{"/start", "/token", "/refresh", "/later"}
	for _, tc := range []struct {
		name   string
		order  []string
		steps  []string
		passed int
		want   []string
	}{
		{"a quarantined last test case", order,
			[]string{"test-1", "", "test-2", "", "test-3", ""}, 4, all},
		{"a quarantined last test case, no pass end", order,
			[]string{"test-1", "", "test-2", "", "test-3", ""}, 0, []string{"/start", "/token", "/refresh"}},
		{"a selection that ends before the set does", order,
			[]string{"test-1", ""}, 4, all},
		{"the main loop's end before the streaming pass", order,
			[]string{"test-1", ""}, 3, []string{"/start", "/token", "/refresh"}},
		{"more than the order holds", order,
			[]string{"test-1", ""}, 9, all},
		{"no order", nil,
			[]string{"test-1", ""}, 4, []string{"/start", "/token"}},
		{"under what the run has passed already", order,
			[]string{"test-3", ""}, 1, []string{"/start", "/token", "/refresh"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p := workerPlaceAgent(t, tc.order, tc.steps)
			require.NoError(t, a.UpdateMockParams(context.Background(), models.MockFilterParams{Exact: &models.ExactServing{Passed: tc.passed}}))
			var answered []string
			for _, path := range all {
				if code, _ := p.call(t, path); code == 200 {
					answered = append(answered, path)
				}
			}
			require.Equal(t, tc.want, answered, "after %v and a pass end of %d the worker scope answered", tc.steps, tc.passed)
		})
	}
}

// Under the background rules the view orders what it reaches by the order the
// run reached each place in: a test case the run passed over (skipped) takes
// its place in the run's order, before the test case whose opening passed it,
// so the recordings placed after it come before those of the test cases after
// it, whatever their recorded order.
func TestAPassedOverPlaceTakesItsTurnInTheRunsOrder(t *testing.T) {
	// test-1 and test-3 are skipped: test-2's opening passes test-1, and
	// test-4's passes test-3.
	a, _ := workerPlaceAgent(t, []string{"test-1", "test-2", "test-3", "test-4"}, []string{"test-2", "", "test-4"})
	// w-0, at the set's start, comes before every placed one, though it is
	// recorded after w-1: a place the run reached keeps its turn after the
	// set's start through the later openings.
	worker := []models.MockEntry{{Name: "w-3", Place: "test-3"}, {Name: "w-2", Place: "test-2"}, {Name: "w-1", Place: "test-1"}, {Name: "w-0"}, {Name: "w-4", Place: "test-4"}}
	a.scopeMu.Lock()
	got := reachableWorker(worker, a.testReached, a.testReachAt, &testBackground{})
	a.scopeMu.Unlock()
	require.Equal(t, []string{"w-0", "w-1", "w-2", "w-3", "w-4"}, got, "the background rules' order of the reached places")
}

// The next set's staging starts its run afresh: what the previous set's run
// passed is not carried over, and the new set's test cases are passed from
// the start of its own order.
func TestTheNextSetsStagingPassesItsTestCasesAfresh(t *testing.T) {
	order := []string{"test-1", "test-2", "test-3", "test-4"}
	a, p := workerPlaceAgent(t, order, []string{"test-4", ""})
	require.NoError(t, a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(),
		Exact: &models.ExactServing{Stage: true, Order: order, Worker: []models.MockEntry{
			{Name: "w-quarantined", Place: "test-1"},
			{Name: "w-filtered", Place: "test-2"},
		}},
	}))
	code, _ := p.call(t, "/token")
	require.Equal(t, 400, code, "the previous set's run reached a place of the next set's")
	require.NoError(t, a.UpdateMockParams(context.Background(), models.MockFilterParams{Exact: &models.ExactServing{Test: "test-2"}}))
	code, _ = p.call(t, "/token")
	require.Equal(t, 200, code, "the next set's test-2 did not pass its test-1")
	code, _ = p.call(t, "/refresh")
	require.Equal(t, 200, code, "the next set's test-2 did not reach its own place")
}
